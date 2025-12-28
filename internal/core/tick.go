package core

import (
	"log"
	"strings"
	"unicode"

	"github.com/shopspring/decimal"
)

// GetTickSize returns the tick size for a given symbol and price based on NSE April 2025 revision.
// Note: This implementation approximates "closing price" with the current price provided.
func GetTickSize(symbol string, price decimal.Decimal) decimal.Decimal {
	// 1. Derivatives (Options) logic
	// Options are identified by "CE" or "PE" suffix preceded by a digit (e.g., NIFTY25JAN23000CE).
	// This distinguishes them from stocks like RELIANCE.
	isOption := false
	if strings.HasSuffix(symbol, "CE") || strings.HasSuffix(symbol, "PE") {
		// Check for preceding digit to confirm it's an option contract
		if len(symbol) > 2 {
			r := rune(symbol[len(symbol)-3])
			if unicode.IsDigit(r) {
				isOption = true
			}
		}
	}

	if isOption {
		return decimal.NewFromFloat(0.05)
	}

	// 2. Indices / Index Futures logic
	// Identifies known indices (e.g. ^NSEI) or futures (suffix FUT).
	isIndex := false
	if symbol == "^NSEI" || symbol == "NSEI" || symbol == "NIFTY 50" ||
		symbol == "^NSEBANK" || symbol == "NSEBANK" || symbol == "NIFTY BANK" {
		isIndex = true
	} else if strings.Contains(symbol, "NIFTY") || strings.Contains(symbol, "BANKNIFTY") {
		// Check for Index Futures (e.g. NIFTY25JANFUT)
		if strings.HasSuffix(symbol, "FUT") {
			isIndex = true
		}
	}

	priceF, _ := price.Float64()

	if isIndex {
		if priceF < 15000 {
			return decimal.NewFromFloat(0.05)
		} else if priceF >= 15000 && priceF <= 30000 {
			return decimal.NewFromFloat(0.10)
		} else { // > 30000
			return decimal.NewFromFloat(0.20)
		}
	}

	// 3. Stocks / Stock Futures logic
	// Default behavior for stocks and stock futures.
	if priceF < 250 {
		return decimal.NewFromFloat(0.01)
	} else if priceF >= 250 && priceF <= 1000 {
		return decimal.NewFromFloat(0.05)
	} else if priceF > 1000 && priceF <= 5000 {
		return decimal.NewFromFloat(0.10)
	} else if priceF > 5000 && priceF <= 10000 {
		return decimal.NewFromFloat(0.50)
	} else if priceF > 10000 && priceF <= 20000 {
		return decimal.NewFromFloat(1.00)
	} else { // > 20000
		return decimal.NewFromFloat(5.00)
	}
}

// AdjustPriceToTick rounds the price to the nearest valid tick size.
func AdjustPriceToTick(price decimal.Decimal, tickSize decimal.Decimal) decimal.Decimal {
	if tickSize.IsZero() {
		log.Println("WARNING: Zero tick size provided, defaulting to 0.05")
		tickSize = decimal.NewFromFloat(0.05)
	}

	// Round(Price / TickSize) * TickSize

	div := price.Div(tickSize)
	round := div.Round(0) // Round to nearest integer
	return round.Mul(tickSize)
}
