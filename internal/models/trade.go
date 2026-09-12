package models

import (
	"time"

	"github.com/shopspring/decimal"
)

// Trade represents a completed Buy+Sell cycle
type Trade struct {
	Symbol     string          `json:"symbol"`
	Strategy   string          `json:"strategy"`
	EntryPrice decimal.Decimal `json:"entry_price"`
	ExitPrice  decimal.Decimal `json:"exit_price"`
	Quantity   decimal.Decimal `json:"quantity"`
	Direction  string          `json:"direction"` // LONG or SHORT
	PnL        decimal.Decimal `json:"pnl"`       // Realized PnL
	EntryTime  time.Time       `json:"entry_time"`
	ExitTime   time.Time       `json:"exit_time"`
	ExitReason string          `json:"exit_reason"` // SL-HIT, TARGET-HIT
	IsPaper    bool            `json:"is_paper"`    // true for paper trading simulation
	// GrossPnL is before charges; PnL is net of Costs (brokerage, statutory
	// charges and any modelled spread on both legs). Zero costs means the
	// broker did not report any, not that there were none.
	GrossPnL     decimal.Decimal `json:"gross_pnl"`
	Costs        decimal.Decimal `json:"costs"`
	EntryOrderID string          `json:"entry_order_id,omitempty"`
	ExitOrderID  string          `json:"exit_order_id,omitempty"`
	// Metadata is the entry signal's metadata merged with what the exit fill
	// added (Reason, UnderlyingLast at exit): Underlying, IndexEntry,
	// IndexStop, IndexSide, Expiry, DaysToExpiry, ATR, and for the exit
	// UnderlyingExit. It is what lets a derivative trade be read in the
	// units the strategy was measured in — bps of the index move.
	Metadata map[string]string `json:"metadata,omitempty"`
}
