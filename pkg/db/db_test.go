package db

import (
	"os"
	"testing"
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
		}

		if err := store.SaveOrder(order, "COMPLETE"); err != nil {
			t.Fatalf("SaveOrder failed: %v", err)
		}
	})
}
