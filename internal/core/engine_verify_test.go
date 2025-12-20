package core

import (
	"testing"
	"time"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// mockBroker for verification
type mockBroker struct {
	GTTs            []models.GTT
	Orders          []models.Order
	Positions       []models.Position
	CancelledGTTs   []int
	CancelledOrders []string
	PlacedOrders    []models.Order
}

func (m *mockBroker) GetBalance() (decimal.Decimal, error) { return decimal.NewFromInt(100000), nil }
func (m *mockBroker) GetQuote(symbol string) (decimal.Decimal, error) {
	return decimal.NewFromInt(100), nil
}
func (m *mockBroker) HasOpenPosition(symbol string) (bool, error) { return false, nil }

func (m *mockBroker) PlaceOrder(order models.Order) (models.Order, error) {
	order.ID = "MOCK-ORDER-ID"
	m.PlacedOrders = append(m.PlacedOrders, order)
	return order, nil
}

func (m *mockBroker) GetGTTs() ([]models.GTT, error) { return m.GTTs, nil }
func (m *mockBroker) CancelGTT(triggerID int) error {
	m.CancelledGTTs = append(m.CancelledGTTs, triggerID)
	return nil
}
func (m *mockBroker) GetOpenOrders() ([]models.Order, error) { return m.Orders, nil }
func (m *mockBroker) CancelOrder(orderID string) error {
	m.CancelledOrders = append(m.CancelledOrders, orderID)
	return nil
}
func (m *mockBroker) GetPositions() ([]models.Position, error) { return m.Positions, nil }

// mockStrategy
type mockStrategy struct{}

func (m *mockStrategy) Name() string                     { return "Mock" }
func (m *mockStrategy) Init(provider DataProvider) error { return nil }
func (m *mockStrategy) OnCandle(candle models.Candle) *models.Signal {
	panic("Strategy should not be called!")
}

func TestTradeCutoff(t *testing.T) {
	broker := &mockBroker{}
	strategy := &mockStrategy{}
	engine := NewEngine(strategy, broker, nil, nil, nil, nil) // Risk/Journal nil might cause panic if accessed, but cutoff is before that

	// Test 15:06 Candle
	candle := models.Candle{
		EndTime: time.Date(2025, 12, 20, 15, 6, 0, 0, time.UTC),
	}

	// Should NOT panic because Engine returns early
	engine.Execute(candle)
}

func TestSquareOff(t *testing.T) {
	broker := &mockBroker{
		GTTs: []models.GTT{
			{ID: 101, Tradingsymbol: "INFY", Status: "active"},
		},
		Orders: []models.Order{
			{ID: "ORD-1", Symbol: "TCS", ProductType: "MIS", Status: "OPEN"},
			{ID: "ORD-2", Symbol: "WIPRO", ProductType: "CNC", Status: "OPEN"}, // Should NOT be cancelled
		},
		Positions: []models.Position{
			{Tradingsymbol: "RELIANCE", Product: "MIS", NetQuantity: 50}, // Should Close (Sell)
			{Tradingsymbol: "HDFC", Product: "MIS", NetQuantity: -25},    // Should Close (Buy)
			{Tradingsymbol: "ITC", Product: "CNC", NetQuantity: 100},     // Should Ignore
		},
	}

	engine := NewEngine(&mockStrategy{}, broker, nil, nil, nil, nil)

	engine.SquareOff()

	// Verify Cancellations
	if len(broker.CancelledGTTs) != 1 || broker.CancelledGTTs[0] != 101 {
		t.Errorf("Expected GTT 101 cancelled, got %v", broker.CancelledGTTs)
	}

	if len(broker.CancelledOrders) != 1 || broker.CancelledOrders[0] != "ORD-1" {
		t.Errorf("Expected Order ORD-1 cancelled, got %v", broker.CancelledOrders)
	}

	// Verify Trades
	if len(broker.PlacedOrders) != 2 {
		t.Errorf("Expected 2 closing orders, got %d", len(broker.PlacedOrders))
	}

	// Check Reliance Close (Sell 50)
	foundRel := false
	for _, o := range broker.PlacedOrders {
		if o.Symbol == "RELIANCE" {
			foundRel = true
			if o.Side != models.SellSignal {
				t.Errorf("Expected Reliance Close Side SELL, got %v", o.Side)
			}
			if !o.Quantity.Equal(decimal.NewFromInt(50)) {
				t.Errorf("Expected Reliance Close Qty 50, got %s", o.Quantity)
			}
		}
	}
	if !foundRel {
		t.Error("Reliance closing order not found")
	}

	// Check HDFC Close (Buy 25)
	foundHDFC := false
	for _, o := range broker.PlacedOrders {
		if o.Symbol == "HDFC" {
			foundHDFC = true
			if o.Side != models.BuySignal {
				t.Errorf("Expected HDFC Close Side BUY, got %v", o.Side)
			}
			if !o.Quantity.Equal(decimal.NewFromInt(25)) {
				t.Errorf("Expected HDFC Close Qty 25, got %s", o.Quantity)
			}
		}
	}
	if !foundHDFC {
		t.Error("HDFC closing order not found")
	}
}
