package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"zerobha/pkg/statistics"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/journal"
	"zerobha/pkg/strategy" // Import your strategies package

	"github.com/shopspring/decimal"
)

func main() {
	// Parse command line flags
	startDateStr := flag.String("start", "", "Start date for backtest (YYYY-MM-DD)")
	endDateStr := flag.String("end", "", "End date for backtest (YYYY-MM-DD)")
	strategyName := flag.String("strategy", "orb", "Strategy to run: orb, gapfade, donchian (all intraday 5m) or dailyrev (daily short-term reversal)")
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

	// log.SetFlags(log.LstdFlags | log.Lshortfile)
	// Use custom filter to only show errors
	log.SetFlags(0) // Remove flags to make filtering easier or keep them? Keep them for context.
	log.SetFlags(log.LstdFlags | log.Lshortfile)
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
		ncol := buildColumnIndex(nh)
		for _, r := range nrows {
			c := parseCandle(r, ncol, "NIFTY 50", "1d")
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

	for _, sym := range symbols {
		fmt.Printf("\n--------------------------------------------------\n")
		fmt.Printf("TESTING SYMBOL: %s\n", sym)
		fmt.Printf("--------------------------------------------------\n")

		// 1. Setup Environment
		initialCapital := decimal.NewFromInt(500000) // ₹5 Lakh
		simBroker := broker.NewSimBroker(initialCapital)

		// Risk: Max Loss ₹1000/day, Max 20 trades/day Total, Max 2 trades/day per stock
		// (overridden by the [risk] section when -config is given)
		maxLoss, maxTrades, maxPerStock := decimal.NewFromInt(1000), 20, 2
		if appCfg != nil {
			maxLoss = decimal.NewFromInt(int64(appCfg.Risk.MaxDailyLoss))
			maxTrades = appCfg.Risk.MaxTradesPerDay
			maxPerStock = appCfg.Risk.MaxTradesPerStock
		}
		// Donchian states its kill switch as a percent of capital rather than a
		// rupee figure. NOTE: the engine never feeds realised PnL back to the
		// risk manager (see the TODO in engine.Execute), so this limit is
		// carried but never actually trips in a backtest.
		if *strategyName == config.StrategyDonchian {
			maxLoss = initialCapital.Mul(decimal.NewFromFloat(dcCfg.MaxDailyLossPct / 100))
		}
		riskMgr := risk.NewManager(nil, maxLoss, maxTrades, maxPerStock)

		// Strategy
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
		case config.StrategyGapFade:
			gfCfg := config.DefaultGapFadeConfig()
			if appCfg != nil {
				gfCfg = appCfg.GapFade
			}
			// nil gate: Upstox serves current news and current fundamentals
			// with no as-of-date history, so a backtest cannot reproduce the
			// live news/earnings check. This run fades every qualifying gap,
			// informed or not — see the banner printed above.
			myStrategy = strategy.NewGapFadeStrategy([]string{sym}, gfCfg, nil)
		}

		// Journal
		j, _ := journal.NewJournal("backtest_journal.csv")
		defer j.Close()

		// 4. Create Engine. Only engage the uptrend filter when requested; with
		// it off we disable it explicitly so the engine doesn't fail-open + warn.
		engine := core.NewEngine(myStrategy, simBroker, riskMgr, j, nil, nil)
		engine.UptrendOnly = *uptrend

		// Apply the [engine] section, as cmd/trader does. Without this the
		// backtester silently ignored it and ran on the built-in defaults, so
		// the same config file produced different capital allocation and a
		// different entry cutoff in backtest than in live. It also made an
		// instrument priced above max_capital_per_trade untradeable with no
		// diagnostic: quantity floors to zero and the signal vanishes, which
		// reads as "the strategy found nothing" (SENSEX at 78,000 against the
		// 50,000 stock-sized cap).
		if appCfg != nil {
			engine.MinBalance = int64(appCfg.Engine.MinBalance)
			engine.MinCapitalPerTrade = int64(appCfg.Engine.MinCapitalPerTrade)
			engine.MaxCapitalPerTrade = int64(appCfg.Engine.MaxCapitalPerTrade)
			engine.TradeCutoffMin = appCfg.Engine.TradeCutoffMin
		}

		// The engine's own cutoff (14:05 by default) is earlier than Donchian's
		// last-entry time, and it gates every signal — left alone it would
		// silently truncate the strategy's entry window.
		if *strategyName == config.StrategyDonchian {
			engine.TradeCutoffMin = dcCfg.EntryCutoffMin + 1
			engine.MaxConcurrent = dcCfg.MaxConcurrent
			// The engine's cap is calibrated for cash equities; this strategy
			// trades indices, where the same number floors quantity to zero and
			// produces no trades at all rather than smaller ones.
			if dcCfg.MaxCapitalPerTrade > 0 {
				engine.MaxCapitalPerTrade = dcCfg.MaxCapitalPerTrade
			}
		}

		// 2. Load Data
		// Try timeframe specific folder first
		filename := fmt.Sprintf("test/data/%s/%s_real.csv", *timeframe, strings.ToLower(sym))
		if _, err := os.Stat(filename); os.IsNotExist(err) {
			// Fallback to root test/data
			filename = fmt.Sprintf("test/data/%s_real.csv", strings.ToLower(sym))
			if _, err := os.Stat(filename); os.IsNotExist(err) {
				fmt.Printf("Skipping %s: Data file %s not found\n", sym, filename)
				continue
			}
		}

		header, records := readCSV(filename)
		colIdx := buildColumnIndex(header)

		// 3. The Backtest Loop
		var lastDate string
		for _, record := range records {
			candle := parseCandle(record, colIdx, sym, *timeframe)

			// Filter by date
			// if !startDate.IsZero() && candle.StartTime.Before(startDate) {
			// 	continue
			// }
			if !endDate.IsZero() && candle.StartTime.After(endDate) {
				continue
			}

			// Check for new day
			currentDate := candle.StartTime.Format("2006-01-02")
			if currentDate != lastDate {
				riskMgr.ResetDaily()
				lastDate = currentDate
			}

			// Warmup Phase: Update strategy but don't execute trades
			if !startDate.IsZero() && candle.StartTime.Before(startDate) {
				myStrategy.OnCandle(candle)
				continue
			}

			// Feed the matching NIFTY-50 daily candle first so the uptrend
			// filter reflects this date before the stock signal is evaluated.
			if *uptrend {
				if nifty, ok := niftyByDate[currentDate]; ok {
					engine.Execute(nifty)
				}
			}

			simBroker.CheckExits(candle)
			engine.Execute(candle)

			// Force Square-off at 15:15
			// Convert to IST
			loc, _ := time.LoadLocation("Asia/Kolkata")
			if loc == nil {
				loc = time.FixedZone("IST", 5*3600+1800)
			}
			istTime := candle.StartTime.In(loc)
			h, m, _ := istTime.Clock()
			timeInMinutes := h*60 + m
			// 15:15 for the strategies written against the exchange's MIS
			// square-off; Donchian flattens earlier, on its own knob.
			squareOffTime := 15*60 + 15
			if *strategyName == config.StrategyDonchian {
				squareOffTime = dcCfg.SquareOffMin
			}

			if timeInMinutes >= squareOffTime {
				simBroker.SquareOffAll(candle)
			}
		}

		// 4. Report
		fmt.Println("\n--- TRADE LOG ---")
		fmt.Printf("%-20s | %-6s | %-10s | %-10s | %-20s | %-10s | %-10s | %-15s\n", "Entry Time", "Type", "Price", "Qty", "Exit Time", "Exit Price", "PnL", "Reason")

		loc, _ := time.LoadLocation("Asia/Kolkata") // Ignore error, fallback to UTC if needed (or nil causes panic? LoadLocation returns UTC if error? No)
		if loc == nil {
			loc = time.FixedZone("IST", 5*3600+1800)
		}

		for _, t := range simBroker.Trades {
			exitTime := ""
			exitPrice := ""
			pnl := ""
			if !t.ExitTime.IsZero() {
				exitTime = t.ExitTime.In(loc).Format("2006-01-02 15:04")
				exitPrice = t.ExitPrice.StringFixed(2)
				pnl = t.PnL.StringFixed(2)
			}
			fmt.Printf("%-20s | %-6s | %-10s | %-10s | %-20s | %-10s | %-10s | %-15s\n",
				t.EntryTime.In(loc).Format("2006-01-02 15:04"),
				t.Direction,
				t.EntryPrice.StringFixed(2),
				t.Quantity.StringFixed(0),
				exitTime,
				exitPrice,
				pnl,
				t.ExitReason,
			)
		}
		fmt.Println("-----------------")

		// Deduct round-trip transaction costs (brokerage + STT + exchange/GST/etc.)
		// from each trade so baseline and updated runs are compared net of costs.
		allTradesRaw = append(allTradesRaw, simBroker.Trades...)
		tradesNet := applyCosts(simBroker.Trades, *costBps)

		stats := statistics.Analyze(tradesNet, initialCapital)
		// Per-symbol Sharpe is unreliable below ~20 trades (tiny-sample stddev
		// collapse produces absurd values), so show "n/a" there. The pooled
		// aggregate Sharpe at the end is the trustworthy figure.
		sharpeStr := "n/a"
		if stats.TotalTrades >= 20 {
			sharpeStr = fmt.Sprintf("%.3f", stats.Sharpe)
		}
		fmt.Printf("Symbol: %s | Total Trades: %d | Win Rate: %.2f%% | Sharpe: %s | Net Profit: %s\n",
			sym, stats.TotalTrades, stats.WinRate, sharpeStr, stats.NetProfit)

		allTrades = append(allTrades, tradesNet...)

		// Collect result
		// stats.NetProfit, GrossProfit, GrossLoss are decimal.Decimal.
		// WinningTrades is not in Performance struct, calculate it.
		winningTrades := int(float64(stats.TotalTrades) * stats.WinRate / 100.0)

		results = append(results, Result{
			Symbol:        sym,
			NetProfit:     stats.NetProfit,
			GrossProfit:   stats.GrossProfit,
			GrossLoss:     stats.GrossLoss,
			TotalTrades:   stats.TotalTrades,
			WinningTrades: winningTrades,
		})
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

func parseCandle(record []string, col map[string]int, sym string, timeframe string) models.Candle {
	field := func(name string) string {
		i, ok := col[name]
		if !ok || i >= len(record) {
			return ""
		}
		return record[i]
	}

	// Handle +0000 format which is not strict RFC3339
	tsStr := field("timestamp")
	layout := "2006-01-02T15:04:05+0000"
	t, err := time.Parse(layout, tsStr)
	if err != nil {
		// Fallback to RFC3339
		t, err = time.Parse(time.RFC3339, tsStr)
		if err != nil {
			// Fallback to format without timezone (assume UTC or local, here assuming UTC for simplicity)
			t, err = time.Parse("2006-01-02T15:04:05", tsStr)
			if err != nil {
				fmt.Printf("ERROR parsing date '%s': %v\n", tsStr, err)
			}
		}
	}
	o, _ := decimal.NewFromString(field("open"))
	h, _ := decimal.NewFromString(field("high"))
	l, _ := decimal.NewFromString(field("low"))
	c, _ := decimal.NewFromString(field("close"))
	v, _ := decimal.NewFromString(field("volume"))

	return models.Candle{
		Symbol:    sym,
		Timeframe: timeframe,
		Open:      o, High: h, Low: l, Close: c, Volume: v,
		StartTime:  t,
		EndTime:    t.Add(parseDuration(timeframe)),
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
