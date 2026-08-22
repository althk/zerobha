package strategy

import (
	"testing"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

var istZone = time.FixedZone("IST", 5*3600+1800)

// dailyCandle builds a daily bar with a range wide enough to keep ATR positive.
func dailyCandle(sym string, day time.Time, close float64) models.Candle {
	c := decimal.NewFromFloat(close)
	span := c.Mul(decimal.NewFromFloat(0.01)) // 1% high-low range
	return models.Candle{
		Symbol:    sym,
		Timeframe: "day",
		Open:      c,
		High:      c.Add(span),
		Low:       c.Sub(span),
		Close:     c,
		Volume:    decimal.NewFromInt(100000),
		StartTime: day,
		EndTime:   day,
	}
}

// feed replays a close series one session per weekday and returns every signal.
func feed(s *DailyReversal, sym string, closes []float64) []*models.Signal {
	var out []*models.Signal
	day := time.Date(2022, 1, 3, 0, 0, 0, 0, istZone)
	for _, px := range closes {
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
		}
		if sig := s.OnCandle(dailyCandle(sym, day, px)); sig != nil {
			out = append(out, sig)
		}
		day = day.AddDate(0, 0, 1)
	}
	return out
}

func testCfg() config.DailyRevConfig {
	cfg := config.DefaultDailyRevConfig()
	cfg.TrendPeriod = 20 // keep test series short
	cfg.MinPrice = 0
	return cfg
}

// A dip inside an established uptrend is the setup the strategy exists to take.
func TestDailyReversalEntersDipInUptrend(t *testing.T) {
	s := NewDailyReversalStrategy([]string{"TEST"}, testCfg())

	closes := make([]float64, 0, 60)
	for i := 0; i < 50; i++ { // steady uptrend to lift price above the trend SMA
		closes = append(closes, 100+2*float64(i))
	}
	// Sharp three-day drop: pushes RSI(2) under 10 while the close stays above
	// SMA(20). The trend has to be steep enough that the dip does not itself
	// drag price through the regime filter.
	closes = append(closes, 194, 190, 186)

	sigs := feed(s, "TEST", closes)
	if len(sigs) == 0 {
		t.Fatal("expected an entry on the dip, got none")
	}
	sig := sigs[len(sigs)-1]
	if sig.Type != models.BuySignal {
		t.Errorf("expected BuySignal, got %v", sig.Type)
	}
	if sig.ProductType != "CNC" {
		t.Errorf("multi-day hold must be CNC, got %q", sig.ProductType)
	}
	if !sig.Target.GreaterThan(sig.Price) {
		t.Errorf("target %s must exceed entry %s", sig.Target, sig.Price)
	}
	if !sig.StopLoss.LessThan(sig.Price) {
		t.Errorf("stop %s must sit below entry %s", sig.StopLoss, sig.Price)
	}
	if _, ok := sig.Metadata[models.MetaExitOnOrAfter]; !ok {
		t.Error("expected a time stop in metadata")
	}
}

// The regime filter is the difference between buying a dip and catching a
// falling knife; without it this rule set buys all the way down a downtrend.
func TestDailyReversalSkipsDowntrend(t *testing.T) {
	s := NewDailyReversalStrategy([]string{"TEST"}, testCfg())

	closes := make([]float64, 0, 60)
	for i := 0; i < 50; i++ { // steady downtrend
		closes = append(closes, 200-float64(i))
	}
	closes = append(closes, 145, 140) // oversold, but below the trend SMA

	if sigs := feed(s, "TEST", closes); len(sigs) != 0 {
		t.Fatalf("expected no entry below the trend SMA, got %d", len(sigs))
	}
}

// Regression: zero-volume candles are real on Kite and must neither signal nor
// poison the indicators (see CLAUDE.md and orb_zerovolume_test.go).
func TestDailyReversalIgnoresZeroVolumeCandles(t *testing.T) {
	s := NewDailyReversalStrategy([]string{"TEST"}, testCfg())

	day := time.Date(2022, 1, 3, 0, 0, 0, 0, istZone)
	c := dailyCandle("TEST", day, 100)
	c.Volume = decimal.Zero

	if sig := s.OnCandle(c); sig != nil {
		t.Fatal("zero-volume candle must not produce a signal")
	}
	if s.state["TEST"].bars != 0 {
		t.Error("zero-volume candle must not advance the warmup counter")
	}
}

// No signal may fire before every indicator has warmed up.
func TestDailyReversalRespectsWarmup(t *testing.T) {
	cfg := testCfg()
	s := NewDailyReversalStrategy([]string{"TEST"}, cfg)

	closes := make([]float64, 0, cfg.TrendPeriod)
	for i := 0; i < cfg.TrendPeriod; i++ {
		closes = append(closes, 100+float64(i%3)) // choppy, would otherwise trigger
	}
	if sigs := feed(s, "TEST", closes); len(sigs) != 0 {
		t.Fatalf("expected no signal during warmup, got %d", len(sigs))
	}
}

// holdDeadline counts trading sessions, so a deadline set on a Friday must not
// expire over the weekend.
func TestHoldDeadlineSkipsWeekends(t *testing.T) {
	friday := time.Date(2026, 8, 21, 0, 0, 0, 0, istZone)
	if got := holdDeadline(friday, 1); got != "2026-08-24" { // the following Monday
		t.Errorf("1 session after Friday should be Monday 2026-08-24, got %s", got)
	}
	if got := holdDeadline(friday, 5); got != "2026-08-28" {
		t.Errorf("5 sessions after Friday should be 2026-08-28, got %s", got)
	}
}
