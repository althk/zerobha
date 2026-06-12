package web

import (
	"fmt"
	"log"
	"sort"
	"time"
	"zerobha/internal/models"
	"zerobha/pkg/db"
	"zerobha/pkg/nseutils"

	"github.com/shopspring/decimal"
)

// trackLoop periodically persists equity snapshots and reconciles broker
// fills into round-trip trades so the dashboard has historical performance
// data. It runs until the server's done channel closes and only samples
// during market hours.
func (s *Server) trackLoop() {
	// Run once immediately so the dashboard has data shortly after startup.
	s.recordSnapshot()
	s.reconcileTrades()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if !nseutils.IsMarketHours(time.Now()) {
				continue
			}
			s.recordSnapshot()
			s.reconcileTrades()
		}
	}
}

// summaryData is the shared account snapshot used by both the /api/summary
// handler and the equity snapshot recorder.
type summaryData struct {
	Balance       float64
	RealizedPnL   float64
	UnrealizedPnL float64
	TotalPnL      float64
	OpenPositions int
}

func (s *Server) computeSummary() (summaryData, error) {
	var data summaryData

	balance, err := s.engine.Broker.GetBalance()
	if err != nil {
		return data, err
	}
	data.Balance, _ = balance.Float64()

	positions, err := s.engine.Broker.GetPositions()
	if err != nil {
		return data, err
	}

	for _, p := range positions {
		pnl, _ := p.PnL.Float64()

		if p.NetQuantity != 0 {
			// Broker positions carry stale PnL; refresh with a live quote.
			quote, err := s.engine.Broker.GetQuote(p.Tradingsymbol)
			if err == nil {
				qty := decimal.NewFromInt(int64(p.NetQuantity))
				pnl, _ = quote.Sub(p.AveragePrice).Mul(qty).Float64()
			}
			data.UnrealizedPnL += pnl
			data.OpenPositions++
		} else {
			data.RealizedPnL += pnl
		}
		data.TotalPnL += pnl
	}

	return data, nil
}

func (s *Server) recordSnapshot() {
	if s.engine.DB == nil {
		return
	}

	data, err := s.computeSummary()
	if err != nil {
		log.Printf("Tracker: failed to compute equity snapshot: %v", err)
		return
	}

	point := db.EquityPoint{
		Timestamp:     time.Now(),
		Balance:       data.Balance,
		RealizedPnL:   data.RealizedPnL,
		UnrealizedPnL: data.UnrealizedPnL,
		OpenPositions: data.OpenPositions,
	}
	if err := s.engine.DB.SaveEquitySnapshot(point); err != nil {
		log.Printf("Tracker: failed to save equity snapshot: %v", err)
	}
}

// lot is an open entry fill awaiting an opposite-side exit.
type lot struct {
	side  models.SignalType
	qty   decimal.Decimal
	price decimal.Decimal
	time  time.Time
}

// reconcileTrades FIFO-pairs the day's broker fills into completed
// round-trip trades and persists them. SaveTrade keys rows by
// exit-order-id + lot index, so re-processing the same fills every
// minute is idempotent.
func (s *Server) reconcileTrades() {
	if s.engine.DB == nil {
		return
	}

	fills, err := s.engine.Broker.GetTrades()
	if err != nil {
		log.Printf("Tracker: failed to fetch fills for reconciliation: %v", err)
		return
	}

	sort.Slice(fills, func(i, j int) bool {
		return fills[i].Timestamp.Before(fills[j].Timestamp)
	})

	strategyCache := make(map[string]string)
	openLots := make(map[string][]lot)

	for _, f := range fills {
		if f.Quantity.LessThanOrEqual(decimal.Zero) {
			continue
		}

		lots := openLots[f.Symbol]

		// Same direction as existing lots (or flat): this fill opens/adds.
		if len(lots) == 0 || lots[0].side == f.Side {
			openLots[f.Symbol] = append(lots, lot{side: f.Side, qty: f.Quantity, price: f.Price, time: f.Timestamp})
			continue
		}

		// Opposite direction: close lots FIFO.
		remaining := f.Quantity
		lotIndex := 0
		for remaining.GreaterThan(decimal.Zero) && len(lots) > 0 {
			entry := lots[0]
			matched := decimal.Min(entry.qty, remaining)

			direction := "LONG"
			pnl := f.Price.Sub(entry.price).Mul(matched)
			if entry.side == models.SellSignal {
				direction = "SHORT"
				pnl = entry.price.Sub(f.Price).Mul(matched)
			}

			strategy, ok := strategyCache[f.Symbol]
			if !ok {
				strategy, _ = s.engine.DB.GetOrderStrategy(f.Symbol)
				strategyCache[f.Symbol] = strategy
			}

			trade := models.Trade{
				Symbol:     f.Symbol,
				Strategy:   strategy,
				Direction:  direction,
				Quantity:   matched,
				EntryPrice: entry.price,
				ExitPrice:  f.Price,
				PnL:        pnl,
				EntryTime:  entry.time,
				ExitTime:   f.Timestamp,
			}

			key := fmt.Sprintf("%s:%d", f.ID, lotIndex)
			if err := s.engine.DB.SaveTrade(key, trade); err != nil {
				log.Printf("Tracker: failed to save trade %s: %v", key, err)
			}

			lotIndex++
			remaining = remaining.Sub(matched)
			if entry.qty.LessThanOrEqual(matched) {
				lots = lots[1:]
			} else {
				lots[0].qty = entry.qty.Sub(matched)
			}
		}

		// Any remainder reverses the position into a new lot.
		if remaining.GreaterThan(decimal.Zero) {
			lots = append(lots, lot{side: f.Side, qty: remaining, price: f.Price, time: f.Timestamp})
		}
		openLots[f.Symbol] = lots
	}
}
