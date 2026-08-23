package strategy

import (
	"errors"
	"testing"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// stubGate is a NewsGate whose verdict the test dictates.
type stubGate struct {
	verdict core.GateVerdict
	err     error
	calls   int
}

func (g *stubGate) Assess(string, time.Time) (core.GateVerdict, error) {
	g.calls++
	return g.verdict, g.err
}

func gapFadeTestConfig() config.GapFadeConfig {
	cfg := config.DefaultGapFadeConfig()
	cfg.ATRPeriod = 3 // warm up quickly inside a short fixture
	return cfg
}

func gfCandle(day int, hour, min int, o, h, l, c float64, vol int64) models.Candle {
	start := time.Date(2026, 3, day, hour, min, 0, 0, istLocation)
	return models.Candle{
		Symbol:    "TEST",
		Timeframe: "5m",
		Open:      decimal.NewFromFloat(o),
		High:      decimal.NewFromFloat(h),
		Low:       decimal.NewFromFloat(l),
		Close:     decimal.NewFromFloat(c),
		Volume:    decimal.NewFromInt(vol),
		StartTime: start,
		EndTime:   start.Add(5 * time.Minute),
	}
}

// warmupDay feeds a flat session at `price` so the ATR is seeded and the
// strategy has a previous close to measure tomorrow's gap against.
func warmupDay(s *GapFade, day int, price float64) {
	for i := 0; i < 12; i++ {
		min := 15 + i*5
		h, m := 9+min/60, min%60
		s.OnCandle(gfCandle(day, h, m, price, price+1, price-1, price, 10000))
	}
}

// gapDownDay replays a −6% gap session that sells off to `low` during the
// observation window and then reclaims. It returns the first signal emitted.
func gapDownDay(s *GapFade, day int, prevClose, low float64, reclaimClose float64) *models.Signal {
	open := prevClose * 0.94 // −6% gap

	// Observation window 09:15–09:25: slide down to `low`, high == open.
	s.OnCandle(gfCandle(day, 9, 15, open, open, low+1, low+2, 50000))
	s.OnCandle(gfCandle(day, 9, 20, low+2, low+2, low, low+1, 50000))
	s.OnCandle(gfCandle(day, 9, 25, low+1, low+3, low, low+2, 50000))

	// 09:30 onward: recovery candle closing above the observation high (open).
	return s.OnCandle(gfCandle(day, 9, 30, low+2, reclaimClose, low+2, reclaimClose, 90000))
}

func TestGapFadeEntersOnReclaimWithOneToTwoRR(t *testing.T) {
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), nil)
	warmupDay(s, 2, 1000)

	sig := gapDownDay(s, 3, 1000, 930, 946)
	if sig == nil {
		t.Fatal("expected an entry signal on the reclaim candle")
	}
	if sig.Type != models.BuySignal {
		t.Errorf("expected BUY, got %v", sig.Type)
	}
	if sig.ProductType != "MIS" {
		t.Errorf("gap fade is intraday: expected MIS, got %s", sig.ProductType)
	}
	if !sig.StopLoss.LessThan(sig.Price) || !sig.Target.GreaterThan(sig.Price) {
		t.Fatalf("stop %s / target %s straddle entry %s incorrectly", sig.StopLoss, sig.Target, sig.Price)
	}

	risk := sig.Price.Sub(sig.StopLoss)
	reward := sig.Target.Sub(sig.Price)
	got, _ := reward.Div(risk).Float64()
	if got < 1.99 || got > 2.01 {
		t.Errorf("expected 1:2 reward-risk, got 1:%.3f (risk %s, reward %s)", got, risk, reward)
	}
}

func TestGapFadeIgnoresSmallGap(t *testing.T) {
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), nil)
	warmupDay(s, 2, 100)

	// −2% open: below the 5% qualifying threshold, so the reclaim is not ours.
	open := 98.0
	s.OnCandle(gfCandle(3, 9, 15, open, open, 97, 97.5, 50000))
	s.OnCandle(gfCandle(3, 9, 20, 97.5, 98, 97, 97.2, 50000))
	s.OnCandle(gfCandle(3, 9, 25, 97.2, 98, 97, 97.5, 50000))
	if sig := s.OnCandle(gfCandle(3, 9, 30, 97.5, 99.5, 97.5, 99.5, 90000)); sig != nil {
		t.Fatalf("a 2%% gap must not qualify, got %+v", sig)
	}
}

func TestGapFadeRejectsCorporateActionSizedGap(t *testing.T) {
	cfg := gapFadeTestConfig()
	s := NewGapFadeStrategy([]string{"TEST"}, cfg, nil)
	warmupDay(s, 2, 100)

	// −50%: a split, not a panic. MaxGapDownPct must reject it.
	s.OnCandle(gfCandle(3, 9, 15, 50, 50, 47, 48, 50000))
	s.OnCandle(gfCandle(3, 9, 20, 48, 49, 47, 48, 50000))
	s.OnCandle(gfCandle(3, 9, 25, 48, 49, 47, 48, 50000))
	if sig := s.OnCandle(gfCandle(3, 9, 30, 48, 52, 48, 52, 90000)); sig != nil {
		t.Fatalf("a 50%% gap is a corporate action and must be skipped, got %+v", sig)
	}
}

func TestGapFadeNeedsReclaimOfObservationHigh(t *testing.T) {
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), nil)
	warmupDay(s, 2, 100)

	// Qualifying gap, but the 09:30 candle closes below the observation high.
	s.OnCandle(gfCandle(3, 9, 15, 94, 94, 91, 92, 50000))
	s.OnCandle(gfCandle(3, 9, 20, 92, 92, 90, 90.5, 50000))
	s.OnCandle(gfCandle(3, 9, 25, 90.5, 91, 90, 90.5, 50000))
	if sig := s.OnCandle(gfCandle(3, 9, 30, 90.5, 93, 90.5, 92.5, 90000)); sig != nil {
		t.Fatalf("close below the observation high must not trigger, got %+v", sig)
	}
}

func TestGapFadeTakesOneTradePerDay(t *testing.T) {
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), nil)
	warmupDay(s, 2, 1000)

	if sig := gapDownDay(s, 3, 1000, 930, 946); sig == nil {
		t.Fatal("expected the first reclaim to signal")
	}
	if sig := s.OnCandle(gfCandle(3, 9, 35, 946, 960, 946, 960, 90000)); sig != nil {
		t.Fatalf("a second entry the same day must be suppressed, got %+v", sig)
	}
}

func TestGapFadeIgnoresLateReclaim(t *testing.T) {
	cfg := gapFadeTestConfig()
	s := NewGapFadeStrategy([]string{"TEST"}, cfg, nil)
	warmupDay(s, 2, 100)

	s.OnCandle(gfCandle(3, 9, 15, 94, 94, 91, 92, 50000))
	s.OnCandle(gfCandle(3, 9, 20, 92, 92, 90, 90.5, 50000))
	s.OnCandle(gfCandle(3, 9, 25, 90.5, 91, 90, 90.5, 50000))
	// 12:00 is past the 11:00 entry window.
	if sig := s.OnCandle(gfCandle(3, 12, 0, 90.5, 96, 90.5, 96, 90000)); sig != nil {
		t.Fatalf("reclaim after the entry window must be ignored, got %+v", sig)
	}
}

func TestGapFadeGateBlocksInformedGap(t *testing.T) {
	gate := &stubGate{verdict: core.GateVerdict{Allow: false, Reason: "news matched \"fraud\""}}
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), gate)
	warmupDay(s, 2, 1000)

	if sig := gapDownDay(s, 3, 1000, 930, 946); sig != nil {
		t.Fatalf("a blocked verdict must suppress the entry, got %+v", sig)
	}
	if gate.calls != 1 {
		t.Errorf("expected exactly one gate call, got %d", gate.calls)
	}
}

func TestGapFadeGateAllowsCleanGapAndRecordsReason(t *testing.T) {
	gate := &stubGate{verdict: core.GateVerdict{Allow: true, Reason: "news clean (3 recent items)"}}
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), gate)
	warmupDay(s, 2, 1000)

	sig := gapDownDay(s, 3, 1000, 930, 946)
	if sig == nil {
		t.Fatal("an allowed verdict must let the entry through")
	}
	if sig.Metadata["Gate"] != "news clean (3 recent items)" {
		t.Errorf("gate reason must be journalled, got %q", sig.Metadata["Gate"])
	}
}

func TestGapFadeGateErrorFailsClosedByDefault(t *testing.T) {
	gate := &stubGate{err: errors.New("token expired")}
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), gate)
	warmupDay(s, 2, 1000)

	if sig := gapDownDay(s, 3, 1000, 930, 946); sig != nil {
		t.Fatalf("a gate error must block the trade by default, got %+v", sig)
	}
}

func TestGapFadeGateErrorCanFailOpen(t *testing.T) {
	cfg := gapFadeTestConfig()
	open := true
	cfg.GateFailOpen = &open
	gate := &stubGate{err: errors.New("token expired")}
	s := NewGapFadeStrategy([]string{"TEST"}, cfg, gate)
	warmupDay(s, 2, 1000)

	if sig := gapDownDay(s, 3, 1000, 930, 946); sig == nil {
		t.Fatal("with gate_fail_open the trade must proceed despite the error")
	}
}

// A halted symbol emits zero-volume candles on Kite. They make session VWAP
// zero and carry a degenerate range; neither may crash the strategy nor
// manufacture a reclaim.
func TestGapFadeSurvivesZeroVolumeCandles(t *testing.T) {
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), nil)
	warmupDay(s, 2, 1000)

	// The opening print is a halt: zero volume, zero range. It must still roll
	// the session and measure the gap, but feed nothing to ATR or VWAP.
	if sig := s.OnCandle(gfCandle(3, 9, 15, 940, 940, 940, 940, 0)); sig != nil {
		t.Fatalf("zero-volume candle must not signal, got %+v", sig)
	}
	if sig := s.OnCandle(gfCandle(3, 9, 20, 940, 940, 930, 931, 50000)); sig != nil {
		t.Fatalf("observation-window candle must not signal, got %+v", sig)
	}
	if sig := s.OnCandle(gfCandle(3, 9, 25, 931, 933, 930, 932, 50000)); sig != nil {
		t.Fatalf("observation-window candle must not signal, got %+v", sig)
	}
	// Trading resumes and price reclaims the observation high: the halted
	// opening print must not have cost us the setup.
	if sig := s.OnCandle(gfCandle(3, 9, 30, 932, 946, 932, 946, 90000)); sig == nil {
		t.Fatal("expected the post-halt reclaim to be evaluated normally")
	}
}

// A whole observation window of halted candles leaves no reclaim level. The
// strategy must decline rather than invent one out of a degenerate range.
func TestGapFadeDeclinesWhenObservationWindowIsAllHalted(t *testing.T) {
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), nil)
	warmupDay(s, 2, 1000)

	for i := 0; i < 3; i++ {
		if sig := s.OnCandle(gfCandle(3, 9, 15+i*5, 940, 940, 940, 940, 0)); sig != nil {
			t.Fatalf("zero-volume candle must not signal, got %+v", sig)
		}
	}
	if sig := s.OnCandle(gfCandle(3, 9, 30, 940, 960, 940, 960, 90000)); sig != nil {
		t.Fatalf("no observation high means no entry, got %+v", sig)
	}
}

// The day-low floor must widen the stop rather than leave it inside the panic
// range, and the target must still sit at twice the widened risk.
func TestGapFadeStopIsFlooredAtDayLow(t *testing.T) {
	s := NewGapFadeStrategy([]string{"TEST"}, gapFadeTestConfig(), nil)
	warmupDay(s, 2, 1000)

	// −6% gap to 940, low 930, reclaim at 946. A 3-period ATR here is a few
	// rupees, so the raw ATR stop would sit well above the 930 day low.
	s.OnCandle(gfCandle(3, 9, 15, 940, 940, 934, 935, 50000))
	s.OnCandle(gfCandle(3, 9, 20, 935, 936, 930, 931, 50000))
	s.OnCandle(gfCandle(3, 9, 25, 931, 933, 930, 932, 50000))
	sig := s.OnCandle(gfCandle(3, 9, 30, 932, 946, 932, 946, 90000))
	if sig == nil {
		t.Fatal("expected an entry signal")
	}
	if !sig.StopLoss.LessThanOrEqual(decimal.NewFromInt(930)) {
		t.Errorf("stop %s must not sit above the 930 day low", sig.StopLoss)
	}
	risk := sig.Price.Sub(sig.StopLoss)
	reward := sig.Target.Sub(sig.Price)
	got, _ := reward.Div(risk).Float64()
	if got < 1.99 || got > 2.01 {
		t.Errorf("target must track the widened stop at 1:2, got 1:%.3f", got)
	}
}

// A stop wider than max_stop_pct makes the 1:2 target unreachable inside a
// session, so the setup is dropped rather than traded at a worse ratio.
func TestGapFadeRejectsOverwideStop(t *testing.T) {
	cfg := gapFadeTestConfig()
	cfg.MaxStopPct = 0.5
	s := NewGapFadeStrategy([]string{"TEST"}, cfg, nil)
	warmupDay(s, 2, 1000)

	if sig := gapDownDay(s, 3, 1000, 930, 946); sig != nil {
		t.Fatalf("stop wider than max_stop_pct must be rejected, got %+v", sig)
	}
}
