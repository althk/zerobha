package indicators

import (
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

type ATR struct {
	period    int
	prevClose decimal.Decimal
	value     decimal.Decimal // Current ATR value
	count     int
}

func NewATR(period int) *ATR {
	return &ATR{period: period}
}

func (a *ATR) Update(candle models.Candle) decimal.Decimal {
	// 1. Calculate True Range (TR)
	// For the very first candle, TR = High - Low
	var tr decimal.Decimal

	if a.prevClose.IsZero() {
		tr = candle.High.Sub(candle.Low)
	} else {
		// TR = Max(H-L, |H-PC|, |L-PC|)
		val1 := candle.High.Sub(candle.Low)
		val2 := candle.High.Sub(a.prevClose).Abs()
		val3 := candle.Low.Sub(a.prevClose).Abs()

		tr = decimal.Max(val1, decimal.Max(val2, val3))
	}

	a.prevClose = candle.Close

	// 2. Smooth it
	if a.count < a.period {
		// Accumulate for initial SMA
		a.value = a.value.Add(tr)
		a.count++

		if a.count == a.period {
			// Finalize initial SMA
			a.value = a.value.Div(decimal.NewFromInt(int64(a.period)))
		} else {
			return decimal.Zero // Not ready
		}
	} else {
		// Wilder's Smoothing: (PrevATR * (N-1) + TR) / N
		period := decimal.NewFromInt(int64(a.period))
		prevSum := a.value.Mul(period.Sub(decimal.NewFromInt(1)))
		a.value = prevSum.Add(tr).Div(period)
	}

	return a.value
}

func (a *ATR) IsReady() bool {
	return a.count >= a.period
}
