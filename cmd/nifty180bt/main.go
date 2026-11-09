// Command nifty180bt backtests the "Nifty 180" opening option buy
// (../nifty180/nifty180.py) on real 1-minute candles of expired NIFTY weekly
// options.
//
// The rule, as the live script trades it:
//
//   - 09:25: on the nearest weekly expiry, take the CE and the PE (strikes
//     within 300 points of spot) whose last price is closest to 170.
//   - 09:29-09:45: buy the first of the two whose LTP is inside [180, 190],
//     market order. One trade a day.
//   - Exits are a GTT OCO keyed off the trigger LTP: stop at LTP-20 (a sell
//     limit 2 below that), target at LTP+40. Anything open at 09:45 is sold at
//     market.
//
// Bar data cannot say what happened inside a minute, so the mapping is
// deliberately conservative: a bar that touches both stop and target is a
// stop, and in the minute an entry fills mid-bar only that bar's close is
// treated as post-entry.
//
// -underlying sensex -delta D ports the rule to SENSEX, where a fixed 170
// premium is a different option altogether: at 09:25 it takes the strike
// whose delta is closest to D, then scales the trigger, band, stop, target
// and stop-limit buffer by that day's premium / -search, so every level keeps
// the same percentage of the entry as it has on NIFTY.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/models"
	"zerobha/pkg/costs"
	"zerobha/pkg/options"
	"zerobha/pkg/upstox"

	"github.com/shopspring/decimal"
)

var underlyings = map[string]string{
	"nifty":  "NSE_INDEX|Nifty 50",
	"sensex": "BSE_INDEX|SENSEX",
}

type params struct {
	SearchTarget float64 // premium the 09:25 scan looks for
	Trigger      float64 // entry band low
	BandWidth    float64 // entry band is [Trigger, Trigger+BandWidth]
	SL, TP       float64 // points from the trigger LTP
	SLLimitBuf   float64 // the stop leg is a sell limit this far below the trigger
	StrikeWindow float64 // strikes considered: spot +/- this
	// TargetDelta, when set, picks the strike by |delta| instead of premium
	// and scales the price levels per leg (see forLeg).
	TargetDelta float64
	HalfSpread  float64 // rupees, paid on every market fill
	Lots        int
	// CrossFromBelow only enters a leg already seen under Trigger.
	CrossFromBelow bool
	Setup, Start   time.Duration // offsets from midnight IST
	Exit           time.Duration
	// Trail > 0: a trade in profit at Exit drops its target and instead trails
	// a stop Trail points under its highest price (never under the trigger
	// LTP), until Cutoff. A trade not in profit at Exit is sold as before.
	Trail  float64
	Cutoff time.Duration
}

// scaled returns p with every price level multiplied by k.
func (p params) scaled(k float64) params {
	p.Trigger *= k
	p.BandWidth *= k
	p.SL *= k
	p.TP *= k
	p.SLLimitBuf *= k
	p.Trail *= k
	return p
}

// forLeg is the parameter set a selected leg trades with: as given when
// selecting by premium, scaled by its own 09:25 premium when selecting by delta.
func (p params) forLeg(l pick) params {
	if p.TargetDelta <= 0 {
		return p
	}
	return p.scaled(l.price / p.SearchTarget)
}

func at(day time.Time, off time.Duration) time.Time { return day.Add(off) }

func hm(h, m int) time.Duration { return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute }

// ---------------------------------------------------------------- data

type store struct {
	client  *upstox.Client
	key     string // underlying instrument key
	dir     string
	chains  map[string][]upstox.ExpiredContract
	bars    map[string]map[time.Time]models.Candle
	fetches int
	failed  bool // a download failed since the flag was last cleared
}

func (s *store) chain(exp time.Time) ([]upstox.ExpiredContract, error) {
	k := exp.Format("2006-01-02")
	if c, ok := s.chains[k]; ok {
		return c, nil
	}
	c, err := retry(func() ([]upstox.ExpiredContract, error) { return s.client.ExpiredOptionContracts(s.key, k) })
	if err != nil {
		return nil, err
	}
	s.chains[k] = c
	return c, nil
}

// candles shares optbt's cache file naming, so contracts either tool fetched
// are reused by the other.
func (s *store) candles(c upstox.ExpiredContract, exp time.Time) (map[time.Time]models.Candle, error) {
	if b, ok := s.bars[c.InstrumentKey]; ok {
		return b, nil
	}
	path := filepath.Join(s.dir, cacheName(c.InstrumentKey)+"_1minute.csv")
	list, err := readCache(path)
	if err != nil {
		list, err = retry(func() ([]models.Candle, error) {
			return s.client.ExpiredHistoricalCandles(c.InstrumentKey, "1minute", exp.AddDate(0, 0, -10), exp)
		})
		if err != nil {
			return nil, err
		}
		if err := writeCache(path, list); err != nil {
			return nil, err
		}
		s.fetches++
		// Upstox allows ~2,000 requests per 30 minutes; stay under it.
		time.Sleep(1 * time.Second)
	}
	m := make(map[time.Time]models.Candle, len(list))
	for _, cd := range list {
		m[cd.StartTime.In(options.IST)] = cd
	}
	s.bars[c.InstrumentKey] = m
	return m, nil
}

// indexBars loads the underlying's 1-minute index candles month by month. Completed
// months are cached; the current one is always refetched.
func (s *store) indexBars(from, to time.Time) (map[time.Time]models.Candle, error) {
	out := map[time.Time]models.Candle{}
	now := time.Now().In(options.IST)
	for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, options.IST); !m.After(to); m = m.AddDate(0, 1, 0) {
		end := m.AddDate(0, 1, -1)
		path := filepath.Join(s.dir, cacheName(s.key)+"_1minute_"+m.Format("2006-01")+".csv")
		list, err := readCache(path)
		if err != nil {
			list, err = retry(func() ([]models.Candle, error) { return s.client.HistoricalCandles(s.key, "minutes", 1, m, end) })
			if err != nil {
				return nil, fmt.Errorf("index %s: %w", m.Format("2006-01"), err)
			}
			if end.Before(now.AddDate(0, 0, -1)) {
				if err := writeCache(path, list); err != nil {
					return nil, err
				}
			}
		}
		for _, cd := range list {
			out[cd.StartTime.In(options.IST)] = cd
		}
	}
	return out, nil
}

// lastBefore is the LTP at instant t: the close of the latest bar that ended
// by t on that session.
func lastBefore(bars map[time.Time]models.Candle, day, t time.Time) (float64, bool) {
	open := at(day, hm(9, 15))
	for b := t.Add(-time.Minute); !b.Before(open); b = b.Add(-time.Minute) {
		if c, ok := bars[b]; ok {
			return f(c.Close), true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------- strategy

type pick struct {
	c     upstox.ExpiredContract
	bars  map[time.Time]models.Candle
	price float64
	delta float64 // |delta| at 09:25 from the leg's own implied vol; NaN if unsolvable
}

// absDelta is |delta| at instant t, backing the vol out of the option's own
// price so skew is respected. NaN when the price is outside model bounds.
func absDelta(c upstox.ExpiredContract, price, spot float64, t, exp time.Time) float64 {
	isCall := c.InstrumentType == "CE"
	yrs := options.YearsToExpiry(t, exp)
	iv, ok := options.ImpliedVol(price, spot, c.StrikePrice, yrs, isCall)
	if !ok {
		return math.NaN()
	}
	return math.Abs(options.Delta(spot, c.StrikePrice, yrs, iv, isCall))
}

// selectStrike finds the contract of optType whose 09:25 price (or |delta|,
// with TargetDelta) is closest to the target. Both fall monotonically as the
// strike moves out of the money, so instead of fetching
// every strike in the window it walks from the money towards the target and
// stops one strike past the crossing — the same answer as a full scan, at a
// fraction of the downloads.
func selectStrike(s *store, chain []upstox.ExpiredContract, exp, day time.Time, spot float64, optType string, p params) (pick, bool) {
	var ks []upstox.ExpiredContract
	for _, c := range chain {
		if c.InstrumentType == optType && math.Abs(c.StrikePrice-spot) <= p.StrikeWindow {
			ks = append(ks, c)
		}
	}
	if len(ks) == 0 {
		return pick{}, false
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].StrikePrice < ks[j].StrikePrice })

	setup := at(day, p.Setup)
	seen := map[int]pick{}
	eval := func(i int) (pick, bool) {
		if pk, ok := seen[i]; ok {
			return pk, true
		}
		bars, err := s.candles(ks[i], exp)
		if err != nil {
			// A failed download is not "no price": treating it as one would
			// quietly pick a different strike. Flag it so the day is skipped.
			s.failed = true
			return pick{}, false
		}
		px, ok := lastBefore(bars, day, setup)
		if !ok {
			return pick{}, false
		}
		pk := pick{ks[i], bars, px, absDelta(ks[i], px, spot, setup, exp)}
		if p.TargetDelta > 0 && math.IsNaN(pk.delta) {
			return pick{}, false
		}
		seen[i] = pk
		return pk, true
	}
	val, tgt := func(pk pick) float64 { return pk.price }, p.SearchTarget
	if p.TargetDelta > 0 {
		val, tgt = func(pk pick) float64 { return pk.delta }, p.TargetDelta
	}

	anchor := 0
	for i := range ks {
		if math.Abs(ks[i].StrikePrice-spot) < math.Abs(ks[anchor].StrikePrice-spot) {
			anchor = i
		}
	}
	p0, ok := eval(anchor)
	for d := 1; !ok && d < len(ks); d++ { // nearest strike with a price
		for _, i := range []int{anchor - d, anchor + d} {
			if i >= 0 && i < len(ks) {
				if p0, ok = eval(i); ok {
					anchor = i
					break
				}
			}
		}
	}
	if !ok {
		return pick{}, false
	}

	otm := 1 // CE premium and delta fall as strike rises
	if optType == "PE" {
		otm = -1
	}
	dir := -otm
	if val(p0) > tgt {
		dir = otm
	}
	if back := anchor - dir; back >= 0 && back < len(ks) {
		eval(back) // one step back, in case the anchor overshot
	}
	misses := 0
	for i := anchor + dir; i >= 0 && i < len(ks); i += dir {
		pk, ok := eval(i)
		if !ok {
			if misses++; misses > 3 {
				break
			}
			continue
		}
		misses = 0
		if (val(pk)-tgt)*(val(p0)-tgt) <= 0 {
			break
		}
	}

	best, bestD := pick{}, math.Inf(1)
	for _, pk := range seen {
		if d := math.Abs(val(pk) - tgt); d < bestD {
			best, bestD = pk, d
		}
	}
	return best, true
}

type trigger struct {
	at     time.Time
	price  float64
	atOpen bool // the whole minute is post-entry
}

// firstTrigger is the first moment the leg's LTP is inside the entry band.
func firstTrigger(l pick, day time.Time, p params) (trigger, bool) {
	start, exit := at(day, p.Start), at(day, p.Exit)
	lo, hi := p.Trigger, p.Trigger+p.BandWidth
	// With CrossFromBelow the leg must first be seen under the trigger (the
	// 09:25 price counts), as the fixed live script requires.
	armed := !p.CrossFromBelow || l.price < lo
	if px, ok := lastBefore(l.bars, day, start); ok {
		if px < lo {
			armed = true
		} else if armed && px <= hi {
			return trigger{start, px, true}, true
		}
	}
	for t := start; t.Before(exit); t = t.Add(time.Minute) {
		b, ok := l.bars[t]
		if !ok {
			continue
		}
		o, h, lw, c := f(b.Open), f(b.High), f(b.Low), f(b.Close)
		if !armed {
			if lw < lo {
				armed = true
				if c >= lo && c <= hi { // dipped under and came back inside the bar
					return trigger{t, lo, false}, true
				}
			}
			continue
		}
		switch {
		case o >= lo && o <= hi:
			return trigger{t, o, true}, true
		case o < lo && h >= lo:
			return trigger{t, lo, false}, true
		case o > hi && lw <= hi:
			return trigger{t, hi, false}, true
		}
	}
	return trigger{}, false
}

// exitTrade walks the OCO from the entry to 09:45 and returns the sell price.
func exitTrade(l pick, tr trigger, day time.Time, p params) (float64, string, time.Time) {
	sl, tp := tr.price-p.SL, tr.price+p.TP
	slLimit := sl - p.SLLimitBuf
	hs := p.HalfSpread
	exit := at(day, p.Exit)
	resting := false // stop triggered, limit not yet filled
	for t := tr.at; t.Before(exit); t = t.Add(time.Minute) {
		b, ok := l.bars[t]
		if !ok {
			continue
		}
		o, h, lw, c := f(b.Open), f(b.High), f(b.Low), f(b.Close)
		if t.Equal(tr.at) && !tr.atOpen {
			o, h, lw = c, c, c
		}
		if resting {
			if h >= slLimit {
				return math.Max(o, slLimit), "SL", t
			}
			continue
		}
		switch {
		case o <= sl:
			if o-hs >= slLimit {
				return o - hs, "SL", t
			}
			resting = true
			if h >= slLimit {
				return slLimit, "SL", t
			}
		case o >= tp:
			return math.Max(o-hs, tp), "TARGET", t
		case lw <= sl: // stop first when a bar spans both
			return math.Max(sl-hs, slLimit), "SL", t
		case h >= tp:
			return tp, "TARGET", t
		}
	}
	px := 0.0
	if b, ok := l.bars[exit]; ok {
		px = f(b.Open)
	} else {
		px, _ = lastBefore(l.bars, day, exit)
	}
	if resting {
		return px - hs, "TIME-SL-UNFILLED", exit
	}
	if p.Trail <= 0 || px <= tr.price {
		return px - hs, "TIME", exit
	}
	return trailTrade(l, tr, day, p, px)
}

// trailTrade runs the trailing stop from Exit (price px, in profit) to
// Cutoff. Each bar is checked against the stop set by earlier bars before its
// own high raises it, so a bar never stops out against a peak it made itself.
func trailTrade(l pick, tr trigger, day time.Time, p params, px float64) (float64, string, time.Time) {
	hs := p.HalfSpread
	peak := px
	stop := func() float64 { return math.Max(tr.price, peak-p.Trail) }
	cutoff := at(day, p.Cutoff)
	for t := at(day, p.Exit); t.Before(cutoff); t = t.Add(time.Minute) {
		b, ok := l.bars[t]
		if !ok {
			continue
		}
		o, h, lw := f(b.Open), f(b.High), f(b.Low)
		switch st := stop(); {
		case o <= st:
			return o - hs, "TRAIL", t
		case lw <= st:
			return st - hs, "TRAIL", t
		}
		peak = math.Max(peak, h)
	}
	end := 0.0
	if b, ok := l.bars[cutoff]; ok {
		end = f(b.Open)
	} else {
		end, _ = lastBefore(l.bars, day, cutoff)
	}
	return end - hs, "CUTOFF", cutoff
}

type result struct {
	Day         time.Time
	Expiry      time.Time
	DTE         int
	Symbol      string
	Side        string
	SelPrice    float64
	SelDelta    float64
	TrigPrice   float64
	EntryAt     time.Time
	Entry, Exit float64
	ExitAt      time.Time
	Reason      string
	Qty         int
	Points      float64
	GrossRs     float64
	CostRs      float64
	NetRs       float64
	Ambiguous   bool
}

// ---------------------------------------------------------------- main

func main() {
	cfgPath := flag.String("config", "config.local.toml", "config holding upstox_access_token")
	under := flag.String("underlying", "nifty", "nifty or sensex")
	window := flag.Float64("window", 0, "strike window around spot (default 300 nifty, 1000 sensex)")
	tDelta := flag.Float64("delta", 0, "pick strikes by this |delta| at 09:25 and scale levels by premium/-search (0: pick by premium)")
	cacheDir := flag.String("cache", "test/data/options", "contract candle cache (shared with optbt)")
	fromS := flag.String("from", "", "first session YYYY-MM-DD (default: first expired weekly)")
	toS := flag.String("to", "", "last session YYYY-MM-DD (default: last expired weekly)")
	lots := flag.Int("lots", 1, "lots per trade")
	spreadTicks := flag.Float64("spread-ticks", 2, "full bid-ask in ticks; half is paid on each market fill")
	target := flag.Float64("search", 170, "09:25 premium to look for")
	trig := flag.Float64("trigger", 180, "entry band low")
	band := flag.Float64("band", 10, "entry band width")
	slPts := flag.Float64("sl", 20, "stop, points below trigger")
	tpPts := flag.Float64("tp", 40, "target, points above trigger")
	outPath := flag.String("out", "nifty180_trades.csv", "per-trade CSV")
	split := flag.String("split", "", "optional YYYY-MM-DD; report before/after separately")
	cross := flag.Bool("cross", false, "only enter on an upward cross of -trigger (the fixed live script's rule)")
	trail := flag.Float64("trail", 0, "trail a stop this many points under the high for trades in profit at 09:45 (0: sell at 09:45)")
	cutoffS := flag.String("cutoff", "10:30", "with -trail, sell anything still open at this time (HH:MM)")
	flag.Parse()

	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load %s: %v", *cfgPath, err)
	}
	if cfg.UpstoxAccessToken == "" {
		log.Fatalf("%s has no upstox_access_token", *cfgPath)
	}
	key, ok := underlyings[strings.ToLower(*under)]
	if !ok {
		log.Fatalf("unknown -underlying %q; known: nifty, sensex", *under)
	}
	if *window == 0 {
		*window = 300
		if key != underlyings["nifty"] {
			*window = 1000
		}
	}
	s := &store{
		client: upstox.NewClient(cfg.UpstoxAccessToken, 60*time.Second),
		key:    key,
		dir:    *cacheDir,
		chains: map[string][]upstox.ExpiredContract{},
		bars:   map[string]map[time.Time]models.Candle{},
	}
	if err := os.MkdirAll(*cacheDir, 0o755); err != nil {
		log.Fatal(err)
	}

	p := params{
		SearchTarget: *target, Trigger: *trig, BandWidth: *band, SL: *slPts, TP: *tpPts,
		SLLimitBuf: 2, StrikeWindow: *window, TargetDelta: *tDelta, HalfSpread: *spreadTicks * 0.05 / 2, Lots: *lots,
		Setup: hm(9, 25), Start: hm(9, 29), Exit: hm(9, 45), CrossFromBelow: *cross,
		Trail: *trail, Cutoff: mustHM(*cutoffS),
	}

	expiries, err := loadExpiries(s.client, key)
	if err != nil {
		log.Fatalf("expiries: %v", err)
	}
	from := expiries[0].AddDate(0, 0, -7)
	to := expiries[len(expiries)-1]
	if *fromS != "" {
		from = mustDate(*fromS)
	}
	if *toS != "" {
		to = mustDate(*toS)
	}
	log.Printf("%d expired %s expiries %s..%s; sessions %s..%s", len(expiries), strings.ToUpper(*under),
		expiries[0].Format("2006-01-02"), expiries[len(expiries)-1].Format("2006-01-02"),
		from.Format("2006-01-02"), to.Format("2006-01-02"))

	idx, err := s.indexBars(from, to)
	if err != nil {
		log.Fatal(err)
	}
	daySet := map[time.Time]bool{}
	for t := range idx {
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, options.IST)
		if !d.Before(from) && !d.After(to) {
			daySet[d] = true
		}
	}
	days := make([]time.Time, 0, len(daySet))
	for d := range daySet {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	log.Printf("%d sessions with index data", len(days))

	sheet := costs.Options()
	var results []result
	skipped := map[string]int{}
	noTrade, ambiguous := 0, 0
	var curExp time.Time
	for i, day := range days {
		spot, ok := lastBefore(idx, day, at(day, p.Setup))
		if !ok {
			skipped["no index price at 09:25"]++
			continue
		}
		exp, ok := nearestExpiry(expiries, day)
		if !ok {
			skipped["no expired weekly covering day"]++
			continue
		}
		if !exp.Equal(curExp) {
			// Days run in order, so the previous expiry's candles are done
			// with; dropping them keeps memory flat over a long run.
			s.bars = map[string]map[time.Time]models.Candle{}
			curExp = exp
		}
		chain, err := s.chain(exp)
		if err != nil {
			skipped["chain fetch: "+truncErr(err)]++
			continue
		}
		s.failed = false
		ce, okC := selectStrike(s, chain, exp, day, spot, "CE", p)
		pe, okP := selectStrike(s, chain, exp, day, spot, "PE", p)
		if s.failed {
			skipped["candle download failed"]++
			continue
		}
		if !okC || !okP {
			skipped["no priced CE/PE at 09:25"]++
			continue
		}
		tc, hc := firstTrigger(ce, day, p.forLeg(ce))
		tpe, hp := firstTrigger(pe, day, p.forLeg(pe))
		var leg pick
		var tr trigger
		amb := false
		switch {
		case hc && hp:
			// Earlier minute wins; inside one minute, a fill at the open beats
			// a mid-bar crossing; a true tie goes to CE, as the script checks
			// CE first.
			amb = tc.at.Equal(tpe.at) && tc.atOpen == tpe.atOpen
			if tpe.at.Before(tc.at) || (tpe.at.Equal(tc.at) && tpe.atOpen && !tc.atOpen) {
				leg, tr = pe, tpe
			} else {
				leg, tr = ce, tc
			}
		case hc:
			leg, tr = ce, tc
		case hp:
			leg, tr = pe, tpe
		default:
			noTrade++
			continue
		}
		if amb {
			ambiguous++
		}
		exitPx, reason, exitAt := exitTrade(leg, tr, day, p.forLeg(leg))
		entry := tr.price + p.HalfSpread
		qty := p.Lots * leg.c.LotSize
		pts := exitPx - entry
		cost := sheet.RoundTrip(entry, exitPx, float64(qty))
		results = append(results, result{
			Day: day, Expiry: exp, DTE: int(exp.Sub(day).Hours() / 24), Symbol: leg.c.TradingSymbol,
			Side: leg.c.InstrumentType, SelPrice: leg.price, SelDelta: leg.delta, TrigPrice: tr.price, EntryAt: tr.at,
			Entry: entry, Exit: exitPx, ExitAt: exitAt, Reason: reason, Qty: qty, Points: pts,
			GrossRs: pts * float64(qty), CostRs: cost, NetRs: pts*float64(qty) - cost, Ambiguous: amb,
		})
		if (i+1)%50 == 0 {
			log.Printf("  ... %d/%d sessions, %d contracts downloaded", i+1, len(days), s.fetches)
		}
	}

	log.Printf("sessions: %d | traded %d | no trigger %d | same-minute CE/PE ties %d", len(days), len(results), noTrade, ambiguous)
	for why, n := range skipped {
		log.Printf("  skipped: %-40s %d", why, n)
	}
	if err := dump(*outPath, results); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", *outPath)

	sel := fmt.Sprintf("search %.0f", p.SearchTarget)
	if p.TargetDelta > 0 {
		sel = fmt.Sprintf("delta %.3f, levels scaled by premium/%.0f", p.TargetDelta, p.SearchTarget)
	}
	fmt.Printf("\n=== %s 180: %s, band %.0f-%.0f, SL %.0f / TP %.0f, %d lot(s), %.0f-tick spread ===\n",
		strings.ToUpper(*under), sel, p.Trigger, p.Trigger+p.BandWidth, p.SL, p.TP, p.Lots, *spreadTicks)
	if p.Trail > 0 {
		fmt.Printf("    trail %.0f pts under the high from 09:45, sell at %s\n", p.Trail, *cutoffS)
	}
	report("ALL", results)
	if *split != "" {
		sd := mustDate(*split)
		var a, b []result
		for _, r := range results {
			if r.Day.Before(sd) {
				a = append(a, r)
			} else {
				b = append(b, r)
			}
		}
		report("before "+*split, a)
		report("on/after "+*split, b)
	}
	byYear := map[int][]result{}
	for _, r := range results {
		byYear[r.Day.Year()] = append(byYear[r.Day.Year()], r)
	}
	years := make([]int, 0, len(byYear))
	for y := range byYear {
		years = append(years, y)
	}
	sort.Ints(years)
	for _, y := range years {
		report(fmt.Sprint(y), byYear[y])
	}
	var d0, d1, d2 []result
	for _, r := range results {
		switch {
		case r.DTE == 0:
			d0 = append(d0, r)
		case r.DTE == 1:
			d1 = append(d1, r)
		default:
			d2 = append(d2, r)
		}
	}
	report("expiry day", d0)
	report("1 day to expiry", d1)
	report("2+ days to expiry", d2)
}

// ---------------------------------------------------------------- reporting

func report(label string, rs []result) {
	if len(rs) == 0 {
		fmt.Printf("\n[%s] no trades\n", label)
		return
	}
	var pts, net []float64
	reasons := map[string]int{}
	wins := 0
	cum, peak, dd := 0.0, 0.0, 0.0
	gross, cost := 0.0, 0.0
	for _, r := range rs {
		pts = append(pts, r.Points)
		net = append(net, r.NetRs)
		reasons[r.Reason]++
		if r.NetRs > 0 {
			wins++
		}
		gross += r.GrossRs
		cost += r.CostRs
		cum += r.NetRs
		peak = math.Max(peak, cum)
		dd = math.Max(dd, peak-cum)
	}
	mp, tp := meanT(pts)
	mn, tn := meanT(net)
	var rk []string
	for k, v := range reasons {
		rk = append(rk, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(rk)
	fmt.Printf("\n[%s] trades %d | win %.1f%% | avg %.2f pts/trade (t %.2f)\n", label, len(rs), 100*float64(wins)/float64(len(rs)), mp, tp)
	fmt.Printf("  net Rs %.0f total (gross %.0f, costs %.0f) | avg Rs %.0f/trade (t %.2f) | max drawdown Rs %.0f\n", cum, gross, cost, mn, tn, dd)
	fmt.Printf("  exits: %s\n", strings.Join(rk, ", "))
}

func dump(path string, rs []result) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	w := csv.NewWriter(fh)
	defer w.Flush()
	_ = w.Write([]string{"day", "expiry", "dte", "symbol", "side", "price_0925", "delta_0925", "trigger", "entry_time", "entry", "exit_time", "exit", "reason", "qty", "points", "gross_rs", "cost_rs", "net_rs", "ce_pe_tie"})
	for _, r := range rs {
		_ = w.Write([]string{
			r.Day.Format("2006-01-02"), r.Expiry.Format("2006-01-02"), fmt.Sprint(r.DTE), r.Symbol, r.Side,
			ff(r.SelPrice), fmt.Sprintf("%.3f", r.SelDelta), ff(r.TrigPrice), r.EntryAt.Format("15:04"), ff(r.Entry), r.ExitAt.Format("15:04"), ff(r.Exit),
			r.Reason, fmt.Sprint(r.Qty), ff(r.Points), ff(r.GrossRs), ff(r.CostRs), ff(r.NetRs), fmt.Sprint(r.Ambiguous),
		})
	}
	return w.Error()
}

// ---------------------------------------------------------------- helpers

func nearestExpiry(expiries []time.Time, day time.Time) (time.Time, bool) {
	i := sort.Search(len(expiries), func(i int) bool { return !expiries[i].Before(day) })
	if i == len(expiries) || expiries[i].Sub(day) > 8*24*time.Hour {
		return time.Time{}, false
	}
	return expiries[i], true
}

func loadExpiries(c *upstox.Client, key string) ([]time.Time, error) {
	raw, err := retry(func() ([]string, error) { return c.ExpiredExpiries(key) })
	if err != nil {
		return nil, err
	}
	var out []time.Time
	for _, s := range raw {
		if t, err := time.ParseInLocation("2006-01-02", s, options.IST); err == nil {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no expiries returned")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

func retry[T any](fn func() (T, error)) (T, error) {
	var zero T
	var err error
	for attempt := range 8 {
		var v T
		if v, err = fn(); err == nil {
			return v, nil
		}
		wait := time.Duration(1<<min(attempt, 4)) * time.Second
		if strings.Contains(err.Error(), "HTTP 429") {
			// Rate limited: the window is minutes long, so short retries
			// only burn more of it.
			wait = 2 * time.Minute
			log.Printf("rate limited, waiting %s", wait)
		}
		time.Sleep(wait)
	}
	return zero, err
}

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

func cacheName(k string) string {
	return strings.NewReplacer("|", "_", ":", "_", "/", "_", " ", "_").Replace(k)
}

func readCache(path string) ([]models.Candle, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	rows, err := csv.NewReader(fh).ReadAll()
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
		out = append(out, models.Candle{StartTime: ts, Open: dec(row[1]), High: dec(row[2]), Low: dec(row[3]), Close: dec(row[4]), Volume: dec(row[5])})
	}
	return out, nil
}

func writeCache(path string, list []models.Candle) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	w := csv.NewWriter(fh)
	defer w.Flush()
	if err := w.Write([]string{"timestamp", "open", "high", "low", "close", "volume"}); err != nil {
		return err
	}
	for _, c := range list {
		if err := w.Write([]string{c.StartTime.Format(time.RFC3339), c.Open.StringFixed(2), c.High.StringFixed(2),
			c.Low.StringFixed(2), c.Close.StringFixed(2), c.Volume.StringFixed(0)}); err != nil {
			return err
		}
	}
	return w.Error()
}

func mustDate(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, options.IST)
	if err != nil {
		log.Fatalf("bad date %q", s)
	}
	return t
}

func mustHM(s string) time.Duration {
	t, err := time.Parse("15:04", s)
	if err != nil {
		log.Fatalf("bad time %q", s)
	}
	return hm(t.Hour(), t.Minute())
}

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func f(d decimal.Decimal) float64 { v, _ := d.Float64(); return v }

func ff(v float64) string { return fmt.Sprintf("%.2f", v) }

func meanT(v []float64) (float64, float64) {
	n := float64(len(v))
	if n < 2 {
		return 0, 0
	}
	m := 0.0
	for _, x := range v {
		m += x
	}
	m /= n
	ss := 0.0
	for _, x := range v {
		ss += (x - m) * (x - m)
	}
	sd := math.Sqrt(ss / (n - 1))
	if sd == 0 {
		return m, 0
	}
	return m, m / (sd / math.Sqrt(n))
}
