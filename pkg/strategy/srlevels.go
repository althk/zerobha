package strategy

import (
	"fmt"
	"log"
	"sort"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// SRLevels is an intraday support/resistance strategy on 5-minute bars,
// specified on the NIFTY / SENSEX index chart and intended for execution in
// weekly index options: a break of support buys a PE, a bounce off support
// buys a CE, and the resistance cases mirror both.
//
//	levels  : daily pivots (traditional or fibonacci) computed from the
//	          PREVIOUS session's high/low/close, recomputed each morning
//	zones   : swing highs and lows of the last HTFLookbackDays daily bars,
//	          clustered into bands -- the higher-timeframe structure the
//	          intraday level has to agree with before it is worth trading
//	entry   : a confirmation candle that closes clear of a level, in the
//	          direction of the interaction, inside the entry window
//	exit    : entry -/+ SLATRMult x ATR and entry +/- TPATRMult x ATR, both
//	          resting at the broker, plus the hard square-off
//
// Four interactions are recognised, which is the whole of the rule set:
//
//	break of support     -> close below the level    -> bearish -> PE (SELL)
//	bounce off support   -> wick into, close above   -> bullish -> CE (BUY)
//	break of resistance  -> close above the level    -> bullish -> CE (BUY)
//	rejection at resist. -> wick into, close below   -> bearish -> PE (SELL)
//
// Support and resistance are not fixed labels: a level is support while price
// is above it and resistance while price is below it. Classifying them once at
// the open would mislabel every level price crosses during the day, which on a
// trending session is most of them.
//
// Daily bars are built from the intraday stream rather than read from a second
// file. The backtester never calls Init (see CLAUDE.md), so anything a strategy
// needs has to be derivable from the candles it is replayed -- and a separate
// daily feed would also let a partially formed bar for today leak into a level
// used earlier the same day.
//
// Like Donchian, the signal instrument and the traded instrument differ. This
// file emits a plain directional signal on the index; cmd/optbt prices that
// trade list on the weekly option that was actually listed at the time. An
// index publishes no volume, which the strategy detects rather than being told
// (sawVolume), so there is no volume filter here at all.
type SRLevels struct {
	cfg config.SRLevelsConfig

	// contracts maps trading symbol to its lot size and exchange, injected the
	// same way Donchian's is. Empty for cash equities.
	contracts map[string]ContractSpec

	state map[string]*srState
}

// srState is the per-symbol state. The ATR runs continuously across sessions;
// the level set, the day counters and the position view reset on the date change.
type srState struct {
	atr *indicators.ATR

	lastDate     string
	entriesToday int

	// levels is today's tradeable level set, ascending, derived at the first
	// bar of the session from the previous session's bar.
	levels []srLevel

	// day is the session under construction, folded from the intraday bars.
	// It becomes the source of tomorrow's pivots and joins the daily history
	// the higher-timeframe zones are read from.
	day      models.Candle
	dayOpen  bool
	daily    []models.Candle
	htfZones []srZone

	// prevClose is the previous bar's close, which is what makes a break
	// distinguishable from a bounce: both close on the same side of the level,
	// and only the side price approached from tells them apart.
	prevClose decimal.Decimal
	prevValid bool

	// sawVolume records whether this instrument has ever reported a positive
	// volume. An index never does, and treating that as a halted instrument
	// would reject every bar and produce a backtest of exactly zero trades.
	sawVolume bool

	// usedLevel counts entries already taken against each level today, so one
	// level chopping around its own price cannot fill the day's entry budget
	// on its own.
	usedLevel map[string]int

	// openSide is the side of the position this strategy believes it holds.
	// It is a belief, not a fact: the engine drops signals for risk limits
	// without telling the strategy, and there is no hook by which a broker
	// reports that a resting stop has fired.
	//
	// That missing hook is why the protective levels are mirrored here.
	// Without them openValid is a one-way latch: set on the first entry of the
	// session and cleared only at the date change, so the strategy believed it
	// held a position for the rest of every day and took at most ONE trade per
	// session — which silently made max_entries_per_symbol and
	// max_entries_per_level dead knobs. See closeIfProtectiveHit.
	openSide   models.SignalType
	openValid  bool
	openStop   decimal.Decimal
	openTarget decimal.Decimal
	openTrail  decimal.Decimal
	openBest   decimal.Decimal
}

// closeIfProtectiveHit reproduces the broker's own exit test on this bar and
// drops the strategy's belief in the position when one of its resting orders
// would have filled.
//
// It mirrors pkg/broker/sim.go deliberately, including the ordering: exits are
// evaluated against the stop as it stood at the bar's OPEN, and only then does
// the trail ratchet on this bar's extreme. Crediting a stop improvement earned
// by this bar's high and then letting the same bar's low exit at the improved
// stop would be lookahead, since the sequence of high and low within a bar is
// unknown.
//
// Being wrong here is cheap and one-directional. If this clears the flag while
// the broker still holds the position, the engine's own HasOpenPosition check
// rejects the next signal, so the cost is a dropped entry rather than a
// pyramided one — the engine is authoritative and this is only an optimisation.
func (st *srState) closeIfProtectiveHit(candle models.Candle) {
	if !st.openValid {
		return
	}

	stop := st.openStop
	if st.openTrail.IsPositive() && !st.openBest.IsZero() {
		if st.openSide == models.BuySignal {
			if trailed := st.openBest.Sub(st.openTrail); trailed.GreaterThan(stop) {
				stop = trailed
			}
		} else if trailed := st.openBest.Add(st.openTrail); trailed.LessThan(stop) {
			stop = trailed
		}
	}

	var hit bool
	if st.openSide == models.BuySignal {
		hit = (stop.IsPositive() && candle.Low.LessThanOrEqual(stop)) ||
			(st.openTarget.IsPositive() && candle.High.GreaterThanOrEqual(st.openTarget))
	} else {
		hit = (stop.IsPositive() && candle.High.GreaterThanOrEqual(stop)) ||
			(st.openTarget.IsPositive() && candle.Low.LessThanOrEqual(st.openTarget))
	}
	if hit {
		st.openValid = false
		st.openStop, st.openTarget, st.openTrail, st.openBest = decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
		return
	}

	// Survived the bar: now let the trail follow this bar's extreme.
	if st.openTrail.IsPositive() {
		if st.openSide == models.BuySignal {
			if candle.High.GreaterThan(st.openBest) {
				st.openBest = candle.High
			}
		} else if st.openBest.IsZero() || candle.Low.LessThan(st.openBest) {
			st.openBest = candle.Low
		}
	}
}

// srLevel is one price the strategy watches, carrying its name for the trade
// metadata and whether higher-timeframe structure backs it.
type srLevel struct {
	price     decimal.Decimal
	name      string
	htfBacked bool
}

// srZone is a band of prices where the daily chart has turned. Low and High are
// the extremes of the swings clustered into it; Touches is how many swings that
// was, which is the only measure of a zone's strength here.
type srZone struct {
	Low     decimal.Decimal
	High    decimal.Decimal
	Touches int
}

func (z srZone) mid() decimal.Decimal {
	return z.Low.Add(z.High).Div(decimal.NewFromInt(2))
}

// NewSRLevelsStrategy builds the strategy over the given symbols.
func NewSRLevelsStrategy(symbols []string, cfg config.SRLevelsConfig) *SRLevels {
	s := &SRLevels{
		cfg:       cfg,
		contracts: make(map[string]ContractSpec),
		state:     make(map[string]*srState, len(symbols)),
	}
	for _, sym := range symbols {
		s.state[sym] = s.newState()
	}
	return s
}

// SetContracts injects the lot size and exchange per trading symbol, so signals
// are sized in whole lots and routed to the right segment.
func (s *SRLevels) SetContracts(specs map[string]ContractSpec) {
	for sym, spec := range specs {
		s.contracts[sym] = spec
	}
}

func (s *SRLevels) newState() *srState {
	return &srState{
		atr:       indicators.NewATR(s.cfg.ATRPeriod),
		usedLevel: make(map[string]int),
	}
}

// Name returns the identifier used in logs and reports.
func (s *SRLevels) Name() string { return "SRLevels" }

// Init warms the ATR and the daily history from historical bars so a live
// session can trade from its first candle rather than spending the first days
// accumulating the higher-timeframe zones.
//
// Failures are logged but not fatal: a symbol whose history cannot be fetched
// warms up from the live stream instead, which costs it the early sessions, not
// correctness. The backtester never calls this -- there the warm-up comes from
// the replayed candles.
func (s *SRLevels) Init(provider core.DataProvider) error {
	if provider == nil {
		return nil
	}
	days := s.cfg.HTFLookbackDays + 5
	for symbol, st := range s.state {
		candles, err := provider.History(symbol, s.cfg.Timeframe, days)
		if err != nil {
			log.Printf("[%s] SRLevels: warm-up failed, starting cold: %v", symbol, err)
			continue
		}
		for _, c := range candles {
			if c.High.Equal(c.Low) {
				continue
			}
			st.atr.Update(c)
			s.foldDaily(st, c)
		}
		log.Printf("[%s] SRLevels: warmed up on %d bars, %d daily sessions", symbol, len(candles), len(st.daily))
	}
	return nil
}

func (s *SRLevels) stateFor(symbol string) *srState {
	st, ok := s.state[symbol]
	if !ok {
		st = s.newState()
		s.state[symbol] = st
	}
	return st
}

// foldDaily accumulates one intraday bar into the session under construction,
// closing the previous session and appending it to the daily history when the
// date changes. Only completed sessions ever reach st.daily, so a half-formed
// bar for today can never contribute a level used today.
func (s *SRLevels) foldDaily(st *srState, c models.Candle) {
	date := c.StartTime.In(istLocation).Format("2006-01-02")
	if st.dayOpen && st.day.StartTime.In(istLocation).Format("2006-01-02") == date {
		if c.High.GreaterThan(st.day.High) {
			st.day.High = c.High
		}
		if c.Low.LessThan(st.day.Low) {
			st.day.Low = c.Low
		}
		st.day.Close = c.Close
		return
	}

	if st.dayOpen {
		st.daily = append(st.daily, st.day)
		keep := s.cfg.HTFLookbackDays + 5
		if len(st.daily) > keep {
			st.daily = st.daily[len(st.daily)-keep:]
		}
	}
	st.day = models.Candle{
		Symbol:    c.Symbol,
		Open:      c.Open,
		High:      c.High,
		Low:       c.Low,
		Close:     c.Close,
		StartTime: c.StartTime,
	}
	st.dayOpen = true
}

// OnCandle consumes one bar and returns an entry signal or nil.
func (s *SRLevels) OnCandle(candle models.Candle) *models.Signal {
	st := s.stateFor(candle.Symbol)

	if candle.Volume.IsPositive() {
		st.sawVolume = true
	}
	// Zero-volume candles are real on Kite (halted or illiquid names) and carry
	// a degenerate range that would poison the ATR and the daily bar alike.
	// Only drop them for instruments that report volume at all, or an index
	// would lose every bar.
	if (st.sawVolume && candle.Volume.IsZero()) || candle.High.Equal(candle.Low) {
		return nil
	}

	istTime := candle.StartTime.In(istLocation)
	date := istTime.Format("2006-01-02")
	newSession := date != st.lastDate

	// The daily bar must close BEFORE the new session's levels are built from
	// it. The ATR includes the current bar -- it is a volatility scale for the
	// stop, not a level to be crossed.
	s.foldDaily(st, candle)
	atr := st.atr.Update(candle)

	if newSession {
		st.lastDate = date
		st.entriesToday = 0
		st.usedLevel = make(map[string]int)
		// Everything here is MIS and the square-off flattens it; believing a
		// position survived would suppress the next day's first entry.
		st.openValid = false
		st.prevValid = false
		s.rebuildLevels(st)
	}

	// Retire the position view before the entry gates read it. The backtester
	// runs SimBroker.CheckExits on this same candle before the engine reaches
	// OnCandle, so a bar that stops the position out has already flattened the
	// broker by the time we get here — the same bar may legitimately open the
	// next trade.
	st.closeIfProtectiveHit(candle)

	prevClose, prevValid := st.prevClose, st.prevValid
	st.prevClose, st.prevValid = candle.Close, true

	if !prevValid || !atr.IsPositive() || len(st.levels) == 0 {
		return nil
	}

	h, m, _ := istTime.Clock()
	minuteOfDay := h*60 + m
	if minuteOfDay < s.cfg.EntryStartMin || minuteOfDay > s.cfg.EntryCutoffMin {
		return nil
	}
	if st.entriesToday >= s.cfg.MaxEntriesPerSymbol {
		return nil
	}
	// No re-entries or pyramiding while a position is open.
	if st.openValid {
		return nil
	}

	// Volatility floor: in a session whose ATR is a rounding error on price,
	// the stop distance cannot cover the round-trip cost.
	if s.cfg.MinATRPct > 0 && candle.Close.IsPositive() {
		atrPct := atr.Div(candle.Close).Mul(decimal.NewFromInt(100))
		if atrPct.LessThan(decimal.NewFromFloat(s.cfg.MinATRPct)) {
			return nil
		}
	}

	level, side, kind := s.detect(st, candle, prevClose, atr)
	if level == nil {
		return nil
	}

	signal := s.buildSignal(candle, side, atr, *level, kind)
	if signal == nil {
		return nil
	}

	st.entriesToday++
	st.usedLevel[level.name]++
	st.openSide = side
	st.openValid = true
	st.openStop, st.openTarget, st.openTrail = signal.StopLoss, signal.Target, signal.TrailDistance
	st.openBest = candle.Close

	log.Printf("[%s] SRLevels: %s %s at %s (%s) close=%s ATR=%s -> SL %s TP %s",
		candle.Symbol, signal.Type, kind, level.price.StringFixed(2), level.name,
		candle.Close.StringFixed(2), atr.StringFixed(2),
		signal.StopLoss.StringFixed(2), signal.Target.StringFixed(2))

	return signal
}

// detect walks today's levels nearest-first and returns the first confirmed
// interaction. Nearest-first matters: on a bar that clears two levels at once
// the closer one is the one price actually reacted to, and the further one was
// already stale by the time the bar closed.
func (s *SRLevels) detect(st *srState, candle models.Candle, prevClose, atr decimal.Decimal) (*srLevel, models.SignalType, string) {
	buffer := atr.Mul(decimal.NewFromFloat(s.cfg.BreakBufferATR))
	touch := atr.Mul(decimal.NewFromFloat(s.cfg.TouchATR))
	proximity := atr.Mul(decimal.NewFromFloat(s.cfg.ProximityATR))

	bullBar := candle.Close.GreaterThan(candle.Open)
	bearBar := candle.Close.LessThan(candle.Open)

	type cand struct {
		lvl  srLevel
		dist decimal.Decimal
	}
	var ordered []cand
	for _, l := range st.levels {
		d := candle.Close.Sub(l.price).Abs()
		if d.GreaterThan(proximity) {
			continue
		}
		ordered = append(ordered, cand{lvl: l, dist: d})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].dist.LessThan(ordered[j].dist) })

	for i := range ordered {
		l := ordered[i].lvl
		if cap := s.cfg.EntriesPerLevel(); cap > 0 && st.usedLevel[l.name] >= cap {
			continue
		}

		wasAbove := prevClose.GreaterThanOrEqual(l.price)
		closedAbove := candle.Close.GreaterThan(l.price.Add(buffer))
		closedBelow := candle.Close.LessThan(l.price.Sub(buffer))

		var side models.SignalType
		var kind string
		switch {
		// Support gives way: price was above the level and this bar closed
		// decisively below it, on a bar that is itself bearish.
		case wasAbove && closedBelow && bearBar:
			side, kind = models.SellSignal, "support-break"
		// Support holds: this bar traded into the level and closed back above
		// it, on a bar that is itself bullish.
		case wasAbove && closedAbove && bullBar && candle.Low.LessThanOrEqual(l.price.Add(touch)):
			side, kind = models.BuySignal, "support-bounce"
		// Resistance gives way.
		case !wasAbove && closedAbove && bullBar:
			side, kind = models.BuySignal, "resistance-break"
		// Resistance holds.
		case !wasAbove && closedBelow && bearBar && candle.High.GreaterThanOrEqual(l.price.Sub(touch)):
			side, kind = models.SellSignal, "resistance-reject"
		default:
			continue
		}

		if side == models.SellSignal && (s.cfg.AllowShort == nil || !*s.cfg.AllowShort) {
			continue
		}
		if !s.htfAgrees(st, l, side, candle, atr) {
			continue
		}
		return &l, side, kind
	}
	return nil, models.BuySignal, ""
}

// htfAgrees is the "the thesis does not break" gate, and it asks two questions,
// both of the DAILY structure rather than of today's bars.
//
//  1. Is the level one the higher timeframe recognises? A pivot number nobody
//     is watching is arithmetic on yesterday's bar; a pivot that lands on a
//     band the daily chart has already turned at is a level.
//  2. Is there room to the target? A break with an opposing daily zone sitting
//     a fraction of an ATR beyond it has nowhere to go -- the trade is right
//     about direction and still stops out, because the structure that stopped
//     price before is between the entry and the target.
func (s *SRLevels) htfAgrees(st *srState, l srLevel, side models.SignalType, candle models.Candle, atr decimal.Decimal) bool {
	if s.cfg.UseHTFConfirm != nil && *s.cfg.UseHTFConfirm && !l.htfBacked {
		return false
	}

	room := s.cfg.RoomATR()
	if room <= 0 || len(st.htfZones) == 0 {
		return true
	}
	need := atr.Mul(decimal.NewFromFloat(room))

	// The blocking zone is the nearest one AHEAD of the trade, measured from
	// the entry price. A zone behind it is what the trade just came through.
	for _, z := range st.htfZones {
		if side == models.BuySignal {
			if z.Low.GreaterThan(candle.Close) && z.Low.Sub(candle.Close).LessThan(need) {
				return false
			}
		} else {
			if z.High.LessThan(candle.Close) && candle.Close.Sub(z.High).LessThan(need) {
				return false
			}
		}
	}
	return true
}

// rebuildLevels derives the new session's level set from the previous session's
// bar and the daily swing structure behind it.
func (s *SRLevels) rebuildLevels(st *srState) {
	st.levels = nil
	if len(st.daily) == 0 {
		return
	}
	prev := st.daily[len(st.daily)-1]

	pivots := indicators.ComputePivots(indicators.PivotMethod(s.cfg.PivotMethod), prev.High, prev.Low, prev.Close)
	if !pivots.Valid {
		return
	}

	st.htfZones = s.buildZones(st)

	prices, names := pivots.All(), pivots.Names()
	for i, p := range prices {
		if !p.IsPositive() {
			continue
		}
		st.levels = append(st.levels, srLevel{
			price:     p,
			name:      names[i],
			htfBacked: s.zoneBacking(st, p),
		})
	}
	sort.Slice(st.levels, func(i, j int) bool { return st.levels[i].price.LessThan(st.levels[j].price) })
}

// zoneBacking reports whether a price falls inside -- or within half a zone
// width of -- any higher-timeframe zone.
func (s *SRLevels) zoneBacking(st *srState, price decimal.Decimal) bool {
	if len(st.htfZones) == 0 {
		return false
	}
	tol := price.Mul(decimal.NewFromFloat(s.cfg.ZoneWidthPct / 100)).Div(decimal.NewFromInt(2))
	for _, z := range st.htfZones {
		if price.GreaterThanOrEqual(z.Low.Sub(tol)) && price.LessThanOrEqual(z.High.Add(tol)) {
			return true
		}
	}
	return false
}

// buildZones finds swing highs and lows on the completed daily bars and merges
// the ones that sit within ZoneWidthPct of each other into bands.
//
// A swing is a bar whose high (or low) is the extreme of a window SwingStrength
// bars either side of it. That window is why the last SwingStrength sessions
// can never produce a swing: confirming one needs bars that have not happened
// yet, and accepting an unconfirmed swing would read the future.
func (s *SRLevels) buildZones(st *srState) []srZone {
	bars := st.daily
	if len(bars) > s.cfg.HTFLookbackDays {
		bars = bars[len(bars)-s.cfg.HTFLookbackDays:]
	}
	k := s.cfg.SwingStrength
	if k < 1 || len(bars) < 2*k+1 {
		return nil
	}

	var swings []decimal.Decimal
	for i := k; i < len(bars)-k; i++ {
		isHigh, isLow := true, true
		for j := i - k; j <= i+k; j++ {
			if j == i {
				continue
			}
			if bars[j].High.GreaterThanOrEqual(bars[i].High) {
				isHigh = false
			}
			if bars[j].Low.LessThanOrEqual(bars[i].Low) {
				isLow = false
			}
		}
		if isHigh {
			swings = append(swings, bars[i].High)
		}
		if isLow {
			swings = append(swings, bars[i].Low)
		}
	}
	if len(swings) == 0 {
		return nil
	}
	sort.Slice(swings, func(i, j int) bool { return swings[i].LessThan(swings[j]) })

	var zones []srZone
	cur := srZone{Low: swings[0], High: swings[0], Touches: 1}
	for _, p := range swings[1:] {
		width := cur.mid().Mul(decimal.NewFromFloat(s.cfg.ZoneWidthPct / 100))
		if p.Sub(cur.Low).LessThanOrEqual(width) {
			cur.High = p
			cur.Touches++
			continue
		}
		zones = append(zones, cur)
		cur = srZone{Low: p, High: p, Touches: 1}
	}
	zones = append(zones, cur)

	if s.cfg.MinZoneTouches > 1 {
		var strong []srZone
		for _, z := range zones {
			if z.Touches >= s.cfg.MinZoneTouches {
				strong = append(strong, z)
			}
		}
		return strong
	}
	return zones
}

// buildSignal turns a confirmed interaction into the order parameters: a stop
// and a target at fixed ATR multiples of entry, both resting at the broker.
//
// There is no breakeven move. The reward:risk here is the specification rather
// than something discovered, and CLAUDE.md records four independent
// measurements of what a breakeven move costs a system whose stop stays put --
// mixing the two in would measure neither.
func (s *SRLevels) buildSignal(candle models.Candle, side models.SignalType, atr decimal.Decimal, l srLevel, kind string) *models.Signal {
	entry := candle.Close
	risk := atr.Mul(decimal.NewFromFloat(s.cfg.SLATRMult))
	if !risk.IsPositive() {
		return nil
	}
	reward := atr.Mul(decimal.NewFromFloat(s.cfg.TPMult()))

	var stop, target decimal.Decimal
	if side == models.BuySignal {
		stop = entry.Sub(risk)
		if reward.IsPositive() {
			target = entry.Add(reward)
		}
	} else {
		stop = entry.Add(risk)
		if reward.IsPositive() {
			target = entry.Sub(reward)
		}
	}

	// A stop at or below zero would make the sizer's risk-per-unit meaningless,
	// and a short's target has to stay above zero to be a price at all.
	if !stop.IsPositive() {
		return nil
	}
	if side == models.SellSignal && reward.IsPositive() && !target.IsPositive() {
		return nil
	}

	var trail decimal.Decimal
	if mult := s.cfg.SRTrailMult(); mult > 0 {
		trail = atr.Mul(decimal.NewFromFloat(mult))
	}

	return &models.Signal{
		Symbol:        candle.Symbol,
		Type:          side,
		Price:         entry,
		StopLoss:      stop,
		Target:        target,
		TrailDistance: trail,
		RiskPct:       decimal.NewFromFloat(s.cfg.RiskPct / 100),
		LotSize:       s.contracts[candle.Symbol].LotSize,
		Exchange:      s.contracts[candle.Symbol].Exchange,
		ProductType:   "MIS",
		Metadata: map[string]string{
			"Strategy":  s.Name(),
			"ATR":       atr.StringFixed(2),
			"Level":     l.name,
			"LevelPx":   l.price.StringFixed(2),
			"Kind":      kind,
			"HTFBacked": fmt.Sprintf("%t", l.htfBacked),
			"Reason": fmt.Sprintf("%s %s at %s (%s pivot), close %s, SL %.2f ATR / TP %.2f ATR",
				s.Name(), kind, l.price.StringFixed(2), s.cfg.PivotMethod,
				entry.StringFixed(2), s.cfg.SLATRMult, s.cfg.TPMult()),
		},
	}
}
