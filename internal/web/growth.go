package web

import (
	"math"

	"zerobha/internal/models"
)

// minCAGRSpanDays is the shortest span that gets annualised. A week at +1%
// compounds to several thousand percent a year, which is a number with no
// information in it; below this the row carries the plain return on capital
// and the UI says how long the run has been going.
const minCAGRSpanDays = 30

// growthStats reads a trade list against the capital the run started with:
// the rupee drawdown as a fraction of the equity at its peak, and the return
// annualised over the span from the first entry to the last exit. Both need
// a starting capital; with none (a live run whose balance could not be read)
// they are left unset and only the rupee drawdown stands.
//
// One process runs one strategy against one account, so every strategy in
// the table is read against the same starting capital. That is the account's
// CAGR attributed to the strategy, not a per-strategy allocation.
type growthStats struct {
	StartingCapital float64 `json:"starting_capital"`
	SpanDays        float64 `json:"span_days"`
	ReturnPct       float64 `json:"return_pct"` // net / starting capital, %
	CAGRPct         float64 `json:"cagr_pct"`   // annualised, %; valid only when HasCAGR
	HasCAGR         bool    `json:"has_cagr"`
	MaxDDPct        float64 `json:"max_dd_pct"` // deepest peak-to-trough, % of equity at the peak
}

// growthFor computes growthStats over chronological trades. The equity path
// is starting capital plus cumulative net PnL, so the percentage drawdown is
// against the account, not against cumulative profit — a run that is down
// from the start has a drawdown measured from its capital, not from zero.
func growthFor(trades []models.Trade, startingCapital float64) growthStats {
	g := growthStats{StartingCapital: startingCapital}
	if len(trades) == 0 || startingCapital <= 0 {
		return g
	}
	equity, peak, maxDDPct := startingCapital, startingCapital, 0.0
	first, last := trades[0].EntryTime, trades[0].ExitTime
	for _, t := range trades {
		pnl, _ := t.PnL.Float64()
		equity += pnl
		if equity > peak {
			peak = equity
		}
		if peak > 0 {
			if dd := (peak - equity) / peak * 100; dd > maxDDPct {
				maxDDPct = dd
			}
		}
		if !t.EntryTime.IsZero() && t.EntryTime.Before(first) {
			first = t.EntryTime
		}
		if t.ExitTime.After(last) {
			last = t.ExitTime
		}
	}
	g.MaxDDPct = maxDDPct
	g.ReturnPct = (equity - startingCapital) / startingCapital * 100
	g.SpanDays = last.Sub(first).Hours() / 24
	if g.SpanDays < minCAGRSpanDays {
		return g
	}
	years := g.SpanDays / 365.25
	if equity <= 0 {
		g.CAGRPct, g.HasCAGR = -100, true
		return g
	}
	g.CAGRPct = (math.Pow(equity/startingCapital, 1/years) - 1) * 100
	g.HasCAGR = true
	return g
}
