// Package options turns an index-level trading signal into the specific weekly
// option contract that expresses it.
//
// It exists because the Donchian strategy computes its signal on the NIFTY /
// SENSEX index chart but trades an option, and those are not the same
// instrument. Selecting the contract needs a volatility, and the honest source
// for one is the at-the-money premium trading at that moment — not a constant
// in a config file. Everything here is pure: callers supply spot, the chain and
// a way to price the ATM strike, and get back a contract.
//
// The same code serves the backtest (cmd/optbt, against expired contracts) and
// live trading (cmd/trader, against the Kite instrument dump), so a strike
// chosen in a backtest is chosen the same way in production.
package options

import (
	"math"
	"time"
)

// IST is the exchange timezone. Expiry is a date; the contract dies at 15:30.
var IST = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return time.FixedZone("IST", 5*3600+1800)
	}
	return loc
}()

// Black-Scholes, used here for one job only: choosing a strike by delta rather
// than by a fixed number of index points.
//
// The two are not interchangeable. 150 points ITM on a NIFTY weekly is roughly
// 0.95 delta on expiry morning and roughly 0.68 delta six days out, because
// what sets delta is the move in units of sigma*sqrt(T), not in points. Holding
// the point offset fixed therefore silently sweeps delta across the week, and
// with it both the leverage and the theta the trade carries.
//
// Volatility is not assumed: it is backed out of the at-the-money premium that
// actually traded in the entry bar, so the strike is chosen against the market's
// own vol at that moment. Skew is ignored — the ATM vol is applied to the ITM
// strike — which biases the chosen strike slightly, not the P&L, since the P&L
// then comes from that contract's real candles.

const (
	// riskFree and divYield are the carry terms in d1. Over a holding period of
	// hours on a weekly option their effect on the chosen strike is a point or
	// two; they are here for correctness, not because the result turns on them.
	riskFree = 0.065
	divYield = 0.012

	// minYears floors time to expiry at about ten minutes. Delta is a step
	// function in the last moments before expiry and the inversion below stops
	// being meaningful; the floor keeps both finite rather than pretending to a
	// precision that is not there.
	minYears = 10.0 / (60 * 24 * 365)
)

// normCDF is the standard normal CDF, via the complementary error function.
func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// normInvCDF is the inverse standard normal CDF (Acklam's rational
// approximation, ~1e-9 relative accuracy — far tighter than the 50-point strike
// grid it feeds).
func normInvCDF(p float64) float64 {
	if p <= 0 || p >= 1 {
		return math.NaN()
	}
	a := [6]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [5]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01}
	c := [6]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [4]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00}
	const plow, phigh = 0.02425, 1 - 0.02425

	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > phigh:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	default:
		q := p - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	}
}

func d1of(s, k, t, sigma float64) float64 {
	return (math.Log(s/k) + (riskFree-divYield+sigma*sigma/2)*t) / (sigma * math.Sqrt(t))
}

// Price values a European call or put on an index.
func Price(s, k, t, sigma float64, isCall bool) float64 {
	if t <= 0 || sigma <= 0 || s <= 0 || k <= 0 {
		return math.Max(0, intrinsic(s, k, isCall))
	}
	a := d1of(s, k, t, sigma)
	b := a - sigma*math.Sqrt(t)
	dfR, dfQ := math.Exp(-riskFree*t), math.Exp(-divYield*t)
	if isCall {
		return s*dfQ*normCDF(a) - k*dfR*normCDF(b)
	}
	return k*dfR*normCDF(-b) - s*dfQ*normCDF(-a)
}

func intrinsic(s, k float64, isCall bool) float64 {
	if isCall {
		return s - k
	}
	return k - s
}

// Delta is the option's delta: positive for a call, negative for a put.
func Delta(s, k, t, sigma float64, isCall bool) float64 {
	if t <= 0 || sigma <= 0 {
		if intrinsic(s, k, isCall) > 0 {
			if isCall {
				return 1
			}
			return -1
		}
		return 0
	}
	dq := math.Exp(-divYield * t)
	if isCall {
		return dq * normCDF(d1of(s, k, t, sigma))
	}
	return -dq * normCDF(-d1of(s, k, t, sigma))
}

// ImpliedVol inverts Price for sigma by bisection. Bisection rather than
// Newton because vega collapses on a deep-ITM or nearly-expired option and
// Newton diverges exactly where this is asked the hardest; price is monotone in
// sigma, so bisection always converges.
//
// Returns ok=false when the observed price is outside the arbitrage bounds the
// model can reproduce — which happens on stale or crossed prints, and must not
// be papered over with a made-up vol.
func ImpliedVol(price, s, k, t float64, isCall bool) (float64, bool) {
	if t <= 0 || price <= 0 || s <= 0 || k <= 0 {
		return 0, false
	}
	lo, hi := 0.005, 5.0
	if price <= Price(s, k, t, lo, isCall) || price >= Price(s, k, t, hi, isCall) {
		return 0, false
	}
	for range 100 {
		mid := (lo + hi) / 2
		if Price(s, k, t, mid, isCall) < price {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-6 {
			break
		}
	}
	return (lo + hi) / 2, true
}

// StrikeForDelta returns the strike whose delta has the requested magnitude.
//
// Inverting N(d1) = target for K:
//
//	d1 = N^-1(target)  (call)      d1 = -N^-1(target)  (put)
//	K  = S * exp(-d1*sigma*sqrt(T) + (r-q+sigma^2/2)*T)
//
// which lands below spot for a call and above it for a put — in the money in
// both cases, as intended.
func StrikeForDelta(s, t, sigma, target float64, isCall bool) float64 {
	if t < minYears {
		t = minYears
	}
	if sigma <= 0 || target <= 0 || target >= 1 {
		return s
	}
	z := normInvCDF(target)
	if !isCall {
		z = -z
	}
	return s * math.Exp(-z*sigma*math.Sqrt(t)+(riskFree-divYield+sigma*sigma/2)*t)
}

// YearsToExpiry measures from a bar to the 15:30 IST close on expiry day, in
// calendar years — the convention the listed premium is quoted on.
func YearsToExpiry(now, expiryDay time.Time) float64 {
	exp := time.Date(expiryDay.Year(), expiryDay.Month(), expiryDay.Day(), 15, 30, 0, 0, IST)
	y := exp.Sub(now).Hours() / (24 * 365)
	if y < minYears {
		return minYears
	}
	return y
}
