package strategy

import (
	"testing"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// srTestConfig pins every knob these tests assert on rather than inheriting the
// shipped defaults. Tuning a default is a routine act and must not silently
// rewrite what a test claims to prove — CLAUDE.md records this happening to
// thirteen Donchian tests at once when a threshold moved.
//
// The higher-timeframe gates are off here: the fixtures below are about the
// four level interactions, and a five-day synthetic history produces no swing
// zones at all, so leaving the confirmation on would silence every entry these
// tests exist to assert. TestSRHTFConfirmationBlocksUnbackedLevel covers the
// gate on its own terms.
func srTestConfig() config.SRLevelsConfig {
	cfg := config.DefaultSRLevelsConfig()
	cfg.ATRPeriod = 5
	cfg.PivotMethod = string(indicators.PivotTraditional)
	cfg.BreakBufferATR = 0.10
	cfg.TouchATR = 0.25
	cfg.ProximityATR = 1.0
	cfg.SLATRMult = 1.5
	tp := 3.0
	cfg.TPATRMult = &tp
	noTrail := 0.0
	cfg.TrailATRMult = &noTrail
	cfg.MinATRPct = 0.03
	cfg.EntryStartMin = 9*60 + 30
	cfg.EntryCutoffMin = 14*60 + 31
	cfg.MaxEntriesPerSymbol = 4
	perLevel := 2
	cfg.MaxEntriesPerLevel = &perLevel
	off, on := false, true
	cfg.UseHTFConfirm = &off
	cfg.AllowShort = &on
	room := 0.0
	cfg.MinRoomATR = &room
	return cfg
}

func srCandle(day, hour, min int, o, h, l, c float64) models.Candle {
	start := time.Date(2026, 3, day, hour, min, 0, 0, istLocation)
	return models.Candle{
		Symbol:    "TEST",
		Timeframe: "5m",
		Open:      decimal.NewFromFloat(o),
		High:      decimal.NewFromFloat(h),
		Low:       decimal.NewFromFloat(l),
		Close:     decimal.NewFromFloat(c),
		StartTime: start,
		EndTime:   start.Add(5 * time.Minute),
	}
}

// seedPrevSession walks day 10 from 100 up to 110, down to 90 and back to 100
// in steps of 1, so the completed session is H 110 / L 90 / C 100 and the
// traditional pivots for the next day come out on round numbers:
//
//	S3 70  S2 80  S1 90  PP 100  R1 110  R2 120  R3 130
//
// The small per-bar steps keep ATR near 1, so the buffer (0.1 ATR) and the
// touch tolerance (0.25 ATR) are both comfortably under a tenth of a point.
func seedPrevSession(t *testing.T, s *SRLevels) {
	t.Helper()
	price := 100.0
	minute := 9*60 + 15
	step := func(to float64) {
		for price != to {
			prev := price
			if price < to {
				price++
			} else {
				price--
			}
			h, l := prev, price
			if price > prev {
				h, l = price, prev
			}
			c := srCandle(10, minute/60, minute%60, prev, h, l, price)
			if sig := s.OnCandle(c); sig != nil {
				t.Fatalf("seed bar at %.0f produced a signal: %+v", price, sig)
			}
			minute += 5
		}
	}
	step(110)
	step(90)
	step(100)
}

// openDay10Levels feeds the first bar of day 11, which is what closes the
// previous session and builds the level set. It carries no signal of its own
// because the strategy has no previous close to compare against yet.
func openNextSession(t *testing.T, s *SRLevels, o, h, l, c float64) {
	t.Helper()
	if sig := s.OnCandle(srCandle(11, 9, 30, o, h, l, c)); sig != nil {
		t.Fatalf("the first bar of a session produced a signal: %+v", sig)
	}
}

func newSeeded(t *testing.T, cfg config.SRLevelsConfig) *SRLevels {
	t.Helper()
	s := NewSRLevelsStrategy([]string{"TEST"}, cfg)
	seedPrevSession(t, s)
	return s
}

// Price above the pivot, then a bearish bar closing decisively below it: the
// support has given way, which is the PE leg of the strategy.
func TestSRSupportBreakGoesShort(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	sig := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4))
	if sig == nil {
		t.Fatal("no signal on a confirmed support break")
	}
	if sig.Type != models.SellSignal {
		t.Errorf("side = %v, want SELL (buy a PE)", sig.Type)
	}
	if sig.Metadata["Kind"] != "support-break" {
		t.Errorf("kind = %q, want support-break", sig.Metadata["Kind"])
	}
	if sig.Metadata["Level"] != "PP" {
		t.Errorf("level = %q, want PP", sig.Metadata["Level"])
	}
}

// Price above the pivot, a bar that trades into it and closes back above on a
// bullish candle: the support held, which is the CE leg.
func TestSRSupportBounceGoesLong(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	sig := s.OnCandle(srCandle(11, 9, 35, 100.3, 100.9, 99.9, 100.8))
	if sig == nil {
		t.Fatal("no signal on a confirmed bounce off support")
	}
	if sig.Type != models.BuySignal {
		t.Errorf("side = %v, want BUY (buy a CE)", sig.Type)
	}
	if sig.Metadata["Kind"] != "support-bounce" {
		t.Errorf("kind = %q, want support-bounce", sig.Metadata["Kind"])
	}
}

// Approaching the same level from below reverses both readings: a close above
// it is a resistance break, not a bounce.
func TestSRResistanceBreakGoesLong(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 99.5, 99.8, 99.2, 99.5)

	sig := s.OnCandle(srCandle(11, 9, 35, 99.6, 100.8, 99.5, 100.7))
	if sig == nil {
		t.Fatal("no signal on a confirmed resistance break")
	}
	if sig.Type != models.BuySignal {
		t.Errorf("side = %v, want BUY (buy a CE)", sig.Type)
	}
	if sig.Metadata["Kind"] != "resistance-break" {
		t.Errorf("kind = %q, want resistance-break", sig.Metadata["Kind"])
	}
}

func TestSRResistanceRejectionGoesShort(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 99.5, 99.8, 99.2, 99.5)

	sig := s.OnCandle(srCandle(11, 9, 35, 99.7, 100.1, 99.1, 99.2))
	if sig == nil {
		t.Fatal("no signal on a confirmed rejection at resistance")
	}
	if sig.Type != models.SellSignal {
		t.Errorf("side = %v, want SELL (buy a PE)", sig.Type)
	}
	if sig.Metadata["Kind"] != "resistance-reject" {
		t.Errorf("kind = %q, want resistance-reject", sig.Metadata["Kind"])
	}
}

// The confirmation candle is the whole of the entry rule: a close through the
// level on a bar pointing the other way is a wick, not a break, and the
// strategy must decline it. Without this check the rule set is just "price is
// on the other side of a number", which fires constantly.
func TestSRRequiresTheBarToConfirmTheDirection(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	// Closes below the pivot, but the bar itself is bullish (close > open).
	if sig := s.OnCandle(srCandle(11, 9, 35, 99.2, 99.6, 99.0, 99.4)); sig != nil {
		t.Fatalf("a bullish bar was accepted as a support break: %+v", sig)
	}
}

// A close that merely touches the level has not cleared it. The buffer exists
// so tick noise around a round number does not read as a break.
func TestSRBufferRejectsAMarginalClose(t *testing.T) {
	cfg := srTestConfig()
	cfg.BreakBufferATR = 5.0 // far wider than any move in this fixture
	s := newSeeded(t, cfg)
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	if sig := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4)); sig != nil {
		t.Fatalf("a close inside the buffer was accepted: %+v", sig)
	}
}

// The levels a session trades come from the session BEFORE it. Deriving them
// from a bar that includes today would read the future — the same selection
// lookahead CLAUDE.md warns about for watchlists, in miniature.
func TestSRLevelsComeFromThePreviousSessionOnly(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	st := s.state["TEST"]
	if len(st.levels) != 7 {
		t.Fatalf("got %d levels, want 7", len(st.levels))
	}
	want := map[string]float64{"S3": 70, "S2": 80, "S1": 90, "PP": 100, "R1": 110, "R2": 120, "R3": 130}
	for _, l := range st.levels {
		if !l.price.Equal(decimal.NewFromFloat(want[l.name])) {
			t.Errorf("%s = %s, want %v", l.name, l.price, want[l.name])
		}
	}

	// A violent bar later in the day must not move any of them.
	s.OnCandle(srCandle(11, 10, 0, 100.5, 140, 60, 100.5))
	for _, l := range s.state["TEST"].levels {
		if !l.price.Equal(decimal.NewFromFloat(want[l.name])) {
			t.Errorf("%s moved to %s after an intraday bar", l.name, l.price)
		}
	}
}

// The stop and target are fixed ATR multiples of entry on both sides, and a
// short's target must sit below its entry — the mirror case is where a sign
// error hides.
func TestSRStopAndTargetGeometry(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	sig := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4))
	if sig == nil {
		t.Fatal("no signal on a confirmed support break")
	}
	atr, err := decimal.NewFromString(sig.Metadata["ATR"])
	if err != nil {
		t.Fatalf("ATR metadata unreadable: %v", err)
	}
	risk := sig.StopLoss.Sub(sig.Price)
	reward := sig.Price.Sub(sig.Target)
	if !risk.IsPositive() {
		t.Fatalf("short stop %s is not above entry %s", sig.StopLoss, sig.Price)
	}
	if !reward.IsPositive() {
		t.Fatalf("short target %s is not below entry %s", sig.Target, sig.Price)
	}
	// 1.5 : 3 ATR, so the reward must be exactly twice the risk.
	if !reward.Round(6).Equal(risk.Mul(decimal.NewFromInt(2)).Round(6)) {
		t.Errorf("reward %s is not 2x risk %s (ATR %s)", reward, risk, atr)
	}
}

// An explicit tp_atr_mult of 0 means "no target", and the signal must carry a
// zero Target so the simulator's guard treats it as absent rather than as a
// target at price 0. This is the trap tp_rr and trail_atr_mult each fell into.
func TestSRZeroTargetLeavesTheTargetUnset(t *testing.T) {
	cfg := srTestConfig()
	zero := 0.0
	cfg.TPATRMult = &zero
	s := newSeeded(t, cfg)
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	sig := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4))
	if sig == nil {
		t.Fatal("no signal on a confirmed support break")
	}
	if !sig.Target.IsZero() {
		t.Errorf("Target = %s, want zero when tp_atr_mult is 0", sig.Target)
	}
	if !sig.StopLoss.IsPositive() {
		t.Error("a target-free signal still needs its stop")
	}
}

// With confirmation on and no daily history behind the levels, nothing is
// backed by a higher-timeframe zone and the strategy must take no trade at all.
// This is the gate that separates a pivot the market watches from arithmetic on
// yesterday's bar, and switching it off is what multiplies the trade count.
func TestSRHTFConfirmationBlocksUnbackedLevel(t *testing.T) {
	cfg := srTestConfig()
	on := true
	cfg.UseHTFConfirm = &on
	s := newSeeded(t, cfg)
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	if sig := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4)); sig != nil {
		t.Fatalf("an unbacked level was traded with confirmation on: %+v", sig)
	}
}

// Entries outside the window are not the engine's job alone — the strategy owns
// its own hours, and the engine's earlier default cutoff would otherwise be the
// only thing enforcing them.
func TestSREntryWindowIsEnforced(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	if sig := s.OnCandle(srCandle(11, 15, 0, 100.4, 100.5, 99.3, 99.4)); sig != nil {
		t.Fatalf("a bar past the cutoff produced a signal: %+v", sig)
	}
}

// A zero-volume candle is real on Kite for a halted name and carries a
// degenerate range. It must not reach the ATR or the daily bar — but only for
// instruments that report volume at all, or an index would lose every bar.
func TestSRIndexBarsSurviveTheZeroVolumeGuard(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	// Every bar in these fixtures carries zero volume, exactly like an index.
	if sig := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4)); sig == nil {
		t.Fatal("a zero-volume index bar was rejected as a halt")
	}

	// A symbol that HAS reported volume is a different matter.
	s2 := NewSRLevelsStrategy([]string{"TEST"}, srTestConfig())
	c := srCandle(10, 9, 15, 100, 101, 99, 100)
	c.Volume = decimal.NewFromInt(1000)
	s2.OnCandle(c)
	if !s2.state["TEST"].sawVolume {
		t.Fatal("a positive-volume bar did not set sawVolume")
	}
}

// The strategy took at most ONE trade per session until 2026-08-31, which made
// max_entries_per_symbol and max_entries_per_level dead knobs and cut the
// backtest to 120 trades in three years.
//
// The cause: openValid was a one-way latch. It was set on entry and cleared
// only at the date change, and nothing else could clear it — there is no hook
// by which a broker tells a strategy that a resting stop has fired, and this
// strategy has no ExitAdvisor. So every bar after the day's first entry hit the
// "no re-entries while a position is open" guard and returned nil.
//
// The fix mirrors the broker's protective levels in the strategy. This test
// pins it: stop the first trade out, then take a second on a later bar.
func TestSRReentersAfterItsStopIsHit(t *testing.T) {
	cfg := srTestConfig()
	noTrail := 0.0
	cfg.TrailATRMult = &noTrail
	s := newSeeded(t, cfg)
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	first := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4))
	if first == nil {
		t.Fatal("no signal on the first support break")
	}
	if first.Type != models.SellSignal {
		t.Fatalf("first side = %v, want SELL", first.Type)
	}

	// A bar whose high runs through the short's stop closes that position at
	// the broker, so the strategy must stop believing it holds one. It closes
	// ON the pivot, inside the break buffer, so this bar cannot itself confirm
	// a fresh interaction — otherwise it would re-arm openValid immediately and
	// the assertion below would pass for the wrong reason.
	through := first.StopLoss.Add(decimal.NewFromFloat(0.5))
	stopBar := srCandle(11, 9, 40, 99.5, through.InexactFloat64(), 99.4, 100.0)
	if sig := s.OnCandle(stopBar); sig != nil {
		t.Fatalf("the stop bar was expected to be signal-free, got %+v", sig)
	}
	if st := s.state["TEST"]; st.openValid {
		t.Fatal("the strategy still believes it holds a position after its stop was taken out")
	}

	// Price is back above the pivot, so a fresh break is a fresh trade.
	second := s.OnCandle(srCandle(11, 9, 45, 100.4, 100.5, 99.3, 99.4))
	if second == nil {
		t.Fatal("no second entry after the first was stopped out")
	}
	if st := s.state["TEST"]; st.entriesToday != 2 {
		t.Errorf("entriesToday = %d, want 2", st.entriesToday)
	}
}

// The mirror must not retire a position the broker still holds: a bar that
// merely moves against the trade, without reaching the stop, leaves it open.
func TestSRKeepsThePositionWhileTheStopHolds(t *testing.T) {
	s := newSeeded(t, srTestConfig())
	openNextSession(t, s, 100.5, 100.8, 100.2, 100.5)

	sig := s.OnCandle(srCandle(11, 9, 35, 100.4, 100.5, 99.3, 99.4))
	if sig == nil {
		t.Fatal("no signal on the support break")
	}
	short := sig.StopLoss.Sub(decimal.NewFromFloat(0.2))
	s.OnCandle(srCandle(11, 9, 40, 99.5, short.InexactFloat64(), 99.4, 99.6))
	if st := s.state["TEST"]; !st.openValid {
		t.Fatal("the position was retired by a bar that never reached its stop")
	}
}
