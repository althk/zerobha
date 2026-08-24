// Command optbt prices an index-signal trade list on real weekly-option
// premium candles.
//
// The Donchian work computes its signal on the NIFTY / SENSEX 5-minute index
// chart but intends to trade a weekly option, and those are not the same
// instrument: an ITM weekly runs roughly 0.65 delta on a premium a fraction of
// index notional, so the index's per-trade edge is multiplied by ~80x — and so
// is every cost, plus theta, which has no counterpart on the index at all.
// Whether that sum is positive cannot be argued from index bps. It has to be
// measured, which is what this does.
//
//	go run ./cmd/backtest -strategy donchian ... -trades-csv idx.csv
//	go run ./cmd/optbt -trades idx.csv -underlying nifty -itm 150
//
// Method, and its limits:
//
//   - Each index trade is mapped to the nearest weekly expiry on or after its
//     entry date, and to a strike chosen either by a fixed point offset
//     (-itm) or by target delta (-delta, which backs the vol out of the
//     at-the-money premium that traded in the entry bar). A call for a long
//     signal, a put for a short — the strategy is never short an option.
//   - Entry and exit are taken at the CLOSE of the option's own 5-minute bar
//     matching the index bar the backtest entered and exited on. An intrabar
//     stop on the index therefore prices at the end of the bar it fired in,
//     not at the stop level. That is pessimistic for stop exits; it is the
//     closest honest mapping available from bar data.
//   - Premium candles are last-traded prices, so the bid-ask spread is not in
//     them. -spread-pct models it as a half-spread paid on both legs.
//   - Costs are itemised, not a flat percentage, because brokerage is a flat
//     rupee charge per order and therefore shrinks as a fraction of the trade
//     as -lots rises. Position size genuinely changes the edge here.
//   - Expired-contract history begins around 2024-10, so trades before that
//     are reported as unpriced rather than silently dropped.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/models"
	"zerobha/pkg/options"
	"zerobha/pkg/upstox"

	"github.com/shopspring/decimal"
)

var underlyings = map[string]string{
	"nifty":     "NSE_INDEX|Nifty 50",
	"banknifty": "NSE_INDEX|Nifty Bank",
	"sensex":    "BSE_INDEX|SENSEX",
}

// indexTrade is one row of the backtester's -trades-csv dump.
type indexTrade struct {
	Symbol    string
	Direction string
	EntryTime time.Time
	ExitTime  time.Time
	EntrySpot float64
	ExitSpot  float64
	Reason    string
}

// pricedTrade is an index trade after it has been mapped onto an option.
type pricedTrade struct {
	indexTrade
	Contract  upstox.ExpiredContract
	EntryPrem float64
	ExitPrem  float64
	GrossBps  float64 // return on premium, before costs
	NetBps    float64
	NetRupees float64 // one lot
	DaysToExp int
	IndexBps  float64
	// Diagnostics for the strike actually chosen, at entry.
	IV        float64 // ATM implied vol backed out of the entry bar
	Delta     float64 // |delta| of the chosen strike under that vol
	PointsITM float64 // how far in the money, in index points
	TimeValue float64 // premium minus intrinsic — what theta can take
	Capital   float64 // rupees paid for the position at entry
}

// costModel is a Zerodha-style options cost sheet. Everything except brokerage
// scales with turnover; brokerage is a flat charge per executed order, which is
// why the number of lots is a real parameter of this backtest rather than a
// presentational one.
//
// Rates are the ones Zerodha's own brokerage calculator produces for equity
// options, verified against a worked example in costs_test.go rather than
// transcribed from a rate card — an out-of-date STT rate is invisible in a
// backtest and shifts every net figure. They are charged on premium turnover,
// not on notional.
type costModel struct {
	Lots              int
	BrokeragePerOrder float64 // rupees, flat
	STTPct            float64 // sell leg only
	TxnPct            float64 // both legs, exchange transaction charge
	StampPct          float64 // buy leg only
	GSTPct            float64 // on brokerage + transaction charges
	SEBIPct           float64 // both legs
}

// charges returns total rupee costs for a round trip of n lots at the given buy
// and sell premiums.
func (c costModel) charges(buy, sell float64, lotSize int) float64 {
	qty := float64(c.Lots * lotSize)
	buyTurnover, sellTurnover := buy*qty, sell*qty

	brokerage := 2 * c.BrokeragePerOrder
	stt := sellTurnover * c.STTPct / 100
	txn := (buyTurnover + sellTurnover) * c.TxnPct / 100
	stamp := buyTurnover * c.StampPct / 100
	sebi := (buyTurnover + sellTurnover) * c.SEBIPct / 100
	gst := (brokerage + txn + sebi) * c.GSTPct / 100

	return brokerage + stt + txn + stamp + sebi + gst
}

// strikeRule says how to pick the contract for a trade.
type strikeRule struct {
	// MinDTE drops trades whose nearest weekly expiry is closer than this many
	// days. Near expiry a fixed delta buys a nearly-worthless option: at 0.75
	// delta on expiry morning the strike is only ~30 points in the money, so
	// the premium is tiny, the leverage enormous and the decay per unit of
	// premium brutal. Those days dominate a bps-weighted result.
	MinDTE int
	// Points selects `spot -/+ Points` when TargetDelta is zero.
	Points float64
	// TargetDelta, when > 0, selects the strike whose |delta| is closest to it
	// under the vol implied by the at-the-money premium in the entry bar.
	TargetDelta float64
	// SpreadPct is a half-spread as a percent of premium. SpreadTicks is the
	// same cost quoted the way an option book actually quotes it — an absolute
	// number of ticks.
	//
	// The two disagree sharply across strikes: 20 ticks is 0.36% of a 280
	// premium but only 0.07% of a 1,535 one, so a percentage assumption
	// silently over-charges cheap options and under-charges expensive ones.
	// Quoted spreads are absolute. Prefer ticks.
	SpreadPct   float64
	SpreadTicks float64
	Costs       costModel
	// FlatCostBps, when positive, replaces the itemised cost sheet with a flat
	// charge per leg. Kept so earlier runs recorded in CLAUDE.md can be
	// reproduced; the itemised model is the accurate one.
	FlatCostBps float64
}

func main() {
	tradesPath := flag.String("trades", "", "index trade list from backtest -trades-csv (required)")
	under := flag.String("underlying", "nifty", "nifty, banknifty or sensex")
	itm := flag.Float64("itm", 150, "how far in the money to buy, in index points (ignored when -delta is set)")
	targetDelta := flag.Float64("delta", 0, "buy the strike with this |delta| instead of a fixed point offset, e.g. 0.8")
	lots := flag.Int("lots", 1, "position size in lots — brokerage is flat per order, so this changes the edge")
	brokerage := flag.Float64("brokerage", 20, "flat brokerage per executed order, rupees")
	sttPct := flag.Float64("stt-pct", 0.15, "STT on the sell leg, percent of premium turnover")
	txnPct := flag.Float64("txn-pct", 0.03553, "exchange transaction charge, percent, both legs")
	stampPct := flag.Float64("stamp-pct", 0.0025, "stamp duty on the buy leg, percent")
	gstPct := flag.Float64("gst-pct", 18, "GST on brokerage and transaction charges, percent")
	costBps := flag.Float64("cost-bps", 0, "override: flat bps per leg instead of the itemised cost sheet")
	spreadPct := flag.Float64("spread-pct", 0, "half bid-ask spread as a percent of premium, charged on both legs")
	spreadTicks := flag.Float64("spread-ticks", 0, "full bid-ask in ticks (adds to -spread-pct); half is charged on each leg")
	cacheDir := flag.String("cache", "test/data/options", "directory for downloaded contract candles")
	cfgPath := flag.String("config", "config.local.toml", "config holding upstox_access_token")
	outPath := flag.String("out", "", "optional CSV dump of every priced trade")
	minDTE := flag.Int("min-dte", 0, "skip trades with fewer than this many days to expiry (2 skips expiry day and the day before)")
	split := flag.String("split", "", "optional YYYY-MM-DD; reports before/after separately")
	flag.Parse()

	if *tradesPath == "" {
		log.Fatal("-trades is required")
	}
	key, ok := underlyings[strings.ToLower(*under)]
	if !ok {
		log.Fatalf("unknown -underlying %q; known: nifty, banknifty, sensex", *under)
	}

	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load %s: %v", *cfgPath, err)
	}
	if cfg.UpstoxAccessToken == "" {
		log.Fatalf("%s has no upstox_access_token; the expired-contract endpoints need one (and an Upstox Plus plan)", *cfgPath)
	}
	client := upstox.NewClient(cfg.UpstoxAccessToken, 60*time.Second)

	trades, err := loadTrades(*tradesPath)
	if err != nil {
		log.Fatalf("read %s: %v", *tradesPath, err)
	}
	log.Printf("%d index trades from %s", len(trades), *tradesPath)

	expiries, err := loadExpiries(client, key)
	if err != nil {
		log.Fatalf("expiries for %s: %v", key, err)
	}
	log.Printf("%d expired weekly expiries, %s .. %s", len(expiries),
		expiries[0].Format("2006-01-02"), expiries[len(expiries)-1].Format("2006-01-02"))

	if err := os.MkdirAll(*cacheDir, 0o755); err != nil {
		log.Fatalf("create %s: %v", *cacheDir, err)
	}

	store := &contractStore{
		client:   client,
		cacheDir: *cacheDir,
		chains:   map[string][]upstox.ExpiredContract{},
		bars:     map[string]map[time.Time]models.Candle{},
	}

	if *lots < 1 {
		log.Fatal("-lots must be at least 1")
	}
	rule := strikeRule{
		MinDTE:      *minDTE,
		Points:      *itm,
		TargetDelta: *targetDelta,
		SpreadPct:   *spreadPct,
		SpreadTicks: *spreadTicks,
		FlatCostBps: *costBps,
		Costs: costModel{
			Lots:              *lots,
			BrokeragePerOrder: *brokerage,
			STTPct:            *sttPct,
			TxnPct:            *txnPct,
			StampPct:          *stampPct,
			GSTPct:            *gstPct,
			SEBIPct:           0.0001,
		},
	}

	var priced []pricedTrade
	skipped := map[string]int{}
	for i, t := range trades {
		p, why := priceTrade(store, key, t, expiries, rule)
		if why != "" {
			skipped[why]++
			continue
		}
		priced = append(priced, p)
		if (i+1)%100 == 0 {
			log.Printf("  ... %d/%d", i+1, len(trades))
		}
	}

	log.Printf("priced %d of %d trades", len(priced), len(trades))
	for why, n := range skipped {
		log.Printf("  unpriced: %-34s %d", why, n)
	}
	if len(priced) < 2 {
		log.Fatal("not enough priced trades to report on")
	}

	if *outPath != "" {
		if err := dump(*outPath, priced); err != nil {
			log.Fatalf("write %s: %v", *outPath, err)
		}
		log.Printf("wrote %s", *outPath)
	}

	if *targetDelta > 0 {
		fmt.Printf("\n=== %s weekly options, target |delta| %.2f ===\n", strings.ToUpper(*under), *targetDelta)
	} else {
		fmt.Printf("\n=== %s weekly options, ~%.0f points ITM ===\n", strings.ToUpper(*under), *itm)
	}
	spreadDesc := fmt.Sprintf("%.2f%% half-spread/leg", *spreadPct)
	if *spreadTicks > 0 {
		spreadDesc = fmt.Sprintf("%.0f-tick full spread", *spreadTicks)
	}
	if *costBps > 0 {
		fmt.Printf("size: %d lot(s) | costs: flat %.0f bps/leg, %s\n", *lots, *costBps, spreadDesc)
	} else {
		fmt.Printf("size: %d lot(s) | costs: Rs%.0f/order + %.2f%% STT + %.3f%% txn + %.0f%% GST, %s\n",
			*lots, *brokerage, *sttPct, *txnPct, *gstPct, spreadDesc)
	}
	report("ALL", priced)
	if *split != "" {
		before, after := splitAt(priced, *split)
		report("before "+*split, before)
		report("on/after "+*split, after)
	}
	reportByDTE(priced)
}

// ---------------------------------------------------------------- pricing

func priceTrade(store *contractStore, underlyingKey string, t indexTrade, expiries []time.Time,
	rule strikeRule) (pricedTrade, string) {

	entryDay := time.Date(t.EntryTime.Year(), t.EntryTime.Month(), t.EntryTime.Day(), 0, 0, 0, 0, options.IST)
	exp, ok := nearestExpiry(expiries, entryDay)
	if !ok {
		return pricedTrade{}, "no expired-contract data for that date"
	}

	if rule.MinDTE > 0 {
		if dte := int(exp.Sub(entryDay).Hours() / 24); dte < rule.MinDTE {
			return pricedTrade{}, fmt.Sprintf("under -min-dte %d", rule.MinDTE)
		}
	}

	isCall := t.Direction != "SHORT"
	optType := "CE"
	if !isCall {
		optType = "PE"
	}

	chain, err := store.chain(underlyingKey, exp)
	if err != nil {
		return pricedTrade{}, "chain fetch failed: " + truncErr(err)
	}

	// The index trade executes at the CLOSE of the bar starting at EntryTime,
	// so that is the instant the option is bought and the instant its time to
	// expiry is measured from.
	execAt := t.EntryTime.Add(5 * time.Minute)

	// The ATM vol is backed out either way. Under -delta it selects the strike
	// and a failure is fatal to the trade; under -itm it only labels the
	// contract the point offset happened to buy — which is the number that
	// makes the two modes comparable, so it must not gate the trade there.
	iv, why := store.atmIV(chain, t, exp)
	var target float64
	switch {
	case rule.TargetDelta > 0:
		if why != "" {
			return pricedTrade{}, why
		}
		target = options.StrikeForDelta(t.EntrySpot, options.YearsToExpiry(execAt, exp), iv, rule.TargetDelta, isCall)
	case isCall:
		target = t.EntrySpot - rule.Points
	default:
		target = t.EntrySpot + rule.Points
	}

	c, ok := pickStrike(chain, optType, target)
	if !ok {
		return pricedTrade{}, "no strike near the target"
	}

	bars, err := store.candles(c, exp)
	if err != nil {
		return pricedTrade{}, "candle fetch failed: " + truncErr(err)
	}
	// The index trade entered at the close of the bar STARTING at EntryTime,
	// and exited at the close of the bar ENDING at ExitTime.
	entryBar, ok1 := bars[t.EntryTime.In(options.IST)]
	exitBar, ok2 := bars[t.ExitTime.Add(-5*time.Minute).In(options.IST)]
	if !ok1 || !ok2 {
		return pricedTrade{}, "option did not trade in one of those bars"
	}

	entryPrem := entryBar.Close.InexactFloat64()
	exitPrem := exitBar.Close.InexactFloat64()
	if entryPrem <= 0 || exitPrem < 0 {
		return pricedTrade{}, "non-positive premium"
	}

	// Buy at the ask, sell at the bid.
	var half float64
	if rule.SpreadTicks > 0 {
		tick := c.TickSize / 100 // Upstox quotes tick size in paise
		if tick <= 0 {
			tick = 0.05
		}
		half = rule.SpreadTicks * tick / 2
	}
	buy := entryPrem + half + entryPrem*rule.SpreadPct/100
	sell := exitPrem - half - exitPrem*rule.SpreadPct/100
	if sell < 0 {
		sell = 0
	}

	qty := float64(rule.Costs.Lots * c.LotSize)
	var feesRupees float64
	if rule.FlatCostBps > 0 {
		feesRupees = (buy + sell) * qty * rule.FlatCostBps / 1e4
	} else {
		feesRupees = rule.Costs.charges(buy, sell, c.LotSize)
	}
	netRupees := (sell-buy)*qty - feesRupees

	gross := (exitPrem - entryPrem) / entryPrem * 1e4
	net := netRupees / (buy * qty) * 1e4

	sign := 1.0
	if !isCall {
		sign = -1.0
	}
	pointsITM := (t.EntrySpot - c.StrikePrice) * sign
	delta := math.Abs(options.Delta(t.EntrySpot, c.StrikePrice, options.YearsToExpiry(execAt, exp), iv, isCall))
	return pricedTrade{
		indexTrade: t,
		Contract:   c,
		EntryPrem:  entryPrem,
		ExitPrem:   exitPrem,
		GrossBps:   gross,
		NetBps:     net,
		NetRupees:  netRupees,
		Capital:    buy * qty,
		DaysToExp:  int(exp.Sub(entryDay).Hours() / 24),
		IndexBps:   sign * (t.ExitSpot - t.EntrySpot) / t.EntrySpot * 1e4,
		IV:         iv,
		Delta:      delta,
		PointsITM:  pointsITM,
		TimeValue:  entryPrem - math.Max(0, pointsITM),
	}, ""
}

// atmIV backs the implied volatility out of the at-the-money call that
// actually traded in the entry bar.
//
// Choosing a strike by delta needs a vol, and the honest source for it is the
// market at that moment rather than a constant typed into the config: weekly
// index vol moves enough across a two-year sample that a fixed guess would put
// the chosen delta systematically off in calm and stressed periods alike.
func (s *contractStore) atmIV(chain []upstox.ExpiredContract, t indexTrade, exp time.Time) (float64, string) {
	atm, ok := pickStrike(chain, "CE", t.EntrySpot)
	if !ok {
		return 0, "no ATM strike listed"
	}
	bars, err := s.candles(atm, exp)
	if err != nil {
		return 0, "ATM candle fetch failed"
	}
	bar, ok := bars[t.EntryTime.In(options.IST)]
	if !ok {
		return 0, "ATM option did not trade in the entry bar"
	}
	execAt := t.EntryTime.Add(5 * time.Minute)
	iv, ok := options.ImpliedVol(bar.Close.InexactFloat64(), t.EntrySpot, atm.StrikePrice,
		options.YearsToExpiry(execAt, exp), true)
	if !ok {
		return 0, "ATM premium outside model bounds"
	}
	return iv, ""
}

// nearestExpiry returns the first expiry on or after day.
func nearestExpiry(expiries []time.Time, day time.Time) (time.Time, bool) {
	i := sort.Search(len(expiries), func(i int) bool { return !expiries[i].Before(day) })
	if i == len(expiries) {
		return time.Time{}, false
	}
	// A weekly is at most 7 days out; anything further means the series has a
	// hole and we would be pricing the wrong contract.
	if expiries[i].Sub(day) > 8*24*time.Hour {
		return time.Time{}, false
	}
	return expiries[i], true
}

// pickStrike returns the listed strike of the requested type closest to target.
func pickStrike(chain []upstox.ExpiredContract, optType string, target float64) (upstox.ExpiredContract, bool) {
	best, found := upstox.ExpiredContract{}, false
	bestDist := math.Inf(1)
	for _, c := range chain {
		if c.InstrumentType != optType {
			continue
		}
		if d := math.Abs(c.StrikePrice - target); d < bestDist {
			best, bestDist, found = c, d, true
		}
	}
	return best, found
}

// ---------------------------------------------------------------- fetching

type contractStore struct {
	client   *upstox.Client
	cacheDir string
	chains   map[string][]upstox.ExpiredContract
	bars     map[string]map[time.Time]models.Candle
}

func (s *contractStore) chain(underlyingKey string, exp time.Time) ([]upstox.ExpiredContract, error) {
	k := underlyingKey + "|" + exp.Format("2006-01-02")
	if c, ok := s.chains[k]; ok {
		return c, nil
	}
	c, err := retry(func() ([]upstox.ExpiredContract, error) {
		return s.client.ExpiredOptionContracts(underlyingKey, exp.Format("2006-01-02"))
	})
	if err != nil {
		return nil, err
	}
	s.chains[k] = c
	return c, nil
}

// candles returns the contract's 5-minute bars indexed by start time, reading
// the on-disk cache first. One call covers a weekly's whole life, so a week of
// trades on the same strike costs one request.
func (s *contractStore) candles(c upstox.ExpiredContract, exp time.Time) (map[time.Time]models.Candle, error) {
	if b, ok := s.bars[c.InstrumentKey]; ok {
		return b, nil
	}
	path := filepath.Join(s.cacheDir, cacheName(c.InstrumentKey)+".csv")
	list, err := readCache(path)
	if err != nil {
		list, err = retry(func() ([]models.Candle, error) {
			return s.client.ExpiredHistoricalCandles(c.InstrumentKey, "5minute", exp.AddDate(0, 0, -10), exp)
		})
		if err != nil {
			return nil, err
		}
		if err := writeCache(path, list); err != nil {
			return nil, err
		}
		// Upstox rate-limits; a sweep of a thousand contracts must not trip it.
		time.Sleep(120 * time.Millisecond)
	}
	m := make(map[time.Time]models.Candle, len(list))
	for _, cd := range list {
		m[cd.StartTime.In(options.IST)] = cd
	}
	s.bars[c.InstrumentKey] = m
	return m, nil
}

// retry re-attempts a remote call with backoff. Upstox rate-limits, and a
// sweep here makes thousands of calls: without this a burst of 429s turns into
// hundreds of "unpriced" trades that look like missing data rather than
// throttling, which is a far more misleading failure than a slow run.
func retry[T any](f func() (T, error)) (T, error) {
	var zero T
	var err error
	for attempt := range 5 {
		var v T
		v, err = f()
		if err == nil {
			return v, nil
		}
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}
	return zero, err
}

// truncErr keeps a skip reason short enough to group in the summary while
// still naming the cause.
func truncErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "HTTP "); i >= 0 && len(s) > i+8 {
		return s[i : i+8]
	}
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func cacheName(instrumentKey string) string {
	r := strings.NewReplacer("|", "_", ":", "_", "/", "_", " ", "_")
	return r.Replace(instrumentKey)
}

func readCache(path string) ([]models.Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 1 {
		return nil, fmt.Errorf("bad cache %s", path)
	}
	out := make([]models.Candle, 0, len(rows)-1)
	for _, row := range rows[1:] {
		if len(row) < 6 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, row[0])
		if err != nil {
			continue
		}
		out = append(out, models.Candle{
			StartTime: ts,
			Open:      dec(row[1]),
			High:      dec(row[2]),
			Low:       dec(row[3]),
			Close:     dec(row[4]),
			Volume:    dec(row[5]),
		})
	}
	return out, nil
}

func writeCache(path string, list []models.Candle) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"timestamp", "open", "high", "low", "close", "volume"}); err != nil {
		return err
	}
	for _, c := range list {
		if err := w.Write([]string{
			c.StartTime.Format(time.RFC3339),
			c.Open.StringFixed(2), c.High.StringFixed(2), c.Low.StringFixed(2),
			c.Close.StringFixed(2), c.Volume.StringFixed(0),
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

func loadExpiries(c *upstox.Client, underlyingKey string) ([]time.Time, error) {
	raw, err := c.ExpiredExpiries(underlyingKey)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(raw))
	for _, s := range raw {
		t, err := time.ParseInLocation("2006-01-02", s, options.IST)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no expiries returned")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

// ---------------------------------------------------------------- reporting

func report(label string, ts []pricedTrade) {
	if len(ts) < 2 {
		fmt.Printf("\n%-24s (too few trades)\n", label)
		return
	}
	net := make([]float64, len(ts))
	gross := make([]float64, len(ts))
	rupees := make([]float64, len(ts))
	idx := make([]float64, len(ts))
	var gp, gl float64
	wins := 0
	for i, t := range ts {
		net[i], gross[i], rupees[i], idx[i] = t.NetBps, t.GrossBps, t.NetRupees, t.IndexBps
		if t.NetRupees > 0 {
			wins++
			gp += t.NetRupees
		} else {
			gl -= t.NetRupees
		}
	}
	mn, tn := meanT(net)
	mg, _ := meanT(gross)
	mi, ti := meanT(idx)
	pf := math.Inf(1)
	if gl > 0 {
		pf = gp / gl
	}
	fmt.Printf("\n%s  (n = %d)\n", label, len(ts))
	fmt.Printf("  index move        %+8.2f bps/trade   t = %5.2f\n", mi, ti)
	fmt.Printf("  premium gross     %+8.1f bps/trade\n", mg)
	fmt.Printf("  premium NET       %+8.1f bps/trade   t = %5.2f\n", mn, tn)
	fmt.Printf("  net per trade     %+8.0f rupees      total %+.0f\n", mean(rupees), sum(rupees))
	fmt.Printf("  capital per trade %8.0f rupees\n",
		meanOf(ts, func(t pricedTrade) float64 { return t.Capital }))
	fmt.Printf("  win rate          %8.1f%%            PF %.2f\n", 100*float64(wins)/float64(len(ts)), pf)
	fmt.Printf("  contract at entry  |delta| %.2f  %.0f pts ITM  prem %.1f  time value %.1f (%.0f%%)  IV %.1f%%\n",
		meanOf(ts, func(t pricedTrade) float64 { return t.Delta }),
		meanOf(ts, func(t pricedTrade) float64 { return t.PointsITM }),
		meanOf(ts, func(t pricedTrade) float64 { return t.EntryPrem }),
		meanOf(ts, func(t pricedTrade) float64 { return t.TimeValue }),
		100*meanOf(ts, func(t pricedTrade) float64 { return t.TimeValue / t.EntryPrem }),
		100*meanOf(ts, func(t pricedTrade) float64 { return t.IV }))
}

func meanOf(ts []pricedTrade, f func(pricedTrade) float64) float64 {
	v := make([]float64, len(ts))
	for i, t := range ts {
		v[i] = f(t)
	}
	return mean(v)
}

// reportByDTE splits by days to expiry: theta is the cost this whole exercise
// exists to measure, and it is not spread evenly across the week.
func reportByDTE(ts []pricedTrade) {
	buckets := map[int][]pricedTrade{}
	for _, t := range ts {
		buckets[t.DaysToExp] = append(buckets[t.DaysToExp], t)
	}
	keys := make([]int, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	fmt.Printf("\nBy days to expiry\n  %-5s %6s %12s %14s %10s\n", "DTE", "n", "index bps", "net prem bps", "net/lot")
	for _, k := range keys {
		b := buckets[k]
		if len(b) < 2 {
			continue
		}
		net, idx, rup := make([]float64, len(b)), make([]float64, len(b)), make([]float64, len(b))
		for i, t := range b {
			net[i], idx[i], rup[i] = t.NetBps, t.IndexBps, t.NetRupees
		}
		mn, _ := meanT(net)
		mi, _ := meanT(idx)
		fmt.Printf("  %-5d %6d %+12.2f %+14.1f %+10.0f\n", k, len(b), mi, mn, mean(rup))
	}
}

func splitAt(ts []pricedTrade, day string) (before, after []pricedTrade) {
	for _, t := range ts {
		if t.EntryTime.Format("2006-01-02") < day {
			before = append(before, t)
		} else {
			after = append(after, t)
		}
	}
	return before, after
}

func dump(path string, ts []pricedTrade) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"entry_time", "exit_time", "direction", "reason", "entry_spot", "exit_spot",
		"index_bps", "tradingsymbol", "strike", "type", "dte", "lot", "entry_prem", "exit_prem",
		"gross_bps", "net_bps", "net_rupees", "iv", "delta", "points_itm", "time_value"}); err != nil {
		return err
	}
	for _, t := range ts {
		if err := w.Write([]string{
			t.EntryTime.Format(time.RFC3339), t.ExitTime.Format(time.RFC3339), t.Direction, t.Reason,
			fmt.Sprintf("%.2f", t.EntrySpot), fmt.Sprintf("%.2f", t.ExitSpot), fmt.Sprintf("%.2f", t.IndexBps),
			t.Contract.TradingSymbol, fmt.Sprintf("%.0f", t.Contract.StrikePrice), t.Contract.InstrumentType,
			strconv.Itoa(t.DaysToExp), strconv.Itoa(t.Contract.LotSize),
			fmt.Sprintf("%.2f", t.EntryPrem), fmt.Sprintf("%.2f", t.ExitPrem),
			fmt.Sprintf("%.2f", t.GrossBps), fmt.Sprintf("%.2f", t.NetBps), fmt.Sprintf("%.2f", t.NetRupees),
			fmt.Sprintf("%.4f", t.IV), fmt.Sprintf("%.3f", t.Delta),
			fmt.Sprintf("%.0f", t.PointsITM), fmt.Sprintf("%.2f", t.TimeValue),
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

// ---------------------------------------------------------------- helpers

func loadTrades(path string) ([]indexTrade, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	r := csv.NewReader(f)
	head, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	for _, n := range []string{"direction", "entry_time", "exit_time", "entry_price", "exit_price"} {
		if _, ok := col[n]; !ok {
			return nil, fmt.Errorf("column %q missing", n)
		}
	}
	var out []indexTrade
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		et, err1 := time.Parse(time.RFC3339, rec[col["entry_time"]])
		xt, err2 := time.Parse(time.RFC3339, rec[col["exit_time"]])
		if err1 != nil || err2 != nil {
			continue
		}
		ep, _ := strconv.ParseFloat(rec[col["entry_price"]], 64)
		xp, _ := strconv.ParseFloat(rec[col["exit_price"]], 64)
		reason, sym := "", ""
		if i, ok := col["exit_reason"]; ok && i < len(rec) {
			reason = rec[i]
		}
		if i, ok := col["symbol"]; ok && i < len(rec) {
			sym = rec[i]
		}
		out = append(out, indexTrade{sym, rec[col["direction"]], et.In(options.IST), xt.In(options.IST), ep, xp, reason})
	}
	return out, nil
}

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func mean(v []float64) float64 { return sum(v) / float64(len(v)) }

func sum(v []float64) float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t
}

func meanT(v []float64) (float64, float64) {
	m := mean(v)
	if len(v) < 2 {
		return m, math.NaN()
	}
	s := 0.0
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	sd := math.Sqrt(s / float64(len(v)-1))
	if sd == 0 {
		return m, math.NaN()
	}
	return m, m / (sd / math.Sqrt(float64(len(v))))
}
