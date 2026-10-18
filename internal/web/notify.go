package web

import (
	"fmt"
	"strings"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// ExitMessage describes a completed round trip for the operator chat.
func ExitMessage(t models.Trade) string {
	var b strings.Builder
	icon := "✅"
	if t.PnL.IsNegative() {
		icon = "🔴"
	}
	if t.IsPaper {
		b.WriteString("[PAPER] ")
	}
	strategy := t.Strategy
	if strategy == "" {
		strategy = "?"
	}
	fmt.Fprintf(&b, "%s EXIT %s: %s %s x%s\n%s -> %s", icon, strategy, t.Direction, t.Symbol,
		t.Quantity.String(), t.EntryPrice.StringFixed(2), t.ExitPrice.StringFixed(2))
	fmt.Fprintf(&b, "\nNet Rs%s (gross %s, costs %s)", signed(t.PnL), signed(t.GrossPnL), t.Costs.StringFixed(0))
	if t.ExitReason != "" {
		fmt.Fprintf(&b, "\nReason: %s", t.ExitReason)
	}
	if !t.EntryTime.IsZero() && !t.ExitTime.IsZero() {
		fmt.Fprintf(&b, "  Held %s", t.ExitTime.Sub(t.EntryTime).Round(time.Minute))
	}
	if u := t.Metadata["Underlying"]; u != "" && u != t.Symbol && t.Metadata["IndexEntry"] != "" {
		fmt.Fprintf(&b, "\n%s %s -> %s", u, t.Metadata["IndexEntry"], t.Metadata["UnderlyingExit"])
	}
	return b.String()
}

// DaySummary sums a session's completed trades for the end-of-day message.
func DaySummary(trades []models.Trade, paper bool) string {
	var b strings.Builder
	if paper {
		b.WriteString("[PAPER] ")
	}
	if len(trades) == 0 {
		b.WriteString("Day summary: no completed trades")
		return b.String()
	}
	net, gross, costs := decimal.Zero, decimal.Zero, decimal.Zero
	wins := 0
	byStrategy := map[string]decimal.Decimal{}
	var order []string
	for _, t := range trades {
		net = net.Add(t.PnL)
		gross = gross.Add(t.GrossPnL)
		costs = costs.Add(t.Costs)
		if t.PnL.IsPositive() {
			wins++
		}
		if _, seen := byStrategy[t.Strategy]; !seen {
			order = append(order, t.Strategy)
		}
		byStrategy[t.Strategy] = byStrategy[t.Strategy].Add(t.PnL)
	}
	fmt.Fprintf(&b, "Day summary: %d trades, %d won\nNet Rs%s (gross %s, costs %s)",
		len(trades), wins, signed(net), signed(gross), costs.StringFixed(0))
	if len(order) > 1 {
		for _, s := range order {
			name := s
			if name == "" {
				name = "?"
			}
			fmt.Fprintf(&b, "\n  %s: Rs%s", name, signed(byStrategy[s]))
		}
	}
	return b.String()
}

func signed(d decimal.Decimal) string {
	if d.IsPositive() {
		return "+" + d.StringFixed(0)
	}
	return d.StringFixed(0)
}
