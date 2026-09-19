package web

import (
	"math"
	"testing"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func tradeAt(entry, exit time.Time, pnl float64) models.Trade {
	return models.Trade{Strategy: "emacross", EntryTime: entry, ExitTime: exit, PnL: decimal.NewFromFloat(pnl)}
}

// A year of trades that doubles the account is a 100% CAGR, and the
// drawdown is measured against equity at the peak, not against cumulative
// profit — so a dip from 3.0L to 2.4L after starting at 2.0L is 20%, not
// 60% of the profit made so far.
func TestGrowthAnnualisesAgainstStartingCapital(t *testing.T) {
	start := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	trades := []models.Trade{
		tradeAt(start, start.Add(time.Hour), 100000),                               // 2.0L -> 3.0L, peak
		tradeAt(start.AddDate(0, 3, 0), start.AddDate(0, 3, 0), -60000),            // 3.0L -> 2.4L
		tradeAt(start.AddDate(0, 6, 0), start.AddDate(0, 6, 0), 160000),            // 2.4L -> 4.0L
		tradeAt(start.AddDate(1, 0, 0).Add(-time.Hour), start.AddDate(1, 0, 0), 0), // pins the span at one year
	}
	g := growthFor(trades, 200000)
	if !g.HasCAGR || math.Abs(g.CAGRPct-100) > 0.5 {
		t.Errorf("CAGR = %.2f%% (has=%v), want ~100%%", g.CAGRPct, g.HasCAGR)
	}
	if math.Abs(g.ReturnPct-100) > 1e-9 {
		t.Errorf("return = %.2f%%, want 100%%", g.ReturnPct)
	}
	if math.Abs(g.MaxDDPct-20) > 1e-9 {
		t.Errorf("max DD = %.2f%% of equity, want 20%%", g.MaxDDPct)
	}
	if math.Abs(g.SpanDays-365) > 1 {
		t.Errorf("span = %.1f days, want ~365", g.SpanDays)
	}
}

// A week of trades is not annualised: (1.01)^52 is a number with no
// information in it. The plain return is carried instead, and the UI says
// how long the run has been going.
func TestGrowthDoesNotAnnualiseAShortRun(t *testing.T) {
	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	trades := []models.Trade{
		tradeAt(start, start.Add(time.Hour), 20000),
		tradeAt(start.AddDate(0, 0, 4), start.AddDate(0, 0, 4).Add(time.Hour), -10000),
	}
	g := growthFor(trades, 2000000)
	if g.HasCAGR {
		t.Errorf("a %.1f-day run must not be annualised: %+v", g.SpanDays, g)
	}
	if math.Abs(g.ReturnPct-0.5) > 1e-9 {
		t.Errorf("return = %.3f%%, want 0.5%%", g.ReturnPct)
	}
	if math.Abs(g.MaxDDPct-10000.0/2020000*100) > 1e-9 {
		t.Errorf("max DD = %.4f%%, want 10k of the 20.2L peak", g.MaxDDPct)
	}
}

// Without a starting capital there is no denominator, and a live run whose
// balance could not be read shows nothing rather than a figure against a
// made-up account.
func TestGrowthNeedsAStartingCapital(t *testing.T) {
	start := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	trades := []models.Trade{tradeAt(start, start.AddDate(1, 0, 0), 100000)}
	g := growthFor(trades, 0)
	if g.HasCAGR || g.MaxDDPct != 0 || g.ReturnPct != 0 || g.SpanDays != 0 {
		t.Errorf("no capital must leave growth unset: %+v", g)
	}
}

// Losing the whole account is -100% a year, not NaN.
func TestGrowthBottomsOutAtMinus100(t *testing.T) {
	start := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	trades := []models.Trade{tradeAt(start, start.AddDate(0, 6, 0), -250000)}
	g := growthFor(trades, 200000)
	if !g.HasCAGR || g.CAGRPct != -100 || g.MaxDDPct != 125 {
		t.Errorf("wiped-out account: %+v", g)
	}
}

// The per-strategy rows of /api/performance carry the same figures, read
// against the server's starting capital.
func TestPerformanceRowsCarryGrowth(t *testing.T) {
	start := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	trades := []models.Trade{
		tradeAt(start, start.Add(time.Hour), 50000),
		tradeAt(start.AddDate(1, 0, 0).Add(-time.Hour), start.AddDate(1, 0, 0), -10000),
	}
	row := buildStats("emacross", trades, 200000)
	if !row.HasCAGR || math.Abs(row.CAGRPct-20) > 0.5 || row.MaxDrawdown != 10000 || math.Abs(row.MaxDDPct-4) > 1e-9 {
		t.Errorf("row = %+v", row)
	}
	if none := buildStats("emacross", trades, 0); none.HasCAGR || none.MaxDrawdown != 10000 {
		t.Errorf("rupee drawdown must survive an unknown capital: %+v", none)
	}
}
