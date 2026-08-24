package core

import (
	"testing"
	"time"

	"zerobha/internal/models"
	"zerobha/internal/risk"

	"github.com/shopspring/decimal"
)

// advisingStrategy emits one exit advice and never a signal.
type advisingStrategy struct {
	advice *ExitAdvice
	calls  int
}

func (a *advisingStrategy) Name() string            { return "Advising" }
func (a *advisingStrategy) Init(DataProvider) error { return nil }
func (a *advisingStrategy) OnCandle(models.Candle) *models.Signal {
	return nil
}
func (a *advisingStrategy) ExitAdvice(models.Candle) *ExitAdvice {
	a.calls++
	return a.advice
}

// closingBroker is a mockBroker that also implements PositionCloser, the path
// the simulator takes.
type closingBroker struct {
	mockBroker
	closedSymbol string
	closedSide   models.SignalType
	closedPrice  decimal.Decimal
	found        bool
}

func (c *closingBroker) ClosePosition(symbol string, side models.SignalType, price decimal.Decimal, at time.Time, reason string) (bool, error) {
	c.closedSymbol, c.closedSide, c.closedPrice = symbol, side, price
	return c.found, nil
}

func advisoryEngine(t *testing.T, b Broker, s Strategy) *Engine {
	t.Helper()
	e := NewEngine(s, b, risk.NewManager(nil, decimal.NewFromInt(10000), 10, 0), nil, nil, nil)
	e.UptrendOnly = false
	return e
}

func exitCandle() models.Candle {
	start := time.Date(2026, 3, 10, 11, 0, 0, 0, time.UTC)
	return models.Candle{
		Symbol:    "TEST",
		Close:     decimal.NewFromFloat(97),
		StartTime: start,
		EndTime:   start.Add(5 * time.Minute),
	}
}

func TestExitAdviceUsesPositionCloserWhenAvailable(t *testing.T) {
	b := &closingBroker{found: true}
	s := &advisingStrategy{advice: &ExitAdvice{Symbol: "TEST", ForSide: models.BuySignal, Reason: "opposite break"}}
	advisoryEngine(t, b, s).Execute(exitCandle())

	if b.closedSymbol != "TEST" || b.closedSide != models.BuySignal {
		t.Errorf("closed %q side %v, want TEST/BUY", b.closedSymbol, b.closedSide)
	}
	if !b.closedPrice.Equal(decimal.NewFromFloat(97)) {
		t.Errorf("closed at %s, want the candle close 97", b.closedPrice)
	}
	// A broker that can close positions itself must not also receive a counter
	// order — that would open a second position rather than flatten the first.
	if len(b.PlacedOrders) != 0 {
		t.Errorf("counter orders placed alongside ClosePosition: %+v", b.PlacedOrders)
	}
}

// The live broker has no ClosePosition, so the engine flattens with a counter
// market order built from the actual position.
func TestExitAdviceFallsBackToCounterOrder(t *testing.T) {
	b := &mockBroker{Positions: []models.Position{
		{Tradingsymbol: "TEST", NetQuantity: 30, Product: "MIS"},
	}}
	s := &advisingStrategy{advice: &ExitAdvice{Symbol: "TEST", ForSide: models.BuySignal, Reason: "opposite break"}}
	advisoryEngine(t, b, s).Execute(exitCandle())

	if len(b.PlacedOrders) != 1 {
		t.Fatalf("placed %d orders, want 1", len(b.PlacedOrders))
	}
	got := b.PlacedOrders[0]
	if got.Side != models.SellSignal {
		t.Errorf("counter order side = %v, want SELL to close a long", got.Side)
	}
	if !got.Quantity.Equal(decimal.NewFromInt(30)) {
		t.Errorf("counter order qty = %s, want 30", got.Quantity)
	}
	if got.Type != "MARKET" {
		t.Errorf("counter order type = %s, want MARKET", got.Type)
	}
}

// Advice names the side it applies to. A strategy that thinks it is long must
// not close a short that happens to be open in the same symbol.
func TestExitAdviceIgnoresOppositeSidePosition(t *testing.T) {
	b := &mockBroker{Positions: []models.Position{
		{Tradingsymbol: "TEST", NetQuantity: -30, Product: "MIS"},
	}}
	s := &advisingStrategy{advice: &ExitAdvice{Symbol: "TEST", ForSide: models.BuySignal, Reason: "opposite break"}}
	advisoryEngine(t, b, s).Execute(exitCandle())

	if len(b.PlacedOrders) != 0 {
		t.Errorf("closed a short on long-side advice: %+v", b.PlacedOrders)
	}
}

// Exits must survive the entry cutoff: it decides whether a new position may be
// opened, not whether an open one may be closed.
func TestExitAdviceRunsAfterTradeCutoff(t *testing.T) {
	b := &closingBroker{found: true}
	s := &advisingStrategy{advice: &ExitAdvice{Symbol: "TEST", ForSide: models.BuySignal, Reason: "opposite break"}}
	e := advisoryEngine(t, b, s)
	e.TradeCutoffMin = 10 * 60 // 10:00, well before the 11:00 candle

	e.Execute(exitCandle())

	if b.closedSymbol != "TEST" {
		t.Error("exit advice was swallowed by the entry cutoff")
	}
}

func TestNoExitAdviceLeavesPositionsAlone(t *testing.T) {
	b := &closingBroker{found: true}
	s := &advisingStrategy{advice: nil}
	advisoryEngine(t, b, s).Execute(exitCandle())

	if b.closedSymbol != "" {
		t.Errorf("closed %q with no advice given", b.closedSymbol)
	}
	if s.calls != 1 {
		t.Errorf("ExitAdvice called %d times, want 1 per candle", s.calls)
	}
}
