package models

import (
	"time"

	"github.com/shopspring/decimal"
)

type OrderStatus string

const (
	OrderPending OrderStatus = "PENDING"
	OrderFilled  OrderStatus = "FILLED"
	OrderFailed  OrderStatus = "FAILED"
	OrderClosed  OrderStatus = "CLOSED"
)

type Order struct {
	ID          string     `json:"id"`
	Symbol      string     `json:"symbol"`
	Side        SignalType `json:"side"`         // Reusing SignalType (Buy/Sell)
	Type        string     `json:"type"`         // MARKET, LIMIT, SL-M
	ProductType string     `json:"product_type"` // MIS, CNC
	// Exchange the instrument trades on: "NSE" for cash equities, "NFO" for
	// futures and options. Empty means NSE — every strategy here except
	// Donchian is a cash-equity strategy and never sets it.
	Exchange  string          `json:"exchange"`
	Quantity  decimal.Decimal `json:"quantity"`
	Price     decimal.Decimal `json:"price"` // Limit price
	Timestamp time.Time       `json:"timestamp"`
	Status    OrderStatus     `json:"status"`
	StopLoss  decimal.Decimal `json:"stop_loss"`
	Target    decimal.Decimal `json:"target"`
	// PartialExitPrice is the price at which half (see PartialExitPct) of the
	// position is booked and StopLoss moved to breakeven. Zero disables it.
	PartialExitPrice decimal.Decimal `json:"partial_exit_price"`
	PartialExitPct   decimal.Decimal `json:"partial_exit_pct"`
	// PartialExitDone marks that the partial leg has already fired, so
	// CheckExits does not re-trigger it on the remaining quantity.
	PartialExitDone bool `json:"partial_exit_done"`
	// TrailDistance and TrailAnchor carry the chandelier trailing stop. Anchor
	// is the best price reached since entry (highest high for a long, lowest
	// low for a short); the stop rests TrailDistance away from it and only
	// ratchets. TrailDistance zero means no trail.
	TrailDistance decimal.Decimal `json:"trail_distance"`
	TrailAnchor   decimal.Decimal `json:"trail_anchor"`
	// BreakevenTrigger is the price that arms the BREAKEVEN+ move and
	// BreakevenStop is where the stop is parked when it does. BreakevenApplied
	// below marks it as already done.
	BreakevenTrigger decimal.Decimal `json:"breakeven_trigger"`
	BreakevenStop    decimal.Decimal `json:"breakeven_stop"`
	// GTTTriggerID is the Kite GTT trigger ID protecting this live position
	// (SL + Target OCO). Zero when no GTT was placed (e.g. backtest).
	GTTTriggerID int `json:"gtt_trigger_id"`
	// BreakevenApplied marks that the GTT's stop leg has already been moved
	// to entry price, so the live position monitor does not re-trigger it.
	BreakevenApplied bool              `json:"breakeven_applied"`
	ParentID         string            `json:"parent_id"` // Links the Exit order to the Entry order
	Metadata         map[string]string `json:"metadata"`
}
