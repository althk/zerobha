package indicators

import (
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// ADX represents the Average Directional Index indicator
type ADX struct {
	period int
	prev   *models.Candle

	// Smoothed values
	trSmooth      decimal.Decimal
	plusDMSmooth  decimal.Decimal
	minusDMSmooth decimal.Decimal

	// Result
	adx     decimal.Decimal
	plusDI  decimal.Decimal
	minusDI decimal.Decimal

	// We need a history of DX values to calculate the first ADX (SMA of DX)
	// But standard Wilder's smoothing is often used for ADX itself too.
	// Let's stick to the standard Wilder's Smoothing for everything.
	// ADX = Smoothed DX
	dxSmooth decimal.Decimal

	isInitialized bool
	count         int
}

// NewADX creates a new ADX indicator
func NewADX(period int) *ADX {
	return &ADX{
		period: period,
	}
}

// Update calculates the next ADX value
func (a *ADX) Update(candle models.Candle) decimal.Decimal {
	if a.prev == nil {
		a.prev = &candle
		return decimal.Zero
	}

	// 1. Calculate TR, +DM, -DM
	tr := TrueRange(*a.prev, candle)
	plusDM, minusDM := DirectionalMovement(*a.prev, candle)

	// 2. Smooth TR, +DM, -DM (Wilder's Smoothing)
	// First value is simple sum (or we can just start smoothing from 0 if we accept warmup time)
	// Standard Wilder: First value = Sum over period. Subsequent = (Prev - Prev/N) + Current

	if !a.isInitialized {
		a.count++
		a.trSmooth = a.trSmooth.Add(tr)
		a.plusDMSmooth = a.plusDMSmooth.Add(plusDM)
		a.minusDMSmooth = a.minusDMSmooth.Add(minusDM)

		if a.count == a.period {
			a.isInitialized = true
			// Initial DX calculation
			var plusDI, minusDI decimal.Decimal
			if !a.trSmooth.IsZero() {
				plusDI = a.plusDMSmooth.Div(a.trSmooth).Mul(decimal.NewFromInt(100))
				minusDI = a.minusDMSmooth.Div(a.trSmooth).Mul(decimal.NewFromInt(100))
			}

			a.plusDI = plusDI
			a.minusDI = minusDI

			sumDI := plusDI.Add(minusDI)

			if sumDI.IsZero() {
				a.dxSmooth = decimal.Zero // Avoid div by zero
			} else {
				dx := plusDI.Sub(minusDI).Abs().Div(sumDI).Mul(decimal.NewFromInt(100))
				a.dxSmooth = dx // Initialize ADX with the first DX? Or wait another period?
				// Usually ADX is an average of DX.
				// Let's treat the first ADX as the first DX for simplicity in this streaming impl
				a.adx = dx
			}
		}
	} else {
		// Wilder's Smoothing
		// Current = Previous - (Previous / Period) + Current
		factor := decimal.NewFromInt(int64(a.period))

		a.trSmooth = a.trSmooth.Sub(a.trSmooth.Div(factor)).Add(tr)
		a.plusDMSmooth = a.plusDMSmooth.Sub(a.plusDMSmooth.Div(factor)).Add(plusDM)
		a.minusDMSmooth = a.minusDMSmooth.Sub(a.minusDMSmooth.Div(factor)).Add(minusDM)

		// Calculate DI
		if a.trSmooth.IsZero() {
			// Should not happen often
			return a.adx
		}

		plusDI := a.plusDMSmooth.Div(a.trSmooth).Mul(decimal.NewFromInt(100))
		minusDI := a.minusDMSmooth.Div(a.trSmooth).Mul(decimal.NewFromInt(100))

		a.plusDI = plusDI
		a.minusDI = minusDI

		// Calculate DX
		sumDI := plusDI.Add(minusDI)
		var dx decimal.Decimal
		if !sumDI.IsZero() {
			dx = plusDI.Sub(minusDI).Abs().Div(sumDI).Mul(decimal.NewFromInt(100))
		}

		// Calculate ADX (Smoothed DX)
		// ADX = ((Prior ADX * (Period - 1)) + Current DX) / Period
		// This is effectively EMA/Wilder's smoothing on DX
		a.adx = a.adx.Mul(decimal.NewFromInt(int64(a.period - 1))).Add(dx).Div(factor)
	}

	a.prev = &candle
	return a.adx
}

func (a *ADX) Value() decimal.Decimal {
	return a.adx
}

func (a *ADX) PlusDI() decimal.Decimal {
	return a.plusDI
}

func (a *ADX) MinusDI() decimal.Decimal {
	return a.minusDI
}

// Helpers

func TrueRange(prev, curr models.Candle) decimal.Decimal {
	hl := curr.High.Sub(curr.Low)
	hpc := curr.High.Sub(prev.Close).Abs()
	lpc := curr.Low.Sub(prev.Close).Abs()

	return decimal.Max(hl, decimal.Max(hpc, lpc))
}

func DirectionalMovement(prev, curr models.Candle) (decimal.Decimal, decimal.Decimal) {
	upMove := curr.High.Sub(prev.High)
	downMove := prev.Low.Sub(curr.Low)

	plusDM := decimal.Zero
	minusDM := decimal.Zero

	if upMove.GreaterThan(downMove) && upMove.GreaterThan(decimal.Zero) {
		plusDM = upMove
	}
	if downMove.GreaterThan(upMove) && downMove.GreaterThan(decimal.Zero) {
		minusDM = downMove
	}

	return plusDM, minusDM
}
