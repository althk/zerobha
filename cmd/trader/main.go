package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/internal/web"
	"zerobha/pkg/broker"
	"zerobha/pkg/db"
	"zerobha/pkg/journal"
	"zerobha/pkg/options"
	"zerobha/pkg/strategy"
	"zerobha/pkg/upstox"

	"github.com/shopspring/decimal"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	kitemodels "github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"
)

// HolidayResponse (Upstox API Format)
type HolidayResponse struct {
	Status string    `json:"status"`
	Data   []Holiday `json:"data"`
}

type Holiday struct {
	Date            string   `json:"date"`
	HolidayType     string   `json:"holiday_type"`
	ClosedExchanges []string `json:"closed_exchanges"`
}

func fetchRequestToken(loginURL string) (string, error) {
	fmt.Println("Open the following url in your browser:\n", loginURL)

	// Obtain request token after Kite Connect login flow
	// Run a temporary server to listen for callback
	srv := &http.Server{Addr: ":9880"}
	var requestToken string
	http.HandleFunc("/auth/kite/callback", func(w http.ResponseWriter, r *http.Request) {
		requestToken = r.URL.Query()["request_token"][0]
		log.Println("request token", requestToken)
		go func() {
			_ = srv.Shutdown(context.TODO())
		}()
		_, _ = w.Write([]byte("login successful!"))
	})
	err := srv.ListenAndServe()
	return requestToken, err
}

func main() {
	// Config Path
	configPath := flag.String("config", "config.toml", "Path to config file")
	paperFlag := flag.Bool("paper", false, "Enable paper trading mode with virtual execution and live market feeds")
	flag.Parse()

	// Load Config
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	ss := cfg.ActiveStrategySettings()

	// Parse Timeframe
	tf, err := time.ParseDuration(ss.Timeframe)
	if err != nil {
		log.Fatalf("Invalid timeframe: %v", err)
	}
	log.Printf("Timeframe set to: %s", tf)

	apiKey, apiSecret := cfg.APIKey, cfg.APISecret
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		log.Printf("WARNING: Could not load Asia/Kolkata timezone: %v", err)
		loc = time.Local
	}
	today := time.Now().In(loc)

	// Failing to open the log file used to be discarded with `logFile, _ :=`.
	// Because os.Stdout comes first in the MultiWriter the process then kept
	// logging normally, so a deployment writing its journal nowhere looked
	// perfectly healthy in `docker logs`. Report it, and fall back to stdout
	// alone rather than wiring a nil file into the writer.
	if err := os.MkdirAll(cfg.Paths.LogDir, 0o755); err != nil {
		log.Printf("WARNING: cannot create log directory %s: %v - logging to stdout only", cfg.Paths.LogDir, err)
	}
	logFileName := filepath.Join(cfg.Paths.LogDir, fmt.Sprintf("zerobha_%s.log", today.Format("2006-01-02")))
	if logFile, err := os.OpenFile(logFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666); err != nil {
		log.Printf("WARNING: cannot open log file %s: %v - logging to stdout only", logFileName, err)
	} else {
		defer func() { _ = logFile.Close() }()
		// Write to BOTH terminal and file
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	}
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("Paths: db=%s log_dir=%s", cfg.Paths.DBPath, cfg.Paths.LogDir)

	if isMarketClosed() {
		log.Println("Market is closed today, not starting trader")
		return
	}

	if !isTradingTime() {
		log.Println("Outside trading hours (08:55 - 15:30 IST), not starting trader")
		return
	}

	// 1. Initialization
	log.Println("=== ZEROBHA LIVE TRADING SYSTEM STARTING ===")
	logConfig(cfg, ss, tf)

	// Initialize Kite Client (REST API)
	kc := kiteconnect.New(apiKey)
	requestToken, err := fetchRequestToken(kc.GetLoginURL())
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("Error fetching request token: %v", err)
	}

	data, err := kc.GenerateSession(requestToken, apiSecret)
	if err != nil {
		log.Fatalf("Error generating Kite session: %v", err)
	}

	kc.SetAccessToken(data.AccessToken)

	// 2. Instrument Mapping (Crucial	// Load symbols from CSV
	watchlist, err := loadSymbolsFromCSV(ss.CSVFile)
	if err != nil {
		log.Printf("WARNING: Failed to load %s: %v. Using fallback list.", ss.CSVFile, err)
		// Fallback list
		watchlist = []string{"IDEA", "CANBK", "LTF", "NBCC", "RELIANCE"}
	}

	// Limit symbols if requested
	if ss.Limit > 0 && len(watchlist) > ss.Limit {
		fmt.Printf("Limiting watchlist to top %d symbols.\n", ss.Limit)
		watchlist = watchlist[:ss.Limit]
	}

	// One instrument dump serves the NSE cash symbols the equity strategies
	// watch, and the NFO contracts the option work will need.
	im := broker.NewInstrumentManager()
	log.Println("Fetching Instrument Master list from Zerodha (NSE & NFO)...")
	if err := im.FetchInstruments(kc); err != nil {
		log.Fatalf("Failed to fetch instruments: %v", err)
	}
	symbolToToken, tokenToSymbol := im.SymbolTokenMaps()
	log.Printf("Loaded %d instruments", len(symbolToToken))

	// Broker Adapter (The Execution Arm).
	kiteAdapter := broker.NewZerodhaAdapter(kc, symbolToToken)

	fmt.Printf("Loaded %d symbols for trading.\n", len(watchlist))

	// Database (SQLite)
	// db_path may name a subdirectory (the container points it at the data
	// volume as "data/zerobha.db"), and SQLite will not create one.
	if dir := filepath.Dir(cfg.Paths.DBPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("Failed to create database directory %s: %v", dir, err)
		}
	}
	store, err := db.NewStore(cfg.Paths.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer store.Close()

	// 3. Define Watchlist (The stocks you want to trade)
	// Already loaded from final_portfolio.csv above
	var tokensToSubscribe []uint32

	for _, sym := range watchlist {
		if token, ok := symbolToToken[sym]; ok {
			tokensToSubscribe = append(tokensToSubscribe, token)
		} else {
			log.Printf("WARNING: Symbol %s not found in instrument list", sym)
		}
	}

	// 4. Setup Core Components
	// Risk Manager (The Gatekeeper)
	riskMgr := risk.NewManager(store,
		decimal.NewFromInt(int64(cfg.Risk.MaxDailyLoss)),
		cfg.Risk.MaxTradesPerDay,
		cfg.Risk.MaxTradesPerStock,
	)

	// Strategy (The Brain). ORB and gapfade are the intraday (MIS) strategies
	// wired for live trading; dailyrev is backtest-only research (CNC,
	// multi-day holds) whose positions the live loop's square-off would close
	// the same day. Fail loudly on anything else rather than logging the
	// configured name and silently running ORB against another strategy's
	// watchlist and timeframe.
	var strat core.Strategy
	var maxConcurrent int
	switch cfg.Strategy {
	case "", config.StrategyORB:
		orb := strategy.NewORBStrategy(watchlist, cfg.ORB)
		// Inject DB (Manual Dependency Injection) so opening ranges and
		// per-day state survive a mid-session restart.
		orb.SetDB(store)
		strat = orb
		maxConcurrent = cfg.ORB.MaxConcurrent
	case config.StrategyGapFade:
		gate, err := buildUpstoxGate(cfg)
		if err != nil {
			log.Fatalf("gapfade needs the Upstox news/earnings gate: %v", err)
		}
		strat = strategy.NewGapFadeStrategy(watchlist, cfg.GapFade, gate)
		maxConcurrent = cfg.GapFade.MaxConcurrent
	case config.StrategyDonchian:
		dc := strategy.NewDonchianStrategy(watchlist, cfg.Donchian)
		dc.SetOptionExecution(buildOptionExecutor(cfg, im, kc))
		strat = dc
		maxConcurrent = cfg.Donchian.MaxConcurrent
	default:
		log.Fatalf("live trading supports strategy=%q, %q or %q, got %q. %q is backtest-only (go run ./cmd/backtest -strategy %s).",
			config.StrategyORB, config.StrategyGapFade, config.StrategyDonchian,
			cfg.Strategy, cfg.Strategy, cfg.Strategy)
	}

	if err := strat.Init(kiteAdapter); err != nil {
		log.Printf("WARNING: strategy Init failed: %v", err)
	}

	// Journal
	j, err := journal.NewJournal(filepath.Join(cfg.Paths.LogDir, fmt.Sprintf("journal_%s.csv", today.Format("2006-01-02"))))
	if err != nil {
		log.Printf("WARNING: Failed to create journal: %v", err)
	} else {
		defer j.Close()
	}

	// Declared ahead of the broker so the paper adapter's leverage lookup can
	// close over it; it is assigned immediately below.
	var engine *core.Engine

	// Broker Adapter Selection (Live vs Paper)
	isPaper := cfg.PaperTrading || *paperFlag
	var brokerAdapter core.Broker = kiteAdapter
	var paperAdapter *broker.PaperAdapter
	if isPaper {
		paperCap := decimal.NewFromFloat(cfg.PaperCapital)
		if !paperCap.IsPositive() {
			paperCap = decimal.NewFromInt(1000000) // Default Rs 10 Lakhs
		}
		// The engine sizes MIS positions with leverage, so the paper broker has
		// to block margin on the same basis or it would reject positions the
		// real account takes. The map is loaded by the engine below, hence the
		// closure — it reads whatever the engine ended up with.
		paperAdapter = broker.NewPaperAdapter(kiteAdapter, paperCap,
			broker.WithPaperStore(store),
			broker.WithPaperLeverage(func(symbol string) float64 {
				if engine == nil {
					return 1
				}
				if lev, ok := engine.LeverageMap[symbol]; ok {
					return lev
				}
				return 1
			}))
		brokerAdapter = paperAdapter
		log.Println("==================================================================")
		log.Printf("  >>> [PAPER TRADING MODE ACTIVE] Virtual Capital: Rs %s <<<", paperCap.StringFixed(2))
		log.Println("  Live market data feeds active. Real orders will NOT be placed.")
		log.Println("  Stops and targets are held and filled by the paper broker.")
		log.Println("==================================================================")
	}

	// Engine (The Orchestrator)
	engine = core.NewEngine(strat, brokerAdapter, riskMgr, j, im, store)
	if paperAdapter != nil {
		// Refreshes marks for instruments the ticker does not subscribe to —
		// an option contract above all — so their stops can still fire.
		paperAdapter.Start()
		defer paperAdapter.Close()
	}
	engine.MaxConcurrent = maxConcurrent
	engine.UptrendOnly = *cfg.UptrendOnly
	engine.DataProvider = kiteAdapter
	engine.MinBalance = int64(cfg.Engine.MinBalance)
	engine.MinCapitalPerTrade = int64(cfg.Engine.MinCapitalPerTrade)
	engine.MaxCapitalPerTrade = int64(cfg.Engine.MaxCapitalPerTrade)
	engine.TradeCutoffMin = cfg.Engine.TradeCutoffMin

	if cfg.Strategy == config.StrategyDonchian {
		// The engine's global cutoff gates every signal and is earlier than
		// Donchian's own last-entry time, so it has to give way to it.
		engine.TradeCutoffMin = cfg.Donchian.EntryCutoffMin + 1
		// Same reasoning for capital: the engine's cap is sized for cash
		// equities, and an index priced above it is not traded smaller, it is
		// not traded at all.
		if cfg.Donchian.MaxCapitalPerTrade > 0 {
			engine.MaxCapitalPerTrade = cfg.Donchian.MaxCapitalPerTrade
			log.Printf("Donchian: max capital per trade Rs%d (overrides [engine] Rs%d)",
				cfg.Donchian.MaxCapitalPerTrade, int64(cfg.Engine.MaxCapitalPerTrade))
		}
		// The NIFTY uptrend filter blocks every signal on a down day, not just
		// the longs. On a symmetric long/short strategy that is not a filter,
		// it is a switch that turns off half the strategy on exactly the days
		// the short side exists to trade.
		if engine.UptrendOnly {
			log.Println("Donchian: disabling the NIFTY uptrend filter — it would gate the short side out of existence")
			engine.UptrendOnly = false
		}
		// Donchian states its kill switch as a percent of capital, not rupees.
		if balance, err := brokerAdapter.GetBalance(); err == nil {
			riskMgr.MaxDailyLoss = balance.Mul(decimal.NewFromFloat(cfg.Donchian.MaxDailyLossPct / 100))
			log.Printf("Donchian kill switch: %.2f%% of Rs%s = Rs%s",
				cfg.Donchian.MaxDailyLossPct, balance.StringFixed(0), riskMgr.MaxDailyLoss.StringFixed(0))
		} else {
			log.Printf("WARNING: could not read balance to size the daily-loss limit (%v); keeping [risk] max_daily_loss = %d",
				err, cfg.Risk.MaxDailyLoss)
		}
	}
	engine.InitNiftyEMAs()

	// Web Dashboard
	webServer := web.NewServer(engine, 9080, isPaper)
	go webServer.Start()

	// 5. Setup Data Pipeline

	// Channel to carry completed candles from Aggregator to Engine
	candleChan := make(chan models.Candle, 100)

	// The Aggregator (Calculated Timeframe Candles)
	builders := make(map[uint32]*core.CandleBuilder)
	for _, token := range tokensToSubscribe {
		// Create a builder for each subscribed token
		builders[token] = core.NewCandleBuilder(tf, candleChan)
	}

	// 6. Start the Engine Listener (Consumer)
	// This runs in the background and processes candles as they finish
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Println("Engine listening for candles...")
		for candle := range candleChan {
			log.Printf("CANDLE CLOSED: %s [%s] Close: %s", candle.Symbol, candle.Timeframe, candle.Close)

			// This is where the magic happens
			engine.Execute(candle)
		}
	}()

	// 7. Setup WebSocket Ticker (Producer)
	ticker := kiteticker.New(apiKey, data.AccessToken)
	var lastVolMap = sync.Map{}
	// Callback: Triggered when a price update arrives
	ticker.OnTick(func(tick kitemodels.Tick) {
		// A. Convert Kite Tick -> Zerobha Tick
		// We need to look up the Symbol Name using the Token
		symbolName := tokenToSymbol[tick.InstrumentToken]

		// Handle time (Kite sends nil time if no trade occurred recently)
		loc, _ := time.LoadLocation("Asia/Kolkata")
		tickTime := time.Now().In(loc)
		if !tick.LastTradeTime.IsZero() {
			tickTime = tick.LastTradeTime.Time
		}
		// FILTER: Ignore Pre-market data
		if tickTime.Hour() < 9 || (tickTime.Hour() == 9 && tickTime.Minute() < 15) {
			return
		}

		zTick := models.Tick{
			Symbol:    symbolName,
			Price:     decimal.NewFromFloat(tick.LastPrice),
			Volume:    decimal.NewFromInt(int64(tick.VolumeTraded)), // Usually cumulative, diff logic needed for precise vol
			Timestamp: tickTime,
		}
		currentVol := tick.VolumeTraded
		var deltaVol uint32

		// LoadOrStore returns the existing value if present, otherwise stores currentVol
		lastVolVal, _ := lastVolMap.LoadOrStore(tick.InstrumentToken, uint32(0))
		lastVol := lastVolVal.(uint32)
		deltaVol = currentVol - lastVol
		lastVolMap.Store(tick.InstrumentToken, currentVol) // Update for next tick

		// Use deltaVol in your zTick
		zTick.Volume = decimal.NewFromInt(int64(deltaVol))

		// B. Push to the specific Aggregator for this token
		if builder, exists := builders[tick.InstrumentToken]; exists {
			builder.Update(zTick)
		}

		// C. Breakeven-trail check: must run on every tick (not just candle
		// close) since price can cross the partial-exit level intracandle.
		engine.OnTick(symbolName, zTick.Price)
	})

	ticker.OnError(func(err error) {
		log.Printf("Ticker Error: %v\n", err)
	})

	ticker.OnConnect(func() {
		log.Println("Connected to Zerodha WebSocket. Subscribing...")
		err := ticker.Subscribe(tokensToSubscribe)
		if err != nil {
			log.Printf("Subscription failed: %v", err)
		}
		// Set mode to Full to get Volume/OHLC data
		_ = ticker.SetMode(kiteticker.ModeFull, tokensToSubscribe)
	})

	// 8. Start Everything
	// Handle graceful shutdown via Ctrl+C
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Scheduler for SquareOff (15:13), EOD Flush (15:23) and Shutdown (15:30)
	go func() {
		loc, _ := time.LoadLocation("Asia/Kolkata")
		now := time.Now().In(loc)

		// Target Ticker Stop: 15:05 Today
		targetTickerStop := time.Date(now.Year(), now.Month(), now.Day(), 15, 5, 0, 0, loc)
		// Target SquareOff: 15:13 Today, or the strategy's own earlier flatten.
		squareOffMin := 15*60 + 13
		if cfg.Strategy == config.StrategyDonchian {
			squareOffMin = cfg.Donchian.SquareOffMin
		}
		targetSquareOff := time.Date(now.Year(), now.Month(), now.Day(), squareOffMin/60, squareOffMin%60, 0, 0, loc)
		// Target Flush: 15:23 Today
		targetFlush := time.Date(now.Year(), now.Month(), now.Day(), 15, 23, 0, 0, loc)
		// Target Shutdown: 15:30 Today
		targetShutdown := time.Date(now.Year(), now.Month(), now.Day(), 15, 30, 0, 0, loc)

		// 0. Handle Ticker Stop
		durationTickerStop := targetTickerStop.Sub(now)
		log.Printf("Scheduled Ticker Stop in %v (at 15:05 IST)", durationTickerStop)
		time.AfterFunc(durationTickerStop, func() {
			log.Println("⏰ 15:05 PM Trigger: Stopping Ticker (Strategy signaling ends)...")
			ticker.Stop()
		})

		// 1. Handle SquareOff
		durationSquareOff := targetSquareOff.Sub(now)
		log.Printf("Scheduled Auto-SquareOff in %v (at %02d:%02d IST)", durationSquareOff, squareOffMin/60, squareOffMin%60)
		time.AfterFunc(durationSquareOff, func() {
			log.Println("⏰ Square-off trigger: Initiating Auto-SquareOFF...")
			engine.SquareOff()
		})

		// 2. Handle Flush
		durationFlush := targetFlush.Sub(now)
		log.Printf("Scheduled EOD Flush in %v (at 15:23 IST)", durationFlush)
		time.AfterFunc(durationFlush, func() {
			log.Println("⏰ 15:23 PM Trigger: Flushing candles for EOD processing...")
			for _, b := range builders {
				b.Flush()
			}
		})

		// 3. Handle Shutdown
		durationShutdown := targetShutdown.Sub(now)
		log.Printf("Scheduled Auto-Shutdown in %v (at 15:30 IST)", durationShutdown)
		time.AfterFunc(durationShutdown, func() {
			log.Println("🛑 15:30 PM Trigger: Market Closed. Initiating Auto-Shutdown...")
			// Send signal to stop channel to trigger graceful shutdown
			stop <- syscall.SIGTERM
		})
	}()

	go func() {
		// Blocking call that runs the Ticker
		ticker.Serve()
	}()

	log.Println("System is LIVE. Waiting for ticks... (Press Ctrl+C to stop)")

	// 9. Shutdown Waiter
	<-stop
	log.Println("Shutdown signal received. Closing connections...")

	// Shutdown Web Server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := webServer.Stop(ctx); err != nil {
		log.Printf("Web Server Shutdown Error: %v", err)
	} else {
		log.Println("Web Server stopped gracefully")
	}

	close(candleChan)
	log.Println("Waiting for engine to finish processing...")
	wg.Wait()

	log.Println("Zerobha Shutdown Complete.")
}

func logConfig(cfg *config.Config, ss config.StrategySettings, tf time.Duration) {
	log.Println("=== STRATEGY:", cfg.Strategy)
	log.Println("=== UPTREND ONLY:", *cfg.UptrendOnly)
	log.Printf("=== TIMEFRAME: %s | CSV: %s | LIMIT: %d", tf, ss.CSVFile, ss.Limit)

	if cfg.Strategy == config.StrategyGapFade {
		c := cfg.GapFade
		log.Printf("--- GAPFADE CONFIG ---")
		log.Printf("  Gap Band         : %.2f%% – %.2f%% down", c.MinGapDownPct, c.MaxGapDownPct)
		log.Printf("  Observe Until    : %02d:%02d   Entry Until: %02d:%02d",
			c.ObserveEndMin/60, c.ObserveEndMin%60, c.EntryWindowEnd/60, c.EntryWindowEnd%60)
		log.Printf("  Stop / RR        : %.2f ATR (max %.2f%%) / 1:%.2f", c.SLATR, c.MaxStopPct, c.RewardRisk)
		log.Printf("  Stop At Day Low  : %v  Require Above VWAP: %v", *c.StopAtDayLow, *c.RequireAboveVWAP)
		log.Printf("  Max Concurrent   : %d  Gate Fail Open: %v", c.MaxConcurrent, *c.GateFailOpen)
		log.Printf("  Gate             : news %dh lookback, max profit drop %.1f%%, ISIN map %s",
			cfg.Upstox.NewsLookbackHours, cfg.Upstox.MaxProfitDropPct, cfg.Upstox.ISINCSV)
		log.Println("==========================================")
		return
	}

	{
		c := cfg.ORB
		log.Printf("--- ORB CONFIG ---")
		log.Printf("  Entry Window End : %d min from midnight (%02d:%02d)", c.EntryWindowEnd, c.EntryWindowEnd/60, c.EntryWindowEnd%60)
		log.Printf("  RSI Long/Short   : %.1f / %.1f", c.RSILongThreshold, c.RSIShortThreshold)
		log.Printf("  ADX Threshold    : %.1f  (Rising Eps: %.2f)", c.ADXThreshold, c.ADXRisingEps)
		log.Printf("  ATR Range        : %.2f – %.2f", c.MinRangeATR, c.MaxRangeATR)
		log.Printf("  SL/Target Mult   : %.2fx / %.2fx", c.SLMultiplier, c.TargetMultiplier)
		log.Printf("  Max Concurrent   : %d", c.MaxConcurrent)
		log.Printf("  Rel Vol Thresh   : %.2f  Vol Thrust Mult: %.2f", c.RelVolThreshold, c.VolThrustMult)
		log.Printf("  Max VWAP Dist    : %.2f ATR / %.2f%%", c.MaxVWAPDistATR, c.MaxVWAPDistPct)
		log.Printf("  Body Strength    : %.2f  Max Gap Pct: %.2f%%", c.BodyStrengthThreshold, c.MaxGapPct)
		log.Printf("  One Trade/Day    : %v  Stop Floor At Range: %v", *c.OneTradePerDay, *c.StopFloorAtRange)
	}
	log.Println("==========================================")
}

func isMarketClosed() bool {
	if isHoliday() || isWeekend() {
		return true
	}
	return false
}

func isWeekend() bool {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		log.Printf("WARNING: Could not load Asia/Kolkata timezone: %v", err)
		loc = time.Local
	}
	today := time.Now().In(loc)
	if today.Weekday() == time.Saturday || today.Weekday() == time.Sunday {
		log.Println("🛑 MARKET CLOSED: ", today.Weekday())
		return true
	}
	return false
}

// isHoliday returns true if today is an NSE trading holiday
func isHoliday() bool {
	// 1. Get correct date in India Time
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		log.Printf("WARNING: Could not load Asia/Kolkata timezone: %v\n", err)
		loc = time.Local
	}
	today := time.Now().In(loc).Format("2006-01-02")
	log.Printf("Checking holiday status for: %s\n", today)

	// 2. Fetch from Upstox
	resp, err := http.Get("https://api.upstox.com/v2/market/holidays")
	if err != nil {
		log.Printf("ERROR: Failed to fetch holidays: %v. Assuming market OPEN.", err)
		return false // Fail open (risky) or fail closed (safe)? For bots, usually fail open but alert.
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	body, _ := io.ReadAll(resp.Body)

	var holidayResp HolidayResponse
	if err := json.Unmarshal(body, &holidayResp); err != nil {
		log.Printf("ERROR: Failed to parse holiday JSON: %v", err)
		return false
	}

	// 3. Scan the list
	for _, h := range holidayResp.Data {
		if h.Date == today {
			// Found today in the list. Check if NSE is closed.
			for _, ce := range h.ClosedExchanges {
				if ce == "NSE" {
					log.Println("🛑 MARKET CLOSED: Today is", h.Date, "(", h.HolidayType, ")")
					return true
				}
			}
		}
	}

	return false
}

func isTradingTime() bool {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		log.Printf("WARNING: Could not load Asia/Kolkata timezone: %v", err)
		loc = time.Local
	}
	now := time.Now().In(loc)

	// Start: 08:55 AM
	start := time.Date(now.Year(), now.Month(), now.Day(), 8, 55, 0, 0, loc)
	// End: 15:05 PM
	end := time.Date(now.Year(), now.Month(), now.Day(), 15, 05, 0, 0, loc)

	if now.Before(start) || now.After(end) {
		log.Printf("Current time %s is outside trading window (%s - %s)", now.Format("15:04"), start.Format("15:04"), end.Format("15:04"))
		return false
	}
	return true
}

// fetchInstruments downloads the master CSV from Zerodha and builds lookup maps
// buildOptionExecutor assembles the live option-execution path: the Kite
// instrument dump as the chain, Kite quotes as the premium source, and
// pkg/options for the strike selection.
//
// The selection code is shared with cmd/optbt, which is the point — a strike
// chosen in a backtest is chosen the same way in production, rather than by two
// lookalike implementations that drift apart.
func buildOptionExecutor(cfg *config.Config, im *broker.InstrumentManager, kc *kiteconnect.Client) strategy.OptionExecutor {
	quote := func(tokens []uint32) (map[uint32]float64, error) {
		if len(tokens) == 0 {
			return map[uint32]float64{}, nil
		}
		keys := make([]string, 0, len(tokens))
		byKey := make(map[string]uint32, len(tokens))
		for _, tok := range tokens {
			k := strconv.FormatUint(uint64(tok), 10)
			keys = append(keys, k)
			byKey[k] = tok
		}
		quotes, err := kc.GetLTP(keys...)
		if err != nil {
			return nil, err
		}
		out := make(map[uint32]float64, len(quotes))
		for k, q := range quotes {
			if tok, ok := byKey[k]; ok {
				out[tok] = q.LastPrice
			}
		}
		return out, nil
	}

	minDTE := 2
	if cfg.Donchian.MinDaysToExpiry != nil {
		minDTE = *cfg.Donchian.MinDaysToExpiry
	}
	nearExpiryDelta := cfg.Donchian.TargetDeltaNearExpiry
	if nearExpiryDelta == 0 {
		nearExpiryDelta = 0.90
	}
	log.Printf("Donchian option execution: target delta %.2f (DTE<=3 delta %.2f), min %d day(s) to expiry, fallback IV %.1f%%",
		cfg.Donchian.TargetDelta, nearExpiryDelta, minDTE, cfg.Donchian.FallbackIV*100)
	if minDTE == 0 {
		log.Println("WARNING: min_days_to_expiry = 0 — expiry-day trades measured at -1571 bps each. See CLAUDE.md.")
	}

	return broker.OptionExecutor{
		Chain: broker.NewKiteChain(im, quote),
		Selector: options.Selector{
			TargetDelta:           cfg.Donchian.TargetDelta,
			TargetDeltaNearExpiry: nearExpiryDelta,
			MinDaysToExpiry:       minDTE,
			FallbackIV:            cfg.Donchian.FallbackIV,
		},
	}
}

func loadSymbolsFromCSV(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	var symbols []string
	// Skip header (row 0)
	for i, record := range records {
		if i == 0 {
			continue
		}
		if len(record) > 0 {
			// Remove .NS suffix if present (though final_portfolio shouldn't have it)
			sym := strings.ToUpper(strings.ReplaceAll(record[0], ".NS", ""))
			symbols = append(symbols, sym)
		}
	}
	return symbols, nil
}

// buildUpstoxGate assembles the news/earnings gate the gap-fade strategy
// consults before fading a gap down. It is built at startup, not lazily, so a
// missing token or an unreadable ISIN file stops the trader before the open
// rather than blocking every signal at 09:35.
func buildUpstoxGate(cfg *config.Config) (core.NewsGate, error) {
	if cfg.UpstoxAccessToken == "" {
		return nil, fmt.Errorf("upstox_access_token is empty (it must sit above the first [section] header in the TOML)")
	}
	lookup, err := upstox.LoadISINCSV(cfg.Upstox.ISINCSV)
	if err != nil {
		return nil, err
	}
	log.Printf("Upstox gate: loaded %d symbol→ISIN mappings from %s", lookup.Len(), cfg.Upstox.ISINCSV)

	client := upstox.NewClient(cfg.UpstoxAccessToken, time.Duration(cfg.Upstox.TimeoutSeconds)*time.Second)
	return upstox.NewGate(client, lookup, upstox.GateConfig{
		NewsLookback:      time.Duration(cfg.Upstox.NewsLookbackHours) * time.Hour,
		BlockKeywords:     cfg.Upstox.BlockKeywords,
		MaxProfitDropPct:  cfg.Upstox.MaxProfitDropPct,
		MaxResultsAgeDays: cfg.Upstox.MaxResultsAgeDays,
	}), nil
}
