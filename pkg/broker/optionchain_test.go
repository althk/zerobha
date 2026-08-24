package broker

import (
	"errors"
	"testing"
	"time"

	"zerobha/pkg/options"

	"github.com/shopspring/decimal"
)

func chainFixture() *KiteChain {
	im := NewInstrumentManager()
	exp1 := time.Date(2026, 8, 11, 0, 0, 0, 0, options.IST)
	exp2 := time.Date(2026, 8, 18, 0, 0, 0, 0, options.IST)

	var token uint32 = 1000
	add := func(name, exchange string, expiry time.Time, strike float64, typ string) {
		token++
		im.underlyingMap[name] = append(im.underlyingMap[name], Instrument{
			Token:          token,
			Tradingsymbol:  name + typ,
			Name:           name,
			Expiry:         expiry,
			Strike:         strike,
			TickSize:       0.05,
			LotSize:        65,
			InstrumentType: typ,
			Exchange:       exchange,
		})
	}
	for _, e := range []time.Time{exp1, exp2} {
		for _, k := range []float64{24000, 24100, 24200, 24300} {
			add("NIFTY", "NFO", e, k, "CE")
			add("NIFTY", "PE", e, k, "PE")
		}
	}
	// A future on the same underlying must not be offered as an option.
	add("NIFTY", "NFO", exp2, 0, "FUT")

	return NewKiteChain(im, func(tokens []uint32) (map[uint32]float64, error) {
		out := map[uint32]float64{}
		for _, t := range tokens {
			out[t] = 210.5
		}
		return out, nil
	})
}

// The strategy trades a feed symbol ("NIFTY 50"); the derivative rows are keyed
// by the underlying name ("NIFTY"). Getting that mapping wrong yields an empty
// chain and a strategy that silently never trades.
func TestKiteChainResolvesFeedSymbolToUnderlying(t *testing.T) {
	k := chainFixture()
	for _, sym := range []string{"NIFTY 50", "NIFTY", "nifty 50"} {
		if _, err := k.Expiries(sym); err != nil {
			t.Errorf("Expiries(%q): %v", sym, err)
		}
	}
	if _, err := k.Expiries("NOT LISTED"); err == nil {
		t.Error("an unlisted underlying must fail loudly, not return an empty chain")
	}
}

func TestKiteChainExpiriesAreSortedAndDeduped(t *testing.T) {
	k := chainFixture()
	got, err := k.Expiries("NIFTY 50")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d expiries, want 2 (deduped): %v", len(got), got)
	}
	if !got[0].Before(got[1]) {
		t.Errorf("expiries not ascending: %v", got)
	}
}

// A FUT row shares the underlying name. Selecting it would place a futures
// order against an option-sized position.
func TestKiteChainExcludesNonOptions(t *testing.T) {
	k := chainFixture()
	expiries, err := k.Expiries("NIFTY 50")
	if err != nil {
		t.Fatal(err)
	}
	contracts, err := k.Contracts("NIFTY 50", expiries[len(expiries)-1])
	if err != nil {
		t.Fatal(err)
	}
	if len(contracts) == 0 {
		t.Fatal("no contracts")
	}
	for _, c := range contracts {
		if c.Strike.IsZero() {
			t.Errorf("a non-option row leaked into the chain: %+v", c)
		}
	}
}

// A missing or zero quote must fail rather than becoming a zero premium: the
// sizer divides by it and the strike selector inverts it.
func TestKiteChainPremiumRefusesZero(t *testing.T) {
	k := chainFixture()
	c := options.Contract{TradingSymbol: "X", InstrumentToken: 42}

	k.Quote = func([]uint32) (map[uint32]float64, error) { return map[uint32]float64{42: 0}, nil }
	if _, err := k.Premium(c); err == nil {
		t.Error("a zero quote was accepted as a premium")
	}

	k.Quote = func([]uint32) (map[uint32]float64, error) { return map[uint32]float64{}, nil }
	if _, err := k.Premium(c); err == nil {
		t.Error("a missing quote was accepted as a premium")
	}

	k.Quote = func([]uint32) (map[uint32]float64, error) { return nil, errors.New("network") }
	if _, err := k.Premium(c); err == nil {
		t.Error("a failed quote call was accepted as a premium")
	}

	k.Quote = nil
	if _, err := k.Premium(c); err == nil {
		t.Error("no quote function at all was accepted")
	}
}

// End to end through the real selector: the executor must return a listed,
// in-the-money contract of the right type.
func TestOptionExecutorSelectsAListedContract(t *testing.T) {
	k := chainFixture()
	// The fixture prices every strike at the same premium, which is not a real
	// surface, so pin the vol instead of inverting a fake one.
	exec := OptionExecutor{
		Chain:    k,
		Selector: options.Selector{TargetDelta: 0.80, MinDaysToExpiry: 0, FallbackIV: 0.13},
	}
	k.Quote = func([]uint32) (map[uint32]float64, error) { return nil, errors.New("force the fallback") }

	now := time.Date(2026, 8, 10, 10, 30, 0, 0, options.IST)
	spot := decimal.NewFromInt(24250)

	c, err := exec.Select("NIFTY 50", spot, now, true)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if !c.IsCall {
		t.Error("a long view did not buy a call")
	}
	if c.LotSize != 65 || c.Exchange != "NFO" {
		t.Errorf("contract is missing routing/sizing detail: %+v", c)
	}
	if !c.Strike.LessThan(spot) {
		t.Errorf("call strike %s is not in the money against spot %s", c.Strike, spot)
	}
}
