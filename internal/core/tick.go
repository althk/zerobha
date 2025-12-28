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
	// Heuristic: Symbols ending with CE or PE or similar patterns for options.
	// Typically, Zerodha/NSE option symbols look like "NIFTY25JAN23000CE"
	// Suffix Check: Must end in "CE" or "PE" AND the character immediately preceding must be a DIGIT.
	// This prevents matching stocks like "RELIANCE" or "BAJFINANCE".
	isOption := false
	if strings.HasSuffix(symbol, "CE") || strings.HasSuffix(symbol, "PE") {
		// Check for preceding digit
		// Check length to avoid panic
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
	// Heuristic: Check if symbol is a known index or index future.
	// Zerodha Futures: NIFTY25JANFUT, BANKNIFTY25JANFUT
	// Indices: ^NSEI (Nifty 50), ^NSEBANK (Bank Nifty)
	// Simple check: Contains "NIFTY" or "BANK" (Might need refinement if stocks have these names, but unlikely for large caps)
	// Better: explicit check for known indices or suffix "FUT" combined with index root.
	isIndex := false
	if symbol == "^NSEI" || symbol == "NSEI" || symbol == "NIFTY 50" ||
		symbol == "^NSEBANK" || symbol == "NSEBANK" || symbol == "NIFTY BANK" {
		isIndex = true
	} else if strings.Contains(symbol, "NIFTY") || strings.Contains(symbol, "BANKNIFTY") {
		// Likely an index future if it doesn't end in CE/PE (already handled)
		// But wait, "NIFTYBEES" is an ETF (Equity).
		// Let's assume strict naming for now or check suffix "FUT".
		if strings.HasSuffix(symbol, "FUT") {
			isIndex = true
		}
		// If it is just "NIFTY 50" or "NIFTY", it's an index.
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

	// 3. Stocks / Stock Futures logic (Default for others)
	// Futures also follow the stock tick size rules (except Index Futures).
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

	// Formula: Round(Price / TickSize) * TickSize
	// shopspring/decimal doesn't have a direct "RoundToNearest" for arbitrary steps easily without division.

	div := price.Div(tickSize)
	round := div.Round(0) // Round to nearest integer
	return round.Mul(tickSize)
}
