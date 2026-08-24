package strategy

import (
	"errors"
	"fmt"
	"log"
	"time"

	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/options"

	"github.com/shopspring/decimal"
)

// Option execution for the Donchian strategy.
//
// The signal is computed on the index chart and the position is taken in a
// weekly option. That split has one consequence that shapes everything here:
// **no resting order on the option can express a level on the index.** A stop
// at "NIFTY below 24,200" is not a premium level — it moves with time, with
// volatility, and with the index itself. So when option execution is on, the
// strategy stops delegating its stop and trail to the broker and tracks them
// itself against the index, closing the option at market through ExitAdvice.
//
// The engine needs no changes for this. It already type-asserts for
// core.ExitAdvisor and calls it ahead of every entry gate, and the sizer
// already rounds to whole lots from Signal.LotSize.
//
// In backtest mode (no executor injected) none of this runs and the simulator
// keeps handling stops intrabar, so the recorded index-leg results stay
// reproducible.

// OptionExecutor turns a directional index signal into the contract to buy.
// pkg/options implements the selection; this is the seam so the strategy does
// not depend on an instrument dump or a quote feed.
type OptionExecutor interface {
	// Select returns the contract expressing a signal on `underlying` at
	// `spot`, as of `now`. isCall is true for a long index signal.
	Select(underlying string, spot decimal.Decimal, now time.Time, isCall bool) (options.Contract, error)
	// Premium is the current traded price of a contract, used as the signal's
	// entry price and to size the position.
	Premium(c options.Contract) (decimal.Decimal, error)
}

// SetOptionExecution switches the strategy from trading the signal instrument
// to trading an option on it.
//
// Passing nil (the default, and what cmd/backtest does) leaves the strategy
// trading the index symbol directly, which is what every recorded backtest
// measures.
func (s *Donchian) SetOptionExecution(exec OptionExecutor) {
	s.optionExec = exec
}

// optionLeg is what the strategy remembers about an open option position so it
// can close the right instrument when the INDEX says to.
type optionLeg struct {
	contract options.Contract
	// indexEntry is the index price the signal fired at; indexStop is the
	// current stop, which ratchets with the chandelier trail; best is the most
	// favourable index price seen since entry.
	indexEntry decimal.Decimal
	indexStop  decimal.Decimal
	best       decimal.Decimal
	trail      decimal.Decimal
	side       models.SignalType
}

// buildOptionSignal converts an index entry into an order for a contract.
// Returns nil when no tradeable contract could be chosen — including the
// routine "too close to expiry" case, which is a no-trade, not a failure.
func (s *Donchian) buildOptionSignal(candle models.Candle, st *donchianState, index *models.Signal,
	atr decimal.Decimal) *models.Signal {

	isCall := index.Type == models.BuySignal
	contract, err := s.optionExec.Select(candle.Symbol, candle.Close, candle.StartTime, isCall)
	if err != nil {
		if errors.Is(err, options.ErrTooCloseToExpiry) {
			log.Printf("[%s] Donchian: skipping entry — %v", candle.Symbol, err)
		} else {
			log.Printf("[%s] Donchian: no option contract for this signal: %v", candle.Symbol, err)
		}
		return nil
	}

	premium, err := s.optionExec.Premium(contract)
	if err != nil || !premium.IsPositive() {
		log.Printf("[%s] Donchian: no premium for %s (%v) — skipping entry",
			candle.Symbol, contract.TradingSymbol, err)
		return nil
	}

	// Sizing needs risk per unit in rupees of premium. The real exit is the
	// index level below; this is a delta approximation for the sizer only.
	years := options.YearsToExpiry(candle.StartTime, contract.Expiry)
	delta := 0.5
	if iv, ok := s.impliedVolFor(contract, premium, candle.Close, years); ok {
		delta = options.Delta(candle.Close.InexactFloat64(),
			contract.Strike.InexactFloat64(), years, iv, contract.IsCall)
	}
	premiumStop := options.PremiumStop(premium, delta, index.Price, index.StopLoss)

	trail := index.TrailDistance
	st.leg = &optionLeg{
		contract:   contract,
		indexEntry: index.Price,
		indexStop:  index.StopLoss,
		best:       index.Price,
		trail:      trail,
		side:       index.Type,
	}

	log.Printf("[%s] Donchian: %s %s @ %s (delta %.2f, index %s, index stop %s, trail %s)",
		candle.Symbol, index.Type, contract.TradingSymbol, premium.StringFixed(2),
		delta, index.Price.StringFixed(2), index.StopLoss.StringFixed(2), trail.StringFixed(2))

	return &models.Signal{
		Symbol: contract.TradingSymbol,
		// Always a BUY: a long index view buys a call and a short view buys a
		// put. The strategy is never short an option.
		Type:        models.BuySignal,
		Price:       premium,
		StopLoss:    premiumStop,
		RiskPct:     index.RiskPct,
		LotSize:     contract.LotSize,
		Exchange:    contract.Exchange,
		ProductType: "MIS",
		Metadata: map[string]string{
			"Strategy":     s.Name(),
			"Underlying":   candle.Symbol,
			"IndexEntry":   index.Price.StringFixed(2),
			"IndexStop":    index.StopLoss.StringFixed(2),
			"IndexTrail":   trail.StringFixed(2),
			"IndexSide":    index.Type.String(),
			"ATR":          atr.StringFixed(2),
			"Expiry":       contract.Expiry.Format("2006-01-02"),
			"DaysToExpiry": fmt.Sprintf("%d", options.DaysToExpiry(candle.StartTime, contract.Expiry)),
			"Reason":       index.Metadata["Reason"],
		},
	}
}

func (s *Donchian) impliedVolFor(c options.Contract, premium, spot decimal.Decimal, years float64) (float64, bool) {
	return options.ImpliedVol(premium.InexactFloat64(), spot.InexactFloat64(),
		c.Strike.InexactFloat64(), years, c.IsCall)
}

// optionExitAdvice runs the stop and the chandelier trail against the INDEX and
// closes the option when either is breached.
//
// This is deliberately evaluated on bar closes rather than intrabar. A live
// stop on the index would need tick-by-tick evaluation of one instrument to
// place a market order in another, and the round trip through the option book
// makes the difference between a tick and a bar close largely notional. It is
// also the pessimistic choice, which is the right way to be wrong.
func (s *Donchian) optionExitAdvice(candle models.Candle, st *donchianState) *core.ExitAdvice {
	leg := st.leg
	if leg == nil {
		return nil
	}

	// Ratchet the trail from the best index price seen since entry.
	if leg.trail.IsPositive() {
		if leg.side == models.BuySignal {
			if candle.High.GreaterThan(leg.best) {
				leg.best = candle.High
			}
			if candidate := leg.best.Sub(leg.trail); candidate.GreaterThan(leg.indexStop) {
				leg.indexStop = candidate
			}
		} else {
			if candle.Low.LessThan(leg.best) {
				leg.best = candle.Low
			}
			if candidate := leg.best.Add(leg.trail); candidate.LessThan(leg.indexStop) {
				leg.indexStop = candidate
			}
		}
	}

	hit := leg.side == models.BuySignal && candle.Close.LessThanOrEqual(leg.indexStop) ||
		leg.side == models.SellSignal && candle.Close.GreaterThanOrEqual(leg.indexStop)
	if !hit {
		return nil
	}

	symbol := leg.contract.TradingSymbol
	reason := fmt.Sprintf("index %s %s through stop %s",
		candle.Symbol, candle.Close.StringFixed(2), leg.indexStop.StringFixed(2))

	st.leg = nil
	st.openValid = false
	st.exitAdvisedAt = candle.StartTime

	return &core.ExitAdvice{
		// The option is what gets closed, always — never the index.
		Symbol:  symbol,
		ForSide: models.BuySignal,
		Reason:  "Donchian index stop: " + reason,
	}
}

// optionSymbolFor returns the instrument an opposite-band exit should close.
// Without option execution that is the signal instrument itself.
func (st *donchianState) optionSymbolFor(fallback string) (symbol string, side models.SignalType) {
	if st.leg != nil {
		return st.leg.contract.TradingSymbol, models.BuySignal
	}
	return fallback, st.openSide
}
