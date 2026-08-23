package core

import (
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// DataProvider defines the read-only access to market data.
// The Strategy consumes this, but the Engine usually injects it.
// (We define it here as part of the contract the Engine supports).
type DataProvider interface {
	History(symbol string, timeframe string, days int) ([]models.Candle, error)
	// We avoid 'Get' prefix as per style guide
}

// Strategy defines the behavior required for any logic
// that wishes to run within the Engine's event loop.
type Strategy interface {
	// Name returns the identifier for logs/reporting
	Name() string

	// Init allows the strategy to load historical data or setup indicators
	// before live trading begins.
	Init(provider DataProvider) error

	// OnCandle is the primary trigger for swing trading logic.
	// It returns a *Signal if a trade decision is made, or nil.
	OnCandle(candle models.Candle) *models.Signal
}

// Broker defines the execution capability.
// It abstracts away Zerodha, Upstox, or the Simulator.
type Broker interface {
	// GetBalance returns the available trading capital.
	GetBalance() (decimal.Decimal, error)

	// PlaceOrder sends the instruction to the exchange.
	// It returns the OrderID or an error.
	PlaceOrder(order models.Order) (models.Order, error)

	// GetQuote fetches the current market price for a symbol
	GetQuote(symbol string) (decimal.Decimal, error)

	// HasOpenPosition checks if there is an active position for the symbol
	HasOpenPosition(symbol string) (bool, error)

	// GetPositions returns all open positions
	GetPositions() ([]models.Position, error)

	// GetGTTs returns listing of all GTT triggers
	GetGTTs() ([]models.GTT, error)

	// CancelGTT deletes a GTT trigger
	CancelGTT(triggerID int) error

	// ModifyPositionStop replaces the SL leg of an existing OCO GTT with a new
	// stop price (e.g. breakeven), keeping the target leg and quantity
	// unchanged. Used by the partial-exit/breakeven-trail live position monitor.
	ModifyPositionStop(order models.Order, newStopLoss decimal.Decimal) error

	// CancelOrder cancels an open order
	CancelOrder(orderID string) error

	// GetOpenOrders returns all pending orders
	GetOpenOrders() ([]models.Order, error)

	// GetTrades returns all completed orders filtered by "ZEROBHA_BOT" tag
	GetTrades() ([]models.Order, error)
}

// GateVerdict is the outcome of a NewsGate consultation. Reason is always
// populated — it is journalled with the signal so a blocked or allowed entry
// can be audited after the fact.
type GateVerdict struct {
	Allow  bool
	Reason string
}

// NewsGate lets a strategy ask an external research source whether a price
// move is explained by genuinely bad information (a fraud probe, a collapsed
// quarter) before trading against it.
//
// It is deliberately narrow and lives beside the other contracts so that
// strategies depend on this abstraction rather than on any particular vendor:
// the live trader injects an Upstox-backed implementation, a backtest injects
// nil (no gate) or a recorded one.
//
// asOf is the market timestamp of the decision, not wall-clock time, so an
// implementation can bound "recent news" against the candle being evaluated.
type NewsGate interface {
	Assess(symbol string, asOf time.Time) (GateVerdict, error)
}
