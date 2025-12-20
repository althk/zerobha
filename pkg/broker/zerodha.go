package broker

import (
	"context"
	"fmt"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"golang.org/x/time/rate"
)

// ZerodhaAdapter satisfies the Broker and DataProvider interfaces
type ZerodhaAdapter struct {
	client        *kiteconnect.Client
	apiKey        string
	limiter       *rate.Limiter
	symbolToToken map[string]uint32
}

func NewZerodhaAdapter(kc *kiteconnect.Client, symbolToToken map[string]uint32) *ZerodhaAdapter {

	return &ZerodhaAdapter{
		client:        kc,
		symbolToToken: symbolToToken,
		limiter:       rate.NewLimiter(2, 5),
	}
}

// GetBalance fetches available cash from the equity account
func (z *ZerodhaAdapter) GetBalance() (decimal.Decimal, error) {
	margins, err := z.client.GetUserMargins()
	if err != nil {
		return decimal.Zero, err
	}

	// Convert float64 to Decimal
	// "equity" segment usually holds the cash
	cashAvailable := decimal.NewFromFloat(margins.Equity.Available.Cash)
	return cashAvailable.Round(1), nil
}

// GetQuote fetches the current market price for a symbol
func (z *ZerodhaAdapter) GetQuote(symbol string) (decimal.Decimal, error) {
	// We need the instrument token to fetch the quote.
	// If we only have the symbol, we need to look it up.
	_, ok := z.symbolToToken[symbol]
	if !ok {
		// Try fetching it from the API directly if not in our map?
		// Or just error out.
		return decimal.Zero, fmt.Errorf("symbol %s not found in map", symbol)
	}

	// Fetch Quote
	quotes, err := z.client.GetQuote(fmt.Sprintf("NSE:%s", symbol)) // Try NSE first
	if err != nil {
		// Try NFO
		quotes, err = z.client.GetQuote(fmt.Sprintf("NFO:%s", symbol))
		if err != nil {
			return decimal.Zero, err
		}
	}

	// The key in the response will be "NSE:SYMBOL" or "NFO:SYMBOL"
	for _, q := range quotes {
		return decimal.NewFromFloat(q.LastPrice), nil
	}

	return decimal.Zero, fmt.Errorf("no quote found for %s", symbol)
}

func (z *ZerodhaAdapter) HasOpenPosition(symbol string) (bool, error) {
	positions, err := z.client.GetPositions()
	if err != nil {
		return false, err
	}

	// Check Net Positions
	for _, p := range positions.Net {
		if p.Tradingsymbol == symbol {
			if p.Quantity != 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// PlaceOrder converts internal Order model to Kite API call
func (z *ZerodhaAdapter) PlaceOrder(order models.Order) (models.Order, error) {

	err := z.limiter.Wait(context.Background())
	if err != nil {
		return order, err
	}

	// 1. Map Transaction Type
	var txType string
	if order.Side == models.BuySignal {
		txType = kiteconnect.TransactionTypeBuy
	} else {
		txType = kiteconnect.TransactionTypeSell
	}

	// 2. Map Product Type
	product := kiteconnect.ProductMIS // Default to MIS for safety
	switch order.ProductType {
	case "CNC":
		product = kiteconnect.ProductCNC
	case "MIS":
		product = kiteconnect.ProductMIS
	case "NRML":
		product = kiteconnect.ProductNRML
	}

	// 3. Execute
	kiteOrderResponse, err := z.client.PlaceOrder(kiteconnect.VarietyRegular, kiteconnect.OrderParams{
		Exchange:        kiteconnect.ExchangeNSE,
		Tradingsymbol:   order.Symbol,
		TransactionType: txType,
		Quantity:        int(order.Quantity.IntPart()),
		Product:         product,
		OrderType:       kiteconnect.OrderTypeMarket,
		Validity:        kiteconnect.ValidityDay,
		Tag:             "ZEROBHA_BOT", // Helps identify bot orders in Kite Console
	})

	if err != nil {
		return order, fmt.Errorf("kite execution failed: %v", err)
	}
	order.ID = kiteOrderResponse.OrderID

	// 4. Place GTT OCO if StopLoss and Target are present
	if !order.StopLoss.IsZero() && !order.Target.IsZero() {
		// Poll for order fill status
		var filledOrder *kiteconnect.Order
		maxRetries := 10
		for i := range maxRetries {
			orders, err := z.client.GetOrderHistory(kiteOrderResponse.OrderID)
			if err != nil {
				fmt.Printf("Error fetching order history (attempt %d): %v\n", i+1, err)
				time.Sleep(1 * time.Second)
				continue
			}
			if len(orders) > 0 {
				lastOrder := orders[len(orders)-1]
				if lastOrder.Status == "COMPLETE" {
					filledOrder = &lastOrder
					break
				}
			}
			time.Sleep(1 * time.Second)
		}

		if filledOrder == nil {
			fmt.Printf("Error: Order %s not filled after retries. Skipping GTT.\n", kiteOrderResponse.OrderID)
			return order, nil
		}

		var gttTxType string
		if order.Side == models.BuySignal {
			gttTxType = kiteconnect.TransactionTypeSell
		} else {
			gttTxType = kiteconnect.TransactionTypeBuy
		}

		slPrice := order.StopLoss.InexactFloat64()
		targetPrice := order.Target.InexactFloat64()

		// Enforce Minimum Gap of 0.3% (Broker requires > 0.25%)
		executionPrice := filledOrder.AveragePrice
		minGap := executionPrice * 0.003

		if order.Side == models.BuySignal {
			// Long Position: SL below, Target above
			if executionPrice-slPrice < minGap {
				newSL := executionPrice - minGap
				fmt.Printf("Adjusting SL from %.2f to %.2f (min gap rule)\n", slPrice, newSL)
				slPrice = newSL
			}
			if targetPrice-executionPrice < minGap {
				newTarget := executionPrice + minGap
				fmt.Printf("Adjusting Target from %.2f to %.2f (min gap rule)\n", targetPrice, newTarget)
				targetPrice = newTarget
			}
		} else {
			// Short Position: SL above, Target below
			if slPrice-executionPrice < minGap {
				newSL := executionPrice + minGap
				fmt.Printf("Adjusting SL from %.2f to %.2f (min gap rule)\n", slPrice, newSL)
				slPrice = newSL
			}
			if executionPrice-targetPrice < minGap {
				newTarget := executionPrice - minGap
				fmt.Printf("Adjusting Target from %.2f to %.2f (min gap rule)\n", targetPrice, newTarget)
				targetPrice = newTarget
			}
		}

		qty := float64(order.Quantity.IntPart())

		// Construct GTT Params
		gttParams := kiteconnect.GTTParams{
			Tradingsymbol:   order.Symbol,
			Exchange:        kiteconnect.ExchangeNSE,
			LastPrice:       filledOrder.AveragePrice,
			TransactionType: gttTxType,
			Product:         product,
			Trigger: &kiteconnect.GTTOneCancelsOtherTrigger{
				Upper: kiteconnect.TriggerParams{
					TriggerValue: func() float64 {
						if targetPrice > slPrice {
							return targetPrice
						}
						return slPrice
					}(),
					LimitPrice: func() float64 {
						if targetPrice > slPrice {
							return targetPrice
						}
						return slPrice
					}(),
					Quantity: qty,
				},
				Lower: kiteconnect.TriggerParams{
					TriggerValue: func() float64 {
						if targetPrice < slPrice {
							return targetPrice
						}
						return slPrice
					}(),
					LimitPrice: func() float64 {
						if targetPrice < slPrice {
							return targetPrice
						}
						return slPrice
					}(),
					Quantity: qty,
				},
			},
		}

		gttResponse, err := z.client.PlaceGTT(gttParams)
		if err != nil {
			fmt.Printf("Error placing GTT OCO: %v\n", err)
		} else {
			fmt.Printf("Placed GTT OCO. Trigger ID: %d\n", gttResponse.TriggerID)
		}
	}

	return order, nil
}

// History fetches historical candles for Strategy Warmup
func (z *ZerodhaAdapter) History(symbol string, timeframe string, days int) ([]models.Candle, error) {
	// 1. Convert symbol to token
	token, ok := z.symbolToToken[symbol]
	if !ok {
		return []models.Candle{}, fmt.Errorf("symbol %s not found", symbol)
	}

	// 2. Fetch
	to := time.Now()
	from := to.AddDate(0, 0, -days) // Last 50 days

	data, err := z.client.GetHistoricalData(int(token), timeframe, from, to, false, false)
	if err != nil {
		return nil, err
	}

	// 3. Convert Kite Candle -> Zerobha Candle
	var candles []models.Candle
	for _, k := range data {
		c := models.Candle{
			Symbol:     symbol,
			Open:       decimal.NewFromFloat(k.Open),
			High:       decimal.NewFromFloat(k.High),
			Low:        decimal.NewFromFloat(k.Low),
			Close:      decimal.NewFromFloat(k.Close),
			Volume:     decimal.NewFromInt(int64(k.Volume)),
			StartTime:  k.Date.Time,
			IsComplete: true,
		}
		candles = append(candles, c)
	}

	return candles, nil
}

// GetPositions returns all open positions
func (z *ZerodhaAdapter) GetPositions() ([]models.Position, error) {
	positions, err := z.client.GetPositions()
	if err != nil {
		return nil, err
	}

	var result []models.Position
	// Combine Net and Day positions
	for _, p := range positions.Net {
		if p.Quantity != 0 {
			result = append(result, models.Position{
				Tradingsymbol: p.Tradingsymbol,
				Exchange:      p.Exchange,
				Product:       p.Product,
				Quantity:      p.Quantity,
				AveragePrice:  decimal.NewFromFloat(p.AveragePrice),
				NetQuantity:   p.Quantity,
				LastPrice:     decimal.NewFromFloat(p.LastPrice),
				PnL:           decimal.NewFromFloat(p.PnL),
			})
		}
	}
	return result, nil
}

// GetGTTs returns listing of all GTT triggers
func (z *ZerodhaAdapter) GetGTTs() ([]models.GTT, error) {
	triggers, err := z.client.GetGTTs()
	if err != nil {
		return nil, err
	}

	var result []models.GTT
	for _, t := range triggers {
		// Only active triggers
		if t.Status != "active" {
			continue
		}

		var orders []models.GTTOrder
		for _, o := range t.Orders {
			orders = append(orders, models.GTTOrder{
				TransactionType: o.TransactionType,
				Quantity:        int(o.Quantity),
				Price:           decimal.NewFromFloat(o.Price),
				Product:         o.Product,
			})
		}

		result = append(result, models.GTT{
			ID:            t.ID,
			Tradingsymbol: t.Condition.Tradingsymbol,
			Exchange:      t.Condition.Exchange,
			Type:          string(t.Type),
			Status:        t.Status,
			Orders:        orders,
		})
	}
	return result, nil
}

// CancelGTT deletes a GTT trigger
func (z *ZerodhaAdapter) CancelGTT(triggerID int) error {
	_, err := z.client.DeleteGTT(triggerID)
	return err
}

// CancelOrder cancels an open order
func (z *ZerodhaAdapter) CancelOrder(orderID string) error {
	_, err := z.client.CancelOrder(kiteconnect.VarietyRegular, orderID, nil)
	return err
}

// GetOpenOrders returns all pending orders
func (z *ZerodhaAdapter) GetOpenOrders() ([]models.Order, error) {
	orders, err := z.client.GetOrders()
	if err != nil {
		return nil, err
	}

	var result []models.Order
	for _, o := range orders {
		// Filter for open status: OPEN, TRIGGER PENDING
		if o.Status == "OPEN" || o.Status == "TRIGGER PENDING" {
			result = append(result, models.Order{
				ID:          o.OrderID,
				Symbol:      o.TradingSymbol,
				Type:        o.OrderType,
				ProductType: o.Product,
				Quantity:    decimal.NewFromFloat(o.Quantity),
				Status:      models.OrderStatus(o.Status),
			})
		}
	}
	return result, nil
}
