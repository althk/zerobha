package indicators

import "github.com/shopspring/decimal"

type RSI struct {
	period      int
	avgGain     decimal.Decimal
	avgLoss     decimal.Decimal
	prevPrice   decimal.Decimal
	initialized bool
	count       int // To handle the first N periods (SMA phase)
}

func NewRSI(period int) *RSI {
	return &RSI{
		period: period,
	}
}

func (r *RSI) Update(price decimal.Decimal) decimal.Decimal {
	if !r.initialized {
		// Just store the first price and wait for the second
		if r.count == 0 {
			r.prevPrice = price
			r.count++
			return decimal.Zero // RSI undefined for first candle
		}
	}

	// 1. Calculate Change
	change := price.Sub(r.prevPrice)
	r.prevPrice = price

	var gain, loss decimal.Decimal
	if change.GreaterThan(decimal.Zero) {
		gain = change
	} else {
		loss = change.Abs()
	}

	// 2. Update Averages
	if r.count < r.period {
		// Phase A: Accumulate Simple Average (First 14 candles)
		r.avgGain = r.avgGain.Add(gain)
		r.avgLoss = r.avgLoss.Add(loss)
		r.count++

		if r.count == r.period {
			// Finalize the first SMA
			dPeriod := decimal.NewFromInt(int64(r.period))
			r.avgGain = r.avgGain.Div(dPeriod)
			r.avgLoss = r.avgLoss.Div(dPeriod)
			r.initialized = true
		} else {
			return decimal.Zero // Not enough data yet
		}
	} else {
		// Phase B: Wilder's Smoothing (The Standard RSI Formula)
		// NewAvg = (PrevAvg * (Period - 1) + Current) / Period
		dPeriod := decimal.NewFromInt(int64(r.period))
		dPeriodMinusOne := decimal.NewFromInt(int64(r.period - 1))

		r.avgGain = r.avgGain.Mul(dPeriodMinusOne).Add(gain).Div(dPeriod)
		r.avgLoss = r.avgLoss.Mul(dPeriodMinusOne).Add(loss).Div(dPeriod)
	}

	// 3. Calculate RS and RSI
	if r.avgLoss.IsZero() {
		if r.avgGain.IsZero() {
			return decimal.NewFromInt(50) // Flat line
		}
		return decimal.NewFromInt(100) // Max gain
	}

	rs := r.avgGain.Div(r.avgLoss)

	// RSI = 100 - (100 / (1 + RS))
	hundred := decimal.NewFromInt(100)
	rsi := hundred.Sub(hundred.Div(decimal.NewFromInt(1).Add(rs)))

	return rsi
}
