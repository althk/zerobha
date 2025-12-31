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
	"zerobha/pkg/strategy"

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
	http.HandleFunc("/zerobha/cb/rt", func(w http.ResponseWriter, r *http.Request) {
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
	flag.Parse()

	// Load Config
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Parse Timeframe
	tf, err := time.ParseDuration(cfg.Timeframe)
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
	logFileName := fmt.Sprintf("logs/zerobha_%s.log", today.Format("2006-01-02"))
	logFile, _ := os.OpenFile(logFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	defer func(logFile *os.File) {
		_ = logFile.Close()
	}(logFile)
	// Write to BOTH terminal and file
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	log.SetFlags(log.LstdFlags | log.Lshortfile)

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
	log.Println("=== STRATEGY: ", cfg.Strategy)
	log.Println("=== TIMEFRAME: ", tf)
	log.Println("=== CSV FILE: ", cfg.CSVFile)
	log.Println("=== LIMIT: ", cfg.Limit)

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
	watchlist, err := loadSymbolsFromCSV(cfg.CSVFile)
	if err != nil {
		log.Printf("WARNING: Failed to load %s: %v. Using fallback list.", cfg.CSVFile, err)
		// Fallback list
		watchlist = []string{"IDEA", "CANBK", "LTF", "NBCC", "RELIANCE"}
	}

	// Limit symbols if requested
	if cfg.Limit > 0 && len(watchlist) > cfg.Limit {
		fmt.Printf("Limiting watchlist to top %d symbols.\n", cfg.Limit)
		watchlist = watchlist[:cfg.Limit]
	}

	fmt.Printf("Loaded %d symbols for trading.\n", len(watchlist))
	log.Println("Fetching Instrument Master list from Zerodha...")
	symbolToToken, tokenToSymbol, err := fetchInstruments(kc)
	if err != nil {
		log.Fatalf("Failed to fetch instruments: %v", err)
	}
	log.Printf("Loaded %d instruments", len(symbolToToken))

	// Database (SQLite)
	store, err := db.NewStore("zerobha.db")
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
	// Broker Adapter (The Execution Arm)
	kiteAdapter := broker.NewZerodhaAdapter(kc, symbolToToken)

	// Risk Manager (The Gatekeeper) - Max Loss ₹5000, Max 10 Trades Total, Max 3 Per Stock
	riskMgr := risk.NewManager(store, decimal.NewFromInt(5000), 10, 3)

	// Strategy (The Brain)
	var strat core.Strategy
	switch cfg.Strategy {
	case "orb":
		strat = strategy.NewORBStrategy(watchlist)
	case "adx_rsi_vwap":
		strat = strategy.NewAdxRsiVwap(watchlist)
	case "ema_trend_angle":
		strat = strategy.NewEmaTrendAngle(watchlist, cfg.Timeframe)
	default:
		log.Printf("Using default strategy: Donchian Breakdown")
		strat = strategy.NewDonchianBreakout(watchlist)
	}

	// Inject DB if Strategy supports it (Manual Dependency Injection)
	// Currently only ORBStrategy supports it
	if orbStrat, ok := strat.(*strategy.ORBStrategy); ok {
		orbStrat.SetDB(store)
	}

	strat.Init(kiteAdapter)

	// Journal
	j, err := journal.NewJournal(fmt.Sprintf("logs/journal_%s.csv", today.Format("2006-01-02")))
	if err != nil {
		log.Printf("WARNING: Failed to create journal: %v", err)
	}
	defer j.Close()

	// Instrument Manager
	im := broker.NewInstrumentManager()
	log.Println("Fetching Instruments (NSE & NFO)...")
	if err := im.FetchInstruments(kc); err != nil {
		log.Fatalf("Failed to fetch instruments: %v", err)
	}

	// Engine (The Orchestrator)
	engine := core.NewEngine(strat, kiteAdapter, riskMgr, j, im, store)

	// Web Dashboard
	webServer := web.NewServer(engine, 8080)
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
		// Target SquareOff: 15:13 Today
		targetSquareOff := time.Date(now.Year(), now.Month(), now.Day(), 15, 13, 0, 0, loc)
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
		log.Printf("Scheduled Auto-SquareOff in %v (at 15:13 IST)", durationSquareOff)
		time.AfterFunc(durationSquareOff, func() {
			log.Println("⏰ 15:13 PM Trigger: Initiating Auto-SquareOFF...")
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
func fetchInstruments(kc *kiteconnect.Client) (map[string]uint32, map[uint32]string, error) {
	instruments, err := kc.GetInstrumentsByExchange("NSE") // Fetch only NSE
	if err != nil {
		return nil, nil, err
	}

	symToTok := make(map[string]uint32)
	tokToSym := make(map[uint32]string)

	for _, inst := range instruments {
		if inst.Exchange != "NSE" {
			continue
		}

		symToTok[inst.Tradingsymbol] = uint32(inst.InstrumentToken)
		tokToSym[uint32(inst.InstrumentToken)] = inst.Tradingsymbol
	}

	return symToTok, tokToSym, nil
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
