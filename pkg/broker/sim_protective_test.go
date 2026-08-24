package broker

import (
	"testing"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func simCandle(o, h, l, c float64) models.Candle {
	start := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	return models.Candle{
		Symbol:    "TEST",
		Open:      decimal.NewFromFloat(o),
		High:      decimal.NewFromFloat(h),
		Low:       decimal.NewFromFloat(l),
		Close:     decimal.NewFromFloat(c),
		Volume:    decimal.NewFromInt(1000),
		StartTime: start,
		EndTime:   start.Add(5 * time.Minute),
	}
}

func filledOrder(t *testing.T, s *SimBroker, order models.Order) *models.Order {
	t.Helper()
	if _, err := s.PlaceOrder(order); err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	return &s.Orders[len(s.Orders)-1]
}

// Regression: a strategy exiting purely on a trail leaves Target zero. Treating
// that as a target at price 0 exits every long on its first bar at a price of
// zero, which reads as a catastrophic loss rather than as a bug.
func TestSimZeroTargetIsNotATarget(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	o := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.BuySignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), StopLoss: decimal.NewFromFloat(95),
	})

	s.CheckExits(simCandle(100, 102, 99, 101))

	if o.Status == models.OrderClosed {
		t.Fatalf("long with no target exited on the first bar: %+v", s.Trades)
	}
	if len(s.Trades) != 0 {
		t.Errorf("unexpected trades: %+v", s.Trades)
	}
}

// The mirror case: a short with no stop set must not "stop out" at zero.
func TestSimZeroStopIsNotAStop(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	o := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.SellSignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), Target: decimal.NewFromFloat(90),
	})

	s.CheckExits(simCandle(100, 102, 99, 101))

	if o.Status == models.OrderClosed {
		t.Fatalf("short with no stop exited on the first bar: %+v", s.Trades)
	}
}

func TestSimChandelierTrailRatchets(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	o := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.BuySignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), StopLoss: decimal.NewFromFloat(95),
		TrailDistance: decimal.NewFromFloat(3),
	})

	// Bar makes a high of 110 → stop should follow to 107.
	s.CheckExits(simCandle(100, 110, 99, 109))
	if !o.StopLoss.Equal(decimal.NewFromFloat(107)) {
		t.Fatalf("stop = %s after a 110 high, want 107", o.StopLoss)
	}

	// A quieter bar must not loosen the stop back down.
	s.CheckExits(simCandle(109, 109.5, 108, 108.5))
	if !o.StopLoss.Equal(decimal.NewFromFloat(107)) {
		t.Errorf("stop = %s after a lower-high bar, want it held at 107", o.StopLoss)
	}

	// And it exits when price finally trades back through the trailed stop.
	s.CheckExits(simCandle(108, 108, 106, 106.5))
	if o.Status != models.OrderClosed {
		t.Fatal("position survived a trade through the trailed stop")
	}
	if len(s.Trades) != 1 || !s.Trades[0].ExitPrice.Equal(decimal.NewFromFloat(107)) {
		t.Errorf("exit = %+v, want a fill at the trailed stop 107", s.Trades)
	}
}

// A bar cannot both earn a stop improvement with its high and be exited at the
// improved stop by its own low — the order of the two inside the bar is unknown,
// and assuming the favourable one is lookahead. And where the bar gave the move
// back, the stop is capped at the close: parking it at the chandelier level
// would put it above the market and "fill" it next bar at a price that never
// printed.
func TestSimTrailDoesNotApplyWithinItsOwnBar(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	o := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.BuySignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), StopLoss: decimal.NewFromFloat(95),
		TrailDistance: decimal.NewFromFloat(3),
	})

	// High 110 (the trail would say 107) and low 99 in the same bar, closing
	// back at 100. The bar must not exit: at the stop it actually held, 95,
	// nothing was hit.
	s.CheckExits(simCandle(100, 110, 99, 100))
	if o.Status == models.OrderClosed {
		t.Fatalf("bar exited at a stop its own high created: %+v", s.Trades)
	}
	if !o.StopLoss.Equal(decimal.NewFromFloat(100)) {
		t.Errorf("stop = %s, want it capped at the close 100 rather than the unreachable 107", o.StopLoss)
	}
}

// The BREAKEVEN+ offset can exceed the move that triggers it — 10 points locked
// on a 4-point ATR trigger. The stop must still land at the market, never past
// it; the alternative books 6 points the market never traded.
func TestSimBreakevenPlusCannotOvershootTheMarket(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	o := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.BuySignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), StopLoss: decimal.NewFromFloat(95),
		BreakevenTrigger: decimal.NewFromFloat(104), BreakevenStop: decimal.NewFromFloat(110),
	})

	// Trigger reached (high 105) but the bar closes at 104.5, nowhere near 110.
	s.CheckExits(simCandle(100, 105, 99, 104.5))
	if !o.StopLoss.Equal(decimal.NewFromFloat(104.5)) {
		t.Fatalf("stop = %s, want it capped at the close 104.5, not the 110 the offset asked for", o.StopLoss)
	}

	// The exit that follows is therefore at a price the market actually traded.
	s.CheckExits(simCandle(104.5, 104.6, 100, 101))
	if len(s.Trades) != 1 || !s.Trades[0].ExitPrice.Equal(decimal.NewFromFloat(104.5)) {
		t.Errorf("exit = %+v, want a fill at the capped stop 104.5", s.Trades)
	}
}

func TestSimBreakevenPlusMovesStopAboveEntry(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	o := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.BuySignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), StopLoss: decimal.NewFromFloat(95),
		BreakevenTrigger: decimal.NewFromFloat(104), BreakevenStop: decimal.NewFromFloat(100.5),
	})

	// Below the trigger: nothing moves.
	s.CheckExits(simCandle(100, 103, 99, 102))
	if !o.StopLoss.Equal(decimal.NewFromFloat(95)) {
		t.Fatalf("stop = %s before the trigger, want 95", o.StopLoss)
	}

	// Through the trigger: the stop parks above entry, locking the trade green.
	s.CheckExits(simCandle(102, 105, 101, 104))
	if !o.StopLoss.Equal(decimal.NewFromFloat(100.5)) {
		t.Fatalf("stop = %s after the trigger, want 100.5", o.StopLoss)
	}
	if !o.BreakevenApplied {
		t.Error("BreakevenApplied not set")
	}
}

func TestSimShortTrailAndBreakevenMoveDownward(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	o := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.SellSignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), StopLoss: decimal.NewFromFloat(105),
		TrailDistance:    decimal.NewFromFloat(3),
		BreakevenTrigger: decimal.NewFromFloat(96), BreakevenStop: decimal.NewFromFloat(99.5),
	})

	// Low of 94: breakeven fires (99.5) and the trail improves on it (97).
	s.CheckExits(simCandle(100, 101, 94, 95))
	if !o.StopLoss.Equal(decimal.NewFromFloat(97)) {
		t.Fatalf("stop = %s, want the tighter of BREAKEVEN+ and the trail (97)", o.StopLoss)
	}

	// A bounce must not loosen it.
	s.CheckExits(simCandle(95, 96, 94.5, 95.5))
	if !o.StopLoss.Equal(decimal.NewFromFloat(97)) {
		t.Errorf("stop = %s after a bounce, want it held at 97", o.StopLoss)
	}
}

func TestSimClosePositionFlattensOneSide(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	long := filledOrder(t, s, models.Order{
		Symbol: "TEST", Side: models.BuySignal, Quantity: decimal.NewFromInt(10),
		Price: decimal.NewFromFloat(100), StopLoss: decimal.NewFromFloat(95),
	})

	// A close advised for the other side must not touch this position.
	exitAt := simCandle(100, 100, 100, 100).EndTime
	closed, err := s.ClosePosition("TEST", models.SellSignal, decimal.NewFromFloat(105), exitAt, "wrong side")
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	if closed || long.Status == models.OrderClosed {
		t.Fatal("a short-side exit closed a long position")
	}

	closed, err = s.ClosePosition("TEST", models.BuySignal, decimal.NewFromFloat(105), exitAt, "opposite break")
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	if !closed || long.Status != models.OrderClosed {
		t.Fatal("ClosePosition did not close the long")
	}
	if len(s.Trades) != 1 {
		t.Fatalf("trades = %+v, want one", s.Trades)
	}
	if got := s.Trades[0].PnL; !got.Equal(decimal.NewFromInt(50)) {
		t.Errorf("PnL = %s, want 50 (5 points on 10 units)", got)
	}
	if s.Trades[0].ExitReason != "opposite break" {
		t.Errorf("ExitReason = %q", s.Trades[0].ExitReason)
	}
	// An unstamped exit drops out of the trade log and the -trades-csv dump,
	// which is where every offline analysis starts.
	if !s.Trades[0].ExitTime.Equal(exitAt) {
		t.Errorf("ExitTime = %v, want the candle's end time %v", s.Trades[0].ExitTime, exitAt)
	}
}
