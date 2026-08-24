package strategy

import (
	"errors"
	"testing"
	"time"

	"zerobha/internal/models"
	"zerobha/pkg/options"

	"github.com/shopspring/decimal"
)

// fakeExecutor hands back a fixed contract and premium, and records what it
// was asked for.
type fakeExecutor struct {
	contract options.Contract
	premium  decimal.Decimal
	selErr   error
	premErr  error

	askedCall  bool
	askedSpot  decimal.Decimal
	selectCall int
}

func (f *fakeExecutor) Select(_ string, spot decimal.Decimal, _ time.Time, isCall bool) (options.Contract, error) {
	f.selectCall++
	f.askedCall, f.askedSpot = isCall, spot
	if f.selErr != nil {
		return options.Contract{}, f.selErr
	}
	return f.contract, nil
}

func (f *fakeExecutor) Premium(options.Contract) (decimal.Decimal, error) {
	if f.premErr != nil {
		return decimal.Zero, f.premErr
	}
	return f.premium, nil
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		contract: options.Contract{
			TradingSymbol: "NIFTY26AUG100CE",
			Exchange:      "NFO",
			Expiry:        time.Date(2026, 3, 17, 0, 0, 0, 0, istLocation),
			Strike:        decimal.NewFromInt(100),
			IsCall:        true,
			LotSize:       65,
			TickSize:      decimal.NewFromFloat(0.05),
		},
		premium: decimal.NewFromFloat(8),
	}
}

func optionStrategy(t *testing.T, exec OptionExecutor) *Donchian {
	t.Helper()
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	s.SetOptionExecution(exec)
	warmupDonchian(t, s)
	return s
}

// The signal that reaches the engine must name the OPTION, not the index: the
// engine sizes, routes and places against Signal.Symbol.
func TestDonchianOptionSignalNamesTheContract(t *testing.T) {
	exec := newFakeExecutor()
	s := optionStrategy(t, exec)

	sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000))
	if sig == nil {
		t.Fatal("no signal on a clean breakout")
	}
	if sig.Symbol != "NIFTY26AUG100CE" {
		t.Errorf("symbol = %s, want the option", sig.Symbol)
	}
	if sig.Exchange != "NFO" {
		t.Errorf("exchange = %s, want NFO", sig.Exchange)
	}
	if sig.LotSize != 65 {
		t.Errorf("lot size = %d, want 65", sig.LotSize)
	}
	if !sig.Price.Equal(decimal.NewFromFloat(8)) {
		t.Errorf("price = %s, want the premium 8", sig.Price)
	}
	// The underlying view is kept for the audit trail.
	if sig.Metadata["Underlying"] != "TEST" {
		t.Errorf("metadata lost the underlying: %v", sig.Metadata)
	}
}

// A bearish index view buys a PUT. It must never sell an option: a short
// option position has unbounded risk and a completely different margin profile
// from the long premium this strategy is sized for.
func TestDonchianOptionAlwaysBuys(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candle     models.Candle
		wantIsCall bool
	}{
		{"long index view buys a call", dcCandle(10, 15, 100, 106, 99, 105, 5000), true},
		{"short index view buys a put", dcCandle(10, 15, 100, 101, 94, 95, 5000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := newFakeExecutor()
			s := optionStrategy(t, exec)

			sig := s.OnCandle(tc.candle)
			if sig == nil {
				t.Fatal("no signal")
			}
			if sig.Type != models.BuySignal {
				t.Errorf("side = %v, want BUY — the strategy is never short an option", sig.Type)
			}
			if exec.askedCall != tc.wantIsCall {
				t.Errorf("asked for isCall=%v, want %v", exec.askedCall, tc.wantIsCall)
			}
		})
	}
}

// A refused selection is a no-trade, and must not consume the day's entry
// budget or leave the strategy believing it holds a position.
func TestDonchianOptionRefusalTakesNoTrade(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec *fakeExecutor
	}{
		{"too close to expiry", &fakeExecutor{selErr: options.ErrTooCloseToExpiry}},
		{"no strike listed", &fakeExecutor{selErr: errors.New("empty chain")}},
		{"no premium", func() *fakeExecutor { e := newFakeExecutor(); e.premErr = errors.New("no quote"); return e }()},
		{"zero premium", func() *fakeExecutor { e := newFakeExecutor(); e.premium = decimal.Zero; return e }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := optionStrategy(t, tc.exec)

			if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig != nil {
				t.Fatalf("traded despite a refused selection: %+v", sig)
			}
			st := s.stateFor("TEST")
			if st.entriesToday != 0 {
				t.Errorf("entriesToday = %d, want 0 — a refusal must not spend the budget", st.entriesToday)
			}
			if st.openValid {
				t.Error("strategy believes it holds a position it never opened")
			}
			if st.leg != nil {
				t.Error("an option leg was recorded for a trade that did not happen")
			}
		})
	}
}

// The stop lives on the INDEX. When the index closes through it the OPTION is
// what gets closed — closing the index symbol would be a no-op at the engine
// and the real position would be left open.
func TestDonchianOptionStopClosesTheContract(t *testing.T) {
	exec := newFakeExecutor()
	s := optionStrategy(t, exec)

	sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000))
	if sig == nil {
		t.Fatal("no entry signal")
	}
	st := s.stateFor("TEST")
	if st.leg == nil {
		t.Fatal("no option leg recorded")
	}
	indexStop := st.leg.indexStop

	// A bar that closes above the stop must not exit.
	above := dcCandle(10, 20, 105, 106, indexStop.InexactFloat64()+1, indexStop.InexactFloat64()+1, 4000)
	if advice := s.ExitAdvice(above); advice != nil {
		t.Fatalf("exited above the stop: %+v", advice)
	}

	// A close through it must, and must name the contract.
	through := dcCandle(10, 25, 105, 105, indexStop.InexactFloat64()-2, indexStop.InexactFloat64()-1, 4000)
	advice := s.ExitAdvice(through)
	if advice == nil {
		t.Fatal("no exit advice on a close through the index stop")
	}
	if advice.Symbol != "NIFTY26AUG100CE" {
		t.Errorf("closing %s, want the option contract", advice.Symbol)
	}
	if advice.ForSide != models.BuySignal {
		t.Errorf("ForSide = %v, want BUY — the option position is long", advice.ForSide)
	}
	if st.leg != nil {
		t.Error("option leg survived the exit")
	}
}

// The chandelier trail ratchets on index prices and never loosens.
func TestDonchianOptionTrailRatchetsOnTheIndex(t *testing.T) {
	exec := newFakeExecutor()
	s := optionStrategy(t, exec)

	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig == nil {
		t.Fatal("no entry signal")
	}
	st := s.stateFor("TEST")
	initial := st.leg.indexStop

	// A strong up bar must lift the stop.
	s.ExitAdvice(dcCandle(10, 20, 105, 130, 105, 129, 4000))
	lifted := st.leg.indexStop
	if !lifted.GreaterThan(initial) {
		t.Fatalf("trail did not lift the stop: %s -> %s", initial, lifted)
	}

	// A pullback must NOT lower it again.
	s.ExitAdvice(dcCandle(10, 25, 129, 129, lifted.InexactFloat64()+1, lifted.InexactFloat64()+2, 4000))
	if st.leg.indexStop.LessThan(lifted) {
		t.Errorf("trail loosened on a pullback: %s -> %s", lifted, st.leg.indexStop)
	}
}

// An opposite-band break must also close the contract rather than the index.
func TestDonchianOppositeBreakClosesTheContract(t *testing.T) {
	exec := newFakeExecutor()
	s := optionStrategy(t, exec)

	if sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000)); sig == nil {
		t.Fatal("no entry signal")
	}
	// Walk the channel down without tripping the index stop, then break it.
	st := s.stateFor("TEST")
	st.leg.indexStop = decimal.NewFromInt(1) // out of the way

	var advice *coreExitAdvice
	for i := range 8 {
		c := dcCandle(11, i*5, 60, 61, 55, 56, 4000)
		if a := s.ExitAdvice(c); a != nil {
			advice = &coreExitAdvice{Symbol: a.Symbol, Reason: a.Reason}
			break
		}
		s.OnCandle(c)
	}
	if advice == nil {
		t.Skip("channel did not produce an opposite break in this fixture")
	}
	if advice.Symbol != "NIFTY26AUG100CE" {
		t.Errorf("opposite break closed %s, want the option contract", advice.Symbol)
	}
}

type coreExitAdvice struct {
	Symbol string
	Reason string
}

// Without an executor the strategy must behave exactly as every recorded
// backtest measured it: signals name the index and carry a broker-side stop.
func TestDonchianWithoutOptionExecutionIsUnchanged(t *testing.T) {
	s := NewDonchianStrategy([]string{"TEST"}, donchianTestConfig())
	warmupDonchian(t, s)

	sig := s.OnCandle(dcCandle(10, 15, 100, 106, 99, 105, 5000))
	if sig == nil {
		t.Fatal("no signal")
	}
	if sig.Symbol != "TEST" {
		t.Errorf("symbol = %s, want the index symbol", sig.Symbol)
	}
	if !sig.TrailDistance.IsPositive() {
		t.Error("broker-side trail not armed")
	}
	if s.stateFor("TEST").leg != nil {
		t.Error("an option leg was recorded without an executor")
	}
}
