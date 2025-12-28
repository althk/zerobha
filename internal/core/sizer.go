package core

import (
	"log"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// CalculateQuantity determines how many shares to buy based on 1% Rule.
func CalculateQuantity(capital decimal.Decimal, signal *models.Signal, leverage decimal.Decimal) decimal.Decimal {
	log.Printf("Calculating quantity for %s at %s, total capital %d, leverage %s", signal.Symbol, signal.Price, capital.IntPart(), leverage.String())

	// 1. Safety Check: Avoid division by zero
	if signal.Price.Equal(signal.StopLoss) {
		log.Println("SKIPPED: Price == StopLoss")
		return decimal.Zero
	}

	// 2. Risk Calculation (Absolute value handles Long AND Short)
	riskPerShare := signal.Price.Sub(signal.StopLoss).Abs()
	riskPerTrade := capital.Mul(decimal.NewFromFloat(0.01))

	// 3. Volatility Sizing
	volatilityQty := riskPerTrade.Div(riskPerShare).Floor()

	// 4. Capital Constraint (Max purchasing power)
	// Apply leverage to capital for purchasing power check
	purchasingPower := capital.Mul(leverage)
	capitalQty := purchasingPower.Div(signal.Price).Floor()

	// 5. Return the stricter limit
	return decimal.Min(volatilityQty, capitalQty)
}
