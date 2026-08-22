package broker

import (
	"testing"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

var simIST = time.FixedZone("IST", 5*3600+1800)

func bar(sym string, day time.Time, o, h, l, c float64) models.Candle {
	return models.Candle{
		Symbol:    sym,
		Timeframe: "day",
		Open:      decimal.NewFromFloat(o),
		High:      decimal.NewFromFloat(h),
		Low:       decimal.NewFromFloat(l),
		Close:     decimal.NewFromFloat(c),
		Volume:    decimal.NewFromInt(1000),
		StartTime: day,
		EndTime:   day,
	}
}

// openLong places a filled long with the given stop, target, and optional time stop.
func openLong(t *testing.T, s *SimBroker, sym string, entry, stop, target float64, deadline string) {
	t.Helper()
	meta := map[string]string{}
	if deadline != "" {
		meta[models.MetaExitOnOrAfter] = deadline
	}
	_, err := s.PlaceOrder(models.Order{
		Symbol:      sym,
		Side:        models.BuySignal,
		Type:        "MARKET",
		ProductType: "CNC",
		Quantity:    decimal.NewFromInt(10),
		Price:       decimal.NewFromFloat(entry),
		StopLoss:    decimal.NewFromFloat(stop),
		Target:      decimal.NewFromFloat(target),
		Timestamp:   time.Date(2026, 1, 1, 0, 0, 0, 0, simIST),
		Metadata:    meta,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
}

// A position that touches neither stop nor target must close at the deadline,
// at the closing price of that bar.
func TestTimeStopClosesPositionAtDeadline(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	openLong(t, s, "TEST", 100, 90, 110, "2026-01-09")

	// Before the deadline: nothing happens.
	s.CheckExits(bar("TEST", time.Date(2026, 1, 8, 0, 0, 0, 0, simIST), 100, 102, 98, 101))
	if len(s.Trades) != 0 {
		t.Fatalf("expected no exit before the deadline, got %d trades", len(s.Trades))
	}

	// On the deadline: closed at that bar's close.
	s.CheckExits(bar("TEST", time.Date(2026, 1, 9, 0, 0, 0, 0, simIST), 101, 103, 99, 102))
	if len(s.Trades) != 1 {
		t.Fatalf("expected one exit on the deadline, got %d", len(s.Trades))
	}
	tr := s.Trades[0]
	if tr.ExitReason != "TIME-STOP" {
		t.Errorf("exit reason = %q, want TIME-STOP", tr.ExitReason)
	}
	if !tr.ExitPrice.Equal(decimal.NewFromFloat(102)) {
		t.Errorf("exit price = %s, want 102 (the bar close)", tr.ExitPrice)
	}
}

// A bar that reaches the target on the deadline must be booked as TARGET-HIT,
// not silently downgraded to a time stop at the (worse) close.
func TestTimeStopDoesNotPreemptTargetOnSameBar(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	openLong(t, s, "TEST", 100, 90, 110, "2026-01-09")

	s.CheckExits(bar("TEST", time.Date(2026, 1, 9, 0, 0, 0, 0, simIST), 105, 112, 104, 106))
	if len(s.Trades) != 1 {
		t.Fatalf("expected one exit, got %d", len(s.Trades))
	}
	if got := s.Trades[0].ExitReason; got != "TARGET-HIT" {
		t.Errorf("exit reason = %q, want TARGET-HIT", got)
	}
}

// Orders without the metadata key must never time out — that is every ORB
// position, which relies on stop, target, and intraday square-off alone.
func TestNoTimeStopWithoutMetadata(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	openLong(t, s, "TEST", 100, 90, 110, "")

	s.CheckExits(bar("TEST", time.Date(2030, 1, 9, 0, 0, 0, 0, simIST), 100, 102, 98, 101))
	if len(s.Trades) != 0 {
		t.Fatalf("order without a deadline must not time out, got %d trades", len(s.Trades))
	}
}

// A malformed date must fail closed (no exit) rather than closing everything.
func TestMalformedDeadlineIsIgnored(t *testing.T) {
	s := NewSimBroker(decimal.NewFromInt(500000))
	openLong(t, s, "TEST", 100, 90, 110, "not-a-date")

	s.CheckExits(bar("TEST", time.Date(2030, 1, 9, 0, 0, 0, 0, simIST), 100, 102, 98, 101))
	if len(s.Trades) != 0 {
		t.Fatalf("malformed deadline must not close the position, got %d trades", len(s.Trades))
	}
}
