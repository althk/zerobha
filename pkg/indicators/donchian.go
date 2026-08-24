package indicators

import (
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// Donchian tracks the highest high and lowest low of the last `period`
// completed bars, deliberately EXCLUDING the bar being evaluated.
//
// That exclusion is the whole point for a breakout strategy: a channel that
// included the current bar could never be broken, because the bar's own high
// would always be the channel top. Update therefore returns the channel as it
// stood *before* this candle, then folds the candle in for the next call.
//
// The window is a fixed ring buffer and each Update rescans it. That is O(period)
// rather than the O(1) the other indicators here manage, but period is ~20 and a
// monotonic-deque implementation has to carry two sets of index bookkeeping that
// silently break on the wrap; 20 comparisons per candle per symbol is not worth
// that risk.
type Donchian struct {
	period int
	highs  []decimal.Decimal
	lows   []decimal.Decimal
	next   int // ring cursor: where the next candle is written
	count  int // bars ingested, capped at period
}

func NewDonchian(period int) *Donchian {
	return &Donchian{
		period: period,
		highs:  make([]decimal.Decimal, period),
		lows:   make([]decimal.Decimal, period),
	}
}

// Update returns the (upper, lower) channel over the `period` bars preceding
// this candle, then ingests the candle. Both values are zero until the window
// is full — callers must check IsReady rather than comparing against zero, a
// price is never below a zero lower band.
func (d *Donchian) Update(candle models.Candle) (upper, lower decimal.Decimal) {
	upper, lower = d.Value()

	d.highs[d.next] = candle.High
	d.lows[d.next] = candle.Low
	d.next = (d.next + 1) % d.period
	if d.count < d.period {
		d.count++
	}

	return upper, lower
}

// Value returns the current channel without ingesting a candle.
func (d *Donchian) Value() (upper, lower decimal.Decimal) {
	if !d.IsReady() {
		return decimal.Zero, decimal.Zero
	}
	upper, lower = d.highs[0], d.lows[0]
	for i := 1; i < d.period; i++ {
		if d.highs[i].GreaterThan(upper) {
			upper = d.highs[i]
		}
		if d.lows[i].LessThan(lower) {
			lower = d.lows[i]
		}
	}
	return upper, lower
}

func (d *Donchian) IsReady() bool { return d.count >= d.period }
