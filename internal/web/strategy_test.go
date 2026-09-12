package web

import (
	"math"
	"testing"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func optionTrade(entry, exit float64, side, idxEntry, idxExit, reason string) models.Trade {
	return models.Trade{
		Symbol: "NIFTY26SEP24000CE", Strategy: "emacross", Direction: "LONG",
		Quantity: decimal.NewFromInt(650), EntryPrice: decimal.NewFromFloat(entry), ExitPrice: decimal.NewFromFloat(exit),
		GrossPnL: decimal.NewFromFloat((exit - entry) * 650), Costs: decimal.NewFromFloat(300),
		PnL:       decimal.NewFromFloat((exit-entry)*650 - 300),
		EntryTime: time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC), ExitTime: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
		ExitReason: reason,
		Metadata:   map[string]string{"Underlying": "NIFTY 50", "IndexSide": side, "IndexEntry": idxEntry, "UnderlyingExit": idxExit, "DaysToExpiry": "3", "Reason": "GoldenCross"},
	}
}

// A bought put is a SHORT view: its index bps must be signed by the view, not
// by the direction of the option trade.
func TestEnrichReadsTheTradeInIndexUnits(t *testing.T) {
	put := enrich(optionTrade(200, 230, "SELL", "24300", "24200", "EMACross index stop: index NIFTY 50 24200 through stop 24210"))
	if put.Side != "SHORT" {
		t.Errorf("side = %q, want SHORT for a SELL index view", put.Side)
	}
	if !put.HasIndex || math.Abs(put.IndexBps-41.15) > 0.1 {
		t.Errorf("index bps = %.2f (has=%v), want +41.15 (index fell 100 pts on a short view)", put.IndexBps, put.HasIndex)
	}
	if put.ExitReason != "index stop/trail" {
		t.Errorf("exit bucket = %q", put.ExitReason)
	}
	wantNet := ((230.0-200)*650 - 300) / (200.0 * 650) * 1e4
	if math.Abs(put.NetBps-wantNet) > 0.01 || math.Abs(put.GrossBps-1500) > 0.01 {
		t.Errorf("net bps = %.2f gross = %.2f, want %.2f / 1500", put.NetBps, put.GrossBps, wantNet)
	}
	if put.DTE != 3 || !put.HasDTE || put.HoldMin != 90 {
		t.Errorf("dte=%d hold=%.0f", put.DTE, put.HoldMin)
	}

	eod := enrich(optionTrade(200, 210, "BUY", "24300", "24350", "AutoSquareOff"))
	if eod.ExitReason != "EOD square-off" || math.Abs(eod.IndexBps-20.58) > 0.1 {
		t.Errorf("EOD row: bucket %q index bps %.2f", eod.ExitReason, eod.IndexBps)
	}
	stock := enrich(models.Trade{Symbol: "SBIN", Direction: "SHORT", Quantity: decimal.NewFromInt(10),
		EntryPrice: decimal.NewFromInt(500), ExitPrice: decimal.NewFromInt(490), PnL: decimal.NewFromInt(100), ExitReason: "SL-HIT"})
	if stock.Side != "SHORT" || stock.HasIndex || stock.ExitReason != "stop/trail" {
		t.Errorf("stock row: %+v", stock)
	}
	// A row written before costs were tracked: gross falls back to net.
	if stock.GrossPnL != 100 {
		t.Errorf("legacy gross = %.0f, want 100", stock.GrossPnL)
	}
}

func TestStatsForComputesTheBacktestMetrics(t *testing.T) {
	rows := []tradeRow{
		{NetBps: 100, GrossBps: 120, PnL: 1000, Costs: 200, HasIndex: true, IndexBps: 5, HoldMin: 60},
		{NetBps: -50, GrossBps: -30, PnL: -500, Costs: 200, HasIndex: true, IndexBps: -2, HoldMin: 30},
		{NetBps: 200, GrossBps: 220, PnL: 2000, Costs: 200, HoldMin: 120},
		{NetBps: -100, GrossBps: -80, PnL: -1000, Costs: 200, HoldMin: 45},
	}
	g := statsFor("x", rows)
	if g.N != 4 || math.Abs(g.NetBps-37.5) > 1e-9 || math.Abs(g.GrossBps-57.5) > 1e-9 {
		t.Errorf("means: %+v", g)
	}
	if g.IndexN != 2 || math.Abs(g.IndexBps-1.5) > 1e-9 {
		t.Errorf("index mean over rows that carry one: n=%d mean=%.2f", g.IndexN, g.IndexBps)
	}
	if g.WinRate != 50 || math.Abs(g.ProfitFactor-2) > 1e-9 || g.AvgWinBps != 150 || g.AvgLossBps != -75 {
		t.Errorf("win/pf/avg: %+v", g)
	}
	// sd of {100,-50,200,-100} = 137.69; t = 37.5 / (137.69/2) = 0.545; sharpe = 0.272
	if math.Abs(g.T-0.5447) > 0.001 || math.Abs(g.Sharpe-0.2723) > 0.001 {
		t.Errorf("t = %.4f sharpe = %.4f", g.T, g.Sharpe)
	}
	if g.NetRupees != 1500 || g.CostsRupees != 800 || g.MaxDDRupees != 1000 {
		t.Errorf("rupees: net %.0f costs %.0f dd %.0f", g.NetRupees, g.CostsRupees, g.MaxDDRupees)
	}
	if g.MedianHold != 60 {
		t.Errorf("median hold = %.0f", g.MedianHold)
	}
	if e := statsFor("empty", nil); e.N != 0 || e.T != 0 {
		t.Errorf("empty group: %+v", e)
	}
}
