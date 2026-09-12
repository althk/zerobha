package web

import (
	"fmt"
	"log"
	"sort"
	"strings"
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

	// 15s rather than a minute: every source here is in-memory or a local
	// SQLite read, and a fill should be on the dashboard before the next bar.
	ticker := time.NewTicker(15 * time.Second)
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
		IsPaper:       s.PaperMode,
	}
	if err := s.engine.DB.SaveEquitySnapshot(point); err != nil {
		log.Printf("Tracker: failed to save equity snapshot: %v", err)
	}
}

// lot is an open entry fill awaiting an opposite-side exit.
type lot struct {
	side    models.SignalType
	qty     decimal.Decimal
	price   decimal.Decimal
	time    time.Time
	isPaper bool
	id      string
	// costs is the entry fill's charges, apportioned to the lot by quantity
	// as it is consumed; meta is the entry signal's metadata.
	costs decimal.Decimal
	meta  map[string]string
}

// fillMeta returns a fill's metadata, falling back to what SaveOrder kept
// for brokers whose trade book does not carry it (the live adapter's).
func (s *Server) fillMeta(f models.Order) map[string]string {
	if len(f.Metadata) > 0 {
		return f.Metadata
	}
	if s.engine.DB == nil || f.ID == "" {
		return nil
	}
	meta, _ := s.engine.DB.GetOrderMetadata(f.ID)
	return meta
}

// fillCosts reads the charges a broker stamped on a fill (Costs plus any
// modelled spread), zero when it stamped none.
func fillCosts(meta map[string]string) decimal.Decimal {
	total := decimal.Zero
	for _, key := range []string{"Costs", "SpreadCost"} {
		if v, err := decimal.NewFromString(meta[key]); err == nil {
			total = total.Add(v)
		}
	}
	return total
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

		isPaper := f.IsPaper || s.PaperMode || strings.HasPrefix(f.ID, db.PaperOrderPrefix)
		lots := openLots[f.Symbol]
		meta := s.fillMeta(f)
		costs := fillCosts(meta)

		// Same direction as existing lots (or flat): this fill opens/adds.
		if len(lots) == 0 || lots[0].side == f.Side {
			openLots[f.Symbol] = append(lots, lot{side: f.Side, qty: f.Quantity, price: f.Price, time: f.Timestamp,
				isPaper: isPaper, id: f.ID, costs: costs, meta: meta})
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

			strategy := entry.meta["Strategy"]
			if strategy == "" {
				var ok bool
				if strategy, ok = strategyCache[f.Symbol]; !ok {
					strategy, _ = s.engine.DB.GetOrderStrategy(f.Symbol)
					strategyCache[f.Symbol] = strategy
				}
			}

			// Charges follow the quantity: a lot consumed in parts carries
			// its entry charge in proportion, and the exit's charge is split
			// across the lots it closes the same way.
			share := matched.Div(f.Quantity)
			entryShare := entry.costs.Mul(matched.Div(entry.qty))
			tradeCosts := entryShare.Add(costs.Mul(share)).Round(2)

			tradeMeta := make(map[string]string, len(entry.meta)+2)
			for k, v := range entry.meta {
				tradeMeta[k] = v
			}
			if v := meta["UnderlyingLast"]; v != "" {
				tradeMeta["UnderlyingExit"] = v
			}
			delete(tradeMeta, "Costs")
			delete(tradeMeta, "SpreadCost")
			delete(tradeMeta, "PaperPnL")

			trade := models.Trade{
				Symbol:       f.Symbol,
				Strategy:     strategy,
				Direction:    direction,
				Quantity:     matched,
				EntryPrice:   entry.price,
				ExitPrice:    f.Price,
				GrossPnL:     pnl,
				Costs:        tradeCosts,
				PnL:          pnl.Sub(tradeCosts),
				EntryTime:    entry.time,
				ExitTime:     f.Timestamp,
				ExitReason:   meta["Reason"],
				EntryOrderID: entry.id,
				ExitOrderID:  f.ID,
				IsPaper:      entry.isPaper || isPaper,
				Metadata:     tradeMeta,
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
				lots[0].costs = entry.costs.Sub(entryShare)
			}
		}

		// Any remainder reverses the position into a new lot.
		if remaining.GreaterThan(decimal.Zero) {
			lots = append(lots, lot{side: f.Side, qty: remaining, price: f.Price, time: f.Timestamp,
				isPaper: isPaper, id: f.ID, costs: costs.Mul(remaining.Div(f.Quantity)), meta: meta})
		}
		openLots[f.Symbol] = lots
	}
}
