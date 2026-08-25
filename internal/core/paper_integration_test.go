package core

import (
	"testing"
	"time"

	"zerobha/internal/models"
	"zerobha/internal/risk"

	"github.com/shopspring/decimal"
)

// observingBroker is a mockBroker that also implements TickObserver, the way a
// broker holding its own resting stops does.
type observingBroker struct {
	mockBroker
	ticks []decimal.Decimal
}

func (o *observingBroker) OnTick(symbol string, price decimal.Decimal, at time.Time) {
	o.ticks = append(o.ticks, price)
}

// A broker that fills its own stops has no other way to learn that a price
// traded, so the engine must forward every tick — including for symbols with no
// tracked order, since a position whose strategy sets no trail still has a stop.
func TestOnTickForwardsToObservingBroker(t *testing.T) {
	b := &observingBroker{}
	e := NewEngine(&mockStrategy{}, b, risk.NewManager(nil, decimal.NewFromInt(10000), 10, 0), nil, nil, nil)

	e.OnTick("SBIN", decimal.NewFromInt(500))

	if len(b.ticks) != 1 || !b.ticks[0].Equal(decimal.NewFromInt(500)) {
		t.Fatalf("broker saw %v, want one tick at 500", b.ticks)
	}
}

// A broker that does not implement TickObserver must be left alone: the live
// adapter's stops rest at the exchange and need no help.
func TestOnTickIsANoOpForANonObservingBroker(t *testing.T) {
	b := &mockBroker{}
	e := NewEngine(&mockStrategy{}, b, risk.NewManager(nil, decimal.NewFromInt(10000), 10, 0), nil, nil, nil)

	// Nothing to assert beyond "does not panic and places no order".
	e.OnTick("SBIN", decimal.NewFromInt(500))

	if len(b.PlacedOrders) != 0 {
		t.Errorf("a tick placed %d order(s)", len(b.PlacedOrders))
	}
}

// A position book keeps the day's closed positions with a zero net quantity
// (Kite does, and the paper broker mirrors it). Counting those against
// MaxConcurrent would retire a slot for every trade already exited, tightening
// the cap as the day went on.
func TestConcurrencySlotsIgnoreClosedPositions(t *testing.T) {
	b := &mockBroker{
		Positions: []models.Position{
			{Tradingsymbol: "AAA", NetQuantity: 0, PnL: decimal.NewFromInt(500)},
			{Tradingsymbol: "BBB", NetQuantity: 0, PnL: decimal.NewFromInt(-200)},
			{Tradingsymbol: "CCC", NetQuantity: 10},
		},
	}
	s := &signallingStrategy{}
	e := NewEngine(s, b, risk.NewManager(nil, decimal.NewFromInt(1000000), 100, 0), nil, nil, nil)
	e.UptrendOnly = false
	e.MaxConcurrent = 2 // one open position, so one slot remains

	e.Execute(entryCandle())

	if len(b.PlacedOrders) != 1 {
		t.Fatalf("placed %d orders, want 1 — the two closed positions consumed the free slot",
			len(b.PlacedOrders))
	}
}

// signallingStrategy emits one entry signal per candle.
type signallingStrategy struct{}

func (s *signallingStrategy) Name() string            { return "Signalling" }
func (s *signallingStrategy) Init(DataProvider) error { return nil }
func (s *signallingStrategy) OnCandle(c models.Candle) *models.Signal {
	return &models.Signal{
		Symbol:      c.Symbol,
		Type:        models.BuySignal,
		Price:       decimal.NewFromInt(100),
		StopLoss:    decimal.NewFromInt(95),
		Target:      decimal.NewFromInt(115),
		RiskPct:     decimal.NewFromFloat(0.01),
		ProductType: "MIS",
		Metadata:    map[string]string{"Strategy": "test"},
	}
}

func entryCandle() models.Candle {
	start := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	return models.Candle{
		Symbol:    "TEST",
		Close:     decimal.NewFromInt(100),
		StartTime: start,
		EndTime:   start.Add(5 * time.Minute),
	}
}
