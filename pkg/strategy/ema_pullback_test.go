package strategy

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

func TestCalculateTrendAngle(t *testing.T) {
	// Setup State
	state := NewStrategyState("TEST")

	// 1. Test Not Enough History
	angle := state.CalculateTrendAngle(decimal.NewFromInt(100))
	if angle != 0.0 {
		t.Errorf("Expected 0.0 angle for insufficient history, got %f", angle)
	}

	// 2. Populate History for Flat Trend
	// We need 21 items in history. The 0th item is the "Old" one (20 bars ago).
	// We append 21 items of value 100.
	for i := 0; i < 21; i++ {
		state.EmaHistory = append(state.EmaHistory, decimal.NewFromInt(100))
	}

	// Current value is also 100
	angle = state.CalculateTrendAngle(decimal.NewFromInt(100))
	if angle != 0.0 {
		t.Errorf("Expected 0.0 angle for flat trend, got %f", angle)
	}

	// 3. Test 45 Degree Trend
	// We want slope = 1.
	// Slope = (% change) / 20
	// 1 = (% change) / 20 => % change = 20%
	// If Old = 100, Current should be 120.

	// Reset History
	state.EmaHistory = make([]decimal.Decimal, 0)
	state.EmaHistory = append(state.EmaHistory, decimal.NewFromInt(100)) // Oldest (index 0)
	// Fill the middle with whatever, it doesn't matter for the calculation, only index 0 matters.
	for i := 0; i < 20; i++ {
		state.EmaHistory = append(state.EmaHistory, decimal.NewFromInt(110)) // Dummy values
	}

	// Current = 120
	angle = state.CalculateTrendAngle(decimal.NewFromInt(120))

	expectedAngle := 45.0
	if math.Abs(angle-expectedAngle) > 0.1 {
		t.Errorf("Expected ~45.0 angle, got %f", angle)
	}

	// 4. Test Negative Trend (-45 degrees)
	// Current = 80 (20% drop)
	// (80 - 100) / 100 = -0.2
	// -0.2 * 100 = -20%
	// -20 / 20 = -1 slope
	// Atan(-1) = -45 degrees
	angle = state.CalculateTrendAngle(decimal.NewFromInt(80))
	expectedAngle = -45.0
	if math.Abs(angle-expectedAngle) > 0.1 {
		t.Errorf("Expected ~-45.0 angle, got %f", angle)
	}
}
