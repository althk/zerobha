package core

import (
	"log"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// SymbolPnLReporter is an optional Broker capability: the day's PnL split by
// traded symbol, for the per-stock daily limit. Brokers without it are read
// through GetPositions, whose per-row PnL carries the same thing for Kite and
// the paper broker; the simulator, whose position book holds only open lots,
// implements this.
type SymbolPnLReporter interface {
	DailyPnLBySymbol() (map[string]decimal.Decimal, error)
}

// signalUnderlying is the instrument a signal's risk counts against: the
// index for an option leg, the symbol itself for anything traded directly.
func signalUnderlying(s *models.Signal) string {
	if u := s.Metadata["Underlying"]; u != "" {
		return u
	}
	return s.Symbol
}

// rememberUnderlying records which underlying a traded symbol belongs to, so
// a later signal can find the position's PnL under it.
func (e *Engine) rememberUnderlying(symbol, underlying string) {
	e.underlyingMu.Lock()
	defer e.underlyingMu.Unlock()
	e.underlyings[symbol] = underlying
}

// underlyingOf maps a traded symbol to its underlying. Kite's position book
// does not say which index an option contract expresses, so after a restart
// the answer comes from the order journal; a symbol with no recorded
// underlying is its own.
func (e *Engine) underlyingOf(symbol string) string {
	e.underlyingMu.Lock()
	u, ok := e.underlyings[symbol]
	e.underlyingMu.Unlock()
	if ok {
		return u
	}
	u = symbol
	if e.DB != nil {
		meta, err := e.DB.LatestOrderMetadata(symbol)
		if err != nil {
			log.Printf("WARNING: could not look up the underlying of %s: %v", symbol, err)
			return symbol // not cached: retry next time
		}
		if got := meta["Underlying"]; got != "" {
			u = got
		}
	}
	e.rememberUnderlying(symbol, u)
	return u
}

// underlyingDayPnL sums today's PnL on every symbol that belongs to
// underlying, whichever strategy traded it.
func (e *Engine) underlyingDayPnL(underlying string, positions []models.Position) (decimal.Decimal, error) {
	total := decimal.Zero
	if r, ok := e.Broker.(SymbolPnLReporter); ok {
		bySymbol, err := r.DailyPnLBySymbol()
		if err != nil {
			return decimal.Zero, err
		}
		for sym, pnl := range bySymbol {
			if e.underlyingOf(sym) == underlying {
				total = total.Add(pnl)
			}
		}
		return total, nil
	}
	for _, p := range positions {
		u := p.Underlying
		if u == "" {
			u = e.underlyingOf(p.Tradingsymbol)
		}
		if u == underlying {
			total = total.Add(p.PnL)
		}
	}
	return total, nil
}

// isPositionOpen asks the broker, failing towards "open": a position whose
// state cannot be read keeps counting against the risk budget.
func (e *Engine) isPositionOpen(symbol string) bool {
	open, err := e.Broker.HasOpenPosition(symbol)
	return err != nil || open
}

// monthPnL is the month's PnL so far: realised PnL of the month's earlier
// sessions from the trade journal, plus today's as the broker reports it.
// The earlier sessions cannot change intraday, so they are read once a day.
// Without a store (the backtester) only today is known.
func (e *Engine) monthPnL(day decimal.Decimal, at time.Time) (decimal.Decimal, error) {
	if e.DB == nil {
		return day, nil
	}
	y, m, d := at.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, at.Location())
	key := today.Format("2006-01-02")
	if e.monthBaseDate != key {
		monthStart := time.Date(y, m, 1, 0, 0, 0, 0, at.Location())
		base, err := e.DB.RealisedPnL(monthStart, today, e.PaperMode)
		if err != nil {
			return decimal.Zero, err
		}
		e.monthBase, e.monthBaseDate = base, key
	}
	return e.monthBase.Add(day), nil
}

// riskPerUnit is what one unit loses if the signal's stop is hit. A signal
// without a stop risks its whole price, which is exact for a bought option.
func riskPerUnit(s *models.Signal) decimal.Decimal {
	if s.StopLoss.IsZero() {
		return s.Price
	}
	return s.Price.Sub(s.StopLoss).Abs()
}

// capQuantityToRisk shrinks qty so qty x risk-per-unit fits in budget,
// rounding down to whole lots when the signal carries a lot size.
func capQuantityToRisk(qty decimal.Decimal, s *models.Signal, budget decimal.Decimal) decimal.Decimal {
	per := riskPerUnit(s)
	if !per.IsPositive() {
		return qty
	}
	maxQty := budget.Div(per).Floor()
	if s.LotSize > 1 {
		lot := decimal.NewFromInt(int64(s.LotSize))
		maxQty = maxQty.Div(lot).Floor().Mul(lot)
	}
	return decimal.Min(qty, maxQty)
}
