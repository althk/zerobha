package options

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Contract is one listed option, in the terms an order needs.
type Contract struct {
	TradingSymbol   string
	InstrumentToken uint32
	Exchange        string // NFO for NIFTY, BFO for SENSEX
	Expiry          time.Time
	Strike          decimal.Decimal
	IsCall          bool
	LotSize         int
	TickSize        decimal.Decimal
}

// Chain supplies the listed contracts for an underlying. The backtest
// implements it against Upstox's expired-instrument endpoints and the trader
// against the Kite instrument dump, so both select strikes identically.
type Chain interface {
	// Expiries returns every listed expiry for the underlying, ascending.
	Expiries(underlying string) ([]time.Time, error)
	// Contracts returns the options expiring on that date.
	Contracts(underlying string, expiry time.Time) ([]Contract, error)
}

// Quoter returns the last traded premium for a contract. Selecting by delta
// needs a volatility and the only honest one is the market's, so the selector
// prices the at-the-money strike and inverts it.
type Quoter interface {
	Premium(c Contract) (decimal.Decimal, error)
}

// Selector picks the contract that expresses a directional index signal.
type Selector struct {
	// TargetDelta is the |delta| to buy, e.g. 0.80. A long signal buys a call
	// and a short signal buys a put; the strategy is never short an option.
	TargetDelta float64

	// TargetDeltaNearExpiry is the |delta| used when time to expiry is <= 3 days.
	// Near expiry (Tue/Wed for a Thu expiry), time decay accelerates; buying a
	// deeper ITM strike (e.g. 0.90) reduces extrinsic time value and theta bleed.
	// If 0, TargetDelta is used for all DTEs.
	TargetDeltaNearExpiry float64

	// MinDaysToExpiry refuses to trade when the nearest expiry is closer than
	// this. It is not a preference — near expiry a fixed delta buys a nearly
	// worthless contract, because what sets delta is the move in units of
	// sigma*sqrt(T) and T has collapsed. On NIFTY expiry morning the 0.8-delta
	// strike sits ~30 points in the money: tiny premium, enormous leverage and
	// brutal decay per rupee. Measured at -1571 bps a trade against -104 to
	// +106 for the rest of the week. 2 skips expiry day and the day before.
	MinDaysToExpiry int

	// FallbackIV is used only when the ATM premium cannot be read. Selecting on
	// a stale constant is worse than not trading, so zero means "refuse".
	FallbackIV float64
}

// ErrTooCloseToExpiry is returned when MinDaysToExpiry blocks the trade. It is
// a routine no-trade condition, not a failure, and callers should treat it that
// way rather than logging it as an error.
var ErrTooCloseToExpiry = fmt.Errorf("options: within the minimum days to expiry")

// Select returns the contract to buy for a signal on `underlying` at `spot`,
// as of `now`. isCall is true for a long index signal.
func (s Selector) Select(underlying string, spot decimal.Decimal, now time.Time, isCall bool,
	chain Chain, quoter Quoter) (Contract, error) {

	var zero Contract
	if !spot.IsPositive() {
		return zero, fmt.Errorf("options: non-positive spot %s", spot)
	}
	if s.TargetDelta <= 0 || s.TargetDelta >= 1 {
		return zero, fmt.Errorf("options: target delta %.2f must be between 0 and 1", s.TargetDelta)
	}

	expiries, err := chain.Expiries(underlying)
	if err != nil {
		return zero, fmt.Errorf("options: expiries for %s: %w", underlying, err)
	}
	expiry, ok := NextExpiry(expiries, now)
	if !ok {
		return zero, fmt.Errorf("options: no expiry listed for %s after %s", underlying, now.Format("2006-01-02"))
	}
	dte := DaysToExpiry(now, expiry)
	if dte < s.MinDaysToExpiry {
		return zero, ErrTooCloseToExpiry
	}

	contracts, err := chain.Contracts(underlying, expiry)
	if err != nil {
		return zero, fmt.Errorf("options: chain for %s %s: %w", underlying, expiry.Format("2006-01-02"), err)
	}
	if len(contracts) == 0 {
		return zero, fmt.Errorf("options: empty chain for %s %s", underlying, expiry.Format("2006-01-02"))
	}

	years := YearsToExpiry(now, expiry)
	spotF := spot.InexactFloat64()

	iv, err := s.atmIV(spotF, years, contracts, quoter)
	if err != nil {
		return zero, err
	}

	targetDelta := s.TargetDelta
	if s.TargetDeltaNearExpiry > 0 && dte <= 3 {
		targetDelta = s.TargetDeltaNearExpiry
	}

	target := StrikeForDelta(spotF, years, iv, targetDelta, isCall)
	c, ok := nearestStrike(contracts, isCall, target)
	if !ok {
		return zero, fmt.Errorf("options: no %s strike near %.0f in the %s chain",
			callPut(isCall), target, expiry.Format("2006-01-02"))
	}
	return c, nil
}

// atmIV inverts the at-the-money call premium for a volatility.
func (s Selector) atmIV(spot, years float64, contracts []Contract, quoter Quoter) (float64, error) {
	fail := func(reason string, err error) (float64, error) {
		if s.FallbackIV > 0 {
			return s.FallbackIV, nil
		}
		return 0, fmt.Errorf("options: %s (and no fallback_iv is configured): %w", reason, err)
	}

	if quoter == nil {
		return fail("no quoter to read the ATM premium", nil)
	}
	atm, ok := nearestStrike(contracts, true, spot)
	if !ok {
		return fail("no ATM call listed", nil)
	}
	prem, err := quoter.Premium(atm)
	if err != nil {
		return fail("could not read the ATM premium", err)
	}
	iv, ok := ImpliedVol(prem.InexactFloat64(), spot, atm.Strike.InexactFloat64(), years, true)
	if !ok {
		return fail(fmt.Sprintf("ATM premium %s is outside the model's bounds", prem), nil)
	}
	return iv, nil
}

// nearestStrike returns the listed contract of the requested type whose strike
// is closest to target.
func nearestStrike(contracts []Contract, isCall bool, target float64) (Contract, bool) {
	best, found := Contract{}, false
	bestDist := math.Inf(1)
	for _, c := range contracts {
		if c.IsCall != isCall {
			continue
		}
		if d := math.Abs(c.Strike.InexactFloat64() - target); d < bestDist {
			best, bestDist, found = c, d, true
		}
	}
	return best, found
}

// NextExpiry returns the first expiry on or after now's date.
func NextExpiry(expiries []time.Time, now time.Time) (time.Time, bool) {
	day := dayOf(now)
	sorted := append([]time.Time(nil), expiries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	for _, e := range sorted {
		if !dayOf(e).Before(day) {
			return e, true
		}
	}
	return time.Time{}, false
}

// DaysToExpiry counts calendar days from now's date to expiry's date. Expiry
// day itself is 0.
func DaysToExpiry(now, expiry time.Time) int {
	return int(dayOf(expiry).Sub(dayOf(now)).Hours() / 24)
}

func dayOf(t time.Time) time.Time {
	t = t.In(IST)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, IST)
}

func callPut(isCall bool) string {
	if isCall {
		return "CE"
	}
	return "PE"
}

// PremiumStop converts an index-level stop into an approximate premium level,
// for SIZING only.
//
// The real exit is driven by the index — no resting order on an option can
// express "the NIFTY closed below X" — so this is not where the trade gets
// closed. It exists because the position sizer works in risk per unit, and the
// unit here is a rupee of premium rather than a point of index.
//
// Delta is a local approximation and understates the loss on a large adverse
// move (gamma works against a long option holder's estimate here only in that
// delta falls as the option goes out of the money, so the realised premium loss
// is SMALLER than the linear estimate — the sizing is therefore conservative,
// which is the right direction to be wrong in).
func PremiumStop(premium decimal.Decimal, delta float64, indexEntry, indexStop decimal.Decimal) decimal.Decimal {
	move := indexEntry.Sub(indexStop).Abs()
	drop := move.Mul(decimal.NewFromFloat(math.Abs(delta)))
	stop := premium.Sub(drop)
	if !stop.IsPositive() {
		// A stop at or below zero makes risk-per-unit meaningless. Floor it at
		// a token fraction of premium so the sizer still sees real risk.
		return premium.Mul(decimal.NewFromFloat(0.05))
	}
	return stop
}
