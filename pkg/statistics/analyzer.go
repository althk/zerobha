package statistics

import (
	"fmt"
	"math"
	"os"
	"text/tabwriter"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

type Performance struct {
	TotalTrades  int
	WinRate      float64
	ProfitFactor float64
	NetProfit    decimal.Decimal
	MaxDrawdown  float64 // As a percentage (e.g., 5.5 for 5.5%)
	GrossProfit  decimal.Decimal
	GrossLoss    decimal.Decimal
	AverageWin   decimal.Decimal
	AverageLoss  decimal.Decimal
	// Sharpe is the per-trade Sharpe ratio: mean(PnL) / stddev(PnL) over the
	// trade series (risk-free rate assumed 0). It is unitless and comparable
	// across symbols/runs, which makes it useful for tuning. It is NOT
	// annualized — to annualize, multiply by sqrt(trades per year).
	Sharpe float64
}

// Analyze calculates metrics from a list of closed trades
func Analyze(trades []models.Trade, initialCapital decimal.Decimal) Performance {
	stats := Performance{
		TotalTrades: len(trades),
	}

	if stats.TotalTrades == 0 {
		return stats
	}

	wins := 0
	currentEquity := initialCapital
	peakEquity := initialCapital
	maxDrawdownVal := decimal.Zero

	for _, t := range trades {
		// 1. Basic PnL Stats
		if t.PnL.GreaterThan(decimal.Zero) {
			wins++
			stats.GrossProfit = stats.GrossProfit.Add(t.PnL)
		} else {
			stats.GrossLoss = stats.GrossLoss.Add(t.PnL.Abs()) // Store as positive number
		}

		// 2. Drawdown Logic (The Hard Part)
		// Update Equity
		currentEquity = currentEquity.Add(t.PnL)

		// Did we make a new High?
		if currentEquity.GreaterThan(peakEquity) {
			peakEquity = currentEquity
		}

		// Calculate Drawdown from Peak
		// DD = (Peak - Current) / Peak
		drawdown := peakEquity.Sub(currentEquity).Div(peakEquity)
		if drawdown.GreaterThan(maxDrawdownVal) {
			maxDrawdownVal = drawdown
		}
	}

	stats.NetProfit = stats.GrossProfit.Sub(stats.GrossLoss)
	stats.WinRate = (float64(wins) / float64(stats.TotalTrades)) * 100.0
	stats.MaxDrawdown = maxDrawdownVal.Mul(decimal.NewFromInt(100)).InexactFloat64()

	// Sharpe: mean(PnL) / stddev(PnL) over the trade series. Needs >=2 trades
	// for a defined sample stddev; with fewer, leave it at 0.
	if stats.TotalTrades >= 2 {
		var sum float64
		pnls := make([]float64, len(trades))
		for i, t := range trades {
			v := t.PnL.InexactFloat64()
			pnls[i] = v
			sum += v
		}
		mean := sum / float64(len(pnls))

		var sqDiff float64
		for _, v := range pnls {
			d := v - mean
			sqDiff += d * d
		}
		// Sample standard deviation (n-1 denominator).
		variance := sqDiff / float64(len(pnls)-1)
		std := math.Sqrt(variance)
		if std > 0 {
			stats.Sharpe = mean / std
		}
	}

	// 3. Profit Factor (Avoid division by zero)
	if stats.GrossLoss.IsZero() {
		stats.ProfitFactor = 999.0 // Infinite
	} else {
		pf := stats.GrossProfit.Div(stats.GrossLoss)
		stats.ProfitFactor, _ = pf.Float64()
	}

	return stats
}

// PrintTearSheet outputs a professional report to console
func PrintTearSheet(stats Performance) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "\n----------------------------------------")
	_, _ = fmt.Fprintln(w, "METRIC\tVALUE")
	_, _ = fmt.Fprintln(w, "----------------------------------------\t-----")

	fmt.Fprintf(w, "Total Trades\t%d\n", stats.TotalTrades)
	fmt.Fprintf(w, "Net Profit\t₹ %s\n", stats.NetProfit.StringFixed(2))
	fmt.Fprintf(w, "Win Rate\t%.2f%%\n", stats.WinRate)
	fmt.Fprintf(w, "Profit Factor\t%.2f\n", stats.ProfitFactor)
	fmt.Fprintf(w, "Sharpe (per-trade)\t%.3f\n", stats.Sharpe)
	fmt.Fprintf(w, "Max Drawdown\t%.2f%%\n", stats.MaxDrawdown)
	fmt.Fprintf(w, "Gross Profit\t₹ %s\n", stats.GrossProfit.StringFixed(2))
	fmt.Fprintf(w, "Gross Loss\t₹ %s\n", stats.GrossLoss.StringFixed(2))

	fmt.Fprintln(w, "----------------------------------------")
	_ = w.Flush()
}
