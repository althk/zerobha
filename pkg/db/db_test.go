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

		id, err := store.SaveSignal(sig, false)
		if err != nil {
			t.Fatalf("SaveSignal failed: %v", err)
		}
		if id == 0 {
			t.Fatal("SaveSignal should return the row id")
		}
		if err := store.RecordSignalOutcome(id, SignalPlaced, "PAPER-000001"); err != nil {
			t.Fatalf("RecordSignalOutcome failed: %v", err)
		}
		if err := store.SaveDeclinedSignal("NIFTY 50", "emacross", "BUY", "too close to expiry", false); err != nil {
			t.Fatalf("SaveDeclinedSignal failed: %v", err)
		}
		funnel, err := store.GetSignalFunnel(time.Now().Add(-time.Hour), false)
		if err != nil {
			t.Fatalf("GetSignalFunnel failed: %v", err)
		}
		got := map[string]int{}
		for _, c := range funnel {
			got[c.Outcome] += c.Count
		}
		if got[SignalPlaced] != 1 || got[SignalDeclined] != 1 {
			t.Errorf("funnel = %+v, want one placed and one declined", funnel)
		}
		if paper, _ := store.GetSignalFunnel(time.Now().Add(-time.Hour), true); len(paper) != 0 {
			t.Errorf("paper funnel should be empty, got %+v", paper)
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

		trades, err := store.GetTradeHistory(time.Now().Add(-24*time.Hour), false)
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
		trades, err = store.GetTradeHistory(time.Now().Add(time.Hour), false)
		if err != nil {
			t.Fatalf("GetTradeHistory (future cutoff) failed: %v", err)
		}
		if len(trades) != 0 {
			t.Errorf("Expected 0 trades after future cutoff, got %d", len(trades))
		}

		// Paper and live trades share this table, and every dashboard aggregate
		// is derived from it. A query must never mix the two.
		paperTrade := trade
		paperTrade.IsPaper = true
		paperTrade.PnL = decimal.NewFromFloat(-500)
		if err := store.SaveTrade("PAPER-EXIT-000001:0", paperTrade); err != nil {
			t.Fatalf("SaveTrade (paper) failed: %v", err)
		}

		live, err := store.GetTradeHistory(time.Now().Add(-24*time.Hour), false)
		if err != nil {
			t.Fatalf("GetTradeHistory (live) failed: %v", err)
		}
		if len(live) != 1 || live[0].IsPaper {
			t.Errorf("live query returned %d trades, want 1 live-only: %+v", len(live), live)
		}

		paper, err := store.GetTradeHistory(time.Now().Add(-24*time.Hour), true)
		if err != nil {
			t.Fatalf("GetTradeHistory (paper) failed: %v", err)
		}
		if len(paper) != 1 || !paper[0].IsPaper {
			t.Errorf("paper query returned %d trades, want 1 paper-only: %+v", len(paper), paper)
		}
	})

	// Test Paper State round-trip
	t.Run("PaperState", func(t *testing.T) {
		if blob, err := store.LoadPaperState("2026-01-01"); err != nil || blob != nil {
			t.Fatalf("expected no state for an untouched date, got %q (err %v)", blob, err)
		}

		if err := store.SavePaperState("2026-01-01", []byte(`{"cash":"100"}`)); err != nil {
			t.Fatalf("SavePaperState failed: %v", err)
		}
		// Saving again for the same date replaces rather than duplicates.
		if err := store.SavePaperState("2026-01-01", []byte(`{"cash":"200"}`)); err != nil {
			t.Fatalf("SavePaperState (replace) failed: %v", err)
		}

		blob, err := store.LoadPaperState("2026-01-01")
		if err != nil {
			t.Fatalf("LoadPaperState failed: %v", err)
		}
		if string(blob) != `{"cash":"200"}` {
			t.Errorf("got state %q, want the replacement", blob)
		}

		// A different trading date must not see it.
		if other, err := store.LoadPaperState("2026-01-02"); err != nil || other != nil {
			t.Errorf("state leaked across trading dates: %q (err %v)", other, err)
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

		points, err := store.GetEquitySnapshots(time.Now().Add(-time.Hour), false)
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

// Trades and orders carry their metadata and cost breakdown through the
// store: the fields that let a derivative trade be read in index bps.
func TestTradeAndOrderMetadataRoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/meta.db")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	order := models.Order{
		ID: "PAPER-000007", Symbol: "NIFTY26SEP24000CE", Side: models.BuySignal,
		Quantity: decimal.NewFromInt(650), Price: decimal.NewFromInt(200), IsPaper: true,
		Metadata: map[string]string{"Strategy": "emacross", "Underlying": "NIFTY 50", "IndexEntry": "24300.00"},
	}
	if err := store.SaveOrder(order, "SUBMITTED"); err != nil {
		t.Fatalf("SaveOrder: %v", err)
	}
	meta, err := store.GetOrderMetadata("PAPER-000007")
	if err != nil || meta["IndexEntry"] != "24300.00" {
		t.Errorf("order metadata = %v (%v), want IndexEntry 24300.00", meta, err)
	}

	trade := models.Trade{
		Symbol: "NIFTY26SEP24000CE", Strategy: "emacross", Direction: "LONG",
		Quantity: decimal.NewFromInt(650), EntryPrice: decimal.NewFromInt(200), ExitPrice: decimal.NewFromInt(210),
		GrossPnL: decimal.NewFromInt(6500), Costs: decimal.NewFromFloat(310.5), PnL: decimal.NewFromFloat(6189.5),
		EntryTime: time.Now().Add(-time.Hour), ExitTime: time.Now(), ExitReason: "EMACross index stop", IsPaper: true,
		EntryOrderID: "PAPER-000007", ExitOrderID: "PAPER-EXIT-000008",
		Metadata: map[string]string{"Underlying": "NIFTY 50", "IndexEntry": "24300.00", "UnderlyingExit": "24350.00", "DaysToExpiry": "3"},
	}
	if err := store.SaveTrade("PAPER-EXIT-000008:0", trade); err != nil {
		t.Fatalf("SaveTrade: %v", err)
	}
	got, err := store.GetTradeHistory(time.Now().Add(-24*time.Hour), true)
	if err != nil || len(got) != 1 {
		t.Fatalf("GetTradeHistory: %v, n=%d", err, len(got))
	}
	g := got[0]
	if g.ExitReason != "EMACross index stop" || !g.Costs.Equal(decimal.NewFromFloat(310.5)) ||
		!g.GrossPnL.Equal(decimal.NewFromInt(6500)) || g.Metadata["UnderlyingExit"] != "24350.00" ||
		g.EntryOrderID != "PAPER-000007" {
		t.Errorf("round trip lost fields: %+v", g)
	}
}
