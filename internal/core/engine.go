package core

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/db"
	indicators "zerobha/pkg/indicators"
	"zerobha/pkg/journal"

	"github.com/shopspring/decimal"
)

type Engine struct {
	Strategy           Strategy
	Broker             Broker
	Risk               *risk.Manager
	Journal            *journal.Journal
	InstrumentManager  *broker.InstrumentManager
	DB                 *db.Store
	LeverageMap        map[string]float64
	MaxConcurrent      int
	UptrendOnly        bool
	DataProvider       DataProvider
	MinBalance         int64
	MinCapitalPerTrade int64
	MaxCapitalPerTrade int64
	TradeCutoffMin     int

	niftyEMA50     *indicators.EMA
	niftyEMA200    *indicators.EMA
	niftyUptrendOK bool   // cached result of today's uptrend check
	niftyCheckDate string // "YYYY-MM-DD" of last uptrend evaluation
	niftySeeded    bool   // true once the uptrend filter has valid EMA data

	openOrdersMu sync.Mutex
	// openOrders tracks live positions carrying a PartialExitPrice, keyed by
	// symbol, so OnTick can apply the breakeven-trail without re-querying the
	// broker on every tick. Entries are removed once BreakevenApplied fires;
	// a position that exits before then is simply never cleaned up here (it's
	// a lookup cache, not a source of truth — GetPositions remains authoritative).
	openOrders map[string]*models.Order
	// lastStopPush throttles how often a ratcheting trail is pushed to the
	// broker, keyed by symbol. The in-memory ratchet stays exact; only the
	// network call is rate-limited, and a breakeven move bypasses it.
	lastStopPush map[string]time.Time
}

// trailPushInterval is the minimum gap between two broker-side stop
// modifications for the same symbol. A chandelier trail on a trending 5-minute
// bar would otherwise issue a modify on nearly every tick and spend the account's
// rate limit on stop moves of a few paise.
const trailPushInterval = 30 * time.Second

func NewEngine(s Strategy, b Broker, r *risk.Manager, j *journal.Journal, im *broker.InstrumentManager, d *db.Store) *Engine {
	e := &Engine{
		Strategy:           s,
		Broker:             b,
		Risk:               r,
		Journal:            j,
		InstrumentManager:  im,
		DB:                 d,
		MaxConcurrent:      5,
		UptrendOnly:        true,
		MinBalance:         3000,
		MinCapitalPerTrade: 30000,
		MaxCapitalPerTrade: 50000,
		TradeCutoffMin:     14*60 + 5,
		niftyEMA50:         indicators.NewEMA(50),
		niftyEMA200:        indicators.NewEMA(200),
		openOrders:         make(map[string]*models.Order),
		lastStopPush:       make(map[string]time.Time),
	}
	e.loadLeverageMap()
	return e
}

// InitNiftyEMAs seeds the NIFTY 50 daily EMA indicators using historical data and
// sets the initial uptrend state from the most recent completed daily candle.
// Must be called after UptrendOnly and DataProvider are set.
func (e *Engine) InitNiftyEMAs() {
	if !e.UptrendOnly || e.DataProvider == nil {
		return
	}

	// Fetch enough history to warm up EMA200 (200 + buffer)
	candles, err := e.DataProvider.History("NIFTY 50", "1d", 300)
	if err != nil {
		log.Printf("WARNING: Could not fetch NIFTY 50 history for uptrend filter: %v — uptrend filter NOT seeded", err)
		return
	}

	if len(candles) == 0 {
		log.Printf("WARNING: No NIFTY 50 historical candles returned for uptrend filter — uptrend filter NOT seeded")
		return
	}

	// Feed all but the last candle to build EMA history
	for _, c := range candles[:len(candles)-1] {
		e.niftyEMA50.Update(c.Close)
		e.niftyEMA200.Update(c.Close)
	}

	// Evaluate uptrend using the most recent completed daily candle
	last := candles[len(candles)-1]
	e.advanceNiftyEMAs(last)
	e.niftyUptrendOK = e.isNiftyUptrend(last)
	e.niftyCheckDate = last.StartTime.Format("2006-01-02")
	e.niftySeeded = true

	log.Printf("Uptrend filter initialised from %s: EMA50=%.2f EMA200=%.2f → uptrend=%v",
		e.niftyCheckDate,
		e.niftyEMA50.Value().InexactFloat64(),
		e.niftyEMA200.Value().InexactFloat64(),
		e.niftyUptrendOK)
}

// advanceNiftyEMAs feeds a daily NIFTY 50 close into the EMA indicators, mutating them.
// Call exactly once per daily candle before reading via isNiftyUptrend.
func (e *Engine) advanceNiftyEMAs(niftyCandle models.Candle) {
	e.niftyEMA50.Update(niftyCandle.Close)
	e.niftyEMA200.Update(niftyCandle.Close)
}

// isNiftyUptrend is a pure read: it returns true when the NIFTY 50 close is above
// EMA50 and EMA50 > EMA200. It does not mutate the EMAs — call advanceNiftyEMAs first.
func (e *Engine) isNiftyUptrend(niftyCandle models.Candle) bool {
	if !e.niftyEMA50.IsReady() || !e.niftyEMA200.IsReady() {
		log.Println("Uptrend filter: EMAs not yet ready, allowing trade")
		return true
	}

	ema50 := e.niftyEMA50.Value()
	ema200 := e.niftyEMA200.Value()

	closeAboveEMA50 := niftyCandle.Close.GreaterThan(ema50)
	ema50AboveEMA200 := ema50.GreaterThan(ema200)

	log.Printf("Uptrend check: close=%.2f EMA50=%.2f EMA200=%.2f closeAbove=%v ema50Above=%v",
		niftyCandle.Close.InexactFloat64(), ema50.InexactFloat64(), ema200.InexactFloat64(),
		closeAboveEMA50, ema50AboveEMA200)

	return closeAboveEMA50 && ema50AboveEMA200
}

func (e *Engine) loadLeverageMap() {
	e.LeverageMap = make(map[string]float64)
	file, err := os.Open("zerodha-mis-margins.csv")
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARNING: Failed to open leverage CSV: %v", err)
		}
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
	// Exits first, and ahead of every gate below. The entry cutoff, the trade
	// limits and the uptrend filter all decide whether a NEW position may be
	// opened; none of them is a reason to keep holding one the strategy has
	// just said to close.
	e.applyExitAdvice(candle)

	h, m, _ := candle.EndTime.Clock()
	if h*60+m >= e.TradeCutoffMin {
		return
	}

	// Uptrend master filter: update state on NIFTY 50 daily candles, gate all signals
	if e.UptrendOnly {
		isNifty := candle.Symbol == "NIFTY 50" || candle.Symbol == "^NSEI" || candle.Symbol == "NSEI"
		isDaily := candle.Timeframe == "1d" || candle.Timeframe == "day"

		if isNifty && isDaily {
			dateKey := candle.StartTime.Format("2006-01-02")
			if dateKey != e.niftyCheckDate {
				e.advanceNiftyEMAs(candle)
				e.niftyUptrendOK = e.isNiftyUptrend(candle)
				e.niftyCheckDate = dateKey
				e.niftySeeded = true
				log.Printf("Uptrend filter updated for %s: %v", dateKey, e.niftyUptrendOK)
			}
			// NIFTY daily candles don't trigger strategy signals
			return
		}

		// Fail open: if the filter never seeded (history fetch failed at startup),
		// allow trades rather than silently halting for the whole day — but log loudly.
		if !e.niftySeeded {
			log.Printf("WARNING: Uptrend filter not seeded — allowing signal for %s without trend check", candle.Symbol)
		} else if !e.niftyUptrendOK {
			log.Printf("Uptrend filter: NIFTY not in uptrend, skipping signal for %s", candle.Symbol)
			return
		}
	}

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
	balance, err := e.Broker.GetBalance()
	if err != nil {
		log.Printf("Skipping signal for %s: failed to fetch balance: %v", signal.Symbol, err)
		return
	}
	if balance.LessThan(decimal.NewFromInt(e.MinBalance)) {
		log.Printf("Skipping signal for %s: Insufficient balance", signal.Symbol)
		return
	}

	// Divide available balance by remaining open slots so we don't over-deploy capital.
	// Fail closed: if we can't read positions we can't size safely, so skip rather than
	// assume all slots are free and risk over-deploying capital.
	openPositions, err := e.Broker.GetPositions()
	if err != nil {
		log.Printf("Skipping signal for %s: failed to fetch positions: %v", signal.Symbol, err)
		return
	}
	// Count only positions that are actually open. A broker's position book
	// keeps the day's closed positions in the list with a zero net quantity
	// (Kite does, and the paper broker mirrors it so the two agree), and
	// counting those would retire a concurrency slot for every trade already
	// exited — the cap would tighten as the day went on.
	openCount := int64(0)
	for _, p := range openPositions {
		if p.NetQuantity != 0 {
			openCount++
		}
	}
	maxConcurrent := int64(e.MaxConcurrent)
	remainingSlots := decimal.NewFromInt(maxConcurrent - openCount)
	if remainingSlots.LessThanOrEqual(decimal.Zero) {
		log.Printf("No remaining slots for new positions (open: %d, max: %d)", openCount, maxConcurrent)
		return
	}
	capital := decimal.Max(balance.Div(remainingSlots), decimal.NewFromInt(e.MinCapitalPerTrade))
	capital = decimal.Min(capital, decimal.NewFromInt(e.MaxCapitalPerTrade))

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
		Symbol:           signal.Symbol,
		Side:             signal.Type,
		Type:             "MARKET", // Only market orders supported
		ProductType:      signal.ProductType,
		Exchange:         signal.Exchange,
		Quantity:         qty.Floor(),
		Price:            AdjustPriceToTick(signal.Price, GetTickSize(signal.Symbol, signal.Price)),
		StopLoss:         AdjustPriceToTick(signal.StopLoss, GetTickSize(signal.Symbol, signal.StopLoss)),
		Target:           AdjustPriceToTick(signal.Target, GetTickSize(signal.Symbol, signal.Target)),
		PartialExitPrice: AdjustPriceToTick(signal.PartialExitPrice, GetTickSize(signal.Symbol, signal.PartialExitPrice)),
		PartialExitPct:   signal.PartialExitPct,
		TrailDistance:    signal.TrailDistance,
		BreakevenTrigger: AdjustPriceToTick(signal.BreakevenTrigger, GetTickSize(signal.Symbol, signal.BreakevenTrigger)),
		BreakevenStop:    AdjustPriceToTick(signal.BreakevenStop, GetTickSize(signal.Symbol, signal.BreakevenStop)),
		Metadata:         signal.Metadata,
		Timestamp:        candle.StartTime, // Set Timestamp from Candle
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

		// Track the order for the tick-driven breakeven-trail monitor when
		// the strategy attached a partial-exit level and the broker placed a
		// GTT for it (GTTTriggerID is 0 for brokers without GTT support, e.g.
		// the sim, where CheckExits handles the trail directly instead).
		needsTickManagement := !order.PartialExitPrice.IsZero() ||
			order.TrailDistance.IsPositive() ||
			order.BreakevenTrigger.IsPositive()
		if needsTickManagement && order.GTTTriggerID != 0 {
			ord := order
			e.openOrdersMu.Lock()
			e.openOrders[order.Symbol] = &ord
			e.openOrdersMu.Unlock()
		}
	}
}

// applyExitAdvice asks the strategy whether an open position should be closed
// for a reason no resting order can express — for Donchian, price closing beyond
// the opposite channel band. Strategies that do not implement core.ExitAdvisor
// make this a single type assertion per candle.
//
// The advice names a side, and the close only happens if a position on that side
// exists: a strategy cannot see whether its entry signal was actually filled
// (risk limits and the concurrency cap drop signals silently), so it can only
// say "if you are long this, get out".
func (e *Engine) applyExitAdvice(candle models.Candle) {
	advisor, ok := e.Strategy.(ExitAdvisor)
	if !ok {
		return
	}
	advice := advisor.ExitAdvice(candle)
	if advice == nil {
		return
	}

	if closer, ok := e.Broker.(PositionCloser); ok {
		closed, err := closer.ClosePosition(advice.Symbol, advice.ForSide, candle.Close, candle.EndTime, advice.Reason)
		if err != nil {
			log.Printf("ERROR: Failed to close %s on strategy exit advice: %v", advice.Symbol, err)
			return
		}
		if closed {
			log.Printf(">>> STRATEGY EXIT: %s | %s", advice.Symbol, advice.Reason)
			e.forgetTrackedOrder(advice.Symbol)
		}
		return
	}

	// Live path: no dedicated close, so flatten with a counter market order.
	positions, err := e.Broker.GetPositions()
	if err != nil {
		log.Printf("ERROR: Cannot act on exit advice for %s: %v", advice.Symbol, err)
		return
	}
	for _, p := range positions {
		if p.Tradingsymbol != advice.Symbol || p.NetQuantity == 0 {
			continue
		}
		isLong := p.NetQuantity > 0
		if isLong != (advice.ForSide == models.BuySignal) {
			continue
		}

		qty := decimal.NewFromInt(int64(p.NetQuantity)).Abs()
		side := models.SellSignal
		if !isLong {
			side = models.BuySignal
		}
		counter := models.Order{
			Symbol:      p.Tradingsymbol,
			Side:        side,
			Type:        "MARKET",
			ProductType: p.Product,
			Quantity:    qty,
			Metadata:    map[string]string{"Reason": advice.Reason},
		}
		placed, err := e.Broker.PlaceOrder(counter)
		if err != nil {
			log.Printf("ERROR: Failed to close %s on strategy exit advice: %v", advice.Symbol, err)
			if e.Journal != nil {
				e.Journal.LogOrder(counter, "FAILED", err.Error())
			}
			continue
		}
		log.Printf(">>> STRATEGY EXIT: %s | %s", advice.Symbol, advice.Reason)
		if e.Journal != nil {
			e.Journal.LogOrder(placed, "SUCCESS", advice.Reason)
		}
		if e.DB != nil {
			_ = e.DB.SaveOrder(placed, "SUBMITTED")
		}
		e.forgetTrackedOrder(advice.Symbol)
	}
}

// forgetTrackedOrder drops a symbol from the tick-driven stop manager once its
// position is gone, so a later position in the same symbol does not inherit the
// previous trade's trail anchor.
func (e *Engine) forgetTrackedOrder(symbol string) {
	e.openOrdersMu.Lock()
	delete(e.openOrders, symbol)
	delete(e.lastStopPush, symbol)
	e.openOrdersMu.Unlock()
}

// stopAfterTick returns where the tracked order's stop should rest given the
// latest traded price, whether that differs from the stop currently at the
// broker, and whether the change is urgent (a one-off breakeven move rather than
// an incremental trail step — urgent moves skip the push throttle).
//
// It mutates only TrailAnchor, which is a high-water mark and correct to advance
// whether or not the broker call that follows succeeds.
func stopAfterTick(o *models.Order, price decimal.Decimal) (stop decimal.Decimal, changed, urgent bool) {
	stop = o.StopLoss

	// A stop is never placed on the wrong side of the market: a long's stop
	// above the last trade (or a short's below it) is not a stop but a limit
	// order, and it would be filled at a price the market has not reached.
	// See broker.stopNoBetterThanMarket for the same rule in the simulator.
	capToMarket := func(candidate decimal.Decimal) decimal.Decimal {
		if o.Side == models.BuySignal {
			return decimal.Min(candidate, price)
		}
		return decimal.Max(candidate, price)
	}

	if o.Side == models.BuySignal {
		// Partial-exit breakeven (the original ORB behaviour): once the
		// partial level trades, the remainder's stop goes to entry.
		if !o.BreakevenApplied && o.PartialExitPrice.IsPositive() && price.GreaterThanOrEqual(o.PartialExitPrice) && o.Price.GreaterThan(stop) {
			stop, urgent = capToMarket(o.Price), true
		}
		if !o.BreakevenApplied && o.BreakevenTrigger.IsPositive() && price.GreaterThanOrEqual(o.BreakevenTrigger) && o.BreakevenStop.GreaterThan(stop) {
			stop, urgent = capToMarket(o.BreakevenStop), true
		}
		if o.TrailDistance.IsPositive() {
			if price.GreaterThan(o.TrailAnchor) {
				o.TrailAnchor = price
			}
			if trailed := capToMarket(o.TrailAnchor.Sub(o.TrailDistance)); trailed.GreaterThan(stop) {
				stop = trailed
			}
		}
		return stop, stop.GreaterThan(o.StopLoss), urgent
	}

	improves := func(candidate decimal.Decimal) bool {
		return stop.IsZero() || candidate.LessThan(stop)
	}
	if !o.BreakevenApplied && o.PartialExitPrice.IsPositive() && price.LessThanOrEqual(o.PartialExitPrice) && improves(o.Price) {
		stop, urgent = capToMarket(o.Price), true
	}
	if !o.BreakevenApplied && o.BreakevenTrigger.IsPositive() && price.LessThanOrEqual(o.BreakevenTrigger) && improves(o.BreakevenStop) {
		stop, urgent = capToMarket(o.BreakevenStop), true
	}
	if o.TrailDistance.IsPositive() {
		if o.TrailAnchor.IsZero() || price.LessThan(o.TrailAnchor) {
			o.TrailAnchor = price
		}
		if trailed := capToMarket(o.TrailAnchor.Add(o.TrailDistance)); improves(trailed) {
			stop = trailed
		}
	}
	return stop, !stop.Equal(o.StopLoss), urgent
}

// OnTick manages the protective stop of a live position against the traded
// price: the BREAKEVEN+ move and the chandelier trail both have to react
// intracandle rather than waiting for the 5-minute bar to close, so this is
// called per tick from the live handler. It is a no-op for symbols with no
// tracked order.
//
// The simulator does not use this path — SimBroker.CheckExits advances the same
// stops bar by bar — so backtest and live agree on the rules but not on the
// resolution at which they are applied. Live reacts sooner; a backtest number is
// therefore the conservative one.
func (e *Engine) OnTick(symbol string, price decimal.Decimal) {
	// A broker holding its own resting stops and targets has no other way to
	// learn that a price traded. This runs ahead of the trail logic below and
	// for every symbol, not just tracked ones: a position whose strategy sets
	// no trail still has a stop that must be able to fire.
	if observer, ok := e.Broker.(TickObserver); ok {
		observer.OnTick(symbol, price, time.Now())
	}

	e.openOrdersMu.Lock()
	order, ok := e.openOrders[symbol]
	if !ok {
		e.openOrdersMu.Unlock()
		return
	}
	newStop, changed, urgent := stopAfterTick(order, price)
	throttled := !urgent && time.Since(e.lastStopPush[symbol]) < trailPushInterval
	snapshot := *order
	e.openOrdersMu.Unlock()

	if !changed || throttled {
		return
	}

	if err := e.Broker.ModifyPositionStop(snapshot, newStop); err != nil {
		log.Printf("ERROR: Failed to move %s stop to %s: %v", symbol, newStop, err)
		return
	}

	e.openOrdersMu.Lock()
	order.StopLoss = newStop
	if urgent {
		order.BreakevenApplied = true
	}
	e.lastStopPush[symbol] = time.Now()
	e.openOrdersMu.Unlock()

	if e.Journal != nil {
		e.Journal.LogOrder(snapshot, "STOP-MOVED", fmt.Sprintf("SL moved to %s at price %s", newStop, price))
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
		placedOrder, err := e.Broker.PlaceOrder(order)
		if err != nil {
			log.Printf("SquareOff Error: Failed to close position %s: %v", p.Tradingsymbol, err)
			if e.Journal != nil {
				e.Journal.LogOrder(order, "FAILED", err.Error())
			}
			if e.DB != nil {
				_ = e.DB.SaveOrder(order, "FAILED")
			}
		} else {
			log.Printf("SquareOff: Successfully submitted close order for %s", p.Tradingsymbol)
			if e.Journal != nil {
				e.Journal.LogOrder(placedOrder, "SUCCESS", fmt.Sprintf("OrderID: %s | Reason: AutoSquareOff", placedOrder.ID))
			}
			if e.DB != nil {
				_ = e.DB.SaveOrder(placedOrder, "SUBMITTED")
			}
		}
	}

	log.Println("⚡ AUTO SQUAREOFF SEQUENCE COMPLETED ⚡")
}
