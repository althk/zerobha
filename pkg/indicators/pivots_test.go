package indicators

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

func eq(t *testing.T, name string, got decimal.Decimal, want float64) {
	t.Helper()
	if !got.Round(6).Equal(d(want).Round(6)) {
		t.Errorf("%s = %s, want %v", name, got.String(), want)
	}
}

// Hand-computed floor-trader pivots for a session of H 110 / L 90 / C 100:
// PP 100, R1 110, S1 90, R2 120, S2 80, R3 130, S3 70.
func TestTraditionalPivots(t *testing.T) {
	p := ComputePivots(PivotTraditional, d(110), d(90), d(100))
	if !p.Valid {
		t.Fatal("levels reported invalid for a normal session")
	}
	eq(t, "PP", p.PP, 100)
	eq(t, "R1", p.R1, 110)
	eq(t, "S1", p.S1, 90)
	eq(t, "R2", p.R2, 120)
	eq(t, "S2", p.S2, 80)
	eq(t, "R3", p.R3, 130)
	eq(t, "S3", p.S3, 70)
}

// The fibonacci bands are fixed fractions of the previous range either side of
// the same pivot, so they are symmetric where the traditional ones need not be.
func TestFibonacciPivots(t *testing.T) {
	p := ComputePivots(PivotFibonacci, d(110), d(90), d(100))
	if !p.Valid {
		t.Fatal("levels reported invalid for a normal session")
	}
	eq(t, "PP", p.PP, 100)
	eq(t, "R1", p.R1, 107.64)
	eq(t, "S1", p.S1, 92.36)
	eq(t, "R2", p.R2, 112.36)
	eq(t, "S2", p.S2, 87.64)
	eq(t, "R3", p.R3, 120)
	eq(t, "S3", p.S3, 80)

	// Symmetry is the defining property of this set, and it is what makes the
	// two methods worth measuring against each other rather than assuming.
	if !p.R1.Sub(p.PP).Equal(p.PP.Sub(p.S1)) {
		t.Error("fibonacci R1/S1 are not symmetric about the pivot")
	}
}

// A session with no range yields levels that are all the same number, which is
// not a level set. Trading it would fire on every bar, so it must be rejected
// rather than returned.
func TestDegenerateSessionIsInvalid(t *testing.T) {
	if p := ComputePivots(PivotTraditional, d(100), d(100), d(100)); p.Valid {
		t.Error("a zero-range session produced valid levels")
	}
	if p := ComputePivots(PivotTraditional, d(110), d(90), d(0)); p.Valid {
		t.Error("a zero close produced valid levels")
	}
	if got := (PivotLevels{}).All(); got != nil {
		t.Errorf("All() on an invalid set returned %v, want nil", got)
	}
}

// An unrecognised method falls back to traditional. A typo in a config key must
// not silently disable the strategy: CLAUDE.md records two separate cases where
// a knob quietly stopped a backtest from trading and nothing said so.
func TestUnknownMethodFallsBackToTraditional(t *testing.T) {
	got := ComputePivots(PivotMethod("nonsense"), d(110), d(90), d(100))
	want := ComputePivots(PivotTraditional, d(110), d(90), d(100))
	if !got.Valid {
		t.Fatal("unknown method produced an invalid set")
	}
	// decimal.Decimal carries an exponent, so two equal numbers need not be
	// equal structs — compare value by value.
	gotAll, wantAll := got.All(), want.All()
	for i := range wantAll {
		if !gotAll[i].Equal(wantAll[i]) {
			t.Errorf("%s = %s, want %s", want.Names()[i], gotAll[i], wantAll[i])
		}
	}
}

// All and Names must stay index-aligned — the strategy reads them in lockstep
// to label each level, and a drift would mislabel every trade's metadata.
func TestAllAndNamesAlign(t *testing.T) {
	p := ComputePivots(PivotTraditional, d(110), d(90), d(100))
	prices, names := p.All(), p.Names()
	if len(prices) != len(names) {
		t.Fatalf("All() has %d entries, Names() has %d", len(prices), len(names))
	}
	for i := 1; i < len(prices); i++ {
		if !prices[i].GreaterThan(prices[i-1]) {
			t.Errorf("All() is not ascending at %d (%s then %s)", i, names[i-1], names[i])
		}
	}
	if names[3] != "PP" {
		t.Errorf("middle level is %q, want PP", names[3])
	}
}
