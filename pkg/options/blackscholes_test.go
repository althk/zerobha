package options

import (
	"math"
	"testing"
	"time"
)

// A vol put through the pricer and taken back out again must survive the round
// trip; the strike choice rests entirely on this inversion.
func TestImpliedVolRoundTrip(t *testing.T) {
	const s, k = 24300.0, 24300.0
	for _, years := range []float64{1.0 / 365, 3.0 / 365, 7.0 / 365} {
		for _, want := range []float64{0.08, 0.12, 0.25, 0.60} {
			price := Price(s, k, years, want, true)
			got, ok := ImpliedVol(price, s, k, years, true)
			if !ok {
				t.Fatalf("inversion failed at T=%.4f sigma=%.2f (price %.2f)", years, want, price)
			}
			if math.Abs(got-want) > 1e-3 {
				t.Errorf("T=%.4f sigma=%.2f: got %.4f", years, want, got)
			}
		}
	}
}

// A print outside what the model can produce must be refused rather than
// silently turned into a made-up vol that then picks a strike.
func TestImpliedVolRejectsImpossiblePrice(t *testing.T) {
	if _, ok := ImpliedVol(1, 24300, 24300, 2.0/365, true); ok {
		t.Error("accepted a price below the lowest the model can produce")
	}
	if _, ok := ImpliedVol(24300, 24300, 24300, 2.0/365, true); ok {
		t.Error("accepted a price above the underlying")
	}
}

// StrikeForDelta is the inverse of Delta, and the two must agree — otherwise
// a -delta run silently buys a different option than the one it reports.
func TestStrikeForDeltaInvertsDelta(t *testing.T) {
	const s, sigma = 24300.0, 0.12
	for _, years := range []float64{0.5 / 365, 2.0 / 365, 6.0 / 365} {
		for _, target := range []float64{0.55, 0.65, 0.80, 0.90} {
			for _, isCall := range []bool{true, false} {
				k := StrikeForDelta(s, years, sigma, target, isCall)
				got := math.Abs(Delta(s, k, years, sigma, isCall))
				if math.Abs(got-target) > 1e-3 {
					t.Errorf("T=%.4f target=%.2f call=%v: strike %.0f has |delta| %.3f",
						years, target, isCall, k, got)
				}
			}
		}
	}
}

// The point of targeting delta rather than a point offset: the same delta is a
// very different distance from spot depending on how much time is left.
func TestStrikeForDeltaWidensWithTimeToExpiry(t *testing.T) {
	const s, sigma, target = 24300.0, 0.12, 0.80
	near := s - StrikeForDelta(s, 1.0/365, sigma, target, true)
	far := s - StrikeForDelta(s, 6.0/365, sigma, target, true)
	if !(far > near*1.5) {
		t.Errorf("0.8 delta sits %.0f pts ITM at 1 day and %.0f at 6 days; expected it to widen materially", near, far)
	}
}

// A call at target delta must be in the money (strike below spot) and a put
// likewise (strike above spot) — the whole premise is buying intrinsic.
func TestStrikeForDeltaIsInTheMoney(t *testing.T) {
	const s, sigma = 24300.0, 0.12
	if k := StrikeForDelta(s, 3.0/365, sigma, 0.8, true); k >= s {
		t.Errorf("0.8-delta call strike %.0f is not below spot %.0f", k, s)
	}
	if k := StrikeForDelta(s, 3.0/365, sigma, 0.8, false); k <= s {
		t.Errorf("0.8-delta put strike %.0f is not above spot %.0f", k, s)
	}
}

func TestYearsToExpiryMeasuresToTheClose(t *testing.T) {
	exp := time.Date(2026, 8, 18, 0, 0, 0, 0, IST)
	at := time.Date(2026, 8, 18, 10, 30, 0, 0, IST)
	got := YearsToExpiry(at, exp)
	want := 5.0 / (24 * 365) // 10:30 to 15:30
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %.9f years, want %.9f", got, want)
	}
	// Past the close it must floor rather than go negative, which would make
	// every downstream sqrt(T) a NaN.
	if v := YearsToExpiry(exp.Add(20*time.Hour), exp); v <= 0 {
		t.Errorf("time past expiry gave %v, want a positive floor", v)
	}
}
