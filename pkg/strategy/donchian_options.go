package strategy

import (
	"zerobha/internal/core"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// Option execution for the Donchian strategy. The mechanics — contract
// selection, premium sizing, the index-side stop and chandelier trail — live
// in option_leg.go and are shared with EMACross; this file is only the
// Donchian-specific state plumbing.

// SetOptionExecution switches the strategy from trading the signal instrument
// to trading an option on it.
//
// Passing nil (the default, and what cmd/backtest does) leaves the strategy
// trading the index symbol directly, which is what every recorded backtest
// measures.
func (s *Donchian) SetOptionExecution(exec OptionExecutor) {
	s.optionExec = exec
}

// buildOptionSignal converts an index entry into an order for a contract and
// records the leg. Returns nil when no tradeable contract could be chosen.
func (s *Donchian) buildOptionSignal(candle models.Candle, st *donchianState, index *models.Signal,
	atr decimal.Decimal) *models.Signal {
	sig, leg := buildOptionLeg(s.optionExec, "Donchian", candle, index, atr)
	if sig == nil {
		return nil
	}
	st.leg = leg
	return sig
}

// optionExitAdvice closes the option when the INDEX closes through the leg's
// stop or trail.
func (s *Donchian) optionExitAdvice(candle models.Candle, st *donchianState) *core.ExitAdvice {
	leg := st.leg
	if leg == nil {
		return nil
	}
	hit, reason := leg.stopHit(candle)
	if !hit {
		return nil
	}

	st.leg = nil
	st.openValid = false
	st.exitAdvisedAt = candle.StartTime

	return leg.closeAdvice("Donchian index stop: " + reason)
}

// optionSymbolFor returns the instrument an opposite-band exit should close.
// Without option execution that is the signal instrument itself.
func (st *donchianState) optionSymbolFor(fallback string) (symbol string, side models.SignalType) {
	if st.leg != nil {
		return st.leg.contract.TradingSymbol, models.BuySignal
	}
	return fallback, st.openSide
}
