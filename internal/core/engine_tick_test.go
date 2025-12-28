package core

import (
	"testing"
	"time"
	"zerobha/internal/models"
	"zerobha/internal/risk"

	"github.com/shopspring/decimal"
)

// Helper mock strategy for this test
type mockStrategyHelper struct {
	Signal *models.Signal
}

func (m *mockStrategyHelper) Name() string                     { return "MockHelper" }
func (m *mockStrategyHelper) Init(provider DataProvider) error { return nil }
func (m *mockStrategyHelper) OnCandle(candle models.Candle) *models.Signal {
	return m.Signal
}

func TestEngineTickRounding(t *testing.T) {
	// Setup
	broker := &mockBroker{} // Using mockBroker from engine_verify_test.go (same package)

	// Create a dummy Risk Manager (Store nil is allowed)
	// MaxLoss 5000, MaxTrades 100, MaxPerStock 10
	riskMgr := risk.NewManager(nil, decimal.NewFromInt(5000), 100, 10)

	// Custom strategy to return a specific signal
	// RELIANCE Price 2500 -> Tick Size 0.10 (Range 1001-5000)
	strategy := &mockStrategyHelper{
		Signal: &models.Signal{
			Symbol:      "RELIANCE",
			Type:        models.BuySignal,
			ProductType: "MIS",
			Price:       decimal.NewFromFloat(2500.1234), // Expect 2500.10
			StopLoss:    decimal.NewFromFloat(2490.555),  // Expect 2490.60
			Target:      decimal.NewFromFloat(2520.18),   // Expect 2520.20
		},
	}

	engine := NewEngine(strategy, broker, riskMgr, nil, nil, nil)

	// Execute
	candle := models.Candle{
		Symbol:  "RELIANCE",
		Close:   decimal.NewFromFloat(2500.0),
		EndTime: time.Now(),
	}
	engine.Execute(candle)

	// Verify Order
	if len(broker.PlacedOrders) != 1 {
		t.Fatalf("Expected 1 order placed, got %d", len(broker.PlacedOrders))
	}

	order := broker.PlacedOrders[0]

	// Verify Price Rounding (Tick 0.10)
	expectedPrice := decimal.NewFromFloat(2500.10)
	if !order.Price.Equal(expectedPrice) {
		t.Errorf("Expected Price %s, got %s", expectedPrice, order.Price)
	}

	// Verify StopLoss Rounding (Tick 0.10)
	expectedSL := decimal.NewFromFloat(2490.60)
	if !order.StopLoss.Equal(expectedSL) {
		t.Errorf("Expected StopLoss %s, got %s", expectedSL, order.StopLoss)
	}

	// Verify Target Rounding (Tick 0.10)
	expectedTarget := decimal.NewFromFloat(2520.20)
	if !order.Target.Equal(expectedTarget) {
		t.Errorf("Expected Target %s, got %s", expectedTarget, order.Target)
	}
}
