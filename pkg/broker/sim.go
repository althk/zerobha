package broker

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// SimBroker implements core.Broker for backtesting.
type SimBroker struct {
	mu      sync.Mutex // Protects balance in concurrent tests
	Balance decimal.Decimal
	Orders  []models.Order
	Trades  []models.Trade
}

func (s *SimBroker) HasOpenPosition(symbol string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, o := range s.Orders {
		if o.Symbol == symbol && o.Status == models.OrderFilled {
			return true, nil
		}
	}
	return false, nil
}

// NewSimBroker creates a simulator with starting capital.
func NewSimBroker(capital decimal.Decimal) *SimBroker {
	return &SimBroker{
		Balance: capital,
		Orders:  make([]models.Order, 0),
	}
}

func (s *SimBroker) GetBalance() (decimal.Decimal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Balance, nil
}

func (s *SimBroker) GetQuote(symbol string) (decimal.Decimal, error) {
	// For simulation, we might not have live quotes.
	// We could return a dummy value or try to find it in loaded data.
	return decimal.NewFromInt(100), nil // Placeholder
}

func (s *SimBroker) PlaceOrder(order models.Order) (models.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Calculate Cost
	cost := order.Price.Mul(order.Quantity)

	// 2. Validate Funds (Simple Check)
	if order.Side == models.BuySignal {
		if s.Balance.LessThan(cost) {
			return order, errors.New("insufficient funds in simulation")
		}
		// Deduct immediately (Simplified for "Market" orders)
		s.Balance = s.Balance.Sub(cost)
	} else {
		// Sell logic: Add funds back
		s.Balance = s.Balance.Add(cost)
	}

	// 3. Record the Order
	order.ID = fmt.Sprintf("SIM-%d", len(s.Orders)+1)
	order.Status = models.OrderFilled

	// Use existing Timestamp if set, otherwise try Metadata
	if order.Timestamp.IsZero() {
		if tStr, ok := order.Metadata["CandleTime"]; ok {
			order.Timestamp, _ = time.Parse("2006-01-02 15:04:05", tStr)
		} else {
			order.Timestamp = time.Now() // Fallback to Now if neither is present
		}
	}

	s.Orders = append(s.Orders, order)

	return order, nil
}

func (s *SimBroker) CheckExits(candle models.Candle) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Iterate over orders to find OPEN positions
	for i := range s.Orders {
		// We use a pointer so we can update the status
		o := &s.Orders[i]

		// Handle LONG Positions
		if o.Status == models.OrderFilled && o.Side == models.BuySignal && o.Quantity.GreaterThan(decimal.Zero) {
			var exitPrice decimal.Decimal
			var exitReason string

			// 1. Check Stop Loss
			if candle.Low.LessThanOrEqual(o.StopLoss) {
				exitPrice = o.StopLoss
				exitReason = "SL-HIT"
			} else if candle.High.GreaterThanOrEqual(o.Target) {
				// 2. Check Target
				exitPrice = o.Target
				exitReason = "TARGET-HIT"
			}

			// If triggered, Execute the Sell
			if exitReason != "" {
				// Credit the cash back
				revenue := exitPrice.Mul(o.Quantity)
				s.Balance = s.Balance.Add(revenue)
				realizedPnL := revenue.Sub(o.Price.Mul(o.Quantity))

				// Mark the old order as closed (or reduce qty)
				o.Status = models.OrderClosed

				// Log the Exit Trade
				trade := models.Trade{
					Symbol:     o.Symbol,
					EntryPrice: o.Price,
					ExitPrice:  exitPrice,
					Quantity:   o.Quantity,
					Direction:  "LONG",
					PnL:        realizedPnL,
					EntryTime:  o.Timestamp,
					ExitTime:   candle.EndTime,
					ExitReason: exitReason,
				}
				s.Trades = append(s.Trades, trade)

				log.Printf(">>> EXIT TRIGGERED (LONG): %s | %s @ %s\n", exitReason, o.Symbol, exitPrice)
			}
		}

		// Handle SHORT Positions
		if o.Status == models.OrderFilled && o.Side == models.SellSignal && o.Quantity.GreaterThan(decimal.Zero) {
			var exitPrice decimal.Decimal
			var exitReason string

			// 1. Check Stop Loss (Price goes UP)
			if candle.High.GreaterThanOrEqual(o.StopLoss) {
				exitPrice = o.StopLoss
				exitReason = "SL-HIT"
			} else if candle.Low.LessThanOrEqual(o.Target) {
				// 2. Check Target (Price goes DOWN)
				exitPrice = o.Target
				exitReason = "TARGET-HIT"
			}

			// If triggered, Execute the Buy to Cover
			if exitReason != "" {
				// Debit the cost to buy back
				buyBackCost := exitPrice.Mul(o.Quantity)
				s.Balance = s.Balance.Sub(buyBackCost)

				// PnL = (Entry Price - Exit Price) * Qty
				realizedPnL := o.Price.Sub(exitPrice).Mul(o.Quantity)

				// Mark the old order as closed
				o.Status = models.OrderClosed

				// Log the Exit Trade
				trade := models.Trade{
					Symbol:     o.Symbol,
					EntryPrice: o.Price,
					ExitPrice:  exitPrice,
					Quantity:   o.Quantity,
					Direction:  "SHORT",
					PnL:        realizedPnL,
					EntryTime:  o.Timestamp,
					ExitTime:   candle.EndTime,
					ExitReason: exitReason,
				}
				s.Trades = append(s.Trades, trade)

				log.Printf(">>> EXIT TRIGGERED (SHORT): %s | %s @ %s\n", exitReason, o.Symbol, exitPrice)
			}
		}
	}

}

// GetEquity calculates Total Account Value (Cash + Unrealized PnL)
// We need the current market price of assets to calculate this.
func (s *SimBroker) GetEquity(currentPrice decimal.Decimal) decimal.Decimal {
	s.mu.Lock()
	defer s.mu.Unlock()

	equity := s.Balance

	for _, o := range s.Orders {
		// Only count OPEN positions
		if o.Status == models.OrderFilled {
			switch o.Side {
			case models.BuySignal:
				// Long: Value = Qty * Current Market Price
				positionValue := o.Quantity.Mul(currentPrice)
				equity = equity.Add(positionValue)
			case models.SellSignal:
				// Short: Liability = Qty * Current Market Price
				// Equity = Balance - Liability (since Balance includes initial sale proceeds)
				liability := o.Quantity.Mul(currentPrice)
				equity = equity.Sub(liability)
			}
		}
	}
	return equity
}

// GetPositions returns all open positions (Stub)
func (s *SimBroker) GetPositions() ([]models.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var positions []models.Position
	for _, o := range s.Orders {
		if o.Status == models.OrderFilled {
			qty := o.Quantity.IntPart()
			positions = append(positions, models.Position{
				Tradingsymbol: o.Symbol,
				Quantity:      int(qty),
				Product:       o.ProductType,
				AveragePrice:  o.Price,
			})
		}
	}
	return positions, nil
}

// GetGTTs returns listing of all GTT triggers (Stub)
func (s *SimBroker) GetGTTs() ([]models.GTT, error) {
	return []models.GTT{}, nil
}

// CancelGTT deletes a GTT trigger (Stub)
func (s *SimBroker) CancelGTT(triggerID int) error {
	return nil
}

// CancelOrder cancels an open order (Stub)
func (s *SimBroker) CancelOrder(orderID string) error {
	return nil
}

// GetOpenOrders returns all pending orders (Stub)
func (s *SimBroker) GetOpenOrders() ([]models.Order, error) {
	return []models.Order{}, nil
}

// GetTrades returns all completed orders (Stub)
func (s *SimBroker) GetTrades() ([]models.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Orders, nil
}
