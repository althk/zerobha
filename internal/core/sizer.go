package core

import (
	"log"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// CalculateQuantity determines the position size from the signal's stop
// distance, risking a fixed fraction of capital per trade.
//
// The default budget is the 1% rule; a signal may override it with RiskPct
// (0.005 = 0.5%). When the signal carries a LotSize the result is rounded down
// to a whole number of lots, and a budget too small for one lot sizes to zero
// rather than to a fractional contract — futures cannot be traded in pieces.
func CalculateQuantity(capital decimal.Decimal, signal *models.Signal, leverage decimal.Decimal) decimal.Decimal {
	log.Printf("Calculating quantity for %s at %s, total capital %d, leverage %s", signal.Symbol, signal.Price, capital.IntPart(), leverage.String())

	// 1. Safety Check: Avoid division by zero
	if signal.Price.Equal(signal.StopLoss) {
		log.Println("SKIPPED: Price == StopLoss")
		return decimal.Zero
	}

	// 2. Risk Calculation (Absolute value handles Long AND Short)
	riskPerShare := signal.Price.Sub(signal.StopLoss).Abs()
	riskFraction := decimal.NewFromFloat(0.01)
	if signal.RiskPct.IsPositive() {
		riskFraction = signal.RiskPct
	}
	riskPerTrade := capital.Mul(riskFraction)

	// 3. Volatility Sizing
	volatilityQty := riskPerTrade.Div(riskPerShare).Floor()

	// 4. Capital Constraint (Max purchasing power)
	// Apply leverage to capital for purchasing power check
	purchasingPower := capital.Mul(leverage)
	capitalQty := purchasingPower.Div(signal.Price).Floor()

	// 5. Take the stricter limit, then round down to whole lots
	qty := decimal.Min(volatilityQty, capitalQty)
	if signal.LotSize > 1 {
		lot := decimal.NewFromInt(int64(signal.LotSize))
		lots := qty.Div(lot).Floor()
		if lots.LessThan(decimal.NewFromInt(1)) {
			log.Printf("SKIPPED %s: risk budget affords less than one lot of %d", signal.Symbol, signal.LotSize)
			return decimal.Zero
		}
		qty = lots.Mul(lot)
	}
	return qty
}
