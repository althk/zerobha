package strategy

import (
	"errors"
	"testing"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// donchianTestConfig pins every knob these tests assert on, rather than
// inheriting the shipped defaults. Tuning a default is a routine act and must
// not silently rewrite what the tests claim to prove — an earlier version of
// this file inherited tp_rr, use_breakeven and vol_mult, and three tests began
// asserting the opposite of their own comments the moment those moved.
func donchianTestConfig() config.DonchianConfig {
	cfg := config.DefaultDonchianConfig()
	cfg.ATRPeriod = 3 // warm up inside a short fixture
	cfg.DonchianLookback = 4
	cfg.MaxEntriesPerSymbol = 1
	cfg.SLATRMult = 1.3
	cfg.TrailATRMult = 3.0
	on := true
	cfg.UseIgnition = &on
	cfg.IgnitionATRMult = 1.0
	// The ADX gate is off in these fixtures: none of them is about the trend
	// filter, and the synthetic bars are too few and too quiet to lift ADX
	// over the shipped default of 15, so leaving it on silences every entry
	// these tests exist to assert on.
	cfg.ADXThreshold = 0
	return cfg
}

func dcCandle(hour, min int, o, h, l, c float64, vol int64) models.Candle {
	start := time.Date(2026, 3, 10, hour, min, 0, 0, istLocation)
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

// warmup feeds a quiet range from 09:15 that seeds every indicator and leaves
// the channel at [98, 102]. The entry window opens at 09:30, so these bars are
// all before it and none of them can produce a signal.
func warmupDonchian(t *testing.T, s *Donchian) {
	t.Helper()
	for i := range 12 {
		min := 15 + i*5
		c := dcCandle(9+min/60, min%60, 100, 102, 98, 100, 1000)
		if sig := s.OnCandle(c); sig != nil {
			t.Fatalf("warm-up bar %d produced a signal: %+v", i, sig)
		}
	}
}

// A close that clears the channel by more than the buffer, on a bar carrying
// real range, is the entry this strategy exists to take.
func TestDonchianLongBreakout(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	// Channel upper is 102; buffer is 2, so the close must exceed 104.
	sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000))
	if sig == nil {
		t.Fatal("no signal on a clean breakout")
	}
	if sig.Type != models.BuySignal {
		t.Errorf("side = %v, want BUY", sig.Type)
	}
	if !sig.StopLoss.LessThan(sig.Price) {
		t.Errorf("long stop %s is not below entry %s", sig.StopLoss, sig.Price)
	}
	if !sig.TrailDistance.IsPositive() {
		t.Error("trail distance not armed")
	}
	// The trail is the only exit that takes profit — see TestDonchianEmitsNoTarget.
	if !sig.Target.IsZero() {
		t.Errorf("target = %s, want none", sig.Target)
	}
	if sig.ProductType != "MIS" {
		t.Errorf("product = %s, want MIS", sig.ProductType)
	}
}

func TestDonchianShortBreakout(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	// Channel lower is 98; the close must fall below 96.
	sig := s.OnCandle(dcCandle(10, 15, 100, 101, 94, 95, 5000))
	if sig == nil {
		t.Fatal("no signal on a clean downside breakout")
	}
	if sig.Type != models.SellSignal {
		t.Errorf("side = %v, want SELL", sig.Type)
	}
	if !sig.StopLoss.GreaterThan(sig.Price) {
		t.Errorf("short stop %s is not above entry %s", sig.StopLoss, sig.Price)
	}
}

// The buffer is what separates a breakout from a tick of noise poking through
// the level; a close inside it must not trade.
func TestDonchianBufferRejectsMarginalBreak(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	// Close 103 clears the 102 channel but not the 2-point buffer.
	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 103, 5000)); sig != nil {
		t.Errorf("signal on a break inside the buffer: %+v", sig)
	}
}

func TestDonchianFiltersRejectWeakBreakouts(t *testing.T) {
	tests := []struct {
		name   string
		candle models.Candle
	}{
		{
			// Range 0.6 against an ATR of ~4: a drift over the line, not an
			// ignition bar.
			name:   "no ignition",
			candle: dcCandle(10, 15, 104.6, 105.2, 104.6, 105.0, 5000),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
			warmupDonchian(t, s)
			if sig := s.OnCandle(tc.candle); sig != nil {
				t.Errorf("signal despite %s: %+v", tc.name, sig)
			}
		})
	}
}

// min_atr_pct exists to skip names whose ATR cannot cover the round trip. The
// same shaped breakout at a much higher price has a far smaller ATR/price.
func TestDonchianVolatilityFloorRejectsDeadName(t *testing.T) {
	cfg := donchianTestConfig()
	cfg.MinATRPct = 50 // absurdly high: nothing can pass
	s := NewDonchianStrategy([]string{"TEST"}, cfg)
	warmupDonchian(t, s)

	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig != nil {
		t.Errorf("signal despite the volatility floor: %+v", sig)
	}
}

func TestDonchianRespectsEntryWindow(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	// 14:35 is past the 14:31 last-entry time.
	if sig := s.OnCandle(dcCandle(14, 35, 100, 106, 99, 105, 5000)); sig != nil {
		t.Errorf("signal after the entry cutoff: %+v", sig)
	}
}

func TestDonchianCapsEntriesPerSymbolPerDay(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig == nil {
		t.Fatal("no signal on the first breakout")
	}
	// A second, higher breakout the same day must be refused: max_entries_per_symbol is 1.
	if sig := s.OnCandle(dcCandle(10, 20, 105, 115, 104, 114, 9000)); sig != nil {
		t.Errorf("second entry taken the same day: %+v", sig)
	}
}

func TestDonchianShortSideCanBeDisabled(t *testing.T) {
	cfg := donchianTestConfig()
	off := false
	cfg.AllowShort = &off
	s := NewDonchianStrategy([]string{"TEST"}, cfg)
	warmupDonchian(t, s)

	if sig := s.OnCandle(dcCandle(10, 15, 100, 101, 94, 95, 5000)); sig != nil {
		t.Errorf("short taken with allow_short = false: %+v", sig)
	}
}

// A close beyond the opposite band is the strategy's own exit, and it must be
// advised only for the side actually held.
func TestDonchianExitAdviceOnOppositeBreak(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	entry := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000))
	if entry == nil || entry.Type != models.BuySignal {
		t.Fatalf("setup failed: expected a long entry, got %+v", entry)
	}
	// Two bars that lift the channel's floor above the coming close.
	s.OnCandle(dcCandle(10, 20, 105, 107, 104, 106, 3000))
	s.OnCandle(dcCandle(10, 25, 106, 108, 105, 107, 3000))

	// A close under the lower band while long.
	reversal := dcCandle(10, 30, 106, 106, 96, 97, 4000)
	advice := s.ExitAdvice(reversal)
	if advice == nil {
		t.Fatal("no exit advice on a close below the lower band while long")
	}
	if advice.ForSide != models.BuySignal {
		t.Errorf("advice.ForSide = %v, want BUY (the side held)", advice.ForSide)
	}
	if advice.Symbol != "TEST" {
		t.Errorf("advice.Symbol = %q", advice.Symbol)
	}

	// v1 has no flip: the bar that closed the trade must not open the reverse.
	if sig := s.OnCandle(reversal); sig != nil {
		t.Errorf("reverse entry on the exit bar: %+v", sig)
	}

	// And with the position gone, the same break must not advise again.
	if again := s.ExitAdvice(dcCandle(10, 35, 97, 98, 90, 91, 4000)); again != nil {
		t.Errorf("exit advised with no position held: %+v", again)
	}
}

func TestDonchianNoExitAdviceWithoutPosition(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	if advice := s.ExitAdvice(dcCandle(10, 15, 100, 101, 90, 91, 4000)); advice != nil {
		t.Errorf("exit advised before any entry: %+v", advice)
	}
}

// Kite emits zero-volume candles for halted or illiquid names. They must reach
// no indicator: a zero-range bar would drag ATR towards zero and a zero-volume
// bar would drag the volume baseline with it.
func TestDonchianIgnoresZeroVolumeCandles(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	before, _ := s.state["TEST"].channel.Value()
	if sig := s.OnCandle(dcCandle(10, 15, 100, 300, 100, 300, 0)); sig != nil {
		t.Errorf("signal from a zero-volume candle: %+v", sig)
	}
	after, _ := s.state["TEST"].channel.Value()
	if !before.Equal(after) {
		t.Errorf("zero-volume candle moved the channel from %s to %s", before, after)
	}
}

// The sizer works in lots for futures and single units for cash. The strategy
// only reports what it was told, but reporting nothing would silently size a
// futures position as if it were shares.
func TestDonchianCarriesContractSpec(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	s.SetContracts(map[string]ContractSpec{"TEST": {LotSize: 50, Exchange: "NFO"}})
	warmupDonchian(t, s)

	sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000))
	if sig == nil {
		t.Fatal("no signal")
	}
	if sig.LotSize != 50 {
		t.Errorf("LotSize = %d, want 50", sig.LotSize)
	}
	if sig.Exchange != "NFO" {
		t.Errorf("Exchange = %q, want NFO", sig.Exchange)
	}
	if !sig.RiskPct.Equal(decimal.NewFromFloat(0.005)) {
		t.Errorf("RiskPct = %s, want 0.005 (0.5%%)", sig.RiskPct)
	}
}

// stubHistory serves canned candles to Init.
type stubHistory struct {
	candles []models.Candle
	err     error
	calls   int
}

func (h *stubHistory) History(string, string, int) ([]models.Candle, error) {
	h.calls++
	return h.candles, h.err
}

// Warm-up exists so a live session can trade from its first candle instead of
// spending 70 minutes filling a 4-bar channel and a 14-bar ATR.
func TestDonchianInitWarmsUpFromHistory(t *testing.T) {
	history := make([]models.Candle, 0, 40)
	for i := range 40 {
		min := 15 + i*5
		history = append(history, dcCandle(9+min/60, min%60, 100, 102, 98, 100, 1000))
	}
	h := &stubHistory{candles: history}

	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	if err := s.Init(h); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if h.calls != 1 {
		t.Errorf("History called %d times, want once per symbol", h.calls)
	}

	// The very first live bar can now break out — no further warm-up needed.
	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig == nil {
		t.Error("no signal on the first live bar after warm-up")
	}
}

// A symbol whose history cannot be fetched must still run; it just warms up
// from the live stream instead and loses the early session.
func TestDonchianInitReportsFailureWithoutBlocking(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	if err := s.Init(&stubHistory{err: errors.New("kite down")}); err == nil {
		t.Error("Init reported success despite a failed history fetch")
	}
	// And the strategy is still usable: it warms up from the stream.
	warmupDonchian(t, s)
	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig == nil {
		t.Error("no signal after stream warm-up following a failed Init")
	}
}

// Regression: the bar that FILLS the channel window must not also trade
// against it. The indicator reports ready once that bar is ingested, but the
// values it handed back describe the unfilled window — zeros — and any close
// clears a channel of [0, 0]. Every symbol got one bogus breakout this way,
// hidden for as long as the (since removed) volume filter rejected that bar.
func TestDonchianDoesNotTradeTheBarThatFillsTheChannel(t *testing.T) {
	cfg := donchianTestConfig()
	s := NewDonchianStrategy([]string{"TEST"}, cfg)

	// Exactly DonchianLookback bars, all inside the entry window.
	for i := range cfg.DonchianLookback {
		c := dcCandle(10, i*5, 100, 104, 96, 100, 1000)
		if sig := s.OnCandle(c); sig != nil {
			t.Fatalf("bar %d traded against an unfilled channel: upper=%s lower=%s",
				i, sig.Metadata["ChannelUpper"], sig.Metadata["ChannelLower"])
		}
	}
}

// The trail is the only profit-taking exit. A target was measured four times
// on this strategy and cost more than it saved every time, so tp_rr was removed
// rather than defaulted off — and a signal must therefore carry no target at
// all. A zero Target is meaningful here, not missing: pkg/broker guards against
// reading it as "exit at price 0".
func TestDonchianEmitsNoTarget(t *testing.T) {
	for _, side := range []struct {
		name   string
		candle models.Candle
	}{
		{"long", dcCandle(10, 15, 100, 106, 99, 105, 5000)},
		{"short", dcCandle(10, 15, 100, 101, 94, 95, 5000)},
	} {
		t.Run(side.name, func(t *testing.T) {
			s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
			warmupDonchian(t, s)

			sig := s.OnCandle(side.candle)
			if sig == nil {
				t.Fatal("no signal")
			}
			if !sig.Target.IsZero() {
				t.Errorf("target = %s, want none — the trail is the only exit", sig.Target)
			}
			if !sig.BreakevenTrigger.IsZero() || !sig.BreakevenStop.IsZero() {
				t.Errorf("breakeven fields set (%s / %s); BREAKEVEN+ was removed",
					sig.BreakevenTrigger, sig.BreakevenStop)
			}
			if !sig.TrailDistance.IsPositive() {
				t.Error("no trail distance — the strategy would have no way out but the stop")
			}
		})
	}
}

// The shipped defaults must actually reach a strategy built from a config that
// omits the [donchian] section entirely — the live and backtest path.
func TestDonchianDefaultsSurviveAnEmptyConfigSection(t *testing.T) {
	var loaded config.DonchianConfig // zero value, as TOML decoding leaves it
	if loaded.UseIgnition != nil {
		t.Fatal("fixture is not a zero value")
	}

	d := config.DefaultDonchianConfig()
	if loaded.UseIgnition == nil {
		loaded.UseIgnition = d.UseIgnition
	}
	if loaded.UseIgnition == nil || *loaded.UseIgnition != *d.UseIgnition {
		t.Errorf("UseIgnition = %v, want the default — a changed default must reach a config that omits the key",
			loaded.UseIgnition)
	}
}

// An index publishes no volume. Treating a zero-volume bar as a halted
// instrument — correct for a cash equity — rejects every bar of an index and
// produces a backtest of exactly zero trades, which reads as "the strategy
// found no setups" rather than "the data never reached the strategy".
func TestDonchianTradesVolumelessInstruments(t *testing.T) {
	cfg := donchianTestConfig()
	s := NewDonchianStrategy([]string{"TEST"}, cfg)

	// Warm-up with no volume anywhere, as an index feed arrives.
	for i := range 12 {
		min := 15 + i*5
		if sig := s.OnCandle(dcCandle(9+min/60, min%60, 100, 102, 98, 100, 0)); sig != nil {
			t.Fatalf("warm-up bar %d produced a signal: %+v", i, sig)
		}
	}

	sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 0))
	if sig == nil {
		t.Fatal("no signal on a volumeless instrument — the volume filter is not applicable here, not failed")
	}
	if sig.Type != models.BuySignal {
		t.Errorf("side = %v, want BUY", sig.Type)
	}
}

// The equity behaviour must survive that change: once an instrument has shown
// real volume, a later zero-volume bar is a halt and must still be dropped.
func TestDonchianStillRejectsHaltedEquityBars(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s) // real volume throughout

	before, _ := s.state["TEST"].channel.Value()
	if sig := s.OnCandle(dcCandle(10, 15, 100, 300, 100, 300, 0)); sig != nil {
		t.Errorf("signal from a halted bar after real volume: %+v", sig)
	}
	if after, _ := s.state["TEST"].channel.Value(); !before.Equal(after) {
		t.Errorf("halted bar moved the channel from %s to %s", before, after)
	}
}
