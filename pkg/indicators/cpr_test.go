package indicators

import (
	"testing"
	"time"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func d(v string) decimal.Decimal {
	d, _ := decimal.NewFromString(v)
	return d
}

func makeCandle(dateStr string, open, high, low, close string) models.Candle {
	t, _ := time.Parse("2006-01-02 15:04", dateStr)
	return models.Candle{
		Symbol:    "TEST",
		Open:      d(open),
		High:      d(high),
		Low:       d(low),
		Close:     d(close),
		Volume:    decimal.NewFromInt(1000),
		StartTime: t,
	}
}

func TestCPR_NotReadyBeforeDayTransition(t *testing.T) {
	cpr := NewCPR(nil)

	// Feed candles from a single day
	cpr.Update(makeCandle("2024-01-15 09:15", "100", "105", "98", "103"))
	cpr.Update(makeCandle("2024-01-15 09:20", "103", "107", "101", "106"))

	if cpr.IsReady() {
		t.Error("CPR should not be ready before a day transition")
	}
}

func TestCPR_ReadyAfterDayTransition(t *testing.T) {
	cpr := NewCPR(nil)

	// Day 1
	cpr.Update(makeCandle("2024-01-15 09:15", "100", "105", "98", "103"))
	cpr.Update(makeCandle("2024-01-15 14:00", "103", "110", "97", "108"))

	// Day 2 — triggers computation
	cpr.Update(makeCandle("2024-01-16 09:15", "108", "112", "106", "110"))

	if !cpr.IsReady() {
		t.Error("CPR should be ready after a day transition")
	}
}

func TestCPR_LevelsCalculation(t *testing.T) {
	cpr := NewCPR(nil)

	// Day 1: High=110, Low=97, Close=108
	cpr.Update(makeCandle("2024-01-15 09:15", "100", "105", "98", "103"))
	cpr.Update(makeCandle("2024-01-15 14:00", "103", "110", "97", "108"))

	// Day 2 — triggers computation from Day 1's HLC
	cpr.Update(makeCandle("2024-01-16 09:15", "108", "112", "106", "110"))

	// Hand-calculated:
	// Pivot = (110 + 97 + 108) / 3 = 315 / 3 = 105
	// BC    = (110 + 97) / 2 = 207 / 2 = 103.5
	// TC    = 2 * 105 - 103.5 = 106.5
	// R1    = 2 * 105 - 97 = 113
	// S1    = 2 * 105 - 110 = 100
	// R2    = 105 + (110 - 97) = 105 + 13 = 118
	// S2    = 105 - (110 - 97) = 105 - 13 = 92

	assertDecEqual(t, "Pivot", cpr.Pivot, "105")
	assertDecEqual(t, "BC", cpr.BC, "103.5")
	assertDecEqual(t, "TC", cpr.TC, "106.5")
	assertDecEqual(t, "R1", cpr.R1, "113")
	assertDecEqual(t, "S1", cpr.S1, "100")
	assertDecEqual(t, "R2", cpr.R2, "118")
	assertDecEqual(t, "S2", cpr.S2, "92")

	// Width = TC - BC = 106.5 - 103.5 = 3
	assertDecEqual(t, "Width", cpr.Width(), "3")
}

func TestCPR_LevelsOnlyChangeAtDayBoundary(t *testing.T) {
	cpr := NewCPR(nil)

	// Day 1
	cpr.Update(makeCandle("2024-01-15 09:15", "100", "110", "97", "108"))

	// Day 2 — triggers computation
	cpr.Update(makeCandle("2024-01-16 09:15", "108", "112", "106", "110"))
	pivotAfterTransition := cpr.Pivot

	// More candles on Day 2 — levels should NOT change
	cpr.Update(makeCandle("2024-01-16 10:00", "110", "120", "90", "115"))
	cpr.Update(makeCandle("2024-01-16 14:00", "115", "125", "85", "100"))

	if !cpr.Pivot.Equal(pivotAfterTransition) {
		t.Errorf("Pivot changed within same day: expected %s, got %s", pivotAfterTransition, cpr.Pivot)
	}
}

func TestCPR_TCBCSwapWhenCloseBelow(t *testing.T) {
	cpr := NewCPR(nil)

	// Day 1: High=110, Low=100, Close=101 (close near low)
	// Pivot = (110+100+101)/3 = 103.6667
	// BC = (110+100)/2 = 105
	// TC = 2*103.6667 - 105 = 102.3334
	// TC < BC, so they should be swapped
	cpr.Update(makeCandle("2024-01-15 09:15", "105", "110", "100", "101"))
	cpr.Update(makeCandle("2024-01-16 09:15", "101", "105", "99", "103"))

	if cpr.TC.LessThan(cpr.BC) {
		t.Errorf("TC (%s) should be >= BC (%s) after swap", cpr.TC, cpr.BC)
	}
}

func TestCPR_ISTTimezoneHandling(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	cpr := NewCPR(loc)

	// Create candle with UTC time that is a different date in IST
	// 2024-01-15 19:00 UTC = 2024-01-16 00:30 IST
	utcTime1, _ := time.Parse(time.RFC3339, "2024-01-15T03:45:00Z")
	utcTime2, _ := time.Parse(time.RFC3339, "2024-01-15T09:00:00Z")
	// Next IST day: 2024-01-16 09:15 IST = 2024-01-16T03:45:00Z
	utcTime3, _ := time.Parse(time.RFC3339, "2024-01-16T03:45:00Z")

	cpr.Update(models.Candle{Symbol: "TEST", High: d("110"), Low: d("97"), Close: d("108"), StartTime: utcTime1, Volume: decimal.NewFromInt(100)})
	cpr.Update(models.Candle{Symbol: "TEST", High: d("115"), Low: d("95"), Close: d("105"), StartTime: utcTime2, Volume: decimal.NewFromInt(100)})

	if cpr.IsReady() {
		t.Error("Should not be ready — both candles are on the same IST date (Jan 15)")
	}

	cpr.Update(models.Candle{Symbol: "TEST", High: d("112"), Low: d("100"), Close: d("110"), StartTime: utcTime3, Volume: decimal.NewFromInt(100)})

	if !cpr.IsReady() {
		t.Error("Should be ready after IST day transition")
	}
}

func assertDecEqual(t *testing.T, name string, got decimal.Decimal, expectedStr string) {
	t.Helper()
	expected := d(expectedStr)
	// Compare with 4 decimal places of precision
	if !got.Round(4).Equal(expected.Round(4)) {
		t.Errorf("%s: expected %s, got %s", name, expectedStr, got.StringFixed(4))
	}
}
