package indicators

import (
	"testing"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func bar(high, low float64) models.Candle {
	return models.Candle{
		High:  decimal.NewFromFloat(high),
		Low:   decimal.NewFromFloat(low),
		Close: decimal.NewFromFloat((high + low) / 2),
	}
}

// The channel must exclude the bar being evaluated. If it did not, a breakout
// bar's own high would define the top of the channel and no close could ever
// clear it — the strategy would take zero trades and look like a filter
// problem rather than an indicator bug.
func TestDonchianExcludesCurrentBar(t *testing.T) {
	d := NewDonchian(3)
	d.Update(bar(100, 90))
	d.Update(bar(105, 95))
	d.Update(bar(102, 92))

	// Fourth bar prints a new extreme high: the channel handed back must still
	// describe the first three bars only.
	upper, lower := d.Update(bar(200, 50))
	if !upper.Equal(decimal.NewFromInt(105)) {
		t.Errorf("upper = %s, want 105 (the new bar's 200 must not be included)", upper)
	}
	if !lower.Equal(decimal.NewFromInt(90)) {
		t.Errorf("lower = %s, want 90 (the new bar's 50 must not be included)", lower)
	}

	// The extreme bar is in the window now, and the oldest has dropped out.
	upper, lower = d.Value()
	if !upper.Equal(decimal.NewFromInt(200)) || !lower.Equal(decimal.NewFromInt(50)) {
		t.Errorf("after ingest: channel = [%s, %s], want [50, 200]", lower, upper)
	}
}

func TestDonchianNotReadyBeforeWindowFills(t *testing.T) {
	d := NewDonchian(4)
	for i := range 3 {
		upper, lower := d.Update(bar(100+float64(i), 90))
		if d.IsReady() {
			t.Fatalf("ready after %d of 4 bars", i+1)
		}
		if !upper.IsZero() || !lower.IsZero() {
			t.Errorf("channel = [%s, %s] before warm-up, want zeros", lower, upper)
		}
	}
	d.Update(bar(100, 90))
	if !d.IsReady() {
		t.Error("not ready after 4 bars")
	}
}

// The ring buffer must drop the oldest bar rather than keeping an all-time
// extreme, otherwise the channel widens forever and stops producing signals.
func TestDonchianWindowSlides(t *testing.T) {
	d := NewDonchian(2)
	d.Update(bar(500, 400)) // extreme, must age out
	d.Update(bar(110, 100))
	d.Update(bar(120, 105))

	upper, lower := d.Value()
	if !upper.Equal(decimal.NewFromInt(120)) || !lower.Equal(decimal.NewFromInt(100)) {
		t.Errorf("channel = [%s, %s], want [100, 120] — the 500/400 bar should have aged out", lower, upper)
	}
}
