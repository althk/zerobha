package strategy

import (
	"testing"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// pinNow fixes the warm-up clock for one test.
func pinNow(t *testing.T, now time.Time) {
	t.Helper()
	prev := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = prev })
}

// flatDonchianHistory is 40 quiet 5-minute bars on the session before
// dcCandle's date, with the given volume on every bar.
func flatDonchianHistory(vol int64) []models.Candle {
	history := make([]models.Candle, 0, 40)
	for i := range 40 {
		min := 15 + i*5
		c := dcCandle(9+min/60, min%60, 100, 102, 98, 100, vol)
		c.StartTime = c.StartTime.AddDate(0, 0, -1)
		c.EndTime = c.EndTime.AddDate(0, 0, -1)
		history = append(history, c)
	}
	return history
}

// Regression: an index reports zero volume on every bar, and Init skipped
// zero-volume bars outright, so a live NIFTY/SENSEX session warmed up on
// nothing and could not trade until its own 30-bar channel filled (~11:45).
func TestDonchianInitWarmsUpAnIndex(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	if err := s.Init(&stubHistory{candles: flatDonchianHistory(0)}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 0)); sig == nil {
		t.Error("no signal on the first live index bar after warm-up")
	}
}

// Kite's history includes the bucket still forming. Ingesting it feeds the
// channel a bar that never finished — here a spike that lifts the upper band
// out of reach of a genuine breakout.
func TestDonchianInitSkipsTheFormingBar(t *testing.T) {
	history := flatDonchianHistory(1000)
	last := &history[len(history)-1]
	last.High = decimal.NewFromInt(200)
	pinNow(t, last.StartTime.Add(2*time.Minute))

	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	if err := s.Init(&stubHistory{candles: history}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig == nil {
		t.Error("the forming bar reached the channel and blocked the breakout")
	}
}

// emaWarmupSeries is 20 falling 5-minute bars then 10 rising ones, one
// session, so the fast EMA crosses the slow one on the way back up.
func emaWarmupSeries() []models.Candle {
	start := time.Date(2026, 3, 9, 9, 15, 0, 0, istLocation)
	var out []models.Candle
	p := 100.0
	for i := range 30 {
		if i < 20 {
			p--
		} else {
			p += 2
		}
		ts := start.Add(time.Duration(i) * 5 * time.Minute)
		c := makeCandle("TEST", ts, p, p+1, p-1, p)
		c.EndTime = ts.Add(5 * time.Minute)
		out = append(out, c)
	}
	return out
}

// emaWarmupStrategy pins every knob the comparison depends on, so tuning a
// shipped default cannot change what the test asserts.
func emaWarmupStrategy() *EMACross {
	cfg := config.DefaultEMACrossConfig()
	cfg.FastPeriod = 3
	cfg.SlowPeriod = 5
	cfg.ATRPeriod = 3
	cfg.SLATRMult = 6.0
	cfg.TPATRMult = 0
	cfg.ProductType = "MIS"
	cfg.Timeframe = "5m"
	cfg.EntryStartMin = 0
	cfg.EntryCutoffMin = 0
	cfg.ADXThreshold = 0
	cfg.MinSepATR = 0
	cfg.MaxEntriesPerSymbol = 0
	trail := 3.0
	cfg.TrailATRMult = &trail
	return NewEMACrossStrategy([]string{"TEST"}, cfg)
}

// Regression: EMACross.Init did nothing, and the trader restarts every day,
// so live sessions began with cold EMAs — no cross until the slow EMA filled,
// and different EMA values from the backtest's for hours after. A warmed-up
// strategy must emit exactly what a strategy fed the same bars as a replay
// does. The last history bar is still forming when Init runs and must be
// taken from the live feed instead, or the timestamp guard in updateCandle
// would drop the completed bar as already seen.
func TestEMACross_InitMatchesAReplay(t *testing.T) {
	series := emaWarmupSeries()
	const split = 19 // series[split] is forming at Init

	replay := emaWarmupStrategy()
	want := make([]*models.Signal, len(series))
	for i, c := range series {
		_ = replay.ExitAdvice(c)
		want[i] = replay.OnCandle(c)
	}

	history := append([]models.Candle(nil), series[:split+1]...)
	history[split].Close = decimal.NewFromInt(500) // a partial print
	history[split].High = decimal.NewFromInt(500)
	pinNow(t, series[split].StartTime.Add(2*time.Minute))

	live := emaWarmupStrategy()
	if err := live.Init(&stubHistory{candles: history}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	fired := false
	for i := split; i < len(series); i++ {
		_ = live.ExitAdvice(series[i])
		got := live.OnCandle(series[i])
		if (got == nil) != (want[i] == nil) {
			t.Fatalf("bar %d: live signal %v, replay signal %v", i, got != nil, want[i] != nil)
		}
		if got != nil {
			fired = true
			if !got.Price.Equal(want[i].Price) || got.Type != want[i].Type {
				t.Fatalf("bar %d: live %s @ %s, replay %s @ %s", i, got.Type, got.Price, want[i].Type, want[i].Price)
			}
		}
	}
	if !fired {
		t.Fatal("fixture never crossed; the comparison proves nothing")
	}

	lst, rst := live.state["TEST"], replay.state["TEST"]
	if !lst.fastEMA.Value().Equal(rst.fastEMA.Value()) || !lst.slowEMA.Value().Equal(rst.slowEMA.Value()) {
		t.Errorf("EMAs diverged: live %s/%s, replay %s/%s",
			lst.fastEMA.Value(), lst.slowEMA.Value(), rst.fastEMA.Value(), rst.slowEMA.Value())
	}
}
