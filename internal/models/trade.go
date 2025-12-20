package models

import (
	"time"

	"github.com/shopspring/decimal"
)

// Trade represents a completed Buy+Sell cycle
type Trade struct {
	Symbol     string          `json:"symbol"`
	EntryPrice decimal.Decimal `json:"entry_price"`
	ExitPrice  decimal.Decimal `json:"exit_price"`
	Quantity   decimal.Decimal `json:"quantity"`
	Direction  string          `json:"direction"` // LONG or SHORT
	PnL        decimal.Decimal `json:"pnl"`       // Realized PnL
	EntryTime  time.Time       `json:"entry_time"`
	ExitTime   time.Time       `json:"exit_time"`
	ExitReason string          `json:"exit_reason"` // SL-HIT, TARGET-HIT
}
