package core

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"strconv"
	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/db"
	"zerobha/pkg/journal"

	"github.com/shopspring/decimal"
)

type Engine struct {
	Strategy          Strategy
	Broker            Broker
	Risk              *risk.Manager
	Journal           *journal.Journal
	InstrumentManager *broker.InstrumentManager
	DB                *db.Store
	LeverageMap       map[string]float64
}

func NewEngine(s Strategy, b Broker, r *risk.Manager, j *journal.Journal, im *broker.InstrumentManager, d *db.Store) *Engine {
	e := &Engine{Strategy: s, Broker: b, Risk: r, Journal: j, InstrumentManager: im, DB: d}
	e.loadLeverageMap()
	return e
}

func (e *Engine) loadLeverageMap() {
	e.LeverageMap = make(map[string]float64)
	file, err := os.Open("zerodha-mis-margins.csv")
	if err != nil {
		log.Printf("WARNING: Failed to open leverage CSV: %v", err)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		log.Printf("WARNING: Failed to read leverage CSV: %v", err)
		return
	}

	for i, record := range records {
		if i == 0 {
			continue // Skip header
		}
		if len(record) < 3 {
			continue
		}
		symbol := record[0]
		leverageStr := record[2]
		leverage, err := strconv.ParseFloat(leverageStr, 64)
		if err == nil {
			e.LeverageMap[symbol] = leverage
		}
	}
	log.Printf("Loaded leverage for %d symbols", len(e.LeverageMap))
}

// Execute is called whenever a candle closes
func (e *Engine) Execute(candle models.Candle) {
	// 1. Get Signal from Strategy
	signal := e.Strategy.OnCandle(candle)
	if signal == nil {
		return
	}

	log.Printf("New signal:%+v\n", signal)
	if e.Journal != nil {
		e.Journal.LogSignal(signal)
	}
	if e.DB != nil {
		if err := e.DB.SaveSignal(signal); err != nil {
			log.Printf("ERROR: Failed to save signal to DB: %v", err)
		}
	}

	// 2. Option Selection Logic (Intercept NSEI signals)
	if (signal.Symbol == "^NSEI" || signal.Symbol == "NSEI") && e.InstrumentManager != nil {
		log.Println("Intercepted Index Signal. Selecting Option...")

		// Determine Side
		side := "CE"
		if signal.Type == models.SellSignal {
			side = "PE" // For Index, Sell Signal means Bearish -> Buy PE
			// Wait, if strategy says SELL NSEI, do we Buy PE or Sell CE?
			// Usually easier to Buy PE for directional trade.
			// Let's assume Buy PE.
			signal.Type = models.BuySignal // We are BUYING an option
		}

		// Spot Price from Candle
		spotPrice, _ := candle.Close.Float64()

		// Define Fetcher
		// REAL IMPLEMENTATION: We need to change FindOptionWithSpot to take a fetcher that accepts SYMBOLS?
		// Or we expose GetSymbol(token) from IM.

		// Let's assume we added GetSymbol(token) to IM.
		quoteFetcher := func(tokens []uint32) (map[uint32]float64, error) {
			results := make(map[uint32]float64)
			for _, t := range tokens {
				sym, err := e.InstrumentManager.GetSymbol(t)
				if err != nil {
					continue
				}
				price, err := e.Broker.GetQuote(sym)
				if err == nil {
					p, _ := price.Float64()
					results[t] = p
				}
			}
			return results, nil
		}

		opt, err := e.InstrumentManager.FindOptionWithSpot("NIFTY", side, spotPrice, 250.0, quoteFetcher)
		if err != nil {
			log.Printf("Failed to select option: %v", err)
			return
		}

		log.Printf("Selected Option: %s (Strike: %.0f, Expiry: %s)", opt.Tradingsymbol, opt.Strike, opt.Expiry.Format("2006-01-02"))

		// Update Signal to point to Option
		signal.Symbol = opt.Tradingsymbol
		// We need to update price to Option Price.
		// Fetch fresh quote for the selected option
		q, err := e.Broker.GetQuote(opt.Tradingsymbol)
		if err == nil {
			signal.Price = q
		} else {
			log.Printf("Failed to get quote for selected option %s", opt.Tradingsymbol)
			return
		}

		signal.StopLoss = decimal.Zero
		signal.Target = decimal.Zero
	}

	// 3. Check for Existing Position
	hasPosition, err := e.Broker.HasOpenPosition(signal.Symbol)
	if err == nil && hasPosition {
		log.Printf("Skipping signal for %s: Position already open", signal.Symbol)
		return
	}

	// 4. Risk Management Check
	if err := e.Risk.Evaluate(signal); err != nil {
		log.Printf("BLOCKED: %s | Signal: %v", err, signal.Type)
		if e.Journal != nil {
			e.Journal.LogRiskBlock(signal, err.Error())
		}
		return
	}

	// 4. Order Conversion (Position Sizing)
	balance, _ := e.Broker.GetBalance()
	if balance.LessThan(decimal.NewFromInt(3000)) {
		log.Printf("Skipping signal for %s: Insufficient balance", signal.Symbol)
		return
	}
	capital := decimal.Max(balance.Div(decimal.NewFromInt(5)), decimal.NewFromInt(20000))
	capital = decimal.Min(capital, decimal.NewFromInt(30000))

	leverage := decimal.NewFromInt(1)
	if signal.ProductType == "MIS" {
		if lev, ok := e.LeverageMap[signal.Symbol]; ok {
			leverage = decimal.NewFromFloat(lev)
		}
	}

	qty := CalculateQuantity(capital, signal, leverage)

	if qty.IsZero() {
		return
	}

	// 5. Create Order Object
	order := models.Order{
		Symbol:      signal.Symbol,
		Side:        signal.Type,
		Type:        "MARKET", // Only market orders supported
		ProductType: signal.ProductType,
		Quantity:    qty.Floor(),
		Price:       signal.Price.Round(1),
		StopLoss:    signal.StopLoss.Round(1),
		Target:      signal.Target.Round(1),
		Metadata:    signal.Metadata,
	}

	// 6. Execute
	var errExec error
	order, errExec = e.Broker.PlaceOrder(order)
	if errExec != nil {
		log.Printf("ERROR: Execution failed: %v", errExec)
		if e.Journal != nil {
			e.Journal.LogOrder(order, "FAILED", errExec.Error())
		}
		if e.DB != nil {
			_ = e.DB.SaveOrder(order, "FAILED")
		}
	} else {
		log.Printf("SUCCESS: Order Placed %s | order: %v", order.Symbol, order)
		if e.Journal != nil {
			e.Journal.LogOrder(order, "SUCCESS", fmt.Sprintf("OrderID: %s", order.ID))
		}
		if e.DB != nil {
			_ = e.DB.SaveOrder(order, "SUBMITTED")
		}
		// Update Risk Manager stats
		// TODO: Handle actual pnl
		e.Risk.UpdateTradeLog(order.Symbol, decimal.Zero)
	}
}

// SquareOff cancels all GTTs, Open MIS Orders and Closes all MIS positions
func (e *Engine) SquareOff() {
	log.Println("⚡ STARTING AUTO SQUAREOFF SEQUENCE ⚡")

	// 1. Cancel All Active GTTs
	gtts, err := e.Broker.GetGTTs()
	if err != nil {
		log.Printf("SquareOff Error: Failed to fetch GTTs: %v", err)
	} else {
		for _, g := range gtts {
			log.Printf("SquareOff: Cancelling GTT %d (%s)", g.ID, g.Tradingsymbol)
			if err := e.Broker.CancelGTT(g.ID); err != nil {
				log.Printf("SquareOff Error: Failed to cancel GTT %d: %v", g.ID, err)
			}
		}
	}

	// 2. Cancel All Open MIS Orders
	orders, err := e.Broker.GetOpenOrders()
	if err != nil {
		log.Printf("SquareOff Error: Failed to fetch Orders: %v", err)
	} else {
		for _, o := range orders {
			if o.ProductType == "MIS" {
				log.Printf("SquareOff: Cancelling Open MIS Order %s (%s)", o.ID, o.Symbol)
				if err := e.Broker.CancelOrder(o.ID); err != nil {
					log.Printf("SquareOff Error: Failed to cancel Order %s: %v", o.ID, err)
				}
			}
		}
	}

	// 3. Close All MIS Positions
	positions, err := e.Broker.GetPositions()
	if err != nil {
		log.Printf("SquareOff Error: Failed to fetch Positions: %v", err)
		return
	}

	for _, p := range positions {
		if p.Product != "MIS" || p.NetQuantity == 0 {
			continue
		}

		log.Printf("SquareOff: Closing Position %s (Qty: %d)", p.Tradingsymbol, p.NetQuantity)

		var side models.SignalType
		var qty decimal.Decimal
		if p.NetQuantity > 0 {
			side = models.SellSignal
			qty = decimal.NewFromInt(int64(p.NetQuantity))
		} else {
			side = models.BuySignal
			qty = decimal.NewFromInt(int64(-p.NetQuantity))
		}

		// Create Counter Order
		order := models.Order{
			Symbol:      p.Tradingsymbol,
			Side:        side,
			Type:        "MARKET",
			ProductType: "MIS",
			Quantity:    qty,
			Metadata:    map[string]string{"Reason": "AutoSquareOff"},
		}

		// Execute
		_, err := e.Broker.PlaceOrder(order)
		if err != nil {
			log.Printf("SquareOff Error: Failed to close position %s: %v", p.Tradingsymbol, err)
		} else {
			log.Printf("SquareOff: Successfully submitted close order for %s", p.Tradingsymbol)
		}
	}

	log.Println("⚡ AUTO SQUAREOFF SEQUENCE COMPLETED ⚡")
}
