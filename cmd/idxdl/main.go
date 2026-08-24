// Command idxdl downloads index OHLC history from Upstox into the CSV layout
// the backtester replays (test/data/<timeframe>/<symbol>_real.csv).
//
// It exists because the Donchian option work computes its signal on the index
// chart rather than on an option premium, and Upstox serves index history
// anonymously — unlike cmd/histdl, this needs no interactive login and no
// entitlement.
//
//	go run ./cmd/idxdl -index nifty -from 2023-09-01 -to 2026-08-21
//	go run ./cmd/idxdl -index sensex -from 2023-09-01 -to 2026-08-21
//
// Index bars carry no volume — the exchanges do not publish one for an index —
// so the volume column is written as 0 and the strategy must not treat that as
// a halted instrument. See the sawVolume note in pkg/strategy/donchian.go.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zerobha/internal/models"
	"zerobha/pkg/upstox"
)

// knownIndices maps a short name to its Upstox instrument key and the file
// stem the backtester will look for.
var knownIndices = map[string]struct {
	Key  string
	Stem string
}{
	"nifty":     {"NSE_INDEX|Nifty 50", "nifty50"},
	"banknifty": {"NSE_INDEX|Nifty Bank", "niftybank"},
	"sensex":    {"BSE_INDEX|SENSEX", "sensex"},
}

func main() {
	index := flag.String("index", "nifty", "index to download: nifty, banknifty, sensex")
	fromStr := flag.String("from", "", "start date YYYY-MM-DD (required)")
	toStr := flag.String("to", time.Now().Format("2006-01-02"), "end date YYYY-MM-DD")
	timeframe := flag.String("timeframe", "5minute", "output directory under test/data, and the bar size: 5minute, 15minute, day")
	outDir := flag.String("out", "test/data", "root of the CSV tree")
	flag.Parse()

	spec, ok := knownIndices[strings.ToLower(*index)]
	if !ok {
		log.Fatalf("unknown index %q; known: nifty, banknifty, sensex", *index)
	}
	if *fromStr == "" {
		log.Fatal("-from is required")
	}
	from, err := time.Parse("2006-01-02", *fromStr)
	if err != nil {
		log.Fatalf("invalid -from: %v", err)
	}
	to, err := time.Parse("2006-01-02", *toStr)
	if err != nil {
		log.Fatalf("invalid -to: %v", err)
	}

	unit, interval, err := parseTimeframe(*timeframe)
	if err != nil {
		log.Fatal(err)
	}

	// No token: this endpoint serves index history anonymously, and asking for
	// one would couple this tool to an entitlement it does not need.
	client := upstox.NewClient("", 60*time.Second)

	// Request a month at a time. Intraday history is capped per request, and a
	// single three-year call silently returns a truncated window rather than
	// an error — which would look like missing sessions much later, in a
	// backtest, with no indication of why.
	var all []models.Candle
	seen := make(map[string]bool)
	for start := from; start.Before(to) || start.Equal(to); start = start.AddDate(0, 1, 0) {
		end := start.AddDate(0, 1, 0).AddDate(0, 0, -1)
		if end.After(to) {
			end = to
		}
		batch, err := client.HistoricalCandles(spec.Key, unit, interval, start, end)
		if err != nil {
			log.Fatalf("fetch %s %s..%s: %v", spec.Key, start.Format("2006-01-02"), end.Format("2006-01-02"), err)
		}
		for _, c := range batch {
			// Month windows are inclusive at both ends and the API is
			// occasionally generous at the boundary; dedupe by timestamp so a
			// bar cannot be replayed twice.
			key := c.StartTime.Format(time.RFC3339)
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, c)
		}
		fmt.Printf("%s %s..%s: %d bars (total %d)\n", *index,
			start.Format("2006-01-02"), end.Format("2006-01-02"), len(batch), len(all))
	}

	if len(all) == 0 {
		log.Fatalf("no candles returned for %s — check the date range", spec.Key)
	}

	dir := filepath.Join(*outDir, *timeframe)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("create %s: %v", dir, err)
	}
	path := filepath.Join(dir, spec.Stem+"_real.csv")
	if err := writeCSV(path, all); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}

	fmt.Printf("\nWrote %d bars to %s\n", len(all), path)
	fmt.Printf("Span: %s → %s\n",
		all[0].StartTime.Format("2006-01-02 15:04"),
		all[len(all)-1].StartTime.Format("2006-01-02 15:04"))
	fmt.Printf("Backtest with: -csv <a csv listing %s> -timeframe %s\n", spec.Stem, *timeframe)
}

// parseTimeframe maps the directory name the backtester uses onto the
// unit/interval pair the v3 API takes.
func parseTimeframe(tf string) (unit string, interval int, err error) {
	switch strings.ToLower(tf) {
	case "1minute":
		return "minutes", 1, nil
	case "5minute", "5m":
		return "minutes", 5, nil
	case "15minute":
		return "minutes", 15, nil
	case "30minute":
		return "minutes", 30, nil
	case "day", "1d":
		return "days", 1, nil
	}
	return "", 0, fmt.Errorf("unsupported timeframe %q", tf)
}

func writeCSV(path string, candles []models.Candle) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"timestamp", "open", "high", "low", "close", "volume"}); err != nil {
		return err
	}
	for _, c := range candles {
		if err := w.Write([]string{
			c.StartTime.Format(time.RFC3339),
			c.Open.StringFixed(2),
			c.High.StringFixed(2),
			c.Low.StringFixed(2),
			c.Close.StringFixed(2),
			c.Volume.StringFixed(0),
		}); err != nil {
			return err
		}
	}
	return w.Error()
}
