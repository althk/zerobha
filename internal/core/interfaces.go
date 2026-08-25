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

// ExitAdvice tells the engine to close an existing position at market, outside
// the stop/target the broker already holds. It is advice rather than an order:
// the strategy cannot see whether its signal was ever filled (risk limits and
// concurrency caps may have dropped it), so the engine acts only if a matching
// position actually exists.
type ExitAdvice struct {
	Symbol string
	// ForSide is the side of the position this applies to — a long-exit advice
	// must not close a short that happens to be open in the same symbol.
	ForSide models.SignalType
	Reason  string
}

// ExitAdvisor is implemented by strategies that own an exit condition the
// broker cannot evaluate on its own — one that depends on indicator state
// rather than on a price level, such as "close beyond the opposite Donchian
// band". It is optional: the engine type-asserts for it, so strategies whose
// exits are fully described by StopLoss/Target need not implement it.
type ExitAdvisor interface {
	ExitAdvice(candle models.Candle) *ExitAdvice
}

// PositionCloser is an optional Broker capability: closing one symbol's
// position at a known price, in one call. Brokers that cannot do this (or do
// not need to) simply omit it, and the engine falls back to placing a counter
// order built from GetPositions.
//
// The simulator implements it because its positions live inside its own order
// book with the PnL bookkeeping attached; a generic counter order would open a
// second position there rather than closing the first.
type PositionCloser interface {
	// ClosePosition closes any open position in symbol on the given side at
	// price, tagging the resulting trade with reason. `at` is the market time
	// of the exit, not wall clock — the simulator has no other way to stamp the
	// trade, and an unstamped one drops out of every time-ordered analysis.
	// It reports whether a position was actually found and closed.
	ClosePosition(symbol string, side models.SignalType, price decimal.Decimal, at time.Time, reason string) (bool, error)
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

// TickObserver is an optional Broker capability: a broker that has to fill its
// own resting protective orders needs to see the traded price.
//
// The live Zerodha adapter does not implement it — its stops rest at the
// exchange as GTTs and trigger without the engine's help. The paper broker
// does, because nothing else will ever trigger them: without a price feed a
// simulated position has no stop at all, and every strategy here except
// Donchian's option mode exits solely through broker-held stops and targets.
//
// It is deliberately separate from ModifyPositionStop. The engine still owns
// where the stop belongs (stopAfterTick applies the breakeven and chandelier
// rules identically in both modes); this only tells the broker what price
// traded, so it can decide whether a resting order has been hit.
type TickObserver interface {
	OnTick(symbol string, price decimal.Decimal, at time.Time)
}
