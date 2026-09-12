package strategy

import (
	"errors"
	"testing"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/options"

	"github.com/shopspring/decimal"
)

func emaOptionTestConfig() config.EMACrossConfig {
	cfg := config.DefaultEMACrossConfig()
	cfg.FastPeriod = 3
	cfg.SlowPeriod = 5
	cfg.ATRPeriod = 3
	cfg.SLATRMult = 2.0
	cfg.TPATRMult = 0
	cfg.ProductType = "MIS"
	cfg.AssetType = "options"
	cfg.EntryStartMin = 0
	cfg.EntryCutoffMin = 0
	noTrail := 0.0
	cfg.TrailATRMult = &noTrail
	// Pinned: several tests here assert on the cross exit, which ships off.
	on := true
	cfg.ExitOnOppositeCross = &on
	return cfg
}

// driveToGoldenCross feeds a decline then a thrust and returns the entry signal
// the golden cross produced, plus the time of the bar it fired on.
func driveToGoldenCross(t *testing.T, s *EMACross) (*models.Signal, time.Time) {
	t.Helper()
	start := time.Date(2026, 8, 3, 9, 15, 0, 0, istLocation)
	prices := []float64{100, 99, 98, 97, 96, 95, 105, 115, 125}
	for i, p := range prices {
		at := start.Add(time.Duration(i) * time.Minute)
		c := makeCandle("NIFTY 50", at, p-1, p+2, p-3, p)
		if adv := s.ExitAdvice(c); adv != nil {
			t.Fatalf("unexpected exit advice on bar %d: %+v", i, adv)
		}
		if sig := s.OnCandle(c); sig != nil {
			return sig, at
		}
	}
	t.Fatal("no golden-cross signal produced")
	return nil, time.Time{}
}

// With option execution on, a long index view becomes a BUY of the call the
// executor chose, sized in lots, and the strategy remembers the INDEX stop.
func TestEMACrossOptionModeBuysTheContract(t *testing.T) {
	exec := newFakeExecutor()
	s := NewEMACrossStrategy([]string{"NIFTY 50"}, emaOptionTestConfig())
	s.SetOptionExecution(exec)

	sig, _ := driveToGoldenCross(t, s)

	if !exec.askedCall {
		t.Error("long index signal should ask for a call")
	}
	if sig.Symbol != exec.contract.TradingSymbol {
		t.Errorf("signal symbol = %q, want the contract %q", sig.Symbol, exec.contract.TradingSymbol)
	}
	if sig.Type != models.BuySignal {
		t.Errorf("option signal must always be a BUY, got %v", sig.Type)
	}
	if sig.LotSize != exec.contract.LotSize || sig.Exchange != exec.contract.Exchange {
		t.Errorf("lot/exchange not carried: got %d/%s", sig.LotSize, sig.Exchange)
	}
	if !sig.Price.Equal(exec.premium) {
		t.Errorf("entry price = %s, want the premium %s", sig.Price, exec.premium)
	}
	st := s.stateFor("NIFTY 50")
	if st.leg == nil || st.leg.side != models.BuySignal {
		t.Fatal("strategy should remember an open long leg")
	}
	if !st.leg.indexStop.LessThan(decimal.NewFromInt(125)) {
		t.Errorf("index stop %s should sit below the entry close", st.leg.indexStop)
	}
}

// The index closing through the stop closes the CONTRACT, not the index, and
// frees the strategy to enter again.
func TestEMACrossOptionModeClosesContractOnIndexStop(t *testing.T) {
	exec := newFakeExecutor()
	s := NewEMACrossStrategy([]string{"NIFTY 50"}, emaOptionTestConfig())
	s.SetOptionExecution(exec)

	_, at := driveToGoldenCross(t, s)
	st := s.stateFor("NIFTY 50")
	stop := st.leg.indexStop

	// A bar closing below the index stop, but with the fast EMA still above
	// the slow one (a single bar cannot flip a 3/5 EMA after a 125 thrust).
	crash := stop.Sub(decimal.NewFromInt(1)).InexactFloat64()
	c := makeCandle("NIFTY 50", at.Add(time.Minute), crash+2, crash+3, crash-1, crash)
	adv := s.ExitAdvice(c)
	if adv == nil {
		t.Fatal("expected exit advice when the index closed through the stop")
	}
	if adv.Symbol != exec.contract.TradingSymbol || adv.ForSide != models.BuySignal {
		t.Errorf("advice should close the long contract, got %+v", adv)
	}
	if st.leg != nil || st.openValid {
		t.Error("leg and openValid should be cleared after the stop")
	}
	// Whatever the same bar does next, it must not re-enter on the side that
	// was just stopped: a stop is not a thesis change.
	if sig := s.OnCandle(c); sig != nil && exec.askedCall {
		t.Errorf("stopped long re-entered as a call on the stop bar: %+v", sig)
	}
}

// The opposite cross closes the contract too. Closing the index symbol would be
// a no-op at the engine and leave the real position open.
func TestEMACrossOptionModeOppositeCrossClosesContract(t *testing.T) {
	exec := newFakeExecutor()
	s := NewEMACrossStrategy([]string{"NIFTY 50"}, emaOptionTestConfig())
	s.SetOptionExecution(exec)

	_, at := driveToGoldenCross(t, s)
	st := s.stateFor("NIFTY 50")
	// Park the index stop out of reach so only the cross can fire.
	st.leg.indexStop = decimal.NewFromInt(-1000)

	var adv *core.ExitAdvice
	for i, p := range []float64{110, 95, 80, 65} {
		c := makeCandle("NIFTY 50", at.Add(time.Duration(i+1)*time.Minute), p+2, p+3, p-2, p)
		if a := s.ExitAdvice(c); a != nil {
			adv = a
			break
		}
		_ = s.OnCandle(c)
	}
	if adv == nil {
		t.Fatal("expected the bearish cross to advise an exit")
	}
	if adv.Symbol != exec.contract.TradingSymbol || adv.ForSide != models.BuySignal {
		t.Errorf("advice should close the long contract, got %+v", adv)
	}
	if adv.Reason != emaExitBearishCross {
		t.Errorf("reason = %q, want the constant %q", adv.Reason, emaExitBearishCross)
	}
}

// A declined selection is a no-trade: the strategy must not believe it holds
// a position it never opened.
func TestEMACrossOptionModeDeclinedEntryLeavesStateFlat(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec *fakeExecutor
	}{
		{"too close to expiry", &fakeExecutor{selErr: options.ErrTooCloseToExpiry}},
		{"no strike listed", &fakeExecutor{selErr: errors.New("empty chain")}},
		{"no premium", &fakeExecutor{contract: newFakeExecutor().contract, premErr: errors.New("no quote")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewEMACrossStrategy([]string{"NIFTY 50"}, emaOptionTestConfig())
			s.SetOptionExecution(tc.exec)
			start := time.Date(2026, 8, 3, 9, 15, 0, 0, istLocation)
			for i, p := range []float64{100, 99, 98, 97, 96, 95, 105, 115, 125} {
				c := makeCandle("NIFTY 50", start.Add(time.Duration(i)*time.Minute), p-1, p+2, p-3, p)
				_ = s.ExitAdvice(c)
				if sig := s.OnCandle(c); sig != nil {
					t.Fatalf("declined selection must produce no signal, got %+v", sig)
				}
			}
			st := s.stateFor("NIFTY 50")
			if st.openValid || st.leg != nil {
				t.Error("state should stay flat after a declined entry")
			}
		})
	}
}

// An MIS position does not survive the session, and neither does its leg: a
// stale leg would "close" a contract that the square-off already flattened.
func TestEMACrossMISResetsLegOnNewSession(t *testing.T) {
	exec := newFakeExecutor()
	s := NewEMACrossStrategy([]string{"NIFTY 50"}, emaOptionTestConfig())
	s.SetOptionExecution(exec)

	_, at := driveToGoldenCross(t, s)
	st := s.stateFor("NIFTY 50")
	if st.leg == nil {
		t.Fatal("precondition: leg open")
	}
	next := time.Date(at.Year(), at.Month(), at.Day()+1, 9, 15, 0, 0, istLocation)
	c := makeCandle("NIFTY 50", next, 50, 51, 49, 50) // would breach any stop
	if adv := s.ExitAdvice(c); adv != nil {
		t.Errorf("no exit advice should be issued for a leg from a previous session, got %+v", adv)
	}
	if st.leg != nil || st.openValid {
		t.Error("leg and openValid should reset on the new session")
	}
}

// With the cross exit off, the position runs on its trail and the opposite
// cross must not open a second leg: a put on top of the open call would be a
// different symbol, so the engine would accept it, and the call's index stop
// would be lost with the overwritten leg.
func TestEMACrossOptionModeNoSecondLegWhileOneIsOpen(t *testing.T) {
	exec := newFakeExecutor()
	cfg := emaOptionTestConfig()
	off := false
	cfg.ExitOnOppositeCross = &off
	s := NewEMACrossStrategy([]string{"NIFTY 50"}, cfg)
	s.SetOptionExecution(exec)

	_, at := driveToGoldenCross(t, s)
	st := s.stateFor("NIFTY 50")
	leg := st.leg
	st.leg.indexStop = decimal.NewFromInt(-1000) // out of reach: only a cross could act
	calls := exec.selectCall

	for i, p := range []float64{110, 95, 80, 65, 50} {
		c := makeCandle("NIFTY 50", at.Add(time.Duration(i+1)*time.Minute), p+2, p+3, p-2, p)
		if adv := s.ExitAdvice(c); adv != nil {
			t.Fatalf("cross exit is off, got advice %+v", adv)
		}
		if sig := s.OnCandle(c); sig != nil {
			t.Fatalf("second leg opened on the death cross: %+v", sig)
		}
	}
	if exec.selectCall != calls {
		t.Error("executor was asked for a contract while a leg was open")
	}
	if st.leg != leg {
		t.Error("open leg was replaced")
	}
}
