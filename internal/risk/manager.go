// Package risk implements risk management logic for the trading engine.
package risk

import (
	"errors"
	"fmt"
	"log"
	"time"
	"zerobha/internal/models"
	"zerobha/pkg/db"

	"github.com/shopspring/decimal"
)

type Manager struct {
	MaxDailyLoss      decimal.Decimal
	MaxTradesPerDay   int
	MaxTradesPerStock int

	store *db.Store

	// State (In-memory for now)
	currentPnL     decimal.Decimal
	tradesToday    int
	tradesPerStock map[string]int
}

type RiskState struct {
	Date           string          `json:"date"`
	TradesToday    int             `json:"trades_today"`
	CurrentPnL     decimal.Decimal `json:"current_pnl"`
	TradesPerStock map[string]int  `json:"trades_per_stock"`
}

func NewManager(store *db.Store, maxLoss decimal.Decimal, maxTrades int, maxTradesPerStock int) *Manager {
	rm := &Manager{
		MaxDailyLoss:      maxLoss,
		MaxTradesPerDay:   maxTrades,
		MaxTradesPerStock: maxTradesPerStock,
		store:             store,
		currentPnL:        decimal.Zero,
		tradesToday:       0,
		tradesPerStock:    make(map[string]int),
	}
	rm.LoadState()
	return rm
}

// SetDayPnL tells the manager where the day stands. The engine reads it
// from the broker's position book before every signal, because nothing else
// can: exits happen at resting stops, in ClosePosition and at the square-off,
// none of which pass through the engine's order path. UpdateTradeLog used to
// be the only way PnL reached this switch, and the engine only ever called it
// with zero — so until 2026-09-12 the daily-loss limit could not trip, live or
// paper.
func (rm *Manager) SetDayPnL(pnl decimal.Decimal) {
	rm.currentPnL = pnl
}

// Evaluate decides if a signal is allowed to pass.
func (rm *Manager) Evaluate(signal *models.Signal) error {
	// 1. Check Max Trades (Total)
	if rm.tradesToday >= rm.MaxTradesPerDay {
		return errors.New("risk rejection: max daily trades reached")
	}

	// 2. Check Max Trades (Per Stock)
	if rm.MaxTradesPerStock > 0 {
		if count, ok := rm.tradesPerStock[signal.Symbol]; ok && count >= rm.MaxTradesPerStock {
			return errors.New("risk rejection: max trades per stock reached")
		}
	}

	// 3. Check Daily Loss Limit (Kill Switch)
	// If current PnL is worse than -MaxDailyLoss (e.g., -5000 < -2000)
	if rm.MaxDailyLoss.IsPositive() && rm.currentPnL.LessThan(rm.MaxDailyLoss.Neg()) {
		return fmt.Errorf("risk rejection: daily loss limit hit (day PnL Rs %s, limit Rs %s)",
			rm.currentPnL.StringFixed(0), rm.MaxDailyLoss.StringFixed(0))
	}

	return nil
}

// UpdateTradeLog is called after an entry order is placed. It counts trades;
// PnL comes from SetDayPnL, not from here.
func (rm *Manager) UpdateTradeLog(symbol string) {
	rm.tradesToday++
	rm.tradesPerStock[symbol]++
	rm.SaveState()
}

func (rm *Manager) CurrentPnL() int64 {
	return rm.currentPnL.IntPart()
}

func (rm *Manager) ResetDaily() {
	rm.tradesToday = 0
	rm.currentPnL = decimal.Zero
	rm.tradesPerStock = make(map[string]int)
	rm.SaveState()
}

func (rm *Manager) SaveState() {
	if rm.store == nil {
		return
	}
	state := RiskState{
		Date:           time.Now().Format("2006-01-02"),
		TradesToday:    rm.tradesToday,
		CurrentPnL:     rm.currentPnL,
		TradesPerStock: rm.tradesPerStock,
	}
	if err := rm.store.SetState("risk_state", state); err != nil {
		log.Printf("ERROR: Failed to save risk state: %v", err)
	}
}

func (rm *Manager) LoadState() {
	if rm.store == nil {
		return
	}
	var state RiskState
	if err := rm.store.GetState("risk_state", &state); err != nil {
		log.Printf("WARNING: Failed to load risk state: %v", err)
		return
	}

	// Only load if it matches today
	today := time.Now().Format("2006-01-02")
	if state.Date == today {
		log.Printf("Restoring Risk State for %s: Trades=%d, PnL=%s", today, state.TradesToday, state.CurrentPnL)
		rm.tradesToday = state.TradesToday
		rm.currentPnL = state.CurrentPnL
		rm.tradesPerStock = state.TradesPerStock
		if rm.tradesPerStock == nil {
			rm.tradesPerStock = make(map[string]int)
		}
	} else {
		log.Printf("Found stale risk state from %s. Starting fresh for %s.", state.Date, today)
	}
}
