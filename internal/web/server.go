// Package web provides a web api for monitoring the portfolio.
package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/nseutils"
	"zerobha/pkg/statistics"
	webui "zerobha/web"

	"github.com/shopspring/decimal"
)

type Server struct {
	engine    *core.Engine
	port      int
	PaperMode bool
	srv       *http.Server
	done      chan struct{}
}

func NewServer(engine *core.Engine, port int, paperMode bool) *Server {
	return &Server{
		engine:    engine,
		port:      port,
		PaperMode: paperMode,
		done:      make(chan struct{}),
	}
}

func (s *Server) Start() {
	mux := http.NewServeMux()

	fileServer := http.FileServer(http.FS(webui.Files))
	mux.Handle("/", fileServer)

	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/positions", s.handlePositions)
	mux.HandleFunc("/api/orders", s.handleOrders)
	mux.HandleFunc("/api/trades", s.handleTrades)
	mux.HandleFunc("/api/performance", s.handlePerformance)
	mux.HandleFunc("/api/intraday", s.handleIntraday)

	addr := ":" + strconv.Itoa(s.port)
	log.Printf("Starting Web Dashboard at http://localhost%s", addr)

	// Background recorder: equity snapshots + trade reconciliation
	go s.trackLoop()

	s.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("Web Server Error: %v", err)
	}
}

func (s *Server) Stop(ctx context.Context) error {
	close(s.done)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	data, err := s.computeSummary()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"balance":        data.Balance,
		"pnl":            data.TotalPnL,
		"realized_pnl":   data.RealizedPnL,
		"unrealized_pnl": data.UnrealizedPnL,
		"open_positions": data.OpenPositions,
		"paper_mode":     s.PaperMode,
	})
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	positions, err := s.engine.Broker.GetPositions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Enrich positions with Strategy info and Live Prices
	for i := range positions {
		// 1. Get Strategy
		if s.engine.DB != nil {
			strat, err := s.engine.DB.GetOrderStrategy(positions[i].Tradingsymbol)
			if err == nil {
				positions[i].Strategy = strat
			}
		}

		// 2. Zerodha's GetPositions API can return slightly delayed data;
		// fetch a fresh quote and recalculate PnL for open positions.
		quote, err := s.engine.Broker.GetQuote(positions[i].Tradingsymbol)
		if err == nil {
			positions[i].LastPrice = quote
			qty := decimal.NewFromInt(int64(positions[i].NetQuantity))

			// For closed positions (qty == 0) we trust the broker's realized PnL.
			if !qty.IsZero() {
				positions[i].PnL = quote.Sub(positions[i].AveragePrice).Mul(qty)
			}
		}
	}

	json.NewEncoder(w).Encode(positions)
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.engine.Broker.GetOpenOrders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(orders)
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	trades, err := s.engine.Broker.GetTrades()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(trades)
}

// strategyStats is the per-strategy (and overall) performance row returned
// by /api/performance. MaxDrawdown is in rupees (peak-to-trough of the
// cumulative PnL curve), not a percentage, since per-strategy capital
// allocation is not tracked.
type strategyStats struct {
	Strategy     string  `json:"strategy"`
	Trades       int     `json:"trades"`
	WinRate      float64 `json:"win_rate"`
	NetPnL       float64 `json:"net_pnl"`
	GrossProfit  float64 `json:"gross_profit"`
	GrossLoss    float64 `json:"gross_loss"`
	ProfitFactor float64 `json:"profit_factor"`
	AvgWin       float64 `json:"avg_win"`
	AvgLoss      float64 `json:"avg_loss"`
	Sharpe       float64 `json:"sharpe"`
	MaxDrawdown  float64 `json:"max_drawdown"`
}

type curvePoint struct {
	Time   time.Time `json:"time"`
	CumPnL float64   `json:"cum_pnl"`
}

type dailyPnL struct {
	Date   string  `json:"date"`
	PnL    float64 `json:"pnl"`
	Trades int     `json:"trades"`
}

// handlePerformance serves historical performance derived from the
// persisted trades table: overall stats, per-strategy comparison, the
// cumulative realized PnL curve, daily PnL and recent trade history.
// Query param: days (default 30, 0 = all time).
func (s *Server) handlePerformance(w http.ResponseWriter, r *http.Request) {
	if s.engine.DB == nil {
		http.Error(w, "database not available", http.StatusServiceUnavailable)
		return
	}

	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d >= 0 {
		days = d
	}
	if days == 0 {
		days = 3650 // "all time"
	}
	since := time.Now().AddDate(0, 0, -days)

	trades, err := s.engine.DB.GetTradeHistory(since, s.PaperMode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Per-strategy grouping (preserving chronological order within groups)
	byStrategy := make(map[string][]models.Trade)
	for _, t := range trades {
		name := t.Strategy
		if name == "" {
			name = "UNKNOWN"
		}
		byStrategy[name] = append(byStrategy[name], t)
	}

	strategies := make([]strategyStats, 0, len(byStrategy))
	for name, group := range byStrategy {
		strategies = append(strategies, buildStats(name, group))
	}

	// Cumulative realized PnL curve and daily PnL buckets
	curve := make([]curvePoint, 0, len(trades))
	dailyMap := make(map[string]*dailyPnL)
	var dailyOrder []string
	cum := decimal.Zero
	for _, t := range trades {
		cum = cum.Add(t.PnL)
		cumF, _ := cum.Float64()
		curve = append(curve, curvePoint{Time: t.ExitTime, CumPnL: cumF})

		day := t.ExitTime.Format("2006-01-02")
		if _, ok := dailyMap[day]; !ok {
			dailyMap[day] = &dailyPnL{Date: day}
			dailyOrder = append(dailyOrder, day)
		}
		pnlF, _ := t.PnL.Float64()
		dailyMap[day].PnL += pnlF
		dailyMap[day].Trades++
	}
	daily := make([]dailyPnL, 0, len(dailyOrder))
	for _, day := range dailyOrder {
		daily = append(daily, *dailyMap[day])
	}

	// Recent trades, newest first, capped for the UI table
	const maxRecent = 100
	recent := make([]models.Trade, 0, maxRecent)
	for i := len(trades) - 1; i >= 0 && len(recent) < maxRecent; i-- {
		recent = append(recent, trades[i])
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"overall":    buildStats("OVERALL", trades),
		"strategies": strategies,
		"curve":      curve,
		"daily":      daily,
		"trades":     recent,
	})
}

// buildStats converts a chronological trade list into a strategyStats row,
// reusing the statistics analyzer for the core metrics.
func buildStats(name string, trades []models.Trade) strategyStats {
	// Capital only affects Analyze's percentage drawdown, which we discard
	// in favour of a rupee drawdown computed below.
	perf := statistics.Analyze(trades, decimal.NewFromInt(100000))

	row := strategyStats{
		Strategy:     name,
		Trades:       perf.TotalTrades,
		WinRate:      perf.WinRate,
		ProfitFactor: perf.ProfitFactor,
		Sharpe:       perf.Sharpe,
	}
	row.NetPnL, _ = perf.NetProfit.Float64()
	row.GrossProfit, _ = perf.GrossProfit.Float64()
	row.GrossLoss, _ = perf.GrossLoss.Float64()
	row.AvgWin, _ = perf.AverageWin.Float64()
	row.AvgLoss, _ = perf.AverageLoss.Float64()

	// Max drawdown in rupees: deepest peak-to-trough of cumulative PnL.
	cum, peak, maxDD := decimal.Zero, decimal.Zero, decimal.Zero
	for _, t := range trades {
		cum = cum.Add(t.PnL)
		if cum.GreaterThan(peak) {
			peak = cum
		}
		if dd := peak.Sub(cum); dd.GreaterThan(maxDD) {
			maxDD = dd
		}
	}
	row.MaxDrawdown, _ = maxDD.Float64()

	return row
}

// handleIntraday serves today's equity snapshots (PnL over the trading day).
func (s *Server) handleIntraday(w http.ResponseWriter, r *http.Request) {
	if s.engine.DB == nil {
		http.Error(w, "database not available", http.StatusServiceUnavailable)
		return
	}

	points, err := s.engine.DB.GetEquitySnapshots(nseutils.MarketOpenTime(time.Now()), s.PaperMode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(points)
}
