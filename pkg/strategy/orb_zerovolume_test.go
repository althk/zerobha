package strategy

import (
	"testing"
	"time"
	"zerobha/internal/config"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// istTime parses "2006-01-02 15:04" as IST.
func istTime(dateStr string) time.Time {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	t, _ := time.ParseInLocation("2006-01-02 15:04", dateStr, loc)
	return t
}

// orbBar builds a 5-minute ORB candle at 09:15 IST + idx*5m.
func orbBar(date string, idx int, o, h, l, c float64, vol int64) models.Candle {
	start := istTime(date + " 09:15").Add(time.Duration(idx) * 5 * time.Minute)
	return models.Candle{
		Symbol:     "HALTED",
		Timeframe:  "5m",
		Open:       decimal.NewFromFloat(o),
		High:       decimal.NewFromFloat(h),
		Low:        decimal.NewFromFloat(l),
		Close:      decimal.NewFromFloat(c),
		Volume:     decimal.NewFromInt(vol),
		StartTime:  start,
		EndTime:    start.Add(5 * time.Minute),
		IsComplete: true,
	}
}

// A session that trades no volume at all leaves VWAP at zero. ORB measures the
// breakout's extension as a percentage of VWAP, which used to divide by that
// zero and panic on real broker data (Kite emits zero-volume candles for
// halted or completely illiquid symbols; Yahoo silently omits them).
func TestORB_ZeroVolumeSessionDoesNotPanic(t *testing.T) {
	s := NewORBStrategy([]string{"HALTED"}, config.DefaultORBConfig())

	// A full session of zero-volume candles, including a move that clears the
	// opening range so the breakout path — where the division lives — is
	// actually reached.
	for i := 0; i < 40; i++ {
		price := 100.0
		if i >= 3 {
			price = 105.0 // breaks above the 09:15–09:30 range
		}
		candle := orbBar("2024-01-15", i, price, price+1, price-1, price, 0)
		if sig := s.OnCandle(candle); sig != nil {
			t.Fatalf("bar %d: expected no signal from a zero-volume session, got %+v", i, sig)
		}
	}
}

// A symbol that goes quiet mid-session (zero-volume candles after real
// trading) keeps a non-zero VWAP and must still be evaluated normally.
func TestORB_ZeroVolumeCandlesMidSessionAreSafe(t *testing.T) {
	s := NewORBStrategy([]string{"HALTED"}, config.DefaultORBConfig())

	for i := 0; i < 40; i++ {
		vol := int64(10000)
		if i >= 6 {
			vol = 0 // trading dries up after the opening range
		}
		price := 100.0
		if i >= 6 {
			price = 105.0
		}
		candle := orbBar("2024-01-16", i, price, price+1, price-1, price, vol)
		// The only requirement is that this does not panic; whether it signals
		// depends on the other filters.
		_ = s.OnCandle(candle)
	}
}
