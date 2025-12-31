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
	strategyName := flag.String("strategy", "orb", "Strategy to run: orb, donchian")
	csvFile := flag.String("csv", "high_beta_stocks.csv", "CSV file containing symbols")
	minBeta := flag.Float64("min-beta", 0.0, "Minimum Beta threshold for stock selection")
	timeframe := flag.String("timeframe", "1d", "Timeframe for candles (e.g. 1d, 1h)")
	limit := flag.Int("limit", -1, "Limit number of symbols to process (default -1: all)")
	flag.Parse()

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

	for _, sym := range symbols {
		fmt.Printf("\n--------------------------------------------------\n")
		fmt.Printf("TESTING SYMBOL: %s\n", sym)
		fmt.Printf("--------------------------------------------------\n")

		// 1. Setup Environment
		initialCapital := decimal.NewFromInt(500000) // ₹5 Lakh
		simBroker := broker.NewSimBroker(initialCapital)

		// Risk: Max Loss ₹1000/day, Max 20 trades/day Total, Max 2 trades/day per stock
		riskMgr := risk.NewManager(nil, decimal.NewFromInt(1000), 20, 2)

		// Strategy
		var myStrategy core.Strategy
		switch *strategyName {
		case "donchian":
			myStrategy = strategy.NewDonchianBreakout([]string{sym})
		case "orb":
			myStrategy = strategy.NewORBStrategy([]string{sym})
		default:
			log.Printf("Using default strategy: ORB")
			myStrategy = strategy.NewORBStrategy([]string{sym})
		}

		// Journal
		j, _ := journal.NewJournal("backtest_journal.csv")
		defer j.Close()

		// 4. Create Engine
		engine := core.NewEngine(myStrategy, simBroker, riskMgr, j, nil, nil)

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

		records := readCSV(filename)

		// 3. The Backtest Loop
		var lastDate string
		for _, record := range records {
			candle := parseCandle(record, sym, *timeframe)

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

			simBroker.CheckExits(candle)
			engine.Execute(candle)
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

		stats := statistics.Analyze(simBroker.Trades, initialCapital)
		fmt.Printf("Symbol: %s | Total Trades: %d | Win Rate: %.2f%% | Net Profit: %s\n",
			sym, stats.TotalTrades, stats.WinRate, stats.NetProfit)

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

	fmt.Println("\n=== AGGREGATE STATS (ALL STOCKS) ===")
	fmt.Printf("Total Net Profit: ₹%s\n", aggNetProfit.StringFixed(2))
	fmt.Printf("Total Trades:     %d\n", aggTotalTrades)
	fmt.Printf("Win Rate:         %.2f%%\n", aggWinRate)
	fmt.Printf("Profit Factor:    %.2f\n", aggProfitFactor)

	fmt.Println("\n=== BACKTEST COMPLETE ===")
}

func readCSV(path string) [][]string {
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
	return records[1:] // Skip header
}

func parseDuration(tf string) time.Duration {
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
		return time.Hour // Default fallback
	}
	return d
}

func parseCandle(record []string, sym string, timeframe string) models.Candle {
	// CSV: timestamp,open,high,low,close,volume
	// Fix: Handle +0000 format which is not strict RFC3339
	layout := "2006-01-02T15:04:05+0000"
	t, err := time.Parse(layout, record[0])
	if err != nil {
		// Fallback to RFC3339
		t, err = time.Parse(time.RFC3339, record[0])
		if err != nil {
			// Fallback to format without timezone (assume UTC or local, here assuming UTC for simplicity)
			t, err = time.Parse("2006-01-02T15:04:05", record[0])
			if err != nil {
				fmt.Printf("ERROR parsing date '%s': %v\n", record[0], err)
			}
		}
	}
	o, _ := decimal.NewFromString(record[1])
	h, _ := decimal.NewFromString(record[2])
	l, _ := decimal.NewFromString(record[3])
	c, _ := decimal.NewFromString(record[4])
	v, _ := decimal.NewFromString(record[5])

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
