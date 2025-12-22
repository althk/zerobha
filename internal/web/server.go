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

	"github.com/shopspring/decimal"
)

type Server struct {
	engine *core.Engine
	port   int
	srv    *http.Server
}

func NewServer(engine *core.Engine, port int) *Server {
	return &Server{
		engine: engine,
		port:   port,
	}
}

func (s *Server) Start() {
	mux := http.NewServeMux()

	fileServer := http.FileServer(http.Dir("web"))
	mux.Handle("/", fileServer)

	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/positions", s.handlePositions)
	mux.HandleFunc("/api/orders", s.handleOrders)
	mux.HandleFunc("/api/trades", s.handleTrades)

	addr := ":" + strconv.Itoa(s.port)
	log.Printf("Starting Web Dashboard at http://localhost%s", addr)

	s.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("Web Server Error: %v", err)
	}
}

func (s *Server) Stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	balance, _ := s.engine.Broker.GetBalance()

	positions, _ := s.engine.Broker.GetPositions()
	var totalPnL float64
	var realizedPnL float64
	var unrealizedPnL float64

	for i, p := range positions {
		pnl, _ := p.PnL.Float64()

		// If getting live positions to calc PnL properly, we should probably reuse logic from handlePositions?
		// For summary, let's keep it simple.
		// If Qty != 0 => Unrealized (mostly).
		// If Qty == 0 => Realized.

		// Broker.GetPositions returns stale PnL for Open positions unless we refresh quotes.
		// To be accurate, we should refresh quotes here too or share the logic.
		if p.NetQuantity != 0 {
			quote, err := s.engine.Broker.GetQuote(p.Tradingsymbol)
			if err == nil {
				// Recalc PnL
				qty := decimal.NewFromInt(int64(p.NetQuantity))
				livePnL := quote.Sub(p.AveragePrice).Mul(qty)
				pnl, _ = livePnL.Float64()
				positions[i].PnL = livePnL // Update local var for consistency if needed, but not returned
			}
			unrealizedPnL += pnl
		} else {
			realizedPnL += pnl
		}
		totalPnL += pnl
	}

	balFloat, _ := balance.Float64()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"balance":        balFloat,
		"pnl":            totalPnL,
		"realized_pnl":   realizedPnL,
		"unrealized_pnl": unrealizedPnL,
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

		// 2. Get Live Price to fix stale data
		// Note: Zerodha's GetPositions API might return slightly delayed data.
		// We fetch a fresh quote to be sure.
		quote, err := s.engine.Broker.GetQuote(positions[i].Tradingsymbol)
		if err == nil {
			positions[i].LastPrice = quote
			// Recalculate PnL
			// PnL = (SellPrice - BuyPrice) * Qty
			// For Buy Position: (CurrentPrice - AvgPrice) * Qty
			// For Sell Position: (AvgPrice - CurrentPrice) * Qty
			// Actually simpler: (Current - Avg) * NetQty
			// If NetQty is positive (Long), (Current - Avg) * Positive = Profit if Current > Avg
			// If NetQty is negative (Short), (Current - Avg) * Negative = Profit if Current < Avg
			// Wait, (Current - Avg) * Negative. e.g. Sell @ 100, Curr @ 90. (90 - 100) * -1 = -10 * -1 = 10. Correct.

			qty := decimal.NewFromInt(int64(positions[i].NetQuantity))

			// Only recalculate PnL if Position is OPEN (qty != 0)
			// For Closed positions, we trust the Broker's Realized PnL
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
