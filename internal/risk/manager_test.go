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
	rm.UpdateTradeLog("ACC")

	// 2. Trade 2: ACC (Total 1->2, ACC 1->2) - Allowed
	if err := rm.Evaluate(signalA); err != nil {
		t.Errorf("expected trade 2 to be allowed, got %v", err)
	}
	rm.UpdateTradeLog("ACC")

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
	rm.UpdateTradeLog("BHEL")

	// 5. Trade 5: BHEL (Total 3->4) - Rejected (Max total trades)
	if err := rm.Evaluate(signalB); err == nil {
		t.Error("expected trade 5 to be rejected due to MaxTradesPerDay, got allowed")
	}
}

// The daily-loss limit used to be checked against a PnL nothing ever fed,
// so it could not trip. It is now checked against what the broker reports.
func TestManager_DailyLossLimitTripsOnReportedPnL(t *testing.T) {
	rm := NewManager(nil, decimal.NewFromInt(50000), 100, 0)
	sig := &models.Signal{Symbol: "NIFTY26SEP24000CE"}

	rm.SetDayPnL(decimal.NewFromInt(-49999))
	if err := rm.Evaluate(sig); err != nil {
		t.Errorf("loss inside the limit should pass, got %v", err)
	}
	rm.SetDayPnL(decimal.NewFromInt(-50001))
	if err := rm.Evaluate(sig); err == nil {
		t.Error("loss past the limit should be rejected")
	}
	// A recovery re-opens the gate: the limit is on where the day stands.
	rm.SetDayPnL(decimal.NewFromInt(-1000))
	if err := rm.Evaluate(sig); err != nil {
		t.Errorf("after recovering, got %v", err)
	}
	// A zero limit is "no limit", the config convention.
	off := NewManager(nil, decimal.Zero, 100, 0)
	off.SetDayPnL(decimal.NewFromInt(-1000000))
	if err := off.Evaluate(sig); err != nil {
		t.Errorf("zero limit should never trip, got %v", err)
	}
}
