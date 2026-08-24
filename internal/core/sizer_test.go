package core

import (
	"testing"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func sizingSignal(price, stop float64) *models.Signal {
	return &models.Signal{
		Symbol:   "TEST",
		Type:     models.BuySignal,
		Price:    decimal.NewFromFloat(price),
		StopLoss: decimal.NewFromFloat(stop),
	}
}

// The historical behaviour: 1% of capital risked over the stop distance.
func TestCalculateQuantityDefaultsToOnePercentRisk(t *testing.T) {
	sig := sizingSignal(100, 95)
	got := CalculateQuantity(decimal.NewFromInt(500000), sig, decimal.NewFromInt(5))

	// 1% of 5L = 5,000 risked over 5 points = 1,000 units, and purchasing power
	// (25L / 100) allows 25,000 — so the volatility limit binds.
	if !got.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("qty = %s, want 1000", got)
	}
}

func TestCalculateQuantityHonoursSignalRiskPct(t *testing.T) {
	sig := sizingSignal(100, 95)
	sig.RiskPct = decimal.NewFromFloat(0.005) // Donchian's 0.5%

	got := CalculateQuantity(decimal.NewFromInt(500000), sig, decimal.NewFromInt(5))
	if !got.Equal(decimal.NewFromInt(500)) {
		t.Errorf("qty = %s, want 500 (half the risk, half the size)", got)
	}
}

// Futures trade in lots. A size of 1,000 units against a 250-unit lot is three
// lots, not 1,000 units and not 4 lots.
func TestCalculateQuantityRoundsDownToWholeLots(t *testing.T) {
	sig := sizingSignal(100, 95)
	sig.LotSize = 250

	got := CalculateQuantity(decimal.NewFromInt(500000), sig, decimal.NewFromInt(5))
	if !got.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("qty = %s, want 1000 (exactly 4 lots of 250)", got)
	}

	// A budget that affords 3.6 lots must round down to 3.
	sig.LotSize = 275
	got = CalculateQuantity(decimal.NewFromInt(500000), sig, decimal.NewFromInt(5))
	if !got.Equal(decimal.NewFromInt(825)) {
		t.Errorf("qty = %s, want 825 (3 lots of 275, the 0.6 lot dropped)", got)
	}
}

// Sizing to a fraction of a contract is not a small position, it is an
// impossible one — the trade has to be skipped instead.
func TestCalculateQuantityRefusesSubLotBudget(t *testing.T) {
	sig := sizingSignal(100, 95)
	sig.LotSize = 5000

	got := CalculateQuantity(decimal.NewFromInt(500000), sig, decimal.NewFromInt(5))
	if !got.IsZero() {
		t.Errorf("qty = %s, want 0 — the risk budget affords less than one lot", got)
	}
}

func TestCalculateQuantityRejectsZeroStopDistance(t *testing.T) {
	if got := CalculateQuantity(decimal.NewFromInt(500000), sizingSignal(100, 100), decimal.NewFromInt(5)); !got.IsZero() {
		t.Errorf("qty = %s, want 0 when price == stop", got)
	}
}
