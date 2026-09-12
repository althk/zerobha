package web

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/db"

	"github.com/shopspring/decimal"
)

// The strategy monitor reads a paper (or live) run in the units the strategy
// was measured in. Rupee totals are dominated by high-premium days and have
// reversed sign against per-trade bps more than once in this project
// (CLAUDE.md, gapfade and the tp_rr run), so every aggregate here is a
// per-trade return, with a t-stat, and the backtest's own figures sit beside
// it for comparison.

// tradeRow is one round trip, enriched.
type tradeRow struct {
	ExitTime   time.Time `json:"exit_time"`
	EntryTime  time.Time `json:"entry_time"`
	Symbol     string    `json:"symbol"`
	Underlying string    `json:"underlying"`
	Strategy   string    `json:"strategy"`
	// Side is the direction of the VIEW: for an option it is the index side
	// (a bought put is SHORT), for a stock the trade direction.
	Side        string  `json:"side"`
	Quantity    float64 `json:"quantity"`
	EntryPrice  float64 `json:"entry_price"`
	ExitPrice   float64 `json:"exit_price"`
	GrossPnL    float64 `json:"gross_pnl"`
	Costs       float64 `json:"costs"`
	PnL         float64 `json:"pnl"`
	GrossBps    float64 `json:"gross_bps"` // on the instrument traded (premium for an option)
	NetBps      float64 `json:"net_bps"`
	IndexBps    float64 `json:"index_bps"` // NaN-free: HasIndex says whether it is real
	HasIndex    bool    `json:"has_index"`
	IndexEntry  float64 `json:"index_entry"`
	IndexExit   float64 `json:"index_exit"`
	DTE         int     `json:"dte"`
	HasDTE      bool    `json:"has_dte"`
	ExitReason  string  `json:"exit_reason"` // normalised bucket
	ExitDetail  string  `json:"exit_detail"` // as the broker reported it
	EntryReason string  `json:"entry_reason"`
	HoldMin     float64 `json:"hold_min"`
}

// groupStats is one row of the breakdown tables.
type groupStats struct {
	Group        string  `json:"group"`
	N            int     `json:"n"`
	NetBps       float64 `json:"net_bps"`
	GrossBps     float64 `json:"gross_bps"`
	IndexBps     float64 `json:"index_bps"`
	IndexN       int     `json:"index_n"`
	T            float64 `json:"t"`
	WinRate      float64 `json:"win_rate"`
	ProfitFactor float64 `json:"profit_factor"`
	Sharpe       float64 `json:"sharpe"` // per-trade: mean / sd of net bps
	AvgWinBps    float64 `json:"avg_win_bps"`
	AvgLossBps   float64 `json:"avg_loss_bps"`
	NetRupees    float64 `json:"net_rupees"`
	CostsRupees  float64 `json:"costs_rupees"`
	MaxDDRupees  float64 `json:"max_dd_rupees"`
	MedianHold   float64 `json:"median_hold_min"`
}

// reference is a backtest figure to read a live run against.
type reference struct {
	Label    string  `json:"label"`
	N        int     `json:"n"`
	NetBps   float64 `json:"net_bps"`
	IndexBps float64 `json:"index_bps"`
	T        float64 `json:"t"`
	WinRate  float64 `json:"win_rate"`
	PF       float64 `json:"profit_factor"`
	Note     string  `json:"note"`
}

// references are the recorded backtest results for the shipped configuration
// of each strategy — the numbers in CLAUDE.md, restated here so the dashboard
// can show what the run is being compared against. Update them when the
// shipped config changes; they are a transcription, not a computation.
var references = map[string][]reference{
	"emacross": {
		{Label: "Backtest index leg, both indices", N: 1960, IndexBps: 2.53, T: 3.71, WinRate: 44.5, PF: 1.28,
			Note: "733 sessions 2023-09..2026-09, gross, cross exit off / SL 3 / trail 3 / from 10:00"},
		{Label: "Backtest NIFTY weekly, 0.80δ, 10 lots, 20 ticks, DTE≥1", N: 499, NetBps: 150.8, IndexBps: 4.26, T: 1.64, PF: 1.20,
			Note: "2024-10..2026-09 on real premium candles; IS +32 / OOS +259"},
		{Label: "Backtest SENSEX weekly, same terms", N: 492, NetBps: 233.1, IndexBps: 3.03, T: 2.81, PF: 1.36,
			Note: "IS +188 / OOS +271"},
		{Label: "Exit mix: stop/trail", N: 1290, IndexBps: -6.73, Note: "66% of exits"},
		{Label: "Exit mix: EOD square-off", N: 670, IndexBps: 20.37, Note: "34% of exits; all of the money"},
	},
	"donchian": {
		{Label: "Backtest index leg, both indices", N: 1490, IndexBps: 4.22, T: 4.86, WinRate: 43.6, PF: 1.43,
			Note: "lookback 30 / ATR 9 / SL 3 / trail 3 / ADX 15-14; OOS +1.76"},
	},
}

// handleStrategy serves the strategy monitor: the signal funnel, every
// enriched trade, and the breakdowns. Query param: days (default 90, 0 = all).
func (s *Server) handleStrategy(w http.ResponseWriter, r *http.Request) {
	if s.engine.DB == nil {
		http.Error(w, "database not available", http.StatusServiceUnavailable)
		return
	}
	days := 90
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d >= 0 {
		days = d
	}
	if days == 0 {
		days = 3650
	}
	since := time.Now().AddDate(0, 0, -days)

	trades, err := s.engine.DB.GetTradeHistory(since, s.PaperMode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	funnel, err := s.engine.DB.GetSignalFunnel(since, s.PaperMode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rows := make([]tradeRow, 0, len(trades))
	for _, t := range trades {
		rows = append(rows, enrich(t))
	}

	strategyName := s.engine.Strategy.Name()
	out := map[string]interface{}{
		"strategy":   strategyName,
		"paper_mode": s.PaperMode,
		"since":      since,
		"funnel":     funnelSummary(funnel),
		"overall":    statsFor("ALL", rows),
		"by_underlying": groupBy(rows, func(r tradeRow) string {
			if r.Underlying != "" {
				return r.Underlying
			}
			return r.Symbol
		}),
		"by_side":   groupBy(rows, func(r tradeRow) string { return r.Side }),
		"by_exit":   groupBy(rows, func(r tradeRow) string { return r.ExitReason }),
		"by_dte":    groupBy(rows, dteBucket),
		"by_week":   groupBy(rows, func(r tradeRow) string { y, wk := r.ExitTime.ISOWeek(); return strconv.Itoa(y) + "-W" + pad2(wk) }),
		"trades":    newestFirst(rows, 200),
		"reference": references[strategyName],
		"open_legs": s.openLegs(),
	}
	json.NewEncoder(w).Encode(out)
}

// openLegs asks the strategy for the option legs it is holding an index stop
// for, and marks them against the underlying's last price.
func (s *Server) openLegs() []map[string]interface{} {
	reporter, ok := s.engine.Strategy.(core.LegReporter)
	if !ok {
		return nil
	}
	var out []map[string]interface{}
	for _, leg := range reporter.OpenLegs() {
		row := map[string]interface{}{
			"symbol":      leg.Symbol,
			"underlying":  leg.Underlying,
			"side":        leg.Side,
			"index_entry": leg.IndexEntry,
			"index_stop":  leg.IndexStop,
			"index_best":  leg.IndexBest,
		}
		if last, err := s.engine.Broker.GetQuote(leg.Underlying); err == nil && last.IsPositive() {
			row["index_last"] = last
			move := last.Sub(leg.IndexEntry).Div(leg.IndexEntry).Mul(decimal.NewFromInt(10000))
			room := last.Sub(leg.IndexStop).Div(leg.IndexEntry).Mul(decimal.NewFromInt(10000))
			if leg.Side == "SHORT" {
				move, room = move.Neg(), room.Neg()
			}
			row["index_move_bps"] = move.Round(1)
			row["room_to_stop_bps"] = room.Round(1)
		}
		out = append(out, row)
	}
	return out
}

func enrich(t models.Trade) tradeRow {
	qty, _ := t.Quantity.Float64()
	entry, _ := t.EntryPrice.Float64()
	exit, _ := t.ExitPrice.Float64()
	gross, _ := t.GrossPnL.Float64()
	costs, _ := t.Costs.Float64()
	net, _ := t.PnL.Float64()
	if t.GrossPnL.IsZero() && !t.PnL.IsZero() && t.Costs.IsZero() {
		gross = net // rows written before costs were tracked
	}

	row := tradeRow{
		ExitTime: t.ExitTime, EntryTime: t.EntryTime, Symbol: t.Symbol, Strategy: t.Strategy,
		Quantity: qty, EntryPrice: entry, ExitPrice: exit, GrossPnL: gross, Costs: costs, PnL: net,
		ExitDetail: t.ExitReason, ExitReason: exitBucket(t.ExitReason),
		HoldMin: t.ExitTime.Sub(t.EntryTime).Minutes(),
	}
	if notional := entry * qty; notional > 0 {
		row.GrossBps = gross / notional * 1e4
		row.NetBps = net / notional * 1e4
	}

	meta := t.Metadata
	row.Underlying = meta["Underlying"]
	row.EntryReason = meta["Reason"]
	row.Side = t.Direction
	if side := meta["IndexSide"]; side != "" {
		row.Side = "LONG"
		if strings.EqualFold(side, "SELL") {
			row.Side = "SHORT"
		}
	}
	if ie, err1 := strconv.ParseFloat(meta["IndexEntry"], 64); err1 == nil && ie > 0 {
		if ix, err2 := strconv.ParseFloat(meta["UnderlyingExit"], 64); err2 == nil && ix > 0 {
			row.IndexEntry, row.IndexExit, row.HasIndex = ie, ix, true
			row.IndexBps = (ix - ie) / ie * 1e4
			if row.Side == "SHORT" {
				row.IndexBps = -row.IndexBps
			}
		}
	}
	if d, err := strconv.Atoi(meta["DaysToExpiry"]); err == nil {
		row.DTE, row.HasDTE = d, true
	}
	return row
}

// exitBucket folds the broker's free-text reason into the buckets the
// backtest reports.
func exitBucket(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case r == "":
		return "unknown"
	case strings.Contains(r, "squareoff") || strings.Contains(r, "square-off") || strings.Contains(r, "eod"):
		return "EOD square-off"
	case strings.Contains(r, "index stop"):
		return "index stop/trail"
	case r == "sl-hit" || strings.Contains(r, "stop"):
		return "stop/trail"
	case r == "target-hit" || strings.Contains(r, "target"):
		return "target"
	case strings.Contains(r, "cross"):
		return "EMA cross"
	case strings.Contains(r, "opposite"):
		return "opposite break"
	case strings.Contains(r, "time"):
		return "time stop"
	}
	return reason
}

func dteBucket(r tradeRow) string {
	if !r.HasDTE {
		return "n/a"
	}
	if r.DTE >= 4 {
		return "DTE 4+"
	}
	return "DTE " + strconv.Itoa(r.DTE)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func groupBy(rows []tradeRow, key func(tradeRow) string) []groupStats {
	groups := map[string][]tradeRow{}
	var order []string
	for _, r := range rows {
		k := key(r)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Strings(order)
	out := make([]groupStats, 0, len(order))
	for _, k := range order {
		out = append(out, statsFor(k, groups[k]))
	}
	return out
}

// statsFor is the per-trade summary of a set of rows, in net bps of the
// instrument traded, with the index-move mean over the rows that carry one.
func statsFor(name string, rows []tradeRow) groupStats {
	g := groupStats{Group: name, N: len(rows)}
	if len(rows) == 0 {
		return g
	}
	var sum, sumSq, grossSum, winSum, lossSum float64
	var wins, losses int
	var idxSum float64
	var holds []float64
	cum, peak := 0.0, 0.0
	for _, r := range rows {
		sum += r.NetBps
		sumSq += r.NetBps * r.NetBps
		grossSum += r.GrossBps
		if r.NetBps > 0 {
			wins++
			winSum += r.NetBps
		} else {
			losses++
			lossSum += r.NetBps
		}
		if r.HasIndex {
			idxSum += r.IndexBps
			g.IndexN++
		}
		g.NetRupees += r.PnL
		g.CostsRupees += r.Costs
		holds = append(holds, r.HoldMin)
		cum += r.PnL
		if cum > peak {
			peak = cum
		}
		if dd := peak - cum; dd > g.MaxDDRupees {
			g.MaxDDRupees = dd
		}
	}
	n := float64(len(rows))
	g.NetBps = sum / n
	g.GrossBps = grossSum / n
	if g.IndexN > 0 {
		g.IndexBps = idxSum / float64(g.IndexN)
	}
	g.WinRate = float64(wins) / n * 100
	if wins > 0 {
		g.AvgWinBps = winSum / float64(wins)
	}
	if losses > 0 {
		g.AvgLossBps = lossSum / float64(losses)
	}
	if lossSum < 0 {
		g.ProfitFactor = winSum / -lossSum
	} else if winSum > 0 {
		g.ProfitFactor = math.Inf(1)
	}
	if len(rows) > 1 {
		variance := (sumSq - n*g.NetBps*g.NetBps) / (n - 1)
		if variance > 0 {
			sd := math.Sqrt(variance)
			g.Sharpe = g.NetBps / sd
			g.T = g.NetBps / (sd / math.Sqrt(n))
		}
	}
	sort.Float64s(holds)
	g.MedianHold = holds[len(holds)/2]
	// JSON cannot carry Inf.
	if math.IsInf(g.ProfitFactor, 0) {
		g.ProfitFactor = 999
	}
	return g
}

// funnelSummary folds the signal counts into the headline the dashboard
// shows: how many views became orders, and where the rest went.
func funnelSummary(counts []db.SignalCount) map[string]interface{} {
	byOutcome := map[string]int{}
	total := 0
	var detail []db.SignalCount
	for _, c := range counts {
		byOutcome[c.Outcome] += c.Count
		total += c.Count
		if c.Outcome != db.SignalPlaced {
			detail = append(detail, c)
		}
	}
	return map[string]interface{}{
		"total":      total,
		"placed":     byOutcome[db.SignalPlaced],
		"by_outcome": byOutcome,
		"detail":     detail,
	}
}

func newestFirst(rows []tradeRow, limit int) []tradeRow {
	out := make([]tradeRow, 0, limit)
	for i := len(rows) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, rows[i])
	}
	return out
}
