package indicators

import "github.com/shopspring/decimal"

type EMA struct {
	period      int
	k           decimal.Decimal // The smoothing factor (multiplier)
	current     decimal.Decimal
	initialized bool
	steps       int // Tracks how many data points we've seen
}

func NewEMA(period int) *EMA {
	// k = 2 / (period + 1)
	p := decimal.NewFromInt(int64(period))
	k := decimal.NewFromInt(2).Div(p.Add(decimal.NewFromInt(1)))

	return &EMA{
		period: period,
		k:      k,
	}
}

func (e *EMA) Update(price decimal.Decimal) decimal.Decimal {
	if !e.initialized {
		// Initialization: The first value of an EMA is usually just the price
		// (or SMA of first N periods, but simple seeding works for streaming)
		if e.steps == 0 {
			e.current = price.Round(4)
		} else {
			// SMA seeding logic could go here, but for simplicity we start evolving immediately
			// EMA = (Price - Prev) * k + Prev
			e.current = price.Sub(e.current).Mul(e.k).Add(e.current).Round(4)
		}

		e.steps++
		// We consider it "stable" enough after 'period' steps
		if e.steps >= e.period {
			e.initialized = true
		}
		return e.current
	}

	// Standard Formula: (Price * k) + (Prev * (1-k))
	// Optimization: Prev + k * (Price - Prev)
	delta := price.Sub(e.current)
	e.current = e.current.Add(delta.Mul(e.k)).Round(4)

	return e.current
}

func (e *EMA) Value() decimal.Decimal {
	return e.current
}

func (e *EMA) IsReady() bool {
	return e.initialized
}
