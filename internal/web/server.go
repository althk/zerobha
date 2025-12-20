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
	for _, p := range positions {
		pnl, _ := p.PnL.Float64()
		totalPnL += pnl
	}

	balFloat, _ := balance.Float64()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"balance": balFloat,
		"pnl":     totalPnL,
	})
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	positions, err := s.engine.Broker.GetPositions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
