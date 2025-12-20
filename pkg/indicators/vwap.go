package indicators

import (
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// VWAP represents the Volume Weighted Average Price indicator
// It resets daily based on the candle timestamp
type VWAP struct {
	cumulativePV decimal.Decimal // Cumulative Price * Volume
	cumulativeV  decimal.Decimal // Cumulative Volume
	currentVWAP  decimal.Decimal
	lastDate     string // To track day changes
}

// NewVWAP creates a new VWAP indicator
func NewVWAP() *VWAP {
	return &VWAP{}
}

// Update calculates the next VWAP value
func (v *VWAP) Update(candle models.Candle) decimal.Decimal {
	// Check for day change
	// Assuming candle.StartTime is in the exchange timezone or we just check YYYY-MM-DD
	currentDate := candle.StartTime.Format("2006-01-02")

	if v.lastDate != currentDate {
		// Reset for new day
		v.cumulativePV = decimal.Zero
		v.cumulativeV = decimal.Zero
		v.lastDate = currentDate
	}

	// Typical Price = (High + Low + Close) / 3
	typicalPrice := candle.High.Add(candle.Low).Add(candle.Close).Div(decimal.NewFromInt(3))

	// PV = Typical Price * Volume
	pv := typicalPrice.Mul(candle.Volume)

	// Update cumulatives
	v.cumulativePV = v.cumulativePV.Add(pv)
	v.cumulativeV = v.cumulativeV.Add(candle.Volume)

	// Calculate VWAP
	if v.cumulativeV.IsZero() {
		v.currentVWAP = decimal.Zero
	} else {
		v.currentVWAP = v.cumulativePV.Div(v.cumulativeV)
	}

	return v.currentVWAP
}

func (v *VWAP) Value() decimal.Decimal {
	return v.currentVWAP
}
