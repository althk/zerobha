package main

import (
	"math"
	"testing"
)

func testCosts(lots int) costModel {
	return costModel{
		Lots:              lots,
		BrokeragePerOrder: 20,
		STTPct:            0.15,
		TxnPct:            0.03553,
		StampPct:          0.0025,
		GSTPct:            18,
		SEBIPct:           0.0001,
	}
}

// Brokerage is a flat rupee charge per order, so the cost of a round trip must
// NOT be ten times larger for ten lots. This is the whole reason -lots exists:
// position size changes the edge, it is not a presentational multiplier.
func TestChargesGrowSlowerThanPositionSize(t *testing.T) {
	const buy, sell, lot = 210.0, 215.0, 65

	one := testCosts(1).charges(buy, sell, lot)
	ten := testCosts(10).charges(buy, sell, lot)

	if ten >= one*10 {
		t.Fatalf("ten lots cost %.2f, one lot %.2f — flat brokerage is not being amortised", ten, one)
	}
	// Everything but the two flat 20-rupee charges scales exactly, so ten lots
	// must cost ten times the variable part plus one set of brokerage+GST.
	wantVariable := (one - 40*1.18) * 10
	if got := ten - 40*1.18; math.Abs(got-wantVariable) > 0.01 {
		t.Errorf("variable component: got %.2f, want %.2f", got, wantVariable)
	}
}

// The cost of a round trip as a fraction of the position must fall as size
// rises, and converge on the percentage-only components.
func TestCostsPerRupeeFallWithSize(t *testing.T) {
	const buy, sell, lot = 210.0, 215.0, 65

	prev := math.Inf(1)
	for _, lots := range []int{1, 2, 5, 10, 25, 50} {
		c := testCosts(lots)
		bps := c.charges(buy, sell, lot) / (buy * float64(lots*lot)) * 1e4
		if bps >= prev {
			t.Errorf("%d lots: %.1f bps did not improve on %.1f", lots, bps, prev)
		}
		prev = bps
	}
	// With brokerage amortised to nothing, what remains is STT (10 bps, sell
	// leg) + transaction (7) + stamp + GST — under 20 bps for a round trip.
	//
	// That floor is worth knowing: it is far below the ~50 bps a 0.25%
	// half-spread costs on both legs, so at any size worth trading the bid-ask
	// is the dominant cost and the statutory charges are the small part.
	if prev < 15 || prev > 25 {
		t.Errorf("large-size cost floor is %.1f bps, expected the percentage components (~20)", prev)
	}
}

// STT is charged on the sell leg only; a trade that exits higher must pay more
// than the same trade exiting lower.
func TestSTTChargedOnTheSellLegOnly(t *testing.T) {
	c := testCosts(1)
	up := c.charges(200, 250, 65)
	down := c.charges(200, 150, 65)
	if !(up > down) {
		t.Errorf("exit at 250 cost %.2f, exit at 150 cost %.2f — sell-side STT is not being applied", up, down)
	}
}

// The cost sheet is pinned to a worked example from Zerodha's own brokerage
// calculator, so a stale statutory rate fails here instead of silently shifting
// every net figure in the backtest.
//
//	equity options, buy 100, sell 110, qty 400 (turnover 84,000)
//	brokerage 40, STT 66, exchange txn 29.85, GST 12.59, SEBI 0.08,
//	stamp 1  ->  total 149.52
func TestChargesMatchZerodhaCalculator(t *testing.T) {
	c := testCosts(1)
	c.BrokeragePerOrder = 20
	got := c.charges(100, 110, 400)
	const want = 149.52
	if math.Abs(got-want) > 1.0 {
		t.Errorf("round trip cost %.2f, Zerodha calculator says %.2f", got, want)
	}
}
