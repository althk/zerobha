package options

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// fakeChain lists NIFTY-style weeklies: 50-point strikes either side of a
// centre, for a fixed set of expiries.
type fakeChain struct {
	expiries []time.Time
	centre   float64
	width    int // strikes each side
	err      error
}

func (f fakeChain) Expiries(string) ([]time.Time, error) { return f.expiries, f.err }

func (f fakeChain) Contracts(_ string, expiry time.Time) ([]Contract, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []Contract
	base := math.Round(f.centre/50) * 50
	for i := -f.width; i <= f.width; i++ {
		strike := base + float64(i)*50
		for _, isCall := range []bool{true, false} {
			out = append(out, Contract{
				TradingSymbol: fmt.Sprintf("NIFTY%.0f%s", strike, callPut(isCall)),
				Exchange:      "NFO",
				Expiry:        expiry,
				Strike:        decimal.NewFromFloat(strike),
				IsCall:        isCall,
				LotSize:       65,
				TickSize:      decimal.NewFromFloat(0.05),
			})
		}
	}
	return out, nil
}

// modelQuoter prices whatever it is asked for at a known volatility, so a
// selection round-trips back to that volatility.
type modelQuoter struct {
	spot, years, iv float64
	err             error
}

func (m modelQuoter) Premium(c Contract) (decimal.Decimal, error) {
	if m.err != nil {
		return decimal.Zero, m.err
	}
	return decimal.NewFromFloat(Price(m.spot, c.Strike.InexactFloat64(), m.years, m.iv, c.IsCall)), nil
}

func fixture(daysToExpiry int) (Selector, fakeChain, modelQuoter, time.Time, decimal.Decimal) {
	now := time.Date(2026, 8, 10, 10, 30, 0, 0, IST)
	expiry := dayOf(now).AddDate(0, 0, daysToExpiry)
	spot := 24300.0
	years := YearsToExpiry(now, expiry)
	return Selector{TargetDelta: 0.80, MinDaysToExpiry: 2},
		fakeChain{expiries: []time.Time{expiry}, centre: spot, width: 40},
		modelQuoter{spot: spot, years: years, iv: 0.13},
		now, decimal.NewFromFloat(spot)
}

func TestSelectBuysTheRequestedDelta(t *testing.T) {
	sel, chain, quoter, now, spot := fixture(4)

	for _, isCall := range []bool{true, false} {
		c, err := sel.Select("NIFTY", spot, now, isCall, chain, quoter)
		if err != nil {
			t.Fatalf("call=%v: %v", isCall, err)
		}
		if c.IsCall != isCall {
			t.Errorf("call=%v: got a %s", isCall, callPut(c.IsCall))
		}
		years := YearsToExpiry(now, c.Expiry)
		got := math.Abs(Delta(spot.InexactFloat64(), c.Strike.InexactFloat64(), years, quoter.iv, isCall))
		// The strike grid is 50 wide, so the realised delta lands near, not on,
		// the target.
		if math.Abs(got-0.80) > 0.03 {
			t.Errorf("call=%v: strike %s has delta %.3f, want ~0.80", isCall, c.Strike, got)
		}
	}
}

// The contract bought must be in the money: a call below spot, a put above.
func TestSelectBuysInTheMoney(t *testing.T) {
	sel, chain, quoter, now, spot := fixture(4)

	call, err := sel.Select("NIFTY", spot, now, true, chain, quoter)
	if err != nil {
		t.Fatal(err)
	}
	if !call.Strike.LessThan(spot) {
		t.Errorf("call strike %s is not below spot %s", call.Strike, spot)
	}
	put, err := sel.Select("NIFTY", spot, now, false, chain, quoter)
	if err != nil {
		t.Fatal(err)
	}
	if !put.Strike.GreaterThan(spot) {
		t.Errorf("put strike %s is not above spot %s", put.Strike, spot)
	}
}

// The reason MinDaysToExpiry exists: the same target delta walks in towards
// spot as time runs out, so near expiry it buys a nearly worthless contract.
func TestSelectRefusesNearExpiry(t *testing.T) {
	for _, dte := range []int{0, 1} {
		sel, chain, quoter, now, spot := fixture(dte)
		_, err := sel.Select("NIFTY", spot, now, true, chain, quoter)
		if !errors.Is(err, ErrTooCloseToExpiry) {
			t.Errorf("%d days to expiry: err = %v, want ErrTooCloseToExpiry", dte, err)
		}
	}
	// And two days out it trades.
	sel, chain, quoter, now, spot := fixture(2)
	if _, err := sel.Select("NIFTY", spot, now, true, chain, quoter); err != nil {
		t.Errorf("2 days to expiry should trade: %v", err)
	}
}

// Same delta, less time, closer strike. This is the whole mechanism behind the
// expiry-week rule, so it gets pinned.
func TestTargetDeltaWalksTowardsSpotAsExpiryNears(t *testing.T) {
	var distances []float64
	for _, dte := range []int{2, 4, 6} {
		sel, chain, quoter, now, spot := fixture(dte)
		sel.MinDaysToExpiry = 0
		c, err := sel.Select("NIFTY", spot, now, true, chain, quoter)
		if err != nil {
			t.Fatal(err)
		}
		distances = append(distances, spot.Sub(c.Strike).InexactFloat64())
	}
	for i := 1; i < len(distances); i++ {
		if distances[i] <= distances[i-1] {
			t.Errorf("distance from spot did not widen with time to expiry: %v", distances)
			break
		}
	}
}

// Without a readable ATM premium there is no volatility, and guessing one picks
// the wrong strike silently. Refusing is the correct behaviour.
func TestSelectRefusesWithoutAVolatility(t *testing.T) {
	sel, chain, _, now, spot := fixture(4)
	broken := modelQuoter{err: errors.New("no quote")}

	if _, err := sel.Select("NIFTY", spot, now, true, chain, broken); err == nil {
		t.Error("selected a strike despite having no volatility")
	}
	if _, err := sel.Select("NIFTY", spot, now, true, chain, nil); err == nil {
		t.Error("selected a strike with no quoter at all")
	}

	// Unless a fallback was configured deliberately.
	sel.FallbackIV = 0.13
	if _, err := sel.Select("NIFTY", spot, now, true, chain, broken); err != nil {
		t.Errorf("fallback_iv should have allowed the selection: %v", err)
	}
}

func TestNextExpiryAndDaysToExpiry(t *testing.T) {
	now := time.Date(2026, 8, 10, 10, 30, 0, 0, IST)
	e1 := time.Date(2026, 8, 4, 0, 0, 0, 0, IST)  // past
	e2 := time.Date(2026, 8, 11, 0, 0, 0, 0, IST) // next
	e3 := time.Date(2026, 8, 18, 0, 0, 0, 0, IST)

	got, ok := NextExpiry([]time.Time{e3, e1, e2}, now)
	if !ok || !got.Equal(e2) {
		t.Errorf("NextExpiry = %v (%v), want %v", got, ok, e2)
	}
	if d := DaysToExpiry(now, e2); d != 1 {
		t.Errorf("DaysToExpiry = %d, want 1", d)
	}
	// Expiry day itself is zero, not negative.
	if d := DaysToExpiry(e2.Add(10*time.Hour), e2); d != 0 {
		t.Errorf("expiry day DaysToExpiry = %d, want 0", d)
	}
	if _, ok := NextExpiry([]time.Time{e1}, now); ok {
		t.Error("returned an expiry that has already passed")
	}
}

// The sizer needs risk per unit in rupees of premium. Delta scales the index
// move; the floor keeps a very wide index stop from producing a non-positive
// premium stop, which would make risk-per-unit meaningless.
func TestPremiumStop(t *testing.T) {
	prem := decimal.NewFromFloat(280)
	entry := decimal.NewFromFloat(24300)
	stop := decimal.NewFromFloat(24200) // 100 points

	got := PremiumStop(prem, 0.8, entry, stop)
	if want := decimal.NewFromFloat(200); !got.Equal(want) {
		t.Errorf("premium stop = %s, want %s", got, want)
	}

	// Direction must not matter: a short signal's stop is above entry.
	if up := PremiumStop(prem, -0.8, entry, decimal.NewFromFloat(24400)); !up.Equal(decimal.NewFromFloat(200)) {
		t.Errorf("short-side premium stop = %s, want 200", up)
	}

	// An index stop wider than the whole premium floors rather than going
	// negative.
	wide := PremiumStop(prem, 0.8, entry, decimal.NewFromFloat(23000))
	if !wide.IsPositive() {
		t.Errorf("premium stop %s is not positive", wide)
	}
	if wide.GreaterThan(prem) {
		t.Errorf("floored premium stop %s exceeds the premium %s", wide, prem)
	}
}
