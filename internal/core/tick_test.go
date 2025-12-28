package core

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestGetTickSize(t *testing.T) {
	tests := []struct {
		name     string
		symbol   string
		price    float64
		expected float64
	}{
		// Options
		{"Nifty Option CE", "NIFTY23JAN18000CE", 150.0, 0.05},
		{"Stock Option PE", "RELIANCE23JAN2500PE", 50.0, 0.05},

		// Indices / Index Futures
		{"Nifty Index Low", "^NSEI", 14000.0, 0.05},
		{"Nifty Index Mid", "^NSEI", 18000.0, 0.10},
		{"BankNifty High", "^NSEBANK", 42000.0, 0.20},
		{"Nifty Future High", "NIFTY23JANFUT", 18100.0, 0.10},
		{"BankNifty Future High", "BANKNIFTY23JANFUT", 42000.0, 0.20},

		// Stocks
		{"Stock < 250", "ITC", 200.0, 0.01},
		{"Stock Reliance (CE suffix check)", "RELIANCE", 2500.0, 0.10},
		{"Stock > 250", "SBIN", 500.0, 0.05}, // 250-1000
		{"Stock = 1000", "CIPLA", 1000.0, 0.05},
		{"Stock > 1000", "INFY", 1500.0, 0.10},        // 1001-5000
		{"Stock > 5000", "BAJFINANCE", 6000.0, 0.50},  // 5001-10000
		{"Stock > 10000", "MARUTI", 11000.0, 1.00},    // 10001-20000
		{"Stock > 20000", "NESTLEIND", 22000.0, 5.00}, // >20000
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			price := decimal.NewFromFloat(tt.price)
			got := GetTickSize(tt.symbol, price)
			if !got.Equal(decimal.NewFromFloat(tt.expected)) {
				t.Errorf("GetTickSize(%s, %f) = %s; want %f", tt.symbol, tt.price, got.String(), tt.expected)
			}
		})
	}
}

func TestAdjustPriceToTick(t *testing.T) {
	tests := []struct {
		name     string
		price    float64
		tick     float64
		expected float64
	}{
		{"Round Down 0.05", 100.02, 0.05, 100.00},
		{"Round Up 0.05", 100.03, 0.05, 100.05},
		{"Exact 0.05", 100.05, 0.05, 100.05},

		{"Round Down 0.10", 1500.04, 0.10, 1500.00},
		{"Round Up 0.10", 1500.06, 0.10, 1500.10},

		{"Round 0.01", 200.004, 0.01, 200.00},
		{"Round 0.01 Up", 200.006, 0.01, 200.01},

		{"Round 5.00", 22002.0, 5.00, 22000.0},
		{"Round 5.00 Up", 22003.0, 5.00, 22005.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			price := decimal.NewFromFloat(tt.price)
			tick := decimal.NewFromFloat(tt.tick)
			got := AdjustPriceToTick(price, tick)
			if !got.Equal(decimal.NewFromFloat(tt.expected)) {
				t.Errorf("AdjustPriceToTick(%f, %f) = %s; want %f", tt.price, tt.tick, got.String(), tt.expected)
			}
		})
	}
}
