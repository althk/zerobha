package core

import (
	"log"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// Runner is one strategy trading inside the engine, with the entry settings
// that belong to the strategy rather than to the account. The account-wide
// parts — broker, risk limits, capital, the global concurrency cap and the
// square-off — stay on the Engine and are shared by every runner.
type Runner struct {
	Strategy Strategy
	// Timeframe is the candle.Timeframe this runner consumes ("5m0s" from the
	// live aggregator). Empty consumes every timeframe.
	Timeframe string
	// Symbols is the runner's watchlist. Nil consumes every symbol.
	Symbols map[string]bool
	// TradeCutoffMin: no new entries from candles ending at or after this
	// minute of the day.
	TradeCutoffMin int
	// UptrendOnly gates this runner's entries on the NIFTY 50 uptrend filter.
	UptrendOnly bool
	// MaxCapitalPerTrade overrides the engine's cap when positive.
	MaxCapitalPerTrade int64
	// MaxConcurrent caps this runner's own open positions when several
	// runners share the account; the engine's MaxConcurrent caps them all.
	MaxConcurrent int
	// CapitalPct is this runner's share of the account, in percent. Zero
	// means no share: it sizes against the whole balance. See
	// capitalShareFree.
	CapitalPct float64
}

// Name identifies the runner, and is the owner tag on its orders.
func (r *Runner) Name() string { return r.Strategy.Name() }

func (r *Runner) wants(c models.Candle) bool {
	if r.Timeframe != "" && c.Timeframe != r.Timeframe {
		return false
	}
	return r.Symbols == nil || r.Symbols[c.Symbol]
}

// runners returns the configured runners, or in single-strategy mode one
// built from the engine's own fields, so callers that set Strategy and the
// entry settings directly (the backtester, most tests) behave as before.
func (e *Engine) runners() []*Runner {
	if len(e.Runners) > 0 {
		return e.Runners
	}
	return []*Runner{{
		Strategy:           e.Strategy,
		TradeCutoffMin:     e.TradeCutoffMin,
		UptrendOnly:        e.UptrendOnly,
		MaxCapitalPerTrade: e.MaxCapitalPerTrade,
		MaxConcurrent:      e.MaxConcurrent,
	}}
}

// Strategies lists every strategy the engine runs, in priority order.
func (e *Engine) Strategies() []Strategy {
	rs := e.runners()
	out := make([]Strategy, 0, len(rs))
	for _, r := range rs {
		if r.Strategy != nil {
			out = append(out, r.Strategy)
		}
	}
	return out
}

func (e *Engine) anyUptrendFilter() bool {
	for _, r := range e.runners() {
		if r.UptrendOnly {
			return true
		}
	}
	return false
}

func (e *Engine) rememberOwner(symbol, owner string) {
	e.underlyingMu.Lock()
	defer e.underlyingMu.Unlock()
	e.owners[symbol] = owner
}

// ownerOf names the strategy whose order opened the latest position in
// symbol, or "" when no strategy did (a manual trade, or one from before
// owners were recorded). After a restart the answer comes from the order
// journal, which keeps the engine's Owner tag in the order metadata.
func (e *Engine) ownerOf(symbol string) string {
	e.underlyingMu.Lock()
	o, ok := e.owners[symbol]
	e.underlyingMu.Unlock()
	if ok {
		return o
	}
	if e.DB == nil {
		return ""
	}
	meta, err := e.DB.LatestOrderMetadata(symbol)
	if err != nil {
		log.Printf("WARNING: could not look up the owner of %s: %v", symbol, err)
		return "" // not cached: retry next time
	}
	o = meta["Owner"]
	e.rememberOwner(symbol, o)
	return o
}

// underlyingHeldByOther returns the strategy that holds an open position on
// underlying, if it is not `self`. A position with no recorded owner counts as
// someone else's: the engine does not trade into exposure it cannot account
// for.
func (e *Engine) underlyingHeldByOther(underlying, self string, positions []models.Position) string {
	for _, p := range positions {
		if p.NetQuantity == 0 {
			continue
		}
		u := p.Underlying
		if u == "" {
			u = e.underlyingOf(p.Tradingsymbol)
		}
		if u != underlying {
			continue
		}
		if owner := e.ownerOf(p.Tradingsymbol); owner != self {
			if owner == "" {
				return "an untracked position"
			}
			return owner
		}
	}
	return ""
}

// capitalShareFree is how much of its share a runner may still deploy:
//
//	(free balance + capital tied up in every open position) x share
//	  - capital tied up in the runner's own open positions
//
// The account value is rebuilt from the balance plus open positions because
// the balance alone shrinks as positions open, which would shrink every
// strategy's share whenever any one of them traded. Capital tied up in a
// position is what it blocks: quantity x average price over the MIS leverage
// the engine sized it with (1 for an option bought outright).
func (e *Engine) capitalShareFree(r *Runner, balance decimal.Decimal, positions []models.Position, shared bool) decimal.Decimal {
	all, own := decimal.Zero, decimal.Zero
	for _, p := range positions {
		if p.NetQuantity == 0 {
			continue
		}
		used := decimal.NewFromInt(int64(p.NetQuantity)).Abs().Mul(p.AveragePrice)
		if p.Product == "MIS" {
			if lev, ok := e.LeverageMap[p.Tradingsymbol]; ok && lev > 0 {
				used = used.Div(decimal.NewFromFloat(lev))
			}
		}
		all = all.Add(used)
		if !shared || e.ownerOf(p.Tradingsymbol) == r.Name() {
			own = own.Add(used)
		}
	}
	share := balance.Add(all).Mul(decimal.NewFromFloat(r.CapitalPct / 100))
	return share.Sub(own)
}
