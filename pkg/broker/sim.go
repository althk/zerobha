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

	// Anchor the chandelier trail at the fill: before any bar has printed, the
	// best price reached since entry is the entry itself.
	if order.TrailDistance.IsPositive() && order.TrailAnchor.IsZero() {
		order.TrailAnchor = order.Price
	}

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

// timeStopReached reports whether the order carries a MetaExitOnOrAfter date
// that this candle has reached. Orders without the key, or with an unparseable
// one, never time out — a malformed date must not silently close positions.
func timeStopReached(o *models.Order, candle models.Candle) bool {
	if o.Metadata == nil {
		return false
	}
	raw, ok := o.Metadata[models.MetaExitOnOrAfter]
	if !ok || raw == "" {
		return false
	}
	deadline, err := time.ParseInLocation("2006-01-02", raw, candle.StartTime.Location())
	if err != nil {
		return false
	}
	return !candle.StartTime.Before(deadline)
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
			// Partial exit: book PartialExitPct of the position once price
			// reaches PartialExitPrice, then move the stop to breakeven (entry
			// price) for the remainder so the trade can no longer turn into a
			// loser. Checked before the full SL/target so a bar that reaches
			// both the partial level and the stop in the same candle still
			// books the partial leg's better price first.
			if !o.PartialExitDone && !o.PartialExitPrice.IsZero() && candle.High.GreaterThanOrEqual(o.PartialExitPrice) {
				partialQty := o.Quantity.Mul(o.PartialExitPct).Round(0)
				if partialQty.GreaterThan(decimal.Zero) && partialQty.LessThan(o.Quantity) {
					revenue := o.PartialExitPrice.Mul(partialQty)
					s.Balance = s.Balance.Add(revenue)
					realizedPnL := revenue.Sub(o.Price.Mul(partialQty))

					o.Quantity = o.Quantity.Sub(partialQty)
					o.PartialExitDone = true
					o.StopLoss = o.Price // move remainder's stop to breakeven

					s.Trades = append(s.Trades, models.Trade{
						Symbol:     o.Symbol,
						EntryPrice: o.Price,
						ExitPrice:  o.PartialExitPrice,
						Quantity:   partialQty,
						Direction:  "LONG",
						PnL:        realizedPnL,
						EntryTime:  o.Timestamp,
						ExitTime:   candle.EndTime,
						ExitReason: "PARTIAL-EXIT",
					})

					log.Printf(">>> PARTIAL EXIT (LONG): %s | %s @ %s | SL moved to breakeven %s\n",
						o.Symbol, partialQty.String(), o.PartialExitPrice, o.StopLoss)
				}
			}

			var exitPrice decimal.Decimal
			var exitReason string

			// 1. Check Stop Loss. A zero stop means "no stop", not "stop at
			// zero" — without the guard every long would exit on its first bar.
			if o.StopLoss.IsPositive() && candle.Low.LessThanOrEqual(o.StopLoss) {
				exitPrice = o.StopLoss
				exitReason = "SL-HIT"
			} else if o.Target.IsPositive() && candle.High.GreaterThanOrEqual(o.Target) {
				// 2. Check Target. Same guard: strategies that run a pure
				// trailing exit (tp_rr = 0) leave Target zero on purpose.
				exitPrice = o.Target
				exitReason = "TARGET-HIT"
			} else if timeStopReached(o, candle) {
				// 3. Time stop, for strategies that hold across sessions.
				// Checked last so a bar that also touches stop or target is
				// attributed to the price level it actually hit.
				exitPrice = candle.Close
				exitReason = "TIME-STOP"
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
			} else {
				advanceProtectiveStops(o, candle)
			}
		}

		// Handle SHORT Positions
		if o.Status == models.OrderFilled && o.Side == models.SellSignal && o.Quantity.GreaterThan(decimal.Zero) {
			// Partial exit: book PartialExitPct of the position once price
			// reaches PartialExitPrice (moving down), then move the stop to
			// breakeven for the remainder.
			if !o.PartialExitDone && !o.PartialExitPrice.IsZero() && candle.Low.LessThanOrEqual(o.PartialExitPrice) {
				partialQty := o.Quantity.Mul(o.PartialExitPct).Round(0)
				if partialQty.GreaterThan(decimal.Zero) && partialQty.LessThan(o.Quantity) {
					buyBackCost := o.PartialExitPrice.Mul(partialQty)
					s.Balance = s.Balance.Sub(buyBackCost)
					realizedPnL := o.Price.Sub(o.PartialExitPrice).Mul(partialQty)

					o.Quantity = o.Quantity.Sub(partialQty)
					o.PartialExitDone = true
					o.StopLoss = o.Price // move remainder's stop to breakeven

					s.Trades = append(s.Trades, models.Trade{
						Symbol:     o.Symbol,
						EntryPrice: o.Price,
						ExitPrice:  o.PartialExitPrice,
						Quantity:   partialQty,
						Direction:  "SHORT",
						PnL:        realizedPnL,
						EntryTime:  o.Timestamp,
						ExitTime:   candle.EndTime,
						ExitReason: "PARTIAL-EXIT",
					})

					log.Printf(">>> PARTIAL EXIT (SHORT): %s | %s @ %s | SL moved to breakeven %s\n",
						o.Symbol, partialQty.String(), o.PartialExitPrice, o.StopLoss)
				}
			}

			var exitPrice decimal.Decimal
			var exitReason string

			// 1. Check Stop Loss (Price goes UP). Zero means "no stop".
			if o.StopLoss.IsPositive() && candle.High.GreaterThanOrEqual(o.StopLoss) {
				exitPrice = o.StopLoss
				exitReason = "SL-HIT"
			} else if o.Target.IsPositive() && candle.Low.LessThanOrEqual(o.Target) {
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
			} else {
				advanceProtectiveStops(o, candle)
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

// ModifyPositionStop is a no-op in the simulator: CheckExits applies the
// breakeven-trail directly to the in-memory order's StopLoss field, so there
// is no live GTT to modify.
func (s *SimBroker) ModifyPositionStop(order models.Order, newStopLoss decimal.Decimal) error {
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

// advanceProtectiveStops moves an open order's stop for the BREAKEVEN+ trigger
// and the chandelier trail, using the bar that has just been evaluated.
//
// It runs only after CheckExits has decided that this bar did NOT exit the
// trade, and that ordering is deliberate. Within a single bar the sequence of
// high and low is unknown, so crediting a stop improvement earned by this bar's
// high and then letting the same bar's low exit at the improved stop would be
// lookahead in the strategy's favour. Evaluating exits against the stop as it
// stood at the bar's open, and only then advancing it, is the pessimistic read.
//
// The stop only ever ratchets towards profit — for a long it can rise and never
// fall — and it is never placed on the wrong side of the market: see
// stopNoBetterThanMarket.
func advanceProtectiveStops(o *models.Order, candle models.Candle) {
	if o.Side == models.BuySignal {
		if !o.BreakevenApplied && o.BreakevenTrigger.IsPositive() && candle.High.GreaterThanOrEqual(o.BreakevenTrigger) {
			o.BreakevenApplied = true
			if candidate := stopNoBetterThanMarket(o.Side, o.BreakevenStop, candle.Close); candidate.GreaterThan(o.StopLoss) {
				o.StopLoss = candidate
				log.Printf(">>> BREAKEVEN+ (LONG): %s | SL moved to %s\n", o.Symbol, o.StopLoss)
			}
		}
		if o.TrailDistance.IsPositive() {
			if candle.High.GreaterThan(o.TrailAnchor) {
				o.TrailAnchor = candle.High
			}
			trailed := stopNoBetterThanMarket(o.Side, o.TrailAnchor.Sub(o.TrailDistance), candle.Close)
			if trailed.GreaterThan(o.StopLoss) {
				o.StopLoss = trailed
			}
		}
		return
	}

	if !o.BreakevenApplied && o.BreakevenTrigger.IsPositive() && candle.Low.LessThanOrEqual(o.BreakevenTrigger) {
		o.BreakevenApplied = true
		candidate := stopNoBetterThanMarket(o.Side, o.BreakevenStop, candle.Close)
		if o.StopLoss.IsZero() || candidate.LessThan(o.StopLoss) {
			o.StopLoss = candidate
			log.Printf(">>> BREAKEVEN+ (SHORT): %s | SL moved to %s\n", o.Symbol, o.StopLoss)
		}
	}
	if o.TrailDistance.IsPositive() {
		if o.TrailAnchor.IsZero() || candle.Low.LessThan(o.TrailAnchor) {
			o.TrailAnchor = candle.Low
		}
		trailed := stopNoBetterThanMarket(o.Side, o.TrailAnchor.Add(o.TrailDistance), candle.Close)
		if o.StopLoss.IsZero() || trailed.LessThan(o.StopLoss) {
			o.StopLoss = trailed
		}
	}
}

// stopNoBetterThanMarket caps a protective stop at the current market price: a
// long's stop may not sit above it, a short's may not sit below it.
//
// Without this cap the two stop-advancing rules quietly manufacture profit. A
// BREAKEVEN+ offset of 10 points against a 4-point ATR trigger parks the stop
// 6 points above where the bar actually traded, and a chandelier trail measured
// from a high the bar gave back does the same; the next bar then "fills" the
// stop at a price the market never printed. Every such fill was a winner, which
// is exactly how the bug hides — it reads as a strategy that works.
//
// Capping at the close, rather than refusing the move, keeps the meaning of
// both rules: the stop is as tight as the rule asks for and still achievable.
func stopNoBetterThanMarket(side models.SignalType, stop, market decimal.Decimal) decimal.Decimal {
	if side == models.BuySignal {
		return decimal.Min(stop, market)
	}
	return decimal.Max(stop, market)
}

// ClosePosition implements core.PositionCloser: it flattens one symbol's open
// position on the given side at price, booking the trade under reason.
//
// The engine needs this because a simulated position is a row in this broker's
// own order book, not a real exchange position — placing a counter order (what
// the live path does) would open a second position here instead of closing the
// first.
func (s *SimBroker) ClosePosition(symbol string, side models.SignalType, price decimal.Decimal, at time.Time, reason string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	closed := false
	for i := range s.Orders {
		o := &s.Orders[i]
		if o.Status != models.OrderFilled || o.Symbol != symbol || o.Side != side {
			continue
		}
		if o.Quantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		s.closeOrderLocked(o, price, reason, at)
		closed = true
	}
	return closed, nil
}

// closeOrderLocked flattens one filled order at exitPrice, updating the balance
// and appending the trade. The caller must hold s.mu. A zero exitTime is left
// as-is for callers that have no candle to attribute the exit to.
func (s *SimBroker) closeOrderLocked(o *models.Order, exitPrice decimal.Decimal, exitReason string, exitTime time.Time) {
	var realizedPnL decimal.Decimal
	direction := "SHORT"
	if o.Side == models.BuySignal {
		direction = "LONG"
		revenue := exitPrice.Mul(o.Quantity)
		s.Balance = s.Balance.Add(revenue)
		realizedPnL = revenue.Sub(o.Price.Mul(o.Quantity))
	} else {
		buyBackCost := exitPrice.Mul(o.Quantity)
		s.Balance = s.Balance.Sub(buyBackCost)
		realizedPnL = o.Price.Mul(o.Quantity).Sub(buyBackCost) // (Entry - Exit) * Qty
	}

	o.Status = models.OrderClosed

	s.Trades = append(s.Trades, models.Trade{
		Symbol:     o.Symbol,
		EntryPrice: o.Price,
		ExitPrice:  exitPrice,
		Quantity:   o.Quantity,
		Direction:  direction,
		PnL:        realizedPnL,
		EntryTime:  o.Timestamp,
		ExitTime:   exitTime,
		ExitReason: exitReason,
	})

	log.Printf(">>> POSITION CLOSED (%s): %s | %s @ %s | PnL %s\n", direction, exitReason, o.Symbol, exitPrice, realizedPnL)
}

func (s *SimBroker) SquareOffAll(candle models.Candle) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.Orders {
		o := &s.Orders[i]
		if o.Status != models.OrderFilled || o.Quantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		s.closeOrderLocked(o, candle.Close, "EOD-SQUAREOFF", candle.EndTime)
	}
}
