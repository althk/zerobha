// Package risk implements risk management logic for the trading engine.
package risk

import (
	"errors"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

type Manager struct {
	MaxDailyLoss    decimal.Decimal
	MaxTradesPerDay int

	// State (In-memory for now)
	currentPnL  decimal.Decimal
	tradesToday int
}

func NewManager(maxLoss decimal.Decimal, maxTrades int) *Manager {
	return &Manager{
		MaxDailyLoss:    maxLoss,
		MaxTradesPerDay: maxTrades,
		currentPnL:      decimal.Zero,
		tradesToday:     0,
	}
}

// Evaluate decides if a signal is allowed to pass.
func (rm *Manager) Evaluate(signal *models.Signal) error {
	// 1. Check Max Trades
	if rm.tradesToday >= rm.MaxTradesPerDay {
		return errors.New("risk rejection: max daily trades reached")
	}

	// 2. Check Daily Loss Limit (Kill Switch)
	// If current PnL is worse than -MaxDailyLoss (e.g., -5000 < -2000)
	if rm.currentPnL.LessThan(rm.MaxDailyLoss.Neg()) {
		return errors.New("risk rejection: daily loss limit hit")
	}

	return nil
}

// UpdateTradeLog is called after an order is filled to update state
func (rm *Manager) UpdateTradeLog(pnl decimal.Decimal) {
	rm.tradesToday++
	rm.currentPnL = rm.currentPnL.Add(pnl)
}

func (rm *Manager) CurrentPnL() int64 {
	return rm.currentPnL.IntPart()
}

func (rm *Manager) ResetDaily() {
	rm.tradesToday = 0
	rm.currentPnL = decimal.Zero
}
