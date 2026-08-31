package indicators

import (
	"github.com/shopspring/decimal"
)

// PivotMethod selects how a session's support and resistance levels are derived
// from the previous session's high, low and close.
//
// Both methods share the same central pivot and differ only in how far the
// bands are placed from it, so a run of one is directly comparable with a run
// of the other on the same bars — which is the point: which of the two the
// market actually respects is an empirical question, not a preference.
type PivotMethod string

const (
	// PivotTraditional is the floor-trader formula. Its bands are reflections
	// of the previous high and low about the pivot, so their spacing is
	// asymmetric whenever the previous close sat away from the range midpoint.
	PivotTraditional PivotMethod = "traditional"
	// PivotFibonacci places the bands at fixed 0.382 / 0.618 / 1.000 fractions
	// of the previous range either side of the pivot, so they are symmetric by
	// construction and, at R1/S1, sit closer in than the traditional ones.
	PivotFibonacci PivotMethod = "fibonacci"
)

// PivotLevels is one session's level set. Valid is false when the previous
// session's bar was degenerate (no range, or a non-positive price), in which
// case none of the numbers mean anything and callers must not trade them.
type PivotLevels struct {
	PP decimal.Decimal
	R1 decimal.Decimal
	R2 decimal.Decimal
	R3 decimal.Decimal
	S1 decimal.Decimal
	S2 decimal.Decimal
	S3 decimal.Decimal

	Valid bool
}

// ComputePivots derives one session's levels from the previous session's
// high, low and close.
//
// An unrecognised method falls back to traditional rather than returning an
// invalid set: a typo in a config key must not silently disable the strategy
// (CLAUDE.md records two separate cases where a knob quietly stopped a whole
// backtest from trading and nothing in the output said so).
func ComputePivots(method PivotMethod, high, low, close decimal.Decimal) PivotLevels {
	rng := high.Sub(low)
	if !rng.IsPositive() || !close.IsPositive() {
		return PivotLevels{}
	}

	three := decimal.NewFromInt(3)
	two := decimal.NewFromInt(2)
	pp := high.Add(low).Add(close).Div(three)

	lv := PivotLevels{PP: pp, Valid: true}

	if method == PivotFibonacci {
		f382 := rng.Mul(decimal.NewFromFloat(0.382))
		f618 := rng.Mul(decimal.NewFromFloat(0.618))
		lv.R1, lv.S1 = pp.Add(f382), pp.Sub(f382)
		lv.R2, lv.S2 = pp.Add(f618), pp.Sub(f618)
		lv.R3, lv.S3 = pp.Add(rng), pp.Sub(rng)
		return lv
	}

	lv.R1 = pp.Mul(two).Sub(low)
	lv.S1 = pp.Mul(two).Sub(high)
	lv.R2 = pp.Add(rng)
	lv.S2 = pp.Sub(rng)
	lv.R3 = high.Add(two.Mul(pp.Sub(low)))
	lv.S3 = low.Sub(two.Mul(high.Sub(pp)))
	return lv
}

// All returns the level set as a slice for iteration, PP included, in no
// particular price order.
func (p PivotLevels) All() []decimal.Decimal {
	if !p.Valid {
		return nil
	}
	return []decimal.Decimal{p.S3, p.S2, p.S1, p.PP, p.R1, p.R2, p.R3}
}

// Names pairs with All, in the same order, for logging and trade metadata.
func (p PivotLevels) Names() []string {
	if !p.Valid {
		return nil
	}
	return []string{"S3", "S2", "S1", "PP", "R1", "R2", "R3"}
}
