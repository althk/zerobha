package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"zerobha/pkg/statistics"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/indicators"
	"zerobha/pkg/strategy" // Import your strategies package

	"github.com/shopspring/decimal"
)

var istLoc = time.FixedZone("IST", 5*3600+1800)

func main() {
	// Parse command line flags
	startDateStr := flag.String("start", "", "Start date for backtest (YYYY-MM-DD)")
	endDateStr := flag.String("end", "", "End date for backtest (YYYY-MM-DD)")
	strategyName := flag.String("strategy", "orb", "Strategy to run: orb, gapfade, donchian, srlevels (all intraday 5m) or dailyrev (daily short-term reversal)")
	csvFile := flag.String("csv", "high_beta_stocks.csv", "CSV file containing symbols")
	minBeta := flag.Float64("min-beta", 0.0, "Minimum Beta threshold for stock selection")
	timeframe := flag.String("timeframe", "5m", "Timeframe for candles (e.g. 1d, 1h)")
	limit := flag.Int("limit", -1, "Limit number of symbols to process (default -1: all)")
	baseline := flag.Bool("baseline", false, "ORB only: revert the new robustness/signal knobs to original behavior for A/B comparison")
	costBps := flag.Float64("cost-bps", 0.0, "Round-trip transaction cost in basis points of turnover, deducted per trade (e.g. 6 = 0.06%)")
	knobs := flag.String("knobs", "", "ORB ablation: start from baseline and enable only these new knobs (comma list of: onetrade,stopfloor,vwapdist,thrust,adxeps)")
	tradesCSV := flag.String("trades-csv", "", "Write every trade (all symbols, pre-cost PnL) to this CSV for offline analysis")
	uptrend := flag.Bool("uptrend", false, "Engage the NIFTY-50 uptrend filter (gates long signals to days NIFTY is above EMA50/EMA200)")
	configFile := flag.String("config", "config.local.toml", "TOML config file (e.g. config.local.toml); strategy/risk settings come from it, explicit flags still win")
	// srlevels sweep knobs. The strategy is specified with two choices left
	// open — which pivot formula, and what stop/target multiples — so they are
	// swept from the command line rather than by editing a config per cell.
	srPivot := flag.String("sr-pivot", "", "srlevels: pivot formula, traditional or fibonacci (overrides config)")
	srSL := flag.Float64("sr-sl", 0, "srlevels: stop distance in ATR (overrides config)")
	srTP := flag.Float64("sr-tp", -1, "srlevels: target distance in ATR; 0 disables the target (overrides config)")
	srHTF := flag.String("sr-htf", "", "srlevels: higher-timeframe confirmation, on or off (overrides config)")
	srRoom := flag.Float64("sr-room", -1, "srlevels: minimum ATR of room to the next opposing zone; 0 disables the gate")
	srTrail := flag.Float64("sr-trail", -1, "srlevels: chandelier trail distance in ATR; 0 leaves the initial stop alone")
	emaFast := flag.Int("ema-fast", 0, "emacross: fast EMA period (overrides config)")
	emaSlow := flag.Int("ema-slow", 0, "emacross: slow EMA period (overrides config)")
	emaProduct := flag.String("product", "", "emacross: MIS or CNC (overrides config)")
	emaAsset := flag.String("asset", "", "emacross: stocks or options (overrides config)")
	emaSL := flag.Float64("ema-sl", 0, "emacross: stop distance in ATR (overrides config)")
	emaTP := flag.Float64("ema-tp", -999, "emacross: target distance in ATR; <= 0 disables the target (overrides config)")
	emaATR := flag.Int("ema-atr", 0, "emacross: ATR period (overrides config)")
	emaTrail := flag.Float64("ema-trail", -1, "emacross: chandelier trail in ATR; 0 = none (overrides config)")
	emaCrossExit := flag.String("ema-cross-exit", "", "emacross: exit on the opposite cross, on or off (overrides config)")
	emaSep := flag.Float64("ema-sep", -1, "emacross: minimum |fast-slow| at the cross in ATR; 0 = off (overrides config)")
	emaADX := flag.Float64("ema-adx", -1, "emacross: ADX threshold; 0 = off (overrides config)")
	emaADXPeriod := flag.Int("ema-adx-period", 0, "emacross: ADX period (overrides config)")
	emaMaxEntries := flag.Int("ema-max-entries", -1, "emacross: entries per symbol per session; 0 = unlimited (overrides config)")
	emaStart := flag.Int("ema-start", 0, "emacross: first entry minute of day, e.g. 571 = 09:31 (overrides config)")
	emaCutoff := flag.Int("ema-cutoff", 0, "emacross: last entry minute of day, e.g. 900 = 15:00 (overrides config)")
	maxCapital := flag.Int64("max-capital", 0, "cap on capital allocated per trade in rupees, before MIS leverage (overrides every config cap); a strategy cap sized for index contracts pushes a leveraged stock long past the Rs5L simulated balance and it is silently refused")
	flag.Parse()

	// When a TOML config is given, it supplies the strategy, symbol CSV,
	// timeframe, limit, risk limits, and per-strategy knobs — the same values
	// the live trader runs with. Explicitly passed flags override it.
	var appCfg *config.Config
	if *configFile != "" {
		var cfgErr error
		appCfg, cfgErr = config.LoadConfig(*configFile)
		if cfgErr != nil {
			log.Fatalf("Failed to load config %s: %v", *configFile, cfgErr)
		}
		setFlags := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
		if !setFlags["strategy"] && appCfg.Strategy != "" {
			*strategyName = appCfg.Strategy
		}
		appCfg.Strategy = *strategyName
		settings := appCfg.ActiveStrategySettings()
		if !setFlags["csv"] && settings.CSVFile != "" {
			*csvFile = settings.CSVFile
		}
		if !setFlags["timeframe"] && settings.Timeframe != "" {
			*timeframe = settings.Timeframe
		}
		if !setFlags["limit"] && settings.Limit > 0 {
			*limit = settings.Limit
		}
		fmt.Printf("Loaded config from %s (strategy=%s csv=%s timeframe=%s)\n", *configFile, *strategyName, *csvFile, *timeframe)
	}

	var startDate, endDate time.Time
	var err error

	if *startDateStr != "" {
		startDate, err = time.Parse("2006-01-02", *startDateStr)
		if err != nil {
			log.Fatalf("Invalid start date format: %v", err)
		}
	}
	if *endDateStr != "" {
		endDate, err = time.Parse("2006-01-02", *endDateStr)
		if err != nil {
			log.Fatalf("Invalid end date format: %v", err)
		}
		// Set end date to end of day
		endDate = endDate.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
	}

	// Engine chatter is dropped, but ERRORs still reach stderr: "insufficient
	// funds", a quantity floored to zero and a failed close are the only
	// evidence of the traps CLAUDE.md records, and a silent run hides them.
	log.SetFlags(0)
	log.SetOutput(&LogFilter{})

	// Load symbols from CSV
	symbols, err := loadSymbolsFromCSV(*csvFile, *minBeta)
	if err != nil {
		log.Fatalf("Failed to load symbols: %v", err)
	}

	// Limit symbols if requested
	if *limit != -1 && len(symbols) > *limit {
		fmt.Printf("Limiting backtest to top %d symbols.\n", *limit)
		symbols = symbols[:*limit]
	}

	fmt.Printf("Loaded %d symbols for backtesting.\n", len(symbols))
	if !startDate.IsZero() {
		fmt.Printf("Backtest Period: %s to ", startDate.Format("2006-01-02"))
		if !endDate.IsZero() {
			fmt.Printf("%s\n", endDate.Format("2006-01-02"))
		} else {
			fmt.Println("End")
		}
	}

	fmt.Printf("=== ZEROBHA MULTI-STOCK BACKTEST [%s] ===\n", *strategyName)
	if *strategyName == config.StrategyDonchian {
		fmt.Println("!! DONCHIAN IS A SIGNAL TEST ONLY. It is intended for execution in weekly")
		fmt.Println("!! index options, and this run prices the SIGNAL INSTRUMENT (the index or")
		fmt.Println("!! whatever CSV was given), not the option that would actually be traded.")
		fmt.Println("!! An option adds bid-ask, theta over the hold, and per-order charges, all")
		fmt.Println("!! of which subtract. Read the per-trade return in bps, not the rupee PnL.")
	}
	if *strategyName == config.StrategySRLevels {
		fmt.Println("!! SRLEVELS IS A SIGNAL TEST ONLY. It is intended for execution in weekly")
		fmt.Println("!! index options (break -> PE, bounce -> CE), and this run prices the")
		fmt.Println("!! SIGNAL INSTRUMENT — the index — not the option that would be traded.")
		fmt.Println("!! An option adds bid-ask, theta over the hold and per-order charges, all")
		fmt.Println("!! of which subtract. Read the per-trade return in bps, not the rupee PnL,")
		fmt.Println("!! and price the trade list with cmd/optbt before believing any of it.")
	}
	if *strategyName == config.StrategyGapFade {
		fmt.Println("!! GAPFADE IS UNGATED IN BACKTEST: the Upstox news/earnings check has no")
		fmt.Println("!! as-of-date history, so this run fades EVERY qualifying gap, including the")
		fmt.Println("!! informed ones the live strategy declines. Treat the result as the")
		fmt.Println("!! no-information floor of the strategy, never as an estimate of it.")
	}

	// Store results for sorting
	type Result struct {
		Symbol        string
		NetProfit     decimal.Decimal
		GrossProfit   decimal.Decimal
		GrossLoss     decimal.Decimal
		TotalTrades   int
		WinningTrades int
	}
	var results []Result

	// Pool every closed trade across all symbols so we can compute one
	// portfolio-level Sharpe at the end (averaging per-symbol Sharpes would be
	// dominated by symbols that traded only once or twice).
	var allTrades []models.Trade
	var allTradesRaw []models.Trade

	// When the uptrend filter is engaged, load NIFTY-50 daily candles keyed by
	// date so we can feed the matching index candle into the engine before each
	// stock candle — that updates the filter's per-day uptrend state.
	niftyByDate := map[string]models.Candle{}
	if *uptrend {
		nf := "test/data/1d/nsei_real.csv"
		if _, err := os.Stat(nf); os.IsNotExist(err) {
			log.Fatalf("--uptrend requires NIFTY data at %s (download ^NSEI 1d first)", nf)
		}
		nh, nrows := readCSV(nf)
		cm := getColMap(nh)
		for _, r := range nrows {
			c := parseCandle(r, cm, "NIFTY 50", "1d", 24*time.Hour)
			niftyByDate[c.StartTime.Format("2006-01-02")] = c
		}
		fmt.Printf("Uptrend filter ENGAGED: loaded %d NIFTY-50 daily candles.\n", len(niftyByDate))
	}

	// Donchian's timing knobs are needed in three places inside the loop (engine
	// cutoff, strategy, square-off), so resolve them once up front.
	dcCfg := config.DefaultDonchianConfig()
	if appCfg != nil {
		dcCfg = appCfg.Donchian
	}

	// srlevels uses its timing knobs in the same three places, and its sweep
	// flags are applied here so every symbol in the run sees one config.
	srCfg := config.DefaultSRLevelsConfig()
	if appCfg != nil {
		srCfg = appCfg.SRLevels
	}
	if *srPivot != "" {
		if *srPivot != string(indicators.PivotTraditional) && *srPivot != string(indicators.PivotFibonacci) {
			log.Fatalf("-sr-pivot must be %q or %q, got %q", indicators.PivotTraditional, indicators.PivotFibonacci, *srPivot)
		}
		srCfg.PivotMethod = *srPivot
	}
	if *srSL > 0 {
		srCfg.SLATRMult = *srSL
	}
	// -1 is the "flag absent" sentinel, so an explicit 0 reaches the config and
	// genuinely disables the target — the trap tp_rr and trail_atr_mult each
	// fell into, where a run configured to switch something off silently
	// measured the default instead.
	if *srTP >= 0 {
		tp := *srTP
		srCfg.TPATRMult = &tp
	}
	if *srRoom >= 0 {
		room := *srRoom
		srCfg.MinRoomATR = &room
	}
	if *srTrail >= 0 {
		trail := *srTrail
		srCfg.TrailATRMult = &trail
	}
	switch *srHTF {
	case "":
	case "on", "true":
		on := true
		srCfg.UseHTFConfirm = &on
	case "off", "false":
		off := false
		srCfg.UseHTFConfirm = &off
	default:
		log.Fatalf("-sr-htf must be on or off, got %q", *srHTF)
	}
	if *strategyName == config.StrategySRLevels {
		fmt.Printf("SRLevels: pivot=%s SL=%.2f ATR TP=%.2f ATR trail=%.2f ATR htf-confirm=%t room=%.2f ATR\n",
			srCfg.PivotMethod, srCfg.SLATRMult, srCfg.TPMult(), srCfg.SRTrailMult(),
			srCfg.UseHTFConfirm != nil && *srCfg.UseHTFConfirm, srCfg.RoomATR())
	}

	emaCfg := config.DefaultEMACrossConfig()
	if appCfg != nil {
		emaCfg = appCfg.EMACross
	}
	if *emaFast > 0 {
		emaCfg.FastPeriod = *emaFast
	}
	if *emaSlow > 0 {
		emaCfg.SlowPeriod = *emaSlow
	}
	if *emaProduct != "" {
		emaCfg.ProductType = *emaProduct
	}
	if *emaAsset != "" {
		emaCfg.AssetType = *emaAsset
	}
	if *emaSL > 0 {
		emaCfg.SLATRMult = *emaSL
	}
	if *emaTP != -999 {
		if *emaTP <= 0 {
			emaCfg.TPATRMult = 0
		} else {
			emaCfg.TPATRMult = *emaTP
		}
	}
	if *emaATR > 0 {
		emaCfg.ATRPeriod = *emaATR
	}
	if *emaTrail >= 0 {
		trail := *emaTrail
		emaCfg.TrailATRMult = &trail
	}
	if *emaCrossExit != "" {
		on := strings.EqualFold(*emaCrossExit, "on")
		emaCfg.ExitOnOppositeCross = &on
	}
	if *emaSep >= 0 {
		emaCfg.MinSepATR = *emaSep
	}
	if *emaADX >= 0 {
		emaCfg.ADXThreshold = *emaADX
	}
	if *emaADXPeriod > 0 {
		emaCfg.ADXPeriod = *emaADXPeriod
	}
	if *emaMaxEntries >= 0 {
		emaCfg.MaxEntriesPerSymbol = *emaMaxEntries
	}
	if *emaStart > 0 {
		emaCfg.EntryStartMin = *emaStart
	}
	if *emaCutoff > 0 {
		emaCfg.EntryCutoffMin = *emaCutoff
	}
	if *strategyName == config.StrategyEMACross {
		crossExit := emaCfg.ExitOnOppositeCross == nil || *emaCfg.ExitOnOppositeCross
		fmt.Printf("EMACross: fast=%d slow=%d atr=%d product=%s asset=%s SL=%.1fxATR TP=%.1fxATR trail=%.1fxATR crossExit=%v sep=%.2fATR adx=%.0f/%d maxEntries=%d window=%02d:%02d-%02d:%02d\n",
			emaCfg.FastPeriod, emaCfg.SlowPeriod, emaCfg.ATRPeriod, emaCfg.ProductType, emaCfg.AssetType,
			emaCfg.SLATRMult, emaCfg.TPATRMult, emaCfg.TrailMult(), crossExit, emaCfg.MinSepATR,
			emaCfg.ADXThreshold, emaCfg.ADXPeriod, emaCfg.MaxEntriesPerSymbol,
			emaCfg.EntryStartMin/60, emaCfg.EntryStartMin%60, emaCfg.EntryCutoffMin/60, emaCfg.EntryCutoffMin%60)
	}

	type symWorkResult struct {
		sym        string
		res        Result
		tradesRaw  []models.Trade
		tradesNet  []models.Trade
		tradeTable string
		err        error
	}

	dur := time.Minute
	if *timeframe != "1m" && *timeframe != "1minute" {
		dur = parseDuration(*timeframe)
	}

	testSymbol := func(sym string) symWorkResult {
		initialCapital := decimal.NewFromInt(500000)
		simBroker := broker.NewSimBroker(initialCapital)

		maxLoss, maxTrades, maxPerStock := decimal.NewFromInt(1000), 20, 2
		if appCfg != nil {
			maxLoss = decimal.NewFromInt(int64(appCfg.Risk.MaxDailyLoss))
			maxTrades = appCfg.Risk.MaxTradesPerDay
			maxPerStock = appCfg.Risk.MaxTradesPerStock
		}
		if *strategyName == config.StrategyDonchian {
			maxLoss = initialCapital.Mul(decimal.NewFromFloat(dcCfg.MaxDailyLossPct / 100))
		}
		if *strategyName == config.StrategySRLevels {
			maxLoss = initialCapital.Mul(decimal.NewFromFloat(srCfg.MaxDailyLossPct / 100))
		}
		riskMgr := risk.NewManager(nil, maxLoss, maxTrades, maxPerStock)

		orbCfg := config.DefaultORBConfig()
		if appCfg != nil {
			orbCfg = appCfg.ORB
		}
		if *baseline {
			orbCfg = orbConfigToBaseline(orbCfg)
		}
		if *knobs != "" {
			orbCfg = orbConfigWithKnobs(*knobs)
		}
		var myStrategy core.Strategy = strategy.NewORBStrategy([]string{sym}, orbCfg)
		switch *strategyName {
		case config.StrategyDailyRev:
			drCfg := config.DefaultDailyRevConfig()
			if appCfg != nil {
				drCfg = appCfg.DailyRev
			}
			myStrategy = strategy.NewDailyReversalStrategy([]string{sym}, drCfg)
		case config.StrategyDonchian:
			myStrategy = strategy.NewDonchianStrategy([]string{sym}, dcCfg)
		case config.StrategySRLevels:
			myStrategy = strategy.NewSRLevelsStrategy([]string{sym}, srCfg)
		case config.StrategyGapFade:
			gfCfg := config.DefaultGapFadeConfig()
			if appCfg != nil {
				gfCfg = appCfg.GapFade
			}
			myStrategy = strategy.NewGapFadeStrategy([]string{sym}, gfCfg, nil)
		case config.StrategyEMACross:
			myStrategy = strategy.NewEMACrossStrategy([]string{sym}, emaCfg)
		}

		engine := core.NewEngine(myStrategy, simBroker, riskMgr, nil, nil, nil)
		engine.UptrendOnly = *uptrend

		if appCfg != nil {
			engine.MinBalance = int64(appCfg.Engine.MinBalance)
			engine.MinCapitalPerTrade = int64(appCfg.Engine.MinCapitalPerTrade)
			engine.MaxCapitalPerTrade = int64(appCfg.Engine.MaxCapitalPerTrade)
			engine.TradeCutoffMin = appCfg.Engine.TradeCutoffMin
		}

		if *strategyName == config.StrategyDonchian {
			engine.TradeCutoffMin = dcCfg.EntryCutoffMin + 1
			engine.MaxConcurrent = dcCfg.MaxConcurrent
			if dcCfg.MaxCapitalPerTrade > 0 {
				engine.MaxCapitalPerTrade = dcCfg.MaxCapitalPerTrade
			}
		}
		if *strategyName == config.StrategySRLevels {
			engine.TradeCutoffMin = srCfg.EntryCutoffMin + 1
			engine.MaxConcurrent = srCfg.MaxConcurrent
			if srCfg.MaxCapitalPerTrade > 0 {
				engine.MaxCapitalPerTrade = srCfg.MaxCapitalPerTrade
			}
		}
		if *strategyName == config.StrategyEMACross {
			if emaCfg.EntryCutoffMin > 0 {
				engine.TradeCutoffMin = emaCfg.EntryCutoffMin + 1
			}
			if emaCfg.MaxConcurrent > 0 {
				engine.MaxConcurrent = emaCfg.MaxConcurrent
			}
			if emaCfg.MaxCapitalPerTrade > 0 {
				engine.MaxCapitalPerTrade = emaCfg.MaxCapitalPerTrade
			}
			if strings.ToLower(emaCfg.AssetType) == "options" && engine.MaxCapitalPerTrade < 150000 {
				engine.MaxCapitalPerTrade = 150000
			}
			if strings.ToUpper(emaCfg.ProductType) == "CNC" {
				engine.TradeCutoffMin = 24 * 60
			}
		}
		if *maxCapital > 0 {
			engine.MaxCapitalPerTrade = *maxCapital
		}

		filename, err := findDataFile(*timeframe, sym)
		if err != nil {
			return symWorkResult{sym: sym, err: err}
		}

		header, records := readCSV(filename)
		cm := getColMap(header)

		fmt.Printf("Starting %s: %d candles\n", sym, len(records))
		var lastDate string
		for idx, record := range records {
			if idx > 0 && idx%5000 == 0 {
				fmt.Printf("  %s: candle %d/%d\n", sym, idx, len(records))
			}
			candle := parseCandle(record, cm, sym, *timeframe, dur)

			if !endDate.IsZero() && candle.StartTime.After(endDate) {
				continue
			}

			currentDate := candle.StartTime.Format("2006-01-02")
			if currentDate != lastDate {
				riskMgr.ResetDaily()
				lastDate = currentDate
			}

			if !startDate.IsZero() && candle.StartTime.Before(startDate) {
				myStrategy.OnCandle(candle)
				continue
			}

			if *uptrend {
				if nifty, ok := niftyByDate[currentDate]; ok {
					engine.Execute(nifty)
				}
			}

			simBroker.CheckExits(candle)
			engine.Execute(candle)

			istTime := candle.StartTime.In(istLoc)
			h, m, _ := istTime.Clock()
			timeInMinutes := h*60 + m
			squareOffTime := 15*60 + 15
			if *strategyName == config.StrategyDonchian {
				squareOffTime = dcCfg.SquareOffMin
			}
			if *strategyName == config.StrategySRLevels {
				squareOffTime = srCfg.SquareOffMin
			}
			if *strategyName == config.StrategyEMACross {
				squareOffTime = emaCfg.SquareOffMin
			}

			isCNC := *strategyName == config.StrategyDailyRev || (*strategyName == config.StrategyEMACross && strings.ToUpper(emaCfg.ProductType) == "CNC")
			if !isCNC && squareOffTime > 0 && timeInMinutes >= squareOffTime {
				simBroker.SquareOffAll(candle)
			}
		}
		fmt.Printf("Finished %s candles loop, trades: %d\n", sym, len(simBroker.Trades))

		var tableBuilder strings.Builder
		if len(symbols) <= 2 {
			tableBuilder.WriteString(fmt.Sprintf("\n--------------------------------------------------\nTESTING SYMBOL: %s\n--------------------------------------------------\n", sym))
			tableBuilder.WriteString("--- TRADE LOG ---\n")
			tableBuilder.WriteString(fmt.Sprintf("%-20s | %-6s | %-10s | %-10s | %-20s | %-10s | %-10s | %-15s\n", "Entry Time", "Type", "Price", "Qty", "Exit Time", "Exit Price", "PnL", "Reason"))
			for _, t := range simBroker.Trades {
				exitTime := ""
				exitPrice := ""
				pnl := ""
				if !t.ExitTime.IsZero() {
					exitTime = t.ExitTime.In(istLoc).Format("2006-01-02 15:04")
					exitPrice = t.ExitPrice.StringFixed(2)
					pnl = t.PnL.StringFixed(2)
				}
				tableBuilder.WriteString(fmt.Sprintf("%-20s | %-6s | %-10s | %-10s | %-20s | %-10s | %-10s | %-15s\n",
					t.EntryTime.In(istLoc).Format("2006-01-02 15:04"),
					t.Direction,
					t.EntryPrice.StringFixed(2),
					t.Quantity.StringFixed(0),
					exitTime,
					exitPrice,
					pnl,
					t.ExitReason,
				))
			}
		}

		tradesNet := applyCosts(simBroker.Trades, *costBps)
		stats := statistics.Analyze(tradesNet, initialCapital)
		winningTrades := int(float64(stats.TotalTrades) * stats.WinRate / 100.0)

		res := Result{
			Symbol:        sym,
			NetProfit:     stats.NetProfit,
			GrossProfit:   stats.GrossProfit,
			GrossLoss:     stats.GrossLoss,
			TotalTrades:   stats.TotalTrades,
			WinningTrades: winningTrades,
		}

		return symWorkResult{
			sym:        sym,
			res:        res,
			tradesRaw:  simBroker.Trades,
			tradesNet:  tradesNet,
			tradeTable: tableBuilder.String(),
		}
	}

	numWorkers := 8
	if numWorkers > len(symbols) {
		numWorkers = len(symbols)
	}

	jobs := make(chan string, len(symbols))
	resultsChan := make(chan symWorkResult, len(symbols))

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				resultsChan <- testSymbol(s)
			}
		}()
	}

	for _, s := range symbols {
		jobs <- s
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	doneCount := 0
	for r := range resultsChan {
		doneCount++
		if r.err != nil {
			fmt.Printf("[%2d/%2d] Skipping %s: %v\n", doneCount, len(symbols), r.sym, r.err)
			continue
		}
		if r.tradeTable != "" {
			fmt.Println(r.tradeTable)
		}
		sharpeStr := "n/a"
		if r.res.TotalTrades >= 20 {
			stats := statistics.Analyze(r.tradesNet, decimal.NewFromInt(500000))
			sharpeStr = fmt.Sprintf("%.3f", stats.Sharpe)
		}
		winRate := 0.0
		if r.res.TotalTrades > 0 {
			winRate = float64(r.res.WinningTrades) / float64(r.res.TotalTrades) * 100.0
		}
		fmt.Printf("[%2d/%2d] Symbol: %-12s | Trades: %-4d | WinRate: %5.1f%% | Sharpe: %-5s | NetProfit: ₹%s\n",
			doneCount, len(symbols), r.sym, r.res.TotalTrades, winRate, sharpeStr, r.res.NetProfit.StringFixed(2))

		allTradesRaw = append(allTradesRaw, r.tradesRaw...)
		allTrades = append(allTrades, r.tradesNet...)
		results = append(results, r.res)
	}

	// Sort results by Net Profit (Descending)
	// Simple bubble sort or similar since list is small (50 items)
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].NetProfit.GreaterThan(results[i].NetProfit) {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	// Save All to CSV
	numResults := len(results)

	outputFile, err := os.Create("final_portfolio.csv")
	if err != nil {
		log.Fatalf("Failed to create output file: %v", err)
	}
	defer outputFile.Close()

	writer := csv.NewWriter(outputFile)
	defer writer.Flush()

	// Header
	writer.Write([]string{"symbol", "net_profit"})

	fmt.Println("\n=== ALL PERFORMERS (Saved to final_portfolio.csv) ===")

	var aggNetProfit, aggGrossProfit, aggGrossLoss decimal.Decimal
	var aggTotalTrades, aggWinningTrades int

	for i := 0; i < numResults; i++ {
		r := results[i]
		fmt.Printf("%d. %s: %s\n", i+1, r.Symbol, r.NetProfit.StringFixed(2))
		writer.Write([]string{r.Symbol, r.NetProfit.StringFixed(2)})

		aggNetProfit = aggNetProfit.Add(r.NetProfit)
		aggGrossProfit = aggGrossProfit.Add(r.GrossProfit)
		aggGrossLoss = aggGrossLoss.Add(r.GrossLoss)
		aggTotalTrades += r.TotalTrades
		aggWinningTrades += r.WinningTrades
	}

	// Calculate Aggregate Stats
	var aggWinRate float64
	if aggTotalTrades > 0 {
		aggWinRate = float64(aggWinningTrades) / float64(aggTotalTrades) * 100
	}

	var aggProfitFactor float64
	if !aggGrossLoss.IsZero() {
		aggProfitFactor, _ = aggGrossProfit.Div(aggGrossLoss.Abs()).Float64()
	} else if aggGrossProfit.GreaterThan(decimal.Zero) {
		aggProfitFactor = 999.0 // Infinite
	}

	if *tradesCSV != "" {
		if err := writeTradesCSV(*tradesCSV, allTradesRaw); err != nil {
			log.Printf("WARNING: Failed to write trades CSV: %v", err)
		} else {
			fmt.Printf("Wrote %d trades to %s\n", len(allTradesRaw), *tradesCSV)
		}
	}

	// Portfolio-level Sharpe over the pooled trade series. (Note: the pooled
	// drawdown is not meaningful here because trades are grouped by symbol, not
	// time-ordered into a single equity curve — so we report only Sharpe.)
	portfolio := statistics.Analyze(allTrades, decimal.NewFromInt(500000))

	fmt.Println("\n=== AGGREGATE STATS (ALL STOCKS) ===")
	fmt.Printf("Total Net Profit: ₹%s\n", aggNetProfit.StringFixed(2))
	fmt.Printf("Total Trades:     %d\n", aggTotalTrades)
	fmt.Printf("Win Rate:         %.2f%%\n", aggWinRate)
	fmt.Printf("Profit Factor:    %.2f\n", aggProfitFactor)
	fmt.Printf("Sharpe (per-trade): %.3f\n", portfolio.Sharpe)

	fmt.Println("\n=== BACKTEST COMPLETE ===")
}

// writeTradesCSV dumps every trade with its pre-cost PnL, so regime and exit
// analysis can be done offline without re-running the backtest.
func writeTradesCSV(path string, trades []models.Trade) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"symbol", "direction", "entry_time", "exit_time",
		"entry_price", "exit_price", "quantity", "pnl_gross", "exit_reason"}); err != nil {
		return err
	}
	for _, t := range trades {
		exitTime := ""
		if !t.ExitTime.IsZero() {
			exitTime = t.ExitTime.Format(time.RFC3339)
		}
		if err := w.Write([]string{
			t.Symbol,
			t.Direction,
			t.EntryTime.Format(time.RFC3339),
			exitTime,
			t.EntryPrice.String(),
			t.ExitPrice.String(),
			t.Quantity.String(),
			t.PnL.String(),
			t.ExitReason,
		}); err != nil {
			return err
		}
	}
	return nil
}

// applyCosts returns a copy of the trades with a round-trip transaction cost
// subtracted from each PnL. The cost is costBps basis points applied to the
// turnover of both legs (entry notional + exit notional), which approximates
// Indian intraday equity costs (brokerage + STT + exchange + GST + stamp).
func applyCosts(trades []models.Trade, costBps float64) []models.Trade {
	if costBps <= 0 {
		return trades
	}
	rate := decimal.NewFromFloat(costBps / 10000.0)
	out := make([]models.Trade, len(trades))
	for i, t := range trades {
		turnover := t.EntryPrice.Mul(t.Quantity).Add(t.ExitPrice.Mul(t.Quantity))
		cost := turnover.Mul(rate)
		t.PnL = t.PnL.Sub(cost)
		out[i] = t
	}
	return out
}

// orbConfigToBaseline reverts the robustness/signal knobs added during the
// 2026-05 review back to their original pre-review behavior, so a --baseline
// run can be compared head-to-head against the new defaults on the same data.
func orbConfigToBaseline(c config.ORBConfig) config.ORBConfig {
	off := false
	c.VolThrustMult = 1.0   // volume > avg (no thrust requirement)
	c.MaxVWAPDistATR = 0    // extension filter disabled
	c.ADXRisingEps = 0      // strict "ADX rising"
	c.OneTradePerDay = &off // allow re-entries (old behavior)
	c.StopFloorAtRange = &off
	return c
}

// orbConfigWithKnobs starts from the baseline (all new knobs off) and enables
// only the knobs named in the comma-separated list, for single-knob ablation.
// Recognized: onetrade, stopfloor, vwapdist, thrust, adxeps.
func orbConfigWithKnobs(list string) config.ORBConfig {
	c := orbConfigToBaseline(config.DefaultORBConfig())
	on := true
	for _, k := range strings.Split(list, ",") {
		switch strings.TrimSpace(strings.ToLower(k)) {
		case "onetrade":
			c.OneTradePerDay = &on
		case "stopfloor":
			c.StopFloorAtRange = &on
		case "vwapdist":
			c.MaxVWAPDistATR = 1.5
		case "thrust":
			c.VolThrustMult = 1.5
		case "adxeps":
			c.ADXRisingEps = 2.0
		default:
			log.Printf("WARNING: unknown knob %q ignored", k)
		}
	}
	return c
}

// readCSV returns the header row and the data rows separately, so callers can
// map columns by name (the downloader writes columns in a non-OHLC order).
func readCSV(path string) ([]string, [][]string) {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer func(f *os.File) {
		_ = f.Close()
	}(f)

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil {
		panic(err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	return records[0], records[1:]
}

// buildColumnIndex maps canonical OHLCV field names to their column position
// using the CSV header. Falls back to the conventional
// timestamp,open,high,low,close,volume order for any field the header is
// missing, so older fixed-order files still parse.
func buildColumnIndex(header []string) map[string]int {
	idx := map[string]int{
		"timestamp": 0, "open": 1, "high": 2, "low": 3, "close": 4, "volume": 5,
	}
	for i, col := range header {
		key := strings.ToLower(strings.TrimSpace(col))
		if key == "datetime" || key == "date" {
			key = "timestamp"
		}
		if _, ok := idx[key]; ok {
			idx[key] = i
		}
	}
	return idx
}

// parseDuration turns a -timeframe value into the bar length used to stamp
// Candle.EndTime.
//
// It must understand BOTH spellings the data tree uses. test/data/5m/ comes
// from the Yahoo downloader and is named in Go duration syntax; test/data/
// 5minute/ comes from cmd/histdl and cmd/idxdl and is named the way Kite and
// Upstox name intervals. time.ParseDuration only accepts the first, and the
// old fallback of one hour was silently wrong for the second: every bar's
// EndTime landed an hour late, and since Engine.Execute gates entries on
// EndTime, the effective entry cutoff ran an hour EARLY. On a donchian index
// run that truncated the entry window at 13:30 instead of 14:31.
func parseDuration(tf string) time.Duration {
	switch strings.ToLower(tf) {
	case "minute", "1minute":
		return time.Minute
	case "3minute":
		return 3 * time.Minute
	case "5minute":
		return 5 * time.Minute
	case "10minute":
		return 10 * time.Minute
	case "15minute":
		return 15 * time.Minute
	case "30minute":
		return 30 * time.Minute
	case "60minute", "hour":
		return time.Hour
	case "day", "1day":
		return 24 * time.Hour
	}
	if strings.HasSuffix(tf, "d") {
		daysStr := strings.TrimSuffix(tf, "d")
		days, err := strconv.Atoi(daysStr)
		if err != nil {
			return 24 * time.Hour // Default fallback
		}
		return time.Duration(days) * 24 * time.Hour
	}
	d, err := time.ParseDuration(tf)
	if err != nil {
		// Refuse to guess: an unrecognised timeframe silently shifts every
		// EndTime, and the only symptom is a strategy that stops trading
		// earlier in the day than its config says.
		log.Fatalf("unrecognised -timeframe %q: cannot derive the bar length that stamps Candle.EndTime", tf)
	}
	return d
}

type colMap struct {
	ts, open, high, low, close, vol int
}

func getColMap(header []string) colMap {
	col := buildColumnIndex(header)
	lookup := func(k string) int {
		if idx, ok := col[k]; ok {
			return idx
		}
		return -1
	}
	return colMap{
		ts:    lookup("timestamp"),
		open:  lookup("open"),
		high:  lookup("high"),
		low:   lookup("low"),
		close: lookup("close"),
		vol:   lookup("volume"),
	}
}

func parseCandle(record []string, cm colMap, sym string, timeframe string, dur time.Duration) models.Candle {
	var t time.Time
	if cm.ts >= 0 && cm.ts < len(record) {
		tsStr := record[cm.ts]
		var err error
		t, err = time.Parse(time.RFC3339, tsStr)
		if err != nil {
			layout := "2006-01-02T15:04:05+0000"
			t, err = time.Parse(layout, tsStr)
			if err != nil {
				t, _ = time.Parse("2006-01-02T15:04:05", tsStr)
			}
		}
	}
	getField := func(idx int) decimal.Decimal {
		if idx < 0 || idx >= len(record) {
			return decimal.Zero
		}
		f, err := strconv.ParseFloat(record[idx], 64)
		if err != nil {
			return decimal.Zero
		}
		return decimal.NewFromFloat(f)
	}
	return models.Candle{
		Symbol:     sym,
		Timeframe:  timeframe,
		Open:       getField(cm.open),
		High:       getField(cm.high),
		Low:        getField(cm.low),
		Close:      getField(cm.close),
		Volume:     getField(cm.vol),
		StartTime:  t,
		EndTime:    t.Add(dur),
		IsComplete: true,
	}
}

// LogFilter implements io.Writer to filter log output
type LogFilter struct{}

func (f *LogFilter) Write(p []byte) (n int, err error) {
	msg := string(p)
	if strings.Contains(msg, "ERROR") || strings.Contains(msg, "panic") {
		return os.Stderr.Write(p)
	}
	return len(p), nil
}

func loadSymbolsFromCSV(filename string, minBeta float64) ([]string, error) {
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

	if len(records) < 2 {
		return nil, fmt.Errorf("csv file empty or missing header")
	}

	header := records[0]
	symbolIdx := -1
	betaIdx := -1
	for i, col := range header {
		if strings.EqualFold(col, strings.ToLower("symbol")) {
			symbolIdx = i
		}
		if strings.EqualFold(col, strings.ToLower("beta")) {
			betaIdx = i
		}
	}

	// Fallback to 0 if not found (for files without header or different format)
	// But high_beta_stocks.csv has header.
	if symbolIdx == -1 {
		// Try to guess? Or just default to 0 and warn?
		// For ind_nifty200list.csv, Symbol is there.
		// For high_beta_stocks.csv, symbol is there.
		// If not found, maybe it's a raw list?
		symbolIdx = 0
	}

	var symbols []string
	for i, record := range records {
		if i == 0 {
			continue
		}
		if len(record) > symbolIdx {
			// Check Beta Filter
			if minBeta > 0 && betaIdx != -1 && len(record) > betaIdx {
				betaVal, err := decimal.NewFromString(record[betaIdx])
				if err == nil && betaVal.LessThan(decimal.NewFromFloat(minBeta)) {
					continue
				}
			}

			// Remove .NS suffix and convert to uppercase
			sym := strings.ToUpper(strings.ReplaceAll(record[symbolIdx], ".NS", ""))
			symbols = append(symbols, sym)
		}
	}
	return symbols, nil
}

// dataFileStem turns a trading symbol into the file stem the data tree uses.
//
// The live symbol is the Kite tradingsymbol, which for an index carries a
// space ("NIFTY 50"); the downloaders (cmd/idxdl, cmd/histdl) name their
// output without one ("nifty50_real.csv"). Stripping spaces lets one universe
// CSV serve both the backtest and the trader — spelling the symbol "NIFTY50"
// instead makes it unresolvable against the instrument dump, and the strategy
// then fails warm-up with "symbol NIFTY50 not found".
func dataFileStem(symbol string) string {
	return strings.ReplaceAll(strings.ToLower(symbol), " ", "")
}

// timeframeDirs lists the data directories that hold the given bar size, in
// lookup order. The tree names the same bar size two ways — test/data/5m
// (Yahoo, Go duration syntax) and test/data/5minute (Kite/Upstox naming) —
// and every config file says "5m" because cmd/trader parses it as a Go
// duration, so a run that only looked in test/data/5m found no index data and
// reported zero trades with nothing to say why.
func timeframeDirs(tf string) []string {
	aliases := map[string][]string{
		"1m": {"1minute", "minute"}, "1minute": {"1m"}, "minute": {"1m"},
		"3m": {"3minute"}, "3minute": {"3m"},
		"5m": {"5minute"}, "5minute": {"5m"},
		"10m": {"10minute"}, "10minute": {"10m"},
		"15m": {"15minute"}, "15minute": {"15m"},
		"30m": {"30minute"}, "30minute": {"30m"},
		"1h": {"60minute"}, "60minute": {"1h"},
		"1d": {"day"}, "day": {"1d"},
	}
	return append([]string{tf}, aliases[strings.ToLower(tf)]...)
}

// findDataFile resolves the candle CSV for a symbol, trying the timeframe
// directory, its alias, then the tree root.
func findDataFile(tf, sym string) (string, error) {
	stem := dataFileStem(sym)
	var tried []string
	for _, dir := range timeframeDirs(tf) {
		f := fmt.Sprintf("test/data/%s/%s_real.csv", dir, stem)
		if _, err := os.Stat(f); err == nil {
			return f, nil
		}
		tried = append(tried, f)
	}
	f := fmt.Sprintf("test/data/%s_real.csv", stem)
	if _, err := os.Stat(f); err == nil {
		return f, nil
	}
	tried = append(tried, f)
	return "", fmt.Errorf("no data file found (tried %s)", strings.Join(tried, ", "))
}
