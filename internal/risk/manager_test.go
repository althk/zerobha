package risk

import (
	"testing"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func TestManager_Evaluate_MaxTrades(t *testing.T) {
	// Setup: Max 3 trades total, Max 2 per stock
	rm := NewManager(nil, decimal.NewFromInt(1000), 3, 2)

	signalA := &models.Signal{Symbol: "ACC"}
	signalB := &models.Signal{Symbol: "BHEL"}

	// 1. Trade 1: ACC (Total 0->1, ACC 0->1) - Allowed
	if err := rm.Evaluate(signalA); err != nil {
		t.Errorf("expected trade 1 to be allowed, got %v", err)
	}
	rm.UpdateTradeLog("ACC", decimal.Zero)

	// 2. Trade 2: ACC (Total 1->2, ACC 1->2) - Allowed
	if err := rm.Evaluate(signalA); err != nil {
		t.Errorf("expected trade 2 to be allowed, got %v", err)
	}
	rm.UpdateTradeLog("ACC", decimal.Zero)

	// 3. Trade 3: ACC (Total 2->3, ACC 2->3) - Rejected (Max per stock)
	if err := rm.Evaluate(signalA); err == nil {
		t.Error("expected trade 3 (ACC) to be rejected due to MaxTradesPerStock, got allowed")
	} else if err.Error() != "risk rejection: max trades per stock reached" {
		t.Errorf("unexpected error message: %v", err)
	}

	// 4. Trade 4: BHEL (Total 2->3, BHEL 0->1) - Allowed
	if err := rm.Evaluate(signalB); err != nil {
		t.Errorf("expected trade 4 (BHEL) to be allowed, got %v", err)
	}
	rm.UpdateTradeLog("BHEL", decimal.Zero)

	// 5. Trade 5: BHEL (Total 3->4) - Rejected (Max total trades)
	if err := rm.Evaluate(signalB); err == nil {
		t.Error("expected trade 5 to be rejected due to MaxTradesPerDay, got allowed")
	}
}
