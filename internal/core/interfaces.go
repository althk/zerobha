package core

import (
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

	// CancelOrder cancels an open order
	CancelOrder(orderID string) error

	// GetOpenOrders returns all pending orders
	GetOpenOrders() ([]models.Order, error)

	// GetTrades returns all completed orders filtered by "ZEROBHA_BOT" tag
	GetTrades() ([]models.Order, error)
}
