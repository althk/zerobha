package risk

import (
	"strings"
	"testing"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func d(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

func allOpen(string) bool { return true }

func TestStockRiskBudget_OffWhenZero(t *testing.T) {
	rm := NewManager(nil, d(0), 100, 0)
	if _, limited, err := rm.StockRiskBudget("SBIN", d(-1000000), allOpen); limited || err != nil {
		t.Errorf("zero limit should be off, got limited=%v err=%v", limited, err)
	}
}

func TestStockRiskBudget_CountsLossAndOpenRiskOnTheSameUnderlying(t *testing.T) {
	rm := NewManager(nil, d(0), 100, 0)
	rm.MaxLossPerStockPerDay = d(10000)

	// A profit does not enlarge the budget.
	if b, _, err := rm.StockRiskBudget("NIFTY 50", d(5000), allOpen); err != nil || !b.Equal(d(10000)) {
		t.Fatalf("budget after a profit = %s (%v), want 10000", b, err)
	}

	// Two legs on NIFTY from different strategies, one on another index.
	rm.RecordEntry("NIFTY26OCT24000CE", "NIFTY 50", d(3000))
	rm.RecordEntry("NIFTY26OCT24500PE", "NIFTY 50", d(2000))
	rm.RecordEntry("SENSEX26OCT80000CE", "SENSEX", d(4000))

	b, _, err := rm.StockRiskBudget("NIFTY 50", d(-1500), allOpen)
	if err != nil || !b.Equal(d(3500)) { // 10000 - 1500 lost - 3000 - 2000 open
		t.Fatalf("budget = %s (%v), want 3500", b, err)
	}

	// Once a leg closes its risk stops counting; its result is in dayPnL.
	ceClosed := func(sym string) bool { return sym != "NIFTY26OCT24000CE" }
	if b, _, _ := rm.StockRiskBudget("NIFTY 50", d(-1500), ceClosed); !b.Equal(d(6500)) {
		t.Errorf("budget with the CE closed = %s, want 6500", b)
	}
}

func TestStockRiskBudget_RefusesOnceLossOrOpenRiskUsesItUp(t *testing.T) {
	rm := NewManager(nil, d(0), 100, 0)
	rm.MaxLossPerStockPerDay = d(5000)

	if _, _, err := rm.StockRiskBudget("SBIN", d(-5000), allOpen); err == nil || !strings.Contains(err.Error(), "daily loss limit for SBIN") {
		t.Errorf("a loss at the limit should refuse, got %v", err)
	}
	rm.RecordEntry("SBIN", "SBIN", d(4000))
	if _, _, err := rm.StockRiskBudget("SBIN", d(-1000), allOpen); err == nil || !strings.Contains(err.Error(), "used up") {
		t.Errorf("loss + open risk at the limit should refuse, got %v", err)
	}
	// Another stock is untouched.
	if b, _, err := rm.StockRiskBudget("TCS", d(0), allOpen); err != nil || !b.Equal(d(5000)) {
		t.Errorf("TCS budget = %s (%v), want 5000", b, err)
	}
}

func TestMonthlyLossLimit(t *testing.T) {
	rm := NewManager(nil, d(0), 100, 0)
	rm.MaxMonthlyLoss = d(50000)
	sig := &models.Signal{Symbol: "SBIN"}

	rm.SetMonthPnL(d(-49999))
	if err := rm.Evaluate(sig); err != nil {
		t.Errorf("inside the limit should pass, got %v", err)
	}
	rm.SetMonthPnL(d(-50001))
	if err := rm.Evaluate(sig); err == nil || !strings.Contains(err.Error(), "monthly loss limit") {
		t.Errorf("past the limit should refuse, got %v", err)
	}
	rm.MaxMonthlyLoss = d(0)
	if err := rm.Evaluate(sig); err != nil {
		t.Errorf("zero limit is off, got %v", err)
	}
}
