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
	// MaxLossPerStockPerDay caps, in rupees, what one underlying may cost in a
	// day across every strategy: its loss so far plus the risk still open on
	// it plus the new trade's risk. Zero = no limit. See StockRiskBudget.
	MaxLossPerStockPerDay decimal.Decimal
	// MaxMonthlyLoss stops new entries once the month's PnL (earlier days'
	// realised plus today's) is below -MaxMonthlyLoss. Zero = no limit.
	MaxMonthlyLoss decimal.Decimal

	store *db.Store

	// State (In-memory for now)
	currentPnL     decimal.Decimal
	tradesToday    int
	tradesPerStock map[string]int
	monthPnL       decimal.Decimal
	// openRisk is the rupee risk each of today's entries put on, keyed by the
	// traded symbol. Persisted, because the trader restarts mid-session and
	// the broker has no record of where a position's stop was at entry.
	openRisk map[string]EntryRisk
}

// EntryRisk is what one entry put at stake, and on which underlying.
type EntryRisk struct {
	Underlying string          `json:"underlying"`
	Risk       decimal.Decimal `json:"risk"`
}

type RiskState struct {
	Date           string               `json:"date"`
	TradesToday    int                  `json:"trades_today"`
	CurrentPnL     decimal.Decimal      `json:"current_pnl"`
	TradesPerStock map[string]int       `json:"trades_per_stock"`
	OpenRisk       map[string]EntryRisk `json:"open_risk,omitempty"`
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
		openRisk:          make(map[string]EntryRisk),
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

	// 4. Monthly Loss Limit
	if rm.MaxMonthlyLoss.IsPositive() && rm.monthPnL.LessThan(rm.MaxMonthlyLoss.Neg()) {
		return fmt.Errorf("risk rejection: monthly loss limit hit (month PnL Rs %s, limit Rs %s)",
			rm.monthPnL.StringFixed(0), rm.MaxMonthlyLoss.StringFixed(0))
	}

	return nil
}

// UpdateTradeLog is called after an entry order is placed. It counts trades;
// PnL comes from SetDayPnL, not from here.
func (rm *Manager) UpdateTradeLog(symbol string) {
	rm.RecordEntry(symbol, symbol, decimal.Zero)
}

// RecordEntry counts a placed entry and remembers the rupee risk it put on
// underlying, so later entries on the same underlying see it while the
// position stays open.
func (rm *Manager) RecordEntry(symbol, underlying string, risk decimal.Decimal) {
	rm.tradesToday++
	rm.tradesPerStock[symbol]++
	if risk.IsPositive() {
		rm.openRisk[symbol] = EntryRisk{Underlying: underlying, Risk: risk}
	} else {
		delete(rm.openRisk, symbol)
	}
	rm.SaveState()
}

func (rm *Manager) CurrentPnL() int64 {
	return rm.currentPnL.IntPart()
}

func (rm *Manager) ResetDaily() {
	rm.tradesToday = 0
	rm.currentPnL = decimal.Zero
	rm.tradesPerStock = make(map[string]int)
	rm.openRisk = make(map[string]EntryRisk)
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
		OpenRisk:       rm.openRisk,
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
		rm.openRisk = state.OpenRisk
		if rm.openRisk == nil {
			rm.openRisk = make(map[string]EntryRisk)
		}
	} else {
		log.Printf("Found stale risk state from %s. Starting fresh for %s.", state.Date, today)
	}
}
