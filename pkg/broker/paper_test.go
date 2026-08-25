package broker

import (
	"testing"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func rs(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

// newTestPaper builds an adapter with no live connection and no store, so tests
// drive prices themselves and no background goroutine runs.
func newTestPaper(capital int64) *PaperAdapter {
	return NewPaperAdapter(nil, rs(capital))
}

func entry(symbol string, side models.SignalType, qty, price int64) models.Order {
	return models.Order{
		Symbol:      symbol,
		Side:        side,
		Type:        "MARKET",
		ProductType: "MIS",
		Exchange:    "NFO",
		Quantity:    rs(qty),
		Price:       rs(price),
		Metadata:    map[string]string{"Strategy": "donchian"},
	}
}

func TestPaperAdapterLifecycle(t *testing.T) {
	const capital = 1000000
	p := newTestPaper(capital)

	bal, err := p.GetBalance()
	if err != nil {
		t.Fatalf("GetBalance failed: %v", err)
	}
	if !bal.Equal(rs(capital)) {
		t.Errorf("got balance %s, want %s", bal, rs(capital))
	}

	filled, err := p.PlaceOrder(entry("NIFTY26AUG24200CE", models.BuySignal, 650, 400))
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}
	if filled.Status != models.OrderFilled {
		t.Errorf("order status = %s, want FILLED", filled.Status)
	}
	if !filled.IsPaper {
		t.Error("filled order is not tagged IsPaper")
	}

	// A bought option is paid for in full, so the whole notional is blocked.
	want := rs(capital - 650*400)
	if bal, _ = p.GetBalance(); !bal.Equal(want) {
		t.Errorf("balance after buy = %s, want %s", bal, want)
	}

	if has, err := p.HasOpenPosition("NIFTY26AUG24200CE"); err != nil || !has {
		t.Errorf("expected open position, got has=%v err=%v", has, err)
	}

	positions, err := p.GetPositions()
	if err != nil || len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d (err %v)", len(positions), err)
	}
	if positions[0].Quantity != 650 {
		t.Errorf("position quantity = %d, want 650", positions[0].Quantity)
	}

	closed, err := p.ClosePosition("NIFTY26AUG24200CE", models.BuySignal, rs(450), time.Now(), "trailing stop")
	if err != nil || !closed {
		t.Fatalf("ClosePosition failed: closed=%v err=%v", closed, err)
	}

	// Margin released plus 50 points of profit on 650 units.
	wantFinal := rs(capital + 50*650)
	if bal, _ = p.GetBalance(); !bal.Equal(wantFinal) {
		t.Errorf("balance after profit exit = %s, want %s", bal, wantFinal)
	}

	if has, _ := p.HasOpenPosition("NIFTY26AUG24200CE"); has {
		t.Error("expected position to be closed")
	}

	trades, _ := p.GetTrades()
	if len(trades) != 2 {
		t.Errorf("expected 2 recorded fills (entry + exit), got %d", len(trades))
	}
}

// A closed position must stay in the book with a zero net quantity, the way
// Kite reports it — computeSummary reads realised PnL from exactly those rows,
// and dropping them reported realised PnL as zero for the whole session.
func TestPaperClosedPositionReportsRealizedPnL(t *testing.T) {
	p := newTestPaper(1000000)
	if _, err := p.PlaceOrder(entry("SBIN", models.BuySignal, 100, 500)); err != nil {
		t.Fatalf("entry failed: %v", err)
	}
	if _, err := p.ClosePosition("SBIN", models.BuySignal, rs(520), time.Now(), "target"); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	positions, _ := p.GetPositions()
	if len(positions) != 1 {
		t.Fatalf("expected the closed position to remain listed, got %d", len(positions))
	}
	if positions[0].NetQuantity != 0 {
		t.Errorf("NetQuantity = %d, want 0", positions[0].NetQuantity)
	}
	if !positions[0].PnL.Equal(rs(2000)) {
		t.Errorf("realised PnL = %s, want 2000", positions[0].PnL)
	}
}

// The average price must not be corrupted by a fill that reduces or reverses
// the position. The previous implementation added an unsigned cost regardless
// of side, turning a 400 basis into 1250 on a partial exit.
func TestPaperAveragePriceOnReduceAndFlip(t *testing.T) {
	t.Run("reduce leaves the basis alone", func(t *testing.T) {
		p := newTestPaper(10000000)
		mustPlace(t, p, entry("SBIN", models.BuySignal, 100, 400))
		mustPlace(t, p, entry("SBIN", models.SellSignal, 50, 450))

		pos := p.positions["SBIN"]
		if pos.Quantity != 50 {
			t.Errorf("quantity = %d, want 50", pos.Quantity)
		}
		if !pos.AveragePrice.Equal(rs(400)) {
			t.Errorf("average price = %s, want 400 (unchanged by a partial exit)", pos.AveragePrice)
		}
		if !pos.RealizedPnL.Equal(rs(2500)) {
			t.Errorf("realised = %s, want 2500", pos.RealizedPnL)
		}
	})

	t.Run("reversal rebases at the fill price", func(t *testing.T) {
		p := newTestPaper(10000000)
		mustPlace(t, p, entry("SBIN", models.BuySignal, 100, 400))
		mustPlace(t, p, entry("SBIN", models.SellSignal, 150, 450))

		pos := p.positions["SBIN"]
		if pos.Quantity != -50 {
			t.Errorf("quantity = %d, want -50", pos.Quantity)
		}
		if !pos.AveragePrice.Equal(rs(450)) {
			t.Errorf("average price = %s, want 450", pos.AveragePrice)
		}
	})

	t.Run("adding to a position averages", func(t *testing.T) {
		p := newTestPaper(10000000)
		mustPlace(t, p, entry("SBIN", models.BuySignal, 100, 400))
		mustPlace(t, p, entry("SBIN", models.BuySignal, 100, 500))

		pos := p.positions["SBIN"]
		if pos.Quantity != 200 || !pos.AveragePrice.Equal(rs(450)) {
			t.Errorf("got qty %d avg %s, want 200 @ 450", pos.Quantity, pos.AveragePrice)
		}
	})
}

// A short must block margin like a long. Crediting the notional let the balance
// grow with every short, which then inflated the size of the next trade.
func TestPaperShortBlocksMarginRatherThanAddingCash(t *testing.T) {
	const capital = 1000000
	p := newTestPaper(capital)
	mustPlace(t, p, entry("SBIN", models.SellSignal, 100, 500))

	bal, _ := p.GetBalance()
	if !bal.Equal(rs(capital - 100*500)) {
		t.Fatalf("balance after short = %s, want %s (margin blocked, not cash credited)",
			bal, rs(capital-100*500))
	}

	// Covering 20 lower is a 2,000 profit on top of the released margin.
	if _, err := p.ClosePosition("SBIN", models.SellSignal, rs(480), time.Now(), "target"); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	bal, _ = p.GetBalance()
	if !bal.Equal(rs(capital + 2000)) {
		t.Errorf("balance after covering = %s, want %s", bal, rs(capital+2000))
	}
}

// The engine sizes MIS positions with leverage, so blocking the full notional
// would reject positions the real account takes.
func TestPaperLeverageReducesMarginRequirement(t *testing.T) {
	unlevered := NewPaperAdapter(nil, rs(100000))
	if _, err := unlevered.PlaceOrder(entry("SBIN", models.BuySignal, 100, 2000)); err == nil {
		t.Fatal("expected a 200,000 notional to be refused against 100,000 of cash")
	}

	levered := NewPaperAdapter(nil, rs(100000),
		WithPaperLeverage(func(string) float64 { return 5 }))
	if _, err := levered.PlaceOrder(entry("SBIN", models.BuySignal, 100, 2000)); err != nil {
		t.Fatalf("5x leverage should make a 40,000 margin affordable: %v", err)
	}
	bal, _ := levered.GetBalance()
	if !bal.Equal(rs(60000)) {
		t.Errorf("balance = %s, want 60000 (200,000 notional / 5)", bal)
	}
}

func TestPaperAdapterRejectsInsufficientFunds(t *testing.T) {
	p := newTestPaper(50000)
	order := entry("NIFTY26AUG24200CE", models.BuySignal, 650, 400)

	rejected, err := p.PlaceOrder(order)
	if err == nil {
		t.Fatal("expected an error due to insufficient virtual margin")
	}
	// The rejected order is still a paper order; persisting it as live would
	// contaminate the real order log.
	if !rejected.IsPaper {
		t.Error("rejected order is not tagged IsPaper")
	}
	// A refused fill must leave the book untouched.
	if bal, _ := p.GetBalance(); !bal.Equal(rs(50000)) {
		t.Errorf("balance moved on a rejected order: %s", bal)
	}
	if len(p.positions) != 0 {
		t.Errorf("rejected order created %d position(s)", len(p.positions))
	}
}

// ClosePosition takes the side of the position it is meant to close, and must
// enforce it: a long-exit advice may not close a short in the same symbol.
func TestPaperClosePositionHonoursSide(t *testing.T) {
	p := newTestPaper(1000000)
	mustPlace(t, p, entry("SBIN", models.SellSignal, 100, 500))

	closed, err := p.ClosePosition("SBIN", models.BuySignal, rs(510), time.Now(), "long-exit advice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closed {
		t.Fatal("a long-exit advice closed a short position")
	}
	if has, _ := p.HasOpenPosition("SBIN"); !has {
		t.Error("the short was closed anyway")
	}

	closed, err = p.ClosePosition("SBIN", models.SellSignal, rs(510), time.Now(), "short exit")
	if err != nil || !closed {
		t.Fatalf("a matching short-exit advice must close: closed=%v err=%v", closed, err)
	}
}

// Every strategy here except Donchian's option mode exits solely through the
// stop and target the broker holds. Paper has to fill them itself.
func TestPaperFillsRestingStopAndTarget(t *testing.T) {
	t.Run("long stop", func(t *testing.T) {
		p := newTestPaper(1000000)
		o := entry("SBIN", models.BuySignal, 100, 500)
		o.StopLoss = rs(490)
		filled := mustPlace(t, p, o)
		if filled.GTTTriggerID == 0 {
			t.Fatal("no trigger id returned, so the engine's trail monitor never arms")
		}

		p.OnTick("SBIN", rs(495), time.Now())
		if has, _ := p.HasOpenPosition("SBIN"); !has {
			t.Fatal("position closed above the stop")
		}

		p.OnTick("SBIN", rs(489), time.Now())
		if has, _ := p.HasOpenPosition("SBIN"); has {
			t.Fatal("stop did not fire")
		}
		assertExitReason(t, p, "SL-HIT")
	})

	t.Run("long target", func(t *testing.T) {
		p := newTestPaper(1000000)
		o := entry("SBIN", models.BuySignal, 100, 500)
		o.StopLoss = rs(490)
		o.Target = rs(520)
		mustPlace(t, p, o)

		p.OnTick("SBIN", rs(521), time.Now())
		if has, _ := p.HasOpenPosition("SBIN"); has {
			t.Fatal("target did not fire")
		}
		assertExitReason(t, p, "TARGET-HIT")
	})

	t.Run("short stop", func(t *testing.T) {
		p := newTestPaper(1000000)
		o := entry("SBIN", models.SellSignal, 100, 500)
		o.StopLoss = rs(510)
		mustPlace(t, p, o)

		p.OnTick("SBIN", rs(505), time.Now())
		if has, _ := p.HasOpenPosition("SBIN"); !has {
			t.Fatal("short closed below its stop")
		}
		p.OnTick("SBIN", rs(511), time.Now())
		if has, _ := p.HasOpenPosition("SBIN"); has {
			t.Fatal("short stop did not fire")
		}
	})
}

// A strategy running a pure trailing exit leaves Target zero deliberately.
// Reading that as a level at price 0 exits every position on its first tick —
// a bug this codebase has already had once, in the simulator.
func TestPaperZeroTargetIsNotALevel(t *testing.T) {
	p := newTestPaper(1000000)
	o := entry("SBIN", models.SellSignal, 100, 500)
	o.StopLoss = rs(510)
	o.Target = decimal.Zero
	mustPlace(t, p, o)

	p.OnTick("SBIN", rs(495), time.Now())
	if has, _ := p.HasOpenPosition("SBIN"); !has {
		t.Fatal("a zero target was read as a price level and closed the position")
	}
}

// The engine's trail reaches this broker through ModifyPositionStop, and the
// moved stop must actually fill.
func TestPaperModifyPositionStopMovesTheRestingStop(t *testing.T) {
	p := newTestPaper(1000000)
	o := entry("SBIN", models.BuySignal, 100, 500)
	o.StopLoss = rs(490)
	mustPlace(t, p, o)

	p.OnTick("SBIN", rs(530), time.Now())
	if err := p.ModifyPositionStop(o, rs(520)); err != nil {
		t.Fatalf("ModifyPositionStop failed: %v", err)
	}

	p.OnTick("SBIN", rs(519), time.Now())
	if has, _ := p.HasOpenPosition("SBIN"); has {
		t.Fatal("the moved stop did not fire")
	}
}

// A stop may never rest beyond the market: filling one at a price that never
// printed is how this codebase once manufactured a profitable backtest.
func TestPaperStopIsCappedAtTheMarket(t *testing.T) {
	p := newTestPaper(1000000)
	o := entry("SBIN", models.BuySignal, 100, 500)
	o.StopLoss = rs(490)
	mustPlace(t, p, o)

	p.OnTick("SBIN", rs(505), time.Now())
	// 520 is above the last trade — as a long's stop it is really a limit order.
	if err := p.ModifyPositionStop(o, rs(520)); err != nil {
		t.Fatalf("ModifyPositionStop failed: %v", err)
	}
	if got := p.resting["SBIN"].StopLoss; !got.Equal(rs(505)) {
		t.Errorf("stop rested at %s, want it capped to the market at 505", got)
	}
}

// SquareOff cancels every resting trigger before closing positions, so the GTT
// surface has to reflect what paper is actually holding.
func TestPaperGTTSurfaceReflectsRestingOrders(t *testing.T) {
	p := newTestPaper(1000000)
	o := entry("SBIN", models.BuySignal, 100, 500)
	o.StopLoss = rs(490)
	o.Target = rs(520)
	filled := mustPlace(t, p, o)

	gtts, err := p.GetGTTs()
	if err != nil || len(gtts) != 1 {
		t.Fatalf("expected 1 GTT, got %d (err %v)", len(gtts), err)
	}
	if gtts[0].ID != filled.GTTTriggerID || gtts[0].Type != "two-leg" {
		t.Errorf("unexpected GTT: %+v", gtts[0])
	}

	if err := p.CancelGTT(filled.GTTTriggerID); err != nil {
		t.Fatalf("CancelGTT failed: %v", err)
	}
	if gtts, _ = p.GetGTTs(); len(gtts) != 0 {
		t.Errorf("GTT survived cancellation: %+v", gtts)
	}

	// With the trigger gone the stop must no longer fire.
	p.OnTick("SBIN", rs(400), time.Now())
	if has, _ := p.HasOpenPosition("SBIN"); !has {
		t.Error("a cancelled stop still closed the position")
	}
}

// The square-off counter order carries no price, and GetQuote cannot resolve
// every instrument the strategy trades. The last observed mark has to serve.
func TestPaperPricelessOrderFillsAtTheLastMark(t *testing.T) {
	p := newTestPaper(1000000)
	mustPlace(t, p, entry("SENSEX26AUG80000CE", models.BuySignal, 20, 900))
	p.OnTick("SENSEX26AUG80000CE", rs(950), time.Now())

	counter := entry("SENSEX26AUG80000CE", models.SellSignal, 20, 0)
	counter.Price = decimal.Zero
	filled, err := p.PlaceOrder(counter)
	if err != nil {
		t.Fatalf("square-off counter order failed with no live quote provider: %v", err)
	}
	if !filled.Price.Equal(rs(950)) {
		t.Errorf("filled at %s, want the last observed mark 950", filled.Price)
	}
}

func mustPlace(t *testing.T, p *PaperAdapter, o models.Order) models.Order {
	t.Helper()
	filled, err := p.PlaceOrder(o)
	if err != nil {
		t.Fatalf("PlaceOrder(%s %s) failed: %v", o.Side, o.Symbol, err)
	}
	return filled
}

func assertExitReason(t *testing.T, p *PaperAdapter, want string) {
	t.Helper()
	trades, _ := p.GetTrades()
	last := trades[len(trades)-1]
	if got := last.Metadata["Reason"]; got != want {
		t.Errorf("exit reason = %q, want %q", got, want)
	}
}
