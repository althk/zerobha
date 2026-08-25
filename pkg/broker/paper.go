package broker

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"zerobha/internal/models"
	"zerobha/pkg/db"
	"zerobha/pkg/nseutils"

	"github.com/shopspring/decimal"
)

// paperQuoteInterval is how often the monitor refreshes a quote for a symbol
// that a resting protective order depends on but the tick feed does not cover.
//
// The live ticker only subscribes to the watchlist, so an option contract the
// strategy bought is never ticked — and that is exactly the instrument whose
// stop still has to be able to fire. Polling closes that hole.
const paperQuoteInterval = 5 * time.Second

// paperQuoteWarnInterval throttles the "could not refresh quote" warning to one
// line per symbol per minute. A symbol the quote API cannot resolve would
// otherwise fill the log at the poll rate.
const paperQuoteWarnInterval = time.Minute

// paperMinGapPct mirrors the broker-side rule in buildOCOGTTParams and
// buildStopGTTParams: Kite rejects a GTT trigger closer than 0.25% to the last
// price, so both live builders widen the level to 0.3%. Paper applies the same
// adjustment, or its stops would rest where a live stop could never be placed.
const paperMinGapPct = 0.003

// paperPosition is one symbol's simulated book. Quantity is signed: positive is
// long, negative short, and zero means closed-but-traded-today — such a
// position is kept rather than deleted so its realised PnL still has somewhere
// to live. See GetPositions.
type paperPosition struct {
	Symbol        string          `json:"symbol"`
	Exchange      string          `json:"exchange"`
	Product       string          `json:"product"`
	Strategy      string          `json:"strategy"`
	Quantity      int             `json:"quantity"`
	AveragePrice  decimal.Decimal `json:"average_price"`
	LastPrice     decimal.Decimal `json:"last_price"`
	MarginPerUnit decimal.Decimal `json:"margin_per_unit"`
	RealizedPnL   decimal.Decimal `json:"realized_pnl"`
}

// paperSnapshot is the persisted form of the whole broker. The encoding lives
// here rather than in pkg/db so the simulated book can grow a field without a
// schema migration.
type paperSnapshot struct {
	Cash      decimal.Decimal           `json:"cash"`
	Margin    decimal.Decimal           `json:"margin_blocked"`
	Realized  decimal.Decimal           `json:"realized_pnl"`
	Positions map[string]*paperPosition `json:"positions"`
	Resting   map[string]*models.Order  `json:"resting"`
	Orders    []models.Order            `json:"orders"`
	Trades    []models.Order            `json:"trades"`
	OrderSeq  int64                     `json:"order_seq"`
	GTTSeq    int                       `json:"gtt_seq"`
}

// PaperAdapter provides simulated trade execution against live market data.
// It reads real quotes and candles through the underlying ZerodhaAdapter but
// fills orders virtually, against virtual capital, without ever reaching the
// exchange.
//
// It is deliberately more than a fill stub. Live, a position is protected by a
// GTT resting at the exchange, and every strategy here except Donchian's option
// mode exits solely through that GTT. A paper broker that only recorded fills
// would leave every simulated position running to the 15:13 square-off, and
// would therefore measure a different strategy from the one being run. So this
// holds the stop and target itself and fills them from the price feed.
//
// The division of responsibility matches live exactly: the engine decides where
// the stop belongs (stopAfterTick applies the breakeven and chandelier rules,
// then calls ModifyPositionStop) and the broker decides when it is hit.
type PaperAdapter struct {
	live     *ZerodhaAdapter
	leverage func(symbol string) float64
	store    *db.Store

	mu        sync.Mutex
	cash      decimal.Decimal
	margin    decimal.Decimal
	realized  decimal.Decimal
	positions map[string]*paperPosition
	resting   map[string]*models.Order
	orders    []models.Order
	trades    []models.Order
	orderSeq  int64
	gttSeq    int
	lastSeen  map[string]time.Time
	lastWarn  map[string]time.Time

	tradeDate string
	done      chan struct{}
	stopOnce  sync.Once
	monitorWG sync.WaitGroup
}

// PaperOption configures a PaperAdapter at construction.
type PaperOption func(*PaperAdapter)

// WithPaperLeverage supplies the MIS leverage lookup the engine uses for
// sizing, so the simulated margin requirement matches the one the real account
// would face. Without it every product is treated as fully paid, which rejects
// leveraged positions the live account would have accepted.
func WithPaperLeverage(fn func(symbol string) float64) PaperOption {
	return func(p *PaperAdapter) { p.leverage = fn }
}

// WithPaperStore persists the simulated book across a restart. Paper state is
// otherwise memory-only, so a container restart mid-session would come back
// flat, with full virtual capital, while the day's positions were still open.
func WithPaperStore(s *db.Store) PaperOption {
	return func(p *PaperAdapter) { p.store = s }
}

// NewPaperAdapter initialises the paper broker with virtual starting capital,
// restoring today's book from the store when one is configured and a snapshot
// for the current trading date exists.
func NewPaperAdapter(live *ZerodhaAdapter, initialCapital decimal.Decimal, opts ...PaperOption) *PaperAdapter {
	p := &PaperAdapter{
		live:      live,
		cash:      initialCapital,
		positions: make(map[string]*paperPosition),
		resting:   make(map[string]*models.Order),
		orders:    make([]models.Order, 0),
		trades:    make([]models.Order, 0),
		lastSeen:  make(map[string]time.Time),
		lastWarn:  make(map[string]time.Time),
		tradeDate: nseutils.MarketOpenTime(time.Now()).Format("2006-01-02"),
		done:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	p.restore()
	return p
}

// Start launches the quote monitor. It is separate from construction so that
// tests, and any caller that drives prices itself, run without a background
// goroutine or a live connection.
func (p *PaperAdapter) Start() {
	if p.live == nil {
		return
	}
	p.monitorWG.Add(1)
	go p.monitorLoop()
}

// Close stops the monitor and flushes state. Safe to call more than once.
func (p *PaperAdapter) Close() error {
	p.stopOnce.Do(func() { close(p.done) })
	p.monitorWG.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persistLocked()
	return nil
}

// --- Balance and quotes ---

// GetBalance returns free virtual cash: starting capital, less the margin
// blocked by open positions, plus realised PnL. It is what the sizer divides
// between remaining slots, so it must exclude capital already committed.
func (p *PaperAdapter) GetBalance() (decimal.Decimal, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cash, nil
}

// GetQuote fetches a live market quote. Paper trades on real prices; only the
// fills are simulated.
func (p *PaperAdapter) GetQuote(symbol string) (decimal.Decimal, error) {
	if p.live == nil {
		return decimal.Zero, fmt.Errorf("paper broker: no live quote provider configured")
	}
	return p.live.GetQuote(symbol)
}

// History fetches historical candles from the live provider.
func (p *PaperAdapter) History(symbol string, timeframe string, days int) ([]models.Candle, error) {
	if p.live == nil {
		return nil, fmt.Errorf("paper broker: no live data provider configured")
	}
	return p.live.History(symbol, timeframe, days)
}

// --- Positions ---

// HasOpenPosition reports whether a simulated position is currently open.
func (p *PaperAdapter) HasOpenPosition(symbol string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos, ok := p.positions[symbol]
	return ok && pos.Quantity != 0, nil
}

// GetPositions returns the day's position book in the shape Kite returns it:
// open positions carry a non-zero NetQuantity and mark-to-market PnL, and
// positions closed earlier today stay in the list with NetQuantity 0 and their
// realised PnL. computeSummary splits realised from unrealised on exactly that
// distinction, so dropping the closed rows would report realised PnL as zero
// for the whole session.
//
// It performs no network I/O. Marks come from the last observed price, which
// the tick feed and the monitor keep current; quoting here instead would put a
// rate-limited HTTP call per position on the candle hot path, under the lock.
func (p *PaperAdapter) GetPositions() ([]models.Position, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	result := make([]models.Position, 0, len(p.positions))
	for _, pos := range p.positions {
		if pos.Quantity == 0 && pos.RealizedPnL.IsZero() {
			continue
		}
		result = append(result, models.Position{
			Tradingsymbol: pos.Symbol,
			Exchange:      pos.Exchange,
			Product:       pos.Product,
			Quantity:      pos.Quantity,
			NetQuantity:   pos.Quantity,
			AveragePrice:  pos.AveragePrice,
			LastPrice:     pos.LastPrice,
			PnL:           positionPnL(pos),
			Strategy:      pos.Strategy,
		})
	}
	return result, nil
}

// positionPnL is mark-to-market for an open position and realised PnL for a
// closed one, which is how a broker reports each.
func positionPnL(pos *paperPosition) decimal.Decimal {
	if pos.Quantity == 0 || !pos.LastPrice.IsPositive() {
		return pos.RealizedPnL
	}
	unrealized := pos.LastPrice.Sub(pos.AveragePrice).Mul(decimal.NewFromInt(int64(pos.Quantity)))
	return unrealized.Add(pos.RealizedPnL)
}

// --- Order execution ---

// PlaceOrder fills a simulated order at the given price, the last observed
// price, or a live quote — in that order of preference. When the order carries
// a stop it also arms a resting protective order, exactly as the live adapter
// places a GTT after the entry fills.
func (p *PaperAdapter) PlaceOrder(order models.Order) (models.Order, error) {
	// Tag the order before any early return: a rejected paper order must not be
	// journalled or persisted as a live one.
	order.IsPaper = true

	qty := int(order.Quantity.IntPart())
	if qty <= 0 {
		return order, fmt.Errorf("paper broker: non-positive quantity %s for %s", order.Quantity, order.Symbol)
	}

	fillPrice, err := p.fillPriceFor(order)
	if err != nil {
		return order, err
	}

	signed := qty
	if order.Side == models.SellSignal {
		signed = -qty
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	realized, err := p.applyFillLocked(order, signed, fillPrice)
	if err != nil {
		return order, err
	}

	p.orderSeq++
	order.ID = fmt.Sprintf("%s%06d", db.PaperOrderPrefix, p.orderSeq)
	order.Status = models.OrderFilled
	order.Price = fillPrice
	if order.Timestamp.IsZero() {
		order.Timestamp = time.Now()
	}
	if !realized.IsZero() {
		if order.Metadata == nil {
			order.Metadata = map[string]string{}
		}
		order.Metadata["PaperPnL"] = realized.StringFixed(2)
	}

	pos := p.positions[order.Symbol]
	// Arm the protective order only on a fill that opened or increased the
	// position, and only when a stop was supplied — the same condition the live
	// adapter uses before placing a GTT.
	if pos.Quantity != 0 && sameSign(pos.Quantity, signed) && !order.StopLoss.IsZero() {
		order.GTTTriggerID = p.armProtectiveLocked(order, pos, fillPrice)
	}

	p.orders = append(p.orders, order)
	p.trades = append(p.trades, order)
	p.persistLocked()

	log.Printf("[PAPER] FILLED %s %s qty=%d @ %s | margin blocked Rs %s | free cash Rs %s",
		order.Side, order.Symbol, qty, fillPrice.StringFixed(2),
		p.margin.StringFixed(2), p.cash.StringFixed(2))

	return order, nil
}

// fillPriceFor resolves the price to fill at without holding the lock, because
// the last step may be a rate-limited network call.
//
// The last-observed-price fallback matters for the square-off path: the engine
// builds its counter order with no price, and GetQuote cannot resolve every
// instrument the strategy can trade (it tries NSE then NFO, so a BFO contract
// fails). A position the broker has been marking all session always has a
// price to close at.
func (p *PaperAdapter) fillPriceFor(order models.Order) (decimal.Decimal, error) {
	if order.Price.IsPositive() {
		return order.Price, nil
	}

	p.mu.Lock()
	if pos, ok := p.positions[order.Symbol]; ok && pos.LastPrice.IsPositive() {
		last := pos.LastPrice
		p.mu.Unlock()
		return last, nil
	}
	p.mu.Unlock()

	if p.live == nil {
		return decimal.Zero, fmt.Errorf("paper broker: no price available for %s", order.Symbol)
	}
	quote, err := p.live.GetQuote(order.Symbol)
	if err != nil {
		return decimal.Zero, fmt.Errorf("paper broker: unable to determine fill price for %s: %w", order.Symbol, err)
	}
	if !quote.IsPositive() {
		return decimal.Zero, fmt.Errorf("paper broker: non-positive quote for %s", order.Symbol)
	}
	return quote, nil
}

// applyFillLocked applies a signed fill to the position book and returns the
// PnL it realised.
//
// Margin, not notional, is what a fill consumes: the engine sizes with MIS
// leverage, so debiting the full notional would reject positions the real
// account takes. Both sides block margin — a short releasing cash would let the
// balance grow with every short and inflate the size of the next trade.
//
// It validates before it mutates, so a rejected fill leaves the book untouched.
func (p *PaperAdapter) applyFillLocked(order models.Order, signed int, price decimal.Decimal) (decimal.Decimal, error) {
	// Resolved but not yet inserted: a rejected fill must leave no trace, not
	// even an empty position row.
	pos := p.positions[order.Symbol]
	fresh := pos == nil
	if fresh {
		pos = &paperPosition{
			Symbol:   order.Symbol,
			Exchange: order.Exchange,
			Product:  order.ProductType,
		}
	}
	if pos.Strategy == "" {
		pos.Strategy = order.Metadata["Strategy"]
	}

	perUnit := p.marginPerUnit(order, price)

	// How much of this fill closes existing exposure, and how much opens new.
	closeQty, openQty := 0, abs(signed)
	if pos.Quantity != 0 && !sameSign(pos.Quantity, signed) {
		closeQty = min(abs(pos.Quantity), abs(signed))
		openQty = abs(signed) - closeQty
	}

	// Validate first: a reversal that cannot fund its new leg must not leave the
	// book half-updated.
	if openQty > 0 {
		released := decimal.Zero
		if closeQty > 0 {
			released = pos.MarginPerUnit.Mul(decimal.NewFromInt(int64(closeQty)))
		}
		needed := perUnit.Mul(decimal.NewFromInt(int64(openQty)))
		available := p.cash.Add(released)
		if available.LessThan(needed) {
			return decimal.Zero, fmt.Errorf(
				"paper broker: insufficient virtual margin for %s (need Rs %s, free Rs %s)",
				order.Symbol, needed.StringFixed(2), available.StringFixed(2))
		}
	}

	if fresh {
		p.positions[order.Symbol] = pos
	}

	realized := decimal.Zero
	if closeQty > 0 {
		closeDec := decimal.NewFromInt(int64(closeQty))
		// A long realises exit minus entry, a short the reverse.
		if pos.Quantity > 0 {
			realized = price.Sub(pos.AveragePrice).Mul(closeDec)
			pos.Quantity -= closeQty
		} else {
			realized = pos.AveragePrice.Sub(price).Mul(closeDec)
			pos.Quantity += closeQty
		}
		release := pos.MarginPerUnit.Mul(closeDec)
		p.margin = p.margin.Sub(release)
		p.cash = p.cash.Add(release).Add(realized)
		p.realized = p.realized.Add(realized)
		pos.RealizedPnL = pos.RealizedPnL.Add(realized)
		if pos.Quantity == 0 {
			pos.AveragePrice = decimal.Zero
			pos.MarginPerUnit = decimal.Zero
		}
	}

	if openQty > 0 {
		openDec := decimal.NewFromInt(int64(openQty))
		needed := perUnit.Mul(openDec)
		p.cash = p.cash.Sub(needed)
		p.margin = p.margin.Add(needed)

		// Weighted average over absolute quantities. The previous
		// implementation added an unsigned cost regardless of side, which
		// corrupted the basis on any reduce or reversal.
		prevQty := decimal.NewFromInt(int64(abs(pos.Quantity)))
		newQty := prevQty.Add(openDec)
		pos.AveragePrice = pos.AveragePrice.Mul(prevQty).Add(price.Mul(openDec)).Div(newQty)
		pos.MarginPerUnit = pos.MarginPerUnit.Mul(prevQty).Add(perUnit.Mul(openDec)).Div(newQty)

		if signed > 0 {
			pos.Quantity += openQty
		} else {
			pos.Quantity -= openQty
		}
	}

	pos.LastPrice = price
	p.lastSeen[order.Symbol] = time.Now()

	// A flat position has nothing left to protect; a reduced one is still
	// protected, but for less than it was.
	if pos.Quantity == 0 {
		delete(p.resting, order.Symbol)
	} else if prot, ok := p.resting[order.Symbol]; ok {
		prot.Quantity = decimal.NewFromInt(int64(abs(pos.Quantity)))
	}

	return realized, nil
}

// marginPerUnit is the virtual capital one unit blocks. MIS positions get the
// same leverage the engine sized them with; everything else — a bought option
// above all, which is paid for in full — blocks the whole price.
func (p *PaperAdapter) marginPerUnit(order models.Order, price decimal.Decimal) decimal.Decimal {
	if order.ProductType != "MIS" || p.leverage == nil {
		return price
	}
	lev := p.leverage(order.Symbol)
	if lev <= 1 {
		return price
	}
	return price.Div(decimal.NewFromFloat(lev))
}

// --- Resting protective orders ---

// armProtectiveLocked records the stop and target that protect a position and
// returns the synthetic trigger id. The id is what tells the engine this broker
// accepted a protective order, which is how the tick-driven breakeven and trail
// monitor arms — the same signal a real GTT trigger id carries.
func (p *PaperAdapter) armProtectiveLocked(order models.Order, pos *paperPosition, fillPrice decimal.Decimal) int {
	prot := order
	prot.Price = fillPrice
	prot.Quantity = decimal.NewFromInt(int64(abs(pos.Quantity)))
	prot.TrailAnchor = fillPrice
	prot.StopLoss, prot.Target = applyPaperMinGap(order.Side, fillPrice, order.StopLoss, order.Target)

	p.gttSeq++
	prot.GTTTriggerID = p.gttSeq
	p.resting[order.Symbol] = &prot

	log.Printf("[PAPER] PROTECT %s stop=%s target=%s (trigger %d)",
		order.Symbol, prot.StopLoss.StringFixed(2), prot.Target.StringFixed(2), prot.GTTTriggerID)
	return prot.GTTTriggerID
}

// applyPaperMinGap widens a stop or target that sits closer to the fill than
// the exchange would accept, mirroring the live GTT builders. A zero target is
// left at zero: it means "no target", and reading it as a level at price 0 is a
// bug this codebase has already had once.
func applyPaperMinGap(side models.SignalType, fill, stop, target decimal.Decimal) (decimal.Decimal, decimal.Decimal) {
	gap := fill.Mul(decimal.NewFromFloat(paperMinGapPct))

	if side == models.BuySignal {
		if stop.IsPositive() && fill.Sub(stop).LessThan(gap) {
			stop = fill.Sub(gap)
		}
		if target.IsPositive() && target.Sub(fill).LessThan(gap) {
			target = fill.Add(gap)
		}
		return stop, target
	}

	if stop.IsPositive() && stop.Sub(fill).LessThan(gap) {
		stop = fill.Add(gap)
	}
	if target.IsPositive() && fill.Sub(target).LessThan(gap) {
		target = fill.Sub(gap)
	}
	return stop, target
}

// OnTick records a traded price and fires any resting order it has hit. It
// implements core.TickObserver, which the engine calls for every tick.
func (p *PaperAdapter) OnTick(symbol string, price decimal.Decimal, at time.Time) {
	if !price.IsPositive() {
		return
	}

	p.mu.Lock()
	p.lastSeen[symbol] = time.Now()
	pos, ok := p.positions[symbol]
	if !ok {
		p.mu.Unlock()
		return
	}
	pos.LastPrice = price

	prot, armed := p.resting[symbol]
	if !armed || pos.Quantity == 0 {
		p.mu.Unlock()
		return
	}

	reason, hit := protectiveHit(prot, price)
	if !hit {
		p.mu.Unlock()
		return
	}

	delete(p.resting, symbol)
	side := models.BuySignal
	if pos.Quantity < 0 {
		side = models.SellSignal
	}
	p.mu.Unlock()

	// closePosition re-acquires the lock; the resting order is already removed,
	// so a second tick arriving in between cannot fire the same stop twice.
	if _, err := p.ClosePosition(symbol, side, price, at, reason); err != nil {
		log.Printf("[PAPER] ERROR closing %s on %s: %v", symbol, reason, err)
	}
}

// protectiveHit reports whether price has reached the resting stop or target.
//
// The target is only consulted when positive. A strategy running a pure
// trailing exit leaves Target zero deliberately, and treating that as a level
// would exit every position on its first tick.
func protectiveHit(prot *models.Order, price decimal.Decimal) (string, bool) {
	if prot.Side == models.BuySignal {
		if prot.StopLoss.IsPositive() && price.LessThanOrEqual(prot.StopLoss) {
			return "SL-HIT", true
		}
		if prot.Target.IsPositive() && price.GreaterThanOrEqual(prot.Target) {
			return "TARGET-HIT", true
		}
		return "", false
	}
	if prot.StopLoss.IsPositive() && price.GreaterThanOrEqual(prot.StopLoss) {
		return "SL-HIT", true
	}
	if prot.Target.IsPositive() && price.LessThanOrEqual(prot.Target) {
		return "TARGET-HIT", true
	}
	return "", false
}

// monitorLoop refreshes quotes for symbols the tick feed is not covering, so
// that a stop on an unsubscribed instrument — an option contract, above all —
// can still fire.
func (p *PaperAdapter) monitorLoop() {
	defer p.monitorWG.Done()

	ticker := time.NewTicker(paperQuoteInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			for _, symbol := range p.staleSymbols() {
				quote, err := p.live.GetQuote(symbol)
				if err != nil || !quote.IsPositive() {
					p.warnStaleQuote(symbol, err)
					continue
				}
				p.OnTick(symbol, quote, time.Now())
			}
		}
	}
}

// staleSymbols lists open positions whose last observed price is older than the
// poll interval. Symbols the ticker covers refresh continuously and never
// appear here, so the monitor spends its rate limit only where it is needed.
func (p *PaperAdapter) staleSymbols() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stale []string
	cutoff := time.Now().Add(-paperQuoteInterval)
	for symbol, pos := range p.positions {
		if pos.Quantity == 0 {
			continue
		}
		if seen, ok := p.lastSeen[symbol]; ok && seen.After(cutoff) {
			continue
		}
		stale = append(stale, symbol)
	}
	return stale
}

// warnStaleQuote reports a mark that could not be refreshed, at most once a
// minute per symbol. Silence here would be the dangerous outcome: a position
// marked at a stale price is indistinguishable from one that has not moved, and
// its stop cannot fire while the price feed is dead.
func (p *PaperAdapter) warnStaleQuote(symbol string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if last, ok := p.lastWarn[symbol]; ok && time.Since(last) < paperQuoteWarnInterval {
		return
	}
	p.lastWarn[symbol] = time.Now()
	log.Printf("[PAPER] WARNING: cannot refresh quote for %s — its mark is stale and its stop cannot fire: %v", symbol, err)
}

// --- Exits ---

// ClosePosition implements core.PositionCloser.
//
// forSide names the side of the position the caller means to close, and is
// enforced: a long-exit advice must never close a short that happens to be open
// in the same symbol. The strategy cannot see whether its entry was filled, so
// the side is the only thing tying its advice to a real position.
func (p *PaperAdapter) ClosePosition(symbol string, forSide models.SignalType, price decimal.Decimal, timestamp time.Time, reason string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pos, ok := p.positions[symbol]
	if !ok || pos.Quantity == 0 {
		return false, nil
	}
	if (forSide == models.BuySignal && pos.Quantity < 0) ||
		(forSide == models.SellSignal && pos.Quantity > 0) {
		return false, nil
	}
	if !price.IsPositive() {
		return false, fmt.Errorf("paper broker: refusing to close %s at non-positive price", symbol)
	}

	qty := abs(pos.Quantity)
	qtyDec := decimal.NewFromInt(int64(qty))

	exitSide := models.SellSignal
	if pos.Quantity < 0 {
		exitSide = models.BuySignal
	}

	// Route through the same fill path as any other order so margin release,
	// realised PnL and the average-price rules have exactly one implementation.
	exit := models.Order{
		Symbol:      symbol,
		Side:        exitSide,
		Type:        "MARKET",
		ProductType: pos.Product,
		Exchange:    pos.Exchange,
		Quantity:    qtyDec,
		Metadata:    map[string]string{"Strategy": pos.Strategy, "Reason": reason},
	}
	signed := -pos.Quantity

	realized, err := p.applyFillLocked(exit, signed, price)
	if err != nil {
		return false, err
	}

	p.orderSeq++
	exit.ID = fmt.Sprintf("%sEXIT-%06d", db.PaperOrderPrefix, p.orderSeq)
	exit.Status = models.OrderFilled
	exit.Price = price
	exit.IsPaper = true
	exit.Timestamp = timestamp
	if exit.Timestamp.IsZero() {
		exit.Timestamp = time.Now()
	}
	exit.Metadata["PaperPnL"] = realized.StringFixed(2)

	p.orders = append(p.orders, exit)
	p.trades = append(p.trades, exit)
	p.persistLocked()

	log.Printf("[PAPER] CLOSED %s %s x%d @ %s | realised Rs %s | %s | free cash Rs %s",
		exitSide, symbol, qty, price.StringFixed(2), realized.StringFixed(2), reason, p.cash.StringFixed(2))

	return true, nil
}

// ModifyPositionStop moves the resting stop, which is how the engine's
// breakeven and chandelier trail reach this broker — the same call it makes
// against a live GTT.
//
// The new level is capped at the last observed price. A stop parked beyond the
// market is not a stop but a limit order, and filling one at a price that never
// printed is how this codebase once manufactured a profitable backtest.
func (p *PaperAdapter) ModifyPositionStop(order models.Order, newStopLoss decimal.Decimal) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	prot, ok := p.resting[order.Symbol]
	if !ok {
		return fmt.Errorf("paper broker: no resting order for %s", order.Symbol)
	}
	if !newStopLoss.IsPositive() {
		return fmt.Errorf("paper broker: refusing non-positive stop for %s", order.Symbol)
	}

	capped := newStopLoss
	if pos, ok := p.positions[order.Symbol]; ok && pos.LastPrice.IsPositive() {
		capped = stopNoBetterThanMarket(prot.Side, newStopLoss, pos.LastPrice)
	}
	prot.StopLoss = capped
	p.persistLocked()
	return nil
}

// --- GTT surface ---

// GetGTTs exposes the resting protective orders in the shape SquareOff expects,
// so its cancel-everything-first step works against paper as it does live.
func (p *PaperAdapter) GetGTTs() ([]models.GTT, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	gtts := make([]models.GTT, 0, len(p.resting))
	for _, prot := range p.resting {
		kind := "single"
		triggers := []float64{prot.StopLoss.InexactFloat64()}
		if prot.Target.IsPositive() {
			kind = "two-leg"
			triggers = append(triggers, prot.Target.InexactFloat64())
		}
		gtts = append(gtts, models.GTT{
			ID:            prot.GTTTriggerID,
			Tradingsymbol: prot.Symbol,
			Exchange:      prot.Exchange,
			Type:          kind,
			Product:       prot.ProductType,
			Status:        "active",
			Condition: models.GTTCondition{
				Exchange:      prot.Exchange,
				Tradingsymbol: prot.Symbol,
				LastPrice:     prot.Price,
				TriggerValues: triggers,
			},
		})
	}
	return gtts, nil
}

// CancelGTT removes a resting protective order by trigger id.
func (p *PaperAdapter) CancelGTT(triggerID int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for symbol, prot := range p.resting {
		if prot.GTTTriggerID == triggerID {
			delete(p.resting, symbol)
			p.persistLocked()
			return nil
		}
	}
	return fmt.Errorf("paper broker: no resting order with trigger id %d", triggerID)
}

// --- Order book ---

// GetTrades returns every simulated fill, entries and exits alike, which is
// what the dashboard's FIFO reconciler pairs into round trips.
func (p *PaperAdapter) GetTrades() ([]models.Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]models.Order, len(p.trades))
	copy(out, p.trades)
	return out, nil
}

// GetOpenOrders returns nothing: every paper order fills immediately, so none
// is ever pending.
func (p *PaperAdapter) GetOpenOrders() ([]models.Order, error) {
	return []models.Order{}, nil
}

// CancelOrder has nothing to cancel, for the same reason.
func (p *PaperAdapter) CancelOrder(orderID string) error {
	return nil
}

// --- Persistence ---

// persistLocked writes the book to the store. It is called on state changes
// only — never on a price observation, which would put an SQLite write on the
// tick path.
func (p *PaperAdapter) persistLocked() {
	if p.store == nil {
		return
	}
	blob, err := json.Marshal(paperSnapshot{
		Cash:      p.cash,
		Margin:    p.margin,
		Realized:  p.realized,
		Positions: p.positions,
		Resting:   p.resting,
		Orders:    p.orders,
		Trades:    p.trades,
		OrderSeq:  p.orderSeq,
		GTTSeq:    p.gttSeq,
	})
	if err != nil {
		log.Printf("[PAPER] WARNING: failed to encode state: %v", err)
		return
	}
	if err := p.store.SavePaperState(p.tradeDate, blob); err != nil {
		log.Printf("[PAPER] WARNING: failed to persist state: %v", err)
	}
}

// restore reloads today's book. Only the current trading date is considered —
// yesterday's positions must never reappear in a new session, and an
// intraday strategy has none to carry anyway.
func (p *PaperAdapter) restore() {
	if p.store == nil {
		return
	}
	blob, err := p.store.LoadPaperState(p.tradeDate)
	if err != nil {
		log.Printf("[PAPER] WARNING: failed to read saved state: %v", err)
		return
	}
	if len(blob) == 0 {
		return
	}

	var snap paperSnapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		log.Printf("[PAPER] WARNING: failed to decode saved state, starting fresh: %v", err)
		return
	}

	p.cash = snap.Cash
	p.margin = snap.Margin
	p.realized = snap.Realized
	if snap.Positions != nil {
		p.positions = snap.Positions
	}
	if snap.Resting != nil {
		p.resting = snap.Resting
	}
	if snap.Orders != nil {
		p.orders = snap.Orders
	}
	if snap.Trades != nil {
		p.trades = snap.Trades
	}
	p.orderSeq = snap.OrderSeq
	p.gttSeq = snap.GTTSeq

	open := 0
	for _, pos := range p.positions {
		if pos.Quantity != 0 {
			open++
		}
	}
	log.Printf("[PAPER] Restored %s: free cash Rs %s, margin Rs %s, realised Rs %s, %d open position(s)",
		p.tradeDate, p.cash.StringFixed(2), p.margin.StringFixed(2), p.realized.StringFixed(2), open)
}

// --- helpers ---

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func sameSign(a, b int) bool {
	return (a > 0 && b > 0) || (a < 0 && b < 0)
}
