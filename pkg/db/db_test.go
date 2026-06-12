package db

import (
	"os"
	"testing"
	"time"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func TestDB(t *testing.T) {
	dbPath := "test_zerobha.db"
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer func() {
		store.Close()
		os.Remove(dbPath)
	}()

	// Test KV Store
	t.Run("KVStore", func(t *testing.T) {
		key := "ORB_RELIANCE"
		state := map[string]interface{}{
			"RangeHigh": 1000.50,
			"RangeLow":  990.00,
		}

		if err := store.SetState(key, state); err != nil {
			t.Fatalf("SetState failed: %v", err)
		}

		var fetchedState map[string]interface{}
		if err := store.GetState(key, &fetchedState); err != nil {
			t.Fatalf("GetState failed: %v", err)
		}

		if fetchedState["RangeHigh"] != 1000.50 {
			t.Errorf("Expected 1000.50, got %v", fetchedState["RangeHigh"])
		}
	})

	// Test Save Signal
	t.Run("SaveSignal", func(t *testing.T) {
		sig := &models.Signal{
			Symbol:   "INFY",
			Type:     models.BuySignal,
			Price:    decimal.NewFromFloat(1500),
			StopLoss: decimal.NewFromFloat(1490),
			Target:   decimal.NewFromFloat(1520),
			Metadata: map[string]string{"Strategy": "TEST_STRAT"},
		}

		if err := store.SaveSignal(sig); err != nil {
			t.Fatalf("SaveSignal failed: %v", err)
		}
	})

	// Test Save Order
	t.Run("SaveOrder", func(t *testing.T) {
		order := models.Order{
			ID:       "12345",
			Symbol:   "INFY",
			Side:     models.BuySignal,
			Quantity: decimal.NewFromInt(10),
			Price:    decimal.NewFromFloat(1500),
			Metadata: map[string]string{"Strategy": "TEST_STRAT"},
		}

		if err := store.SaveOrder(order, "COMPLETE"); err != nil {
			t.Fatalf("SaveOrder failed: %v", err)
		}

		// Verify Strategy
		strat, err := store.GetOrderStrategy("INFY")
		if err != nil {
			t.Fatalf("GetOrderStrategy failed: %v", err)
		}
		if strat != "TEST_STRAT" {
			t.Errorf("Expected TEST_STRAT, got %s", strat)
		}
	})

	// Test Trade persistence + idempotency
	t.Run("SaveTrade", func(t *testing.T) {
		trade := models.Trade{
			Symbol:     "INFY",
			Strategy:   "TEST_STRAT",
			Direction:  "LONG",
			Quantity:   decimal.NewFromInt(10),
			EntryPrice: decimal.NewFromFloat(1500),
			ExitPrice:  decimal.NewFromFloat(1520),
			PnL:        decimal.NewFromFloat(200),
			EntryTime:  time.Now().Add(-time.Hour),
			ExitTime:   time.Now(),
		}

		// Saving the same key twice must not duplicate
		if err := store.SaveTrade("ORDER1:0", trade); err != nil {
			t.Fatalf("SaveTrade failed: %v", err)
		}
		if err := store.SaveTrade("ORDER1:0", trade); err != nil {
			t.Fatalf("SaveTrade (repeat) failed: %v", err)
		}

		trades, err := store.GetTradeHistory(time.Now().Add(-24 * time.Hour))
		if err != nil {
			t.Fatalf("GetTradeHistory failed: %v", err)
		}
		if len(trades) != 1 {
			t.Fatalf("Expected 1 trade, got %d", len(trades))
		}
		got := trades[0]
		if got.Strategy != "TEST_STRAT" || got.Direction != "LONG" {
			t.Errorf("Unexpected trade row: %+v", got)
		}
		if !got.PnL.Equal(decimal.NewFromFloat(200)) {
			t.Errorf("Expected PnL 200, got %s", got.PnL)
		}

		// A cutoff in the future must exclude the trade
		trades, err = store.GetTradeHistory(time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("GetTradeHistory (future cutoff) failed: %v", err)
		}
		if len(trades) != 0 {
			t.Errorf("Expected 0 trades after future cutoff, got %d", len(trades))
		}
	})

	// Test Equity Snapshots
	t.Run("EquitySnapshots", func(t *testing.T) {
		point := EquityPoint{
			Timestamp:     time.Now(),
			Balance:       50000,
			RealizedPnL:   1200,
			UnrealizedPnL: -300,
			OpenPositions: 2,
		}

		if err := store.SaveEquitySnapshot(point); err != nil {
			t.Fatalf("SaveEquitySnapshot failed: %v", err)
		}

		points, err := store.GetEquitySnapshots(time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("GetEquitySnapshots failed: %v", err)
		}
		if len(points) != 1 {
			t.Fatalf("Expected 1 snapshot, got %d", len(points))
		}
		if points[0].RealizedPnL != 1200 || points[0].OpenPositions != 2 {
			t.Errorf("Unexpected snapshot: %+v", points[0])
		}
	})
}
