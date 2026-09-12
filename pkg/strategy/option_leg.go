package strategy

import (
	"errors"
	"fmt"
	"log"
	"time"

	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/db"
	"zerobha/pkg/options"

	"github.com/shopspring/decimal"
)

// Option execution shared by every strategy whose signal is computed on an
// index chart and expressed through a weekly option (Donchian, EMACross).
//
// The split has one consequence that shapes everything here: **no resting
// order on the option can express a level on the index.** A stop at "NIFTY
// below 24,200" is not a premium level — it moves with time, with volatility,
// and with the index itself. So when option execution is on, the strategy
// stops delegating its stop and trail to the broker, tracks them itself
// against the index, and closes the option at market through ExitAdvice.
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

// optionLeg is what a strategy remembers about an open option position so it
// can close the right instrument when the INDEX says to.
type optionLeg struct {
	contract options.Contract
	// indexEntry is the index price the signal fired at; indexStop is the
	// current stop, which ratchets with the chandelier trail when one is set;
	// best is the most favourable index price seen since entry.
	indexEntry decimal.Decimal
	indexStop  decimal.Decimal
	best       decimal.Decimal
	trail      decimal.Decimal
	side       models.SignalType
}

// declineRecorder persists a view the strategy could not express as an
// order. The engine never sees a declined signal, so without this the
// dashboard's funnel would show a quiet market where the option layer was
// in fact refusing every entry.
type declineRecorder struct {
	store *db.Store
	paper bool
}

func (r declineRecorder) record(symbol, strategy string, side models.SignalType, reason string) {
	if r.store == nil {
		return
	}
	if err := r.store.SaveDeclinedSignal(symbol, strategy, side.String(), reason, r.paper); err != nil {
		log.Printf("[%s] %s: failed to record declined signal: %v", symbol, strategy, err)
	}
}

// buildOptionLeg converts an index entry into an order for a contract and the
// leg that tracks it. Returns nil when no tradeable contract could be chosen —
// including the routine "too close to expiry" case, which is a no-trade, not a
// failure. stratName labels the log lines and the signal metadata; declines
// records the refusals.
func buildOptionLeg(exec OptionExecutor, stratName string, candle models.Candle,
	index *models.Signal, atr decimal.Decimal, declines declineRecorder) (*models.Signal, *optionLeg) {

	isCall := index.Type == models.BuySignal
	contract, err := exec.Select(candle.Symbol, candle.Close, candle.StartTime, isCall)
	if err != nil {
		if errors.Is(err, options.ErrTooCloseToExpiry) {
			log.Printf("[%s] %s: skipping entry — %v", candle.Symbol, stratName, err)
			declines.record(candle.Symbol, stratName, index.Type, "too close to expiry")
		} else {
			log.Printf("[%s] %s: no option contract for this signal: %v", candle.Symbol, stratName, err)
			declines.record(candle.Symbol, stratName, index.Type, "no contract: "+err.Error())
		}
		return nil, nil
	}

	premium, err := exec.Premium(contract)
	if err != nil || !premium.IsPositive() {
		log.Printf("[%s] %s: no premium for %s (%v) — skipping entry",
			candle.Symbol, stratName, contract.TradingSymbol, err)
		declines.record(candle.Symbol, stratName, index.Type, "no premium for "+contract.TradingSymbol)
		return nil, nil
	}

	// Sizing needs risk per unit in rupees of premium. The real exit is the
	// index level below; this is a delta approximation for the sizer only.
	years := options.YearsToExpiry(candle.StartTime, contract.Expiry)
	delta := 0.5
	if iv, ok := options.ImpliedVol(premium.InexactFloat64(), candle.Close.InexactFloat64(),
		contract.Strike.InexactFloat64(), years, contract.IsCall); ok {
		delta = options.Delta(candle.Close.InexactFloat64(),
			contract.Strike.InexactFloat64(), years, iv, contract.IsCall)
	}
	premiumStop := options.PremiumStop(premium, delta, index.Price, index.StopLoss)

	trail := index.TrailDistance
	leg := &optionLeg{
		contract:   contract,
		indexEntry: index.Price,
		indexStop:  index.StopLoss,
		best:       index.Price,
		trail:      trail,
		side:       index.Type,
	}

	log.Printf("[%s] %s: %s %s @ %s (delta %.2f, index %s, index stop %s, trail %s)",
		candle.Symbol, stratName, index.Type, contract.TradingSymbol, premium.StringFixed(2),
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
			"Strategy":     stratName,
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
	}, leg
}

// stopHit runs the stop and the chandelier trail against the INDEX bar and
// reports whether the leg should be closed, with the reason.
//
// This is deliberately evaluated on bar closes rather than intrabar. A live
// stop on the index would need tick-by-tick evaluation of one instrument to
// place a market order in another, and the round trip through the option book
// makes the difference between a tick and a bar close largely notional. It is
// also the pessimistic choice, which is the right way to be wrong.
func (leg *optionLeg) stopHit(candle models.Candle) (bool, string) {
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
		return false, ""
	}
	return true, fmt.Sprintf("index %s %s through stop %s",
		candle.Symbol, candle.Close.StringFixed(2), leg.indexStop.StringFixed(2))
}

// report is the leg as the dashboard sees it.
func (leg *optionLeg) report(underlying string) core.OpenLeg {
	side := "LONG"
	if leg.side == models.SellSignal {
		side = "SHORT"
	}
	return core.OpenLeg{
		Symbol:     leg.contract.TradingSymbol,
		Underlying: underlying,
		Side:       side,
		IndexEntry: leg.indexEntry,
		IndexStop:  leg.indexStop,
		IndexBest:  leg.best,
	}
}

// closeAdvice is the ExitAdvice that flattens this leg. The option is what
// gets closed, always — never the index — and it is always a long.
func (leg *optionLeg) closeAdvice(reason string) *core.ExitAdvice {
	return &core.ExitAdvice{
		Symbol:  leg.contract.TradingSymbol,
		ForSide: models.BuySignal,
		Reason:  reason,
	}
}
