package strategy

import (
	"log"
	"strings"
	"time"

	"zerobha/internal/models"
)

// warmupDays is how many calendar days of history a live warm-up asks for:
// enough to span a weekend plus a holiday and still hand the indicators more
// than one full session.
const warmupDays = 5

// nowFn is the clock warm-up uses to recognise the still-forming bar. A var
// so tests can pin it.
var nowFn = time.Now

// barDuration is the length of one bar for a strategy's timeframe, in either
// of the spellings the repo uses ("5m" and Kite's "5minute"). An unknown
// spelling falls back to five minutes, the only intraday bar size any
// strategy here is configured with, and says so.
func barDuration(tf string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(tf)) {
	case "1m", "minute", "1minute":
		return time.Minute
	case "3m", "3minute":
		return 3 * time.Minute
	case "", "5m", "5minute":
		return 5 * time.Minute
	case "10m", "10minute":
		return 10 * time.Minute
	case "15m", "15minute":
		return 15 * time.Minute
	case "30m", "30minute":
		return 30 * time.Minute
	case "60m", "1h", "60minute", "hour":
		return time.Hour
	case "1d", "day":
		return 24 * time.Hour
	}
	log.Printf("WARNING: unrecognised timeframe %q, assuming 5-minute bars for warm-up", tf)
	return 5 * time.Minute
}

// completedBars drops the bars that had not closed by now. Kite's historical
// endpoint includes the bucket still forming, and the live feed delivers that
// same bar again once it closes: replaying the partial one would feed the
// indicators a bar that never finished, and a strategy that skips bars it has
// already seen would then drop the real one.
func completedBars(candles []models.Candle, bar time.Duration, now time.Time) []models.Candle {
	out := candles[:0:0]
	for _, c := range candles {
		if c.StartTime.Add(bar).After(now) {
			continue
		}
		out = append(out, c)
	}
	return out
}
