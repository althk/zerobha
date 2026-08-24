package broker

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"zerobha/pkg/options"

	"github.com/shopspring/decimal"
)

// KiteChain exposes the Kite instrument dump as an options.Chain, and live
// quotes as an options.Quoter.
//
// It is the live counterpart of the expired-contract chain cmd/optbt uses, and
// the point of both existing is that pkg/options does the strike selection for
// each — so a contract chosen in a backtest is chosen the same way in
// production, rather than by two lookalike code paths that drift apart.
type KiteChain struct {
	im *InstrumentManager
	// Quote returns the last traded price for instrument tokens.
	Quote func([]uint32) (map[uint32]float64, error)

	// underlyingName maps the symbol the strategy trades on (the index feed's
	// name) to the Name field the derivative rows carry.
	underlyingName map[string]string
}

// NewKiteChain builds a chain over an already-populated InstrumentManager.
func NewKiteChain(im *InstrumentManager, quote func([]uint32) (map[uint32]float64, error)) *KiteChain {
	return &KiteChain{
		im:    im,
		Quote: quote,
		underlyingName: map[string]string{
			"NIFTY 50":   "NIFTY",
			"NIFTY":      "NIFTY",
			"NIFTY BANK": "BANKNIFTY",
			"BANKNIFTY":  "BANKNIFTY",
			"SENSEX":     "SENSEX",
		},
	}
}

// resolve maps a feed symbol onto the derivative Name, defaulting to the
// symbol itself so an unlisted mapping fails loudly at lookup rather than
// silently selecting the wrong underlying.
func (k *KiteChain) resolve(underlying string) string {
	if name, ok := k.underlyingName[strings.ToUpper(underlying)]; ok {
		return name
	}
	return strings.ToUpper(underlying)
}

// optionsFor returns every CE/PE row for an underlying.
func (k *KiteChain) optionsFor(underlying string) []Instrument {
	rows := k.im.underlyingMap[k.resolve(underlying)]
	out := make([]Instrument, 0, len(rows))
	for _, r := range rows {
		if r.InstrumentType == "CE" || r.InstrumentType == "PE" {
			out = append(out, r)
		}
	}
	return out
}

// Expiries lists every expiry with listed options, ascending.
func (k *KiteChain) Expiries(underlying string) ([]time.Time, error) {
	rows := k.optionsFor(underlying)
	if len(rows) == 0 {
		return nil, fmt.Errorf("broker: no option instruments for underlying %q (resolved to %q)",
			underlying, k.resolve(underlying))
	}
	seen := map[string]time.Time{}
	for _, r := range rows {
		if r.Expiry.IsZero() {
			continue
		}
		seen[r.Expiry.Format("2006-01-02")] = r.Expiry
	}
	out := make([]time.Time, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

// Contracts returns the options expiring on a given date.
func (k *KiteChain) Contracts(underlying string, expiry time.Time) ([]options.Contract, error) {
	want := expiry.Format("2006-01-02")
	var out []options.Contract
	for _, r := range k.optionsFor(underlying) {
		if r.Expiry.Format("2006-01-02") != want {
			continue
		}
		out = append(out, options.Contract{
			TradingSymbol:   r.Tradingsymbol,
			InstrumentToken: r.Token,
			Exchange:        r.Exchange,
			Expiry:          r.Expiry,
			Strike:          decimal.NewFromFloat(r.Strike),
			IsCall:          r.InstrumentType == "CE",
			LotSize:         r.LotSize,
			TickSize:        decimal.NewFromFloat(r.TickSize),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("broker: no contracts for %s expiring %s", underlying, want)
	}
	return out, nil
}

// Premium reads the contract's last traded price.
func (k *KiteChain) Premium(c options.Contract) (decimal.Decimal, error) {
	if k.Quote == nil {
		return decimal.Zero, fmt.Errorf("broker: no quote function configured")
	}
	prices, err := k.Quote([]uint32{c.InstrumentToken})
	if err != nil {
		return decimal.Zero, fmt.Errorf("broker: quote %s: %w", c.TradingSymbol, err)
	}
	p, ok := prices[c.InstrumentToken]
	if !ok || p <= 0 {
		// A missing or zero quote must not be turned into a zero premium: the
		// sizer would divide by it and the strike selector would invert it.
		return decimal.Zero, fmt.Errorf("broker: no traded price for %s", c.TradingSymbol)
	}
	return decimal.NewFromFloat(p), nil
}

// OptionExecutor adapts a chain and a selector into the interface the strategy
// consumes, so the strategy depends on neither the instrument dump nor Kite.
type OptionExecutor struct {
	Chain    *KiteChain
	Selector options.Selector
}

// Select picks the contract expressing a signal.
func (e OptionExecutor) Select(underlying string, spot decimal.Decimal, now time.Time, isCall bool) (options.Contract, error) {
	return e.Selector.Select(underlying, spot, now, isCall, e.Chain, e.Chain)
}

// Premium reads a contract's traded price.
func (e OptionExecutor) Premium(c options.Contract) (decimal.Decimal, error) {
	return e.Chain.Premium(c)
}
