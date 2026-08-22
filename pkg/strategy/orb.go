package strategy

import (
	"log"
	"time"
	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/db"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// istLocation is the IST timezone, loaded once and reused to avoid repeated
// tz-database lookups on the per-candle hot path.
var istLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return time.FixedZone("IST", 5*3600+1800)
	}
	return loc
}()

const (
	// Opening-range window in minutes since midnight IST (9:15–9:30).
	// Promote to ORBConfig if you want to test a 30-minute OR.
	rangeStartMin = 9*60 + 15
	rangeEndMin   = 9*60 + 30

	// relVolLookbackDays bounds the AvgMorningVol baseline to a rolling
	// (Wilder-smoothed) window so it adapts to volume-regime changes instead
	// of averaging over all history.
	relVolLookbackDays = 20

	// minWarmupBars is the minimum number of 5-minute candles the indicators
	// must have seen (via Init replay or live/backtest feed) before any
	// signal is allowed. 30 bars ≈ 2× the longest lookback (ATR-14).
	minWarmupBars = 30
)

type ORBState struct {
	Symbol        string
	Vwap          *indicators.VWAP
	VolSma        *indicators.SMA // 9 period volume SMA
	Atr           *indicators.ATR // 14 period ATR
	Adx           *indicators.ADX // 9 period ADX
	Rsi           *indicators.RSI // 9 period RSI
	RangeHigh     decimal.Decimal
	RangeLow      decimal.Decimal
	RangeSet      bool
	LastDate      string
	LastClose     decimal.Decimal // Close price of the previous candle
	AvgMorningVol decimal.Decimal // Rolling avg of opening-range (9:15–9:30) volume
	RangeVolume   decimal.Decimal // Accumulated volume during today's opening range
	RelVolSkip    bool            // True if today is skipped (RelVol fail, gap fail, or no OR data)
	VolSma5       *indicators.SMA // 5 period volume SMA for recent surge
	PrevDayClose  decimal.Decimal // Closing price of the previous trading day
	DayGapPct     decimal.Decimal // Signed gap % of today's open vs PrevDayClose
	Traded        bool            // True once a signal has fired for this symbol today (one-trade-per-day guard)
	Bars          int64           // Total candles seen by the indicators (warmup guard; not persisted)

	// Rolling baseline of opening-range volume, updated as each day's range
	// locks. Wilder-smoothed over relVolLookbackDays so it self-seeds when
	// Init() was not called (e.g. backtests) and adapts over time in live
	// trading.
	MorningVolSum  decimal.Decimal // Smoothed sum of past days' opening-range volumes
	MorningVolDays int64           // Effective window size (capped at relVolLookbackDays)
}

func NewORBState(symbol string) *ORBState {
	return &ORBState{
		Symbol:       symbol,
		Vwap:         indicators.NewVWAP(),
		VolSma:       indicators.NewSMA(9),
		Atr:          indicators.NewATR(14),
		Adx:          indicators.NewADX(9),
		Rsi:          indicators.NewRSI(9),
		VolSma5:      indicators.NewSMA(5),
		RangeHigh:    decimal.Zero,
		RangeLow:     decimal.Zero,
		RangeSet:     false,
		LastClose:    decimal.Zero,
		PrevDayClose: decimal.Zero,
	}
}

type ORBStrategy struct {
	symbols []string
	states  map[string]*ORBState
	db      *db.Store
	cfg     config.ORBConfig
}

func NewORBStrategy(symbols []string, cfg config.ORBConfig) *ORBStrategy {
	return &ORBStrategy{
		symbols: symbols,
		states:  make(map[string]*ORBState),
		cfg:     cfg,
	}
}

func (s *ORBStrategy) Name() string {
	return "ORB"
}

func (s *ORBStrategy) SetDB(store *db.Store) {
	s.db = store
}

func (s *ORBStrategy) SaveState(symbol string) {
	if s.db == nil {
		return
	}
	state, ok := s.states[symbol]
	if !ok {
		return
	}

	// Create a persistent version of state (subset)
	pState := map[string]interface{}{
		"RangeHigh":      state.RangeHigh,
		"RangeLow":       state.RangeLow,
		"RangeSet":       state.RangeSet,
		"LastDate":       state.LastDate,
		"LastClose":      state.LastClose,
		"RelVolSkip":     state.RelVolSkip,
		"RangeVolume":    state.RangeVolume,
		"Traded":         state.Traded,
		"PrevDayClose":   state.PrevDayClose,
		"DayGapPct":      state.DayGapPct,
		"MorningVolSum":  state.MorningVolSum,
		"MorningVolDays": state.MorningVolDays,
	}

	if err := s.db.SetState("ORB_"+symbol, pState); err != nil {
		log.Printf("ERROR: Failed to save ORB state for %s: %v", symbol, err)
	}
}

func (s *ORBStrategy) LoadState(symbol string) {
	if s.db == nil {
		return
	}
	var pState struct {
		RangeHigh      decimal.Decimal
		RangeLow       decimal.Decimal
		RangeSet       bool
		LastDate       string
		LastClose      decimal.Decimal
		RelVolSkip     bool
		RangeVolume    decimal.Decimal
		Traded         bool
		PrevDayClose   decimal.Decimal
		DayGapPct      decimal.Decimal
		MorningVolSum  decimal.Decimal
		MorningVolDays int64
	}

	if err := s.db.GetState("ORB_"+symbol, &pState); err != nil {
		// Just log, maybe it's not there yet
		// log.Printf("INFO: No state found for %s", symbol)
		return
	}

	state, ok := s.states[symbol]
	if !ok {
		state = NewORBState(symbol)
		s.states[symbol] = state
	}

	// Stale dates are fine: OnCandle's LastDate != currentDate check resets
	// per-day fields on the first candle of a new day.

	state.RangeHigh = pState.RangeHigh
	state.RangeLow = pState.RangeLow
	state.RangeSet = pState.RangeSet
	state.LastDate = pState.LastDate
	state.LastClose = pState.LastClose
	state.RelVolSkip = pState.RelVolSkip
	state.RangeVolume = pState.RangeVolume
	state.Traded = pState.Traded
	state.PrevDayClose = pState.PrevDayClose
	state.DayGapPct = pState.DayGapPct
	state.MorningVolSum = pState.MorningVolSum
	state.MorningVolDays = pState.MorningVolDays
	if pState.MorningVolDays > 0 {
		state.AvgMorningVol = pState.MorningVolSum.Div(decimal.NewFromInt(pState.MorningVolDays))
	}
	log.Printf("LOADED STATE for %s: High=%s Low=%s Set=%v Traded=%v", symbol, state.RangeHigh, state.RangeLow, state.RangeSet, state.Traded)
}

func (s *ORBStrategy) Init(provider core.DataProvider) error {
	log.Println("Initializing ORB Strategy...")

	loc := istLocation

	for _, sym := range s.symbols {
		state := NewORBState(sym)
		s.states[sym] = state
		s.LoadState(sym)

		// Fetch 10 days of history: enough to warm up indicators and compute AvgMorningVol
		candles, err := provider.History(sym, "5minute", 10)
		if err != nil {
			log.Printf("WARNING: Failed to fetch history for %s: %v", sym, err)
			continue
		}

		now := time.Now().In(loc)
		today := now.Format("2006-01-02")

		// Accumulate opening-range volume per historical day to compute AvgMorningVol
		morningVolByDay := make(map[string]decimal.Decimal)
		var lastDate string
		var lastClose decimal.Decimal
		var todayRangeHigh, todayRangeLow, todayRangeVol decimal.Decimal
		var todayFirstOpen decimal.Decimal
		sawToday := false
		for _, c := range candles {
			istTime := c.StartTime.In(loc)

			// Skip the still-forming bucket: the live feed delivers this candle
			// when it closes, and replaying it here would double-count it in the
			// indicators and the opening range.
			if istTime.Add(5 * time.Minute).After(now) {
				continue
			}

			dateKey := istTime.Format("2006-01-02")

			// New day in the replay: capture the previous day's close and
			// re-anchor VWAP — VWAP is a session indicator and must not
			// accumulate across days.
			if lastDate != "" && dateKey != lastDate {
				state.PrevDayClose = lastClose
				state.Vwap = indicators.NewVWAP()
			}
			lastDate = dateKey
			lastClose = c.Close

			state.Vwap.Update(c)
			state.VolSma.Update(c.Volume)
			state.VolSma5.Update(c.Volume)
			state.Atr.Update(c)
			state.Adx.Update(c)
			state.Rsi.Update(c.Close)
			state.Bars++

			if dateKey == today && !sawToday {
				sawToday = true
				todayFirstOpen = c.Open
			}

			h, m, _ := istTime.Clock()
			tMin := h*60 + m
			if tMin >= rangeStartMin && tMin < rangeEndMin {
				if dateKey == today {
					// Today's range is rebuilt into live state below; OnCandle
					// folds its volume into the baseline when the range locks,
					// so keep it out of morningVolByDay to avoid double counting.
					todayRangeVol = todayRangeVol.Add(c.Volume)
					if todayRangeHigh.IsZero() {
						todayRangeHigh = c.High
						todayRangeLow = c.Low
					}
					if c.High.GreaterThan(todayRangeHigh) {
						todayRangeHigh = c.High
					}
					if c.Low.LessThan(todayRangeLow) {
						todayRangeLow = c.Low
					}
				} else {
					morningVolByDay[dateKey] = morningVolByDay[dateKey].Add(c.Volume)
				}
			}
		}

		// Fallback for PrevDayClose if only one day of history (though we requested 10)
		if state.PrevDayClose.IsZero() && lastClose.GreaterThan(decimal.Zero) {
			state.PrevDayClose = lastClose
		}

		// Seed the rolling baseline from history only when nothing was
		// restored from the DB — otherwise we'd discard the longer persisted
		// baseline on every restart.
		if state.MorningVolDays == 0 && len(morningVolByDay) > 0 {
			total := decimal.Zero
			for _, v := range morningVolByDay {
				total = total.Add(v)
			}
			state.MorningVolSum = total
			state.MorningVolDays = int64(len(morningVolByDay))
			state.AvgMorningVol = total.Div(decimal.NewFromInt(state.MorningVolDays))
			log.Printf("[%s] AvgMorningVol (9:15-9:30) over %d days: %s", sym, len(morningVolByDay), state.AvgMorningVol.StringFixed(0))
		}

		// Rebuild today's live state from the replay so a mid-day start can
		// still trade: without this, the first live candle after 9:30 finds an
		// empty range and skips the whole day. Skipped when LoadState already
		// restored today (mid-session restart) so RelVolSkip/Traded survive.
		if sawToday && state.LastDate != today {
			state.LastDate = today
			state.RangeSet = false
			state.RelVolSkip = false
			state.Traded = false
			state.RangeHigh = todayRangeHigh
			state.RangeLow = todayRangeLow
			state.RangeVolume = todayRangeVol
			state.DayGapPct = decimal.Zero

			// Same gap filter OnCandle applies on the day's first candle; that
			// branch won't run again now that LastDate is set to today.
			if !state.PrevDayClose.IsZero() {
				state.DayGapPct = todayFirstOpen.Sub(state.PrevDayClose).Div(state.PrevDayClose).Mul(decimal.NewFromInt(100))
				if s.cfg.MaxGapPct > 0 && state.DayGapPct.Abs().GreaterThan(decimal.NewFromFloat(s.cfg.MaxGapPct)) {
					log.Printf("[%s] Gap filter: Gap=%.2f%% > %.2f%% — skipping today", sym, state.DayGapPct.InexactFloat64(), s.cfg.MaxGapPct)
					state.RelVolSkip = true
				}
			}

			if !todayRangeHigh.IsZero() {
				log.Printf("[%s] Rebuilt today's opening range from history: High=%s Low=%s Vol=%s",
					sym, todayRangeHigh, todayRangeLow, todayRangeVol.StringFixed(0))
			}
			s.SaveState(sym)
		}
	}
	return nil
}

func (s *ORBStrategy) OnCandle(candle models.Candle) *models.Signal {
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewORBState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	// Always update LastClose when this candle is done, regardless of early returns or signals.
	defer func() { state.LastClose = candle.Close }()

	istTime := candle.StartTime.In(istLocation)
	h, m, _ := istTime.Clock()
	timeInMinutes := h*60 + m

	// 1. Date Check for Reset — IST date, consistent with all other time logic.
	currentDate := istTime.Format("2006-01-02")
	if state.LastDate != currentDate {
		// Yesterday's final close becomes PrevDayClose for today's gap check.
		// Guarded so the very first candle ever doesn't zero it out (Init may
		// have seeded it from history).
		if !state.LastClose.IsZero() {
			state.PrevDayClose = state.LastClose
		}

		state.RangeHigh = decimal.Zero
		state.RangeLow = decimal.Zero
		state.RangeSet = false
		state.LastDate = currentDate
		state.RangeVolume = decimal.Zero
		state.RelVolSkip = false
		state.Traded = false
		state.DayGapPct = decimal.Zero

		// VWAP is session-anchored: re-create it every day so day-1 volume
		// never pollutes day-2 VWAP.
		state.Vwap = indicators.NewVWAP()

		// Gap filter — evaluated on the day's true opening print (this is the
		// first candle of the day), not 15 minutes later. Gap is kept signed
		// so entries can check gap/trade-direction alignment.
		if !state.PrevDayClose.IsZero() {
			state.DayGapPct = candle.Open.Sub(state.PrevDayClose).Div(state.PrevDayClose).Mul(decimal.NewFromInt(100))
			if s.cfg.MaxGapPct > 0 && state.DayGapPct.Abs().GreaterThan(decimal.NewFromFloat(s.cfg.MaxGapPct)) {
				log.Printf("[%s] Gap filter: Gap=%.2f%% > %.2f%% — skipping today", state.Symbol, state.DayGapPct.InexactFloat64(), s.cfg.MaxGapPct)
				state.RelVolSkip = true
			}
		}

		s.SaveState(candle.Symbol)
	}

	// 2. Update Indicators — always, even on skipped/traded days, so ATR/ADX/
	// RSI/volume SMAs stay continuous into the next session instead of
	// freezing mid-day.
	currentVwap := state.Vwap.Update(candle)
	prevAvgVol := state.VolSma.Value()
	state.VolSma.Update(candle.Volume)
	prevRecentVol := state.VolSma5.Value()
	state.VolSma5.Update(candle.Volume)
	atrVal := state.Atr.Update(candle)
	prevAdx := state.Adx.Value()
	adxVal := state.Adx.Update(candle)
	rsiVal := state.Rsi.Update(candle.Close)
	state.Bars++

	// 3. Skip for the day if a daily filter already fired, or if we've already
	// taken our one allowed trade for this symbol today.
	if state.RelVolSkip {
		return nil
	}
	if state.Traded && s.cfg.OneTradePerDay != nil && *s.cfg.OneTradePerDay {
		return nil
	}

	// 4. Time Window Logic (9:15 - 9:30)
	// Check if this candle is WITHIN the Opening Range
	if timeInMinutes >= rangeStartMin && timeInMinutes < rangeEndMin {
		// Accumulate volume for RelVol check later
		state.RangeVolume = state.RangeVolume.Add(candle.Volume)

		// Initialize if zero (first candle)
		if state.RangeHigh.IsZero() {
			state.RangeHigh = candle.High
			state.RangeLow = candle.Low
		}

		// Expand Range
		if candle.High.GreaterThan(state.RangeHigh) {
			state.RangeHigh = candle.High
		}
		if candle.Low.LessThan(state.RangeLow) || state.RangeLow.IsZero() {
			state.RangeLow = candle.Low
		}
		return nil
	}

	// Check if we just finished the range (or are past it) and need to lock it
	if timeInMinutes >= rangeEndMin && !state.RangeSet {
		if !state.RangeHigh.IsZero() {
			state.RangeSet = true

			// Relative Volume filter: today's opening-range volume must be >=
			// threshold × AvgMorningVol. AvgMorningVol is seeded by Init() from
			// history and/or built up here as a rolling mean of prior days.
			// When there is no baseline yet (first observed day, or Init() never
			// ran), fall open and trade — we fold today's volume into the
			// baseline below so subsequent days are filtered properly.
			if state.AvgMorningVol.IsZero() {
				log.Printf("[%s] RelVol filter: no baseline yet — allowing today and seeding baseline", state.Symbol)
			} else {
				threshold := state.AvgMorningVol.Mul(decimal.NewFromFloat(s.cfg.RelVolThreshold))
				if state.RangeVolume.LessThan(threshold) {
					log.Printf("[%s] RelVol filter: RangeVol=%s < %.1fx AvgMorningVol=%s — skipping today",
						state.Symbol, state.RangeVolume.StringFixed(0), s.cfg.RelVolThreshold, state.AvgMorningVol.StringFixed(0))
					state.RelVolSkip = true
				}
			}

			// Fold today's opening-range volume into the rolling baseline.
			// Wilder smoothing over relVolLookbackDays: once the window is
			// full, one average day is dropped before today is added, so the
			// baseline adapts to volume-regime changes instead of averaging
			// over all history.
			if state.RangeVolume.GreaterThan(decimal.Zero) {
				if state.MorningVolDays >= relVolLookbackDays {
					state.MorningVolSum = state.MorningVolSum.Sub(state.MorningVolSum.Div(decimal.NewFromInt(state.MorningVolDays)))
				} else {
					state.MorningVolDays++
				}
				state.MorningVolSum = state.MorningVolSum.Add(state.RangeVolume)
				state.AvgMorningVol = state.MorningVolSum.Div(decimal.NewFromInt(state.MorningVolDays))
			}

			s.SaveState(candle.Symbol)

			if state.RelVolSkip {
				return nil
			}
		} else {
			// No opening-range data (e.g. the bot started mid-day or the feed
			// missed 9:15–9:30). Mark the day as skipped so we don't re-enter
			// this branch on every remaining candle.
			log.Printf("[%s] No opening-range data for %s — skipping today", state.Symbol, currentDate)
			state.RelVolSkip = true
			s.SaveState(candle.Symbol)
			return nil
		}
	}

	// If range is not set or we are somehow before start (shouldn't happen with logic above), return
	if !state.RangeSet {
		return nil
	}

	// 5. Breakout Logic
	// Restrict entries to morning session (configurable, default 10:30 AM)
	if timeInMinutes >= s.cfg.EntryWindowEnd {
		return nil
	}

	// VWAP Distance Check.
	// VWAP is zero while the session has traded no volume at all (a halted or
	// completely illiquid stock prints zero-volume candles). There is nothing
	// to measure extension against, and such a session is not tradeable, so
	// skip rather than divide by zero.
	if currentVwap.IsZero() {
		return nil
	}
	vwapDistPct := candle.Close.Sub(currentVwap).Div(currentVwap).Abs().Mul(decimal.NewFromInt(100))
	if vwapDistPct.GreaterThan(decimal.NewFromFloat(s.cfg.MaxVWAPDistPct)) {
		return nil
	}

	closePrice := candle.Close
	volume := candle.Volume

	// Warmup guard: never trade on a half-warmed indicator set. The bar
	// counter covers RSI/ADX (which can report non-zero but unreliable values
	// in their first few bars) in addition to ATR's own readiness flag.
	if state.Bars < minWarmupBars || !state.Atr.IsReady() || rsiVal.IsZero() || adxVal.IsZero() {
		return nil
	}

	// Entry Conditions
	// Condition A: breakout candle volume must exceed VolThrustMult × trailing
	// volume SMA. Default mult of 1.0 == original "volume > avg" behavior.
	volThreshold := prevAvgVol.Mul(decimal.NewFromFloat(s.cfg.VolThrustMult))
	volumeCondition := volume.GreaterThan(volThreshold)

	// Condition B: ADX > threshold AND ADX rising (within ADXRisingEps tolerance,
	// so a sub-bar tick down doesn't reject an otherwise valid breakout).
	risingFloor := prevAdx.Sub(decimal.NewFromFloat(s.cfg.ADXRisingEps))
	if adxVal.LessThan(decimal.NewFromFloat(s.cfg.ADXThreshold)) || adxVal.LessThanOrEqual(risingFloor) {
		return nil
	}

	// Condition C: Range Width Check
	rangeSize := state.RangeHigh.Sub(state.RangeLow)
	if !atrVal.IsZero() {
		minRange := atrVal.Mul(decimal.NewFromFloat(s.cfg.MinRangeATR))
		maxRange := atrVal.Mul(decimal.NewFromFloat(s.cfg.MaxRangeATR))

		if rangeSize.LessThan(minRange) || rangeSize.GreaterThan(maxRange) {
			return nil
		}
	}

	// Condition D: Extension filter. Reject entries where the breakout close is
	// already more than MaxVWAPDistATR ATRs away from VWAP — those chase an
	// extended move with poor reward-to-risk. 0 disables the filter.
	if s.cfg.MaxVWAPDistATR > 0 && !atrVal.IsZero() {
		maxDist := atrVal.Mul(decimal.NewFromFloat(s.cfg.MaxVWAPDistATR))
		if closePrice.Sub(currentVwap).Abs().GreaterThan(maxDist) {
			return nil
		}
	}

	// Condition E: gap/direction alignment. A breakout against a significant
	// overnight gap (more than half the max-gap cap) is statistically a
	// gap-fill rotation, not a trend day — skip it. Symmetric small gaps pass
	// both directions. Disabled when MaxGapPct is 0.
	gapAlignedLong := true
	gapAlignedShort := true
	if s.cfg.MaxGapPct > 0 {
		counterGapLimit := decimal.NewFromFloat(s.cfg.MaxGapPct / 2)
		gapAlignedLong = state.DayGapPct.GreaterThanOrEqual(counterGapLimit.Neg())
		gapAlignedShort = state.DayGapPct.LessThanOrEqual(counterGapLimit)
	}

	// LONG Signal
	// Crossover: Close > RangeHigh AND PrevClose <= RangeHigh
	if closePrice.GreaterThan(state.RangeHigh) && state.LastClose.LessThanOrEqual(state.RangeHigh) {
		// Trend Filter: Price > VWAP AND RSI > threshold
		// Additional Filters: Bullish candle, Recent Volume Surge, Close Strength
		candleRange := candle.High.Sub(candle.Low)
		closeStrength := decimal.Zero
		if !candleRange.IsZero() {
			closeStrength = closePrice.Sub(candle.Low).Div(candleRange)
		}

		recentVolSurge := volume.GreaterThan(prevRecentVol.Mul(decimal.NewFromFloat(1.2)))

		if gapAlignedLong && closePrice.GreaterThan(candle.Open) &&
			closePrice.GreaterThan(currentVwap) && volumeCondition && recentVolSurge &&
			rsiVal.GreaterThan(decimal.NewFromFloat(s.cfg.RSILongThreshold)) &&
			closeStrength.GreaterThanOrEqual(decimal.NewFromFloat(s.cfg.BodyStrengthThreshold)) {

			// Structural SL: start at RangeMid, widen down to RangeLow.
			rangeMid := state.RangeHigh.Add(state.RangeLow).Div(decimal.NewFromInt(2))
			stopLoss := rangeMid
			if state.RangeLow.GreaterThan(stopLoss) {
				stopLoss = state.RangeLow
			}

			var target decimal.Decimal
			if !atrVal.IsZero() {
				atrSL := closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(s.cfg.SLMultiplier)))
				if atrSL.GreaterThan(stopLoss) {
					stopLoss = atrSL
				}
				target = closePrice.Add(atrVal.Mul(decimal.NewFromFloat(s.cfg.TargetMultiplier)))
			} else {
				target = closePrice.Add(closePrice.Sub(stopLoss).Mul(decimal.NewFromFloat(2.0)))
			}

			// Floor the long stop at or below RangeLow. The opening-range bottom is
			// the structural invalidation level; the ATR widen above can raise the
			// stop up into the range, where a routine pullback would hit it. Applied
			// last so it overrides the ATR adjustment.
			if s.cfg.StopFloorAtRange != nil && *s.cfg.StopFloorAtRange {
				stopLoss = decimal.Min(stopLoss, state.RangeLow)
			}

			// Persist the Traded flag before returning the signal so a crash/
			// restart between entry and the next save cannot fire a duplicate.
			state.Traded = true
			s.SaveState(candle.Symbol)

			var partialExitPrice, partialExitPct decimal.Decimal
			if s.cfg.PartialExitAtRMultiple > 0 && !atrVal.IsZero() {
				partialExitPrice = closePrice.Add(atrVal.Mul(decimal.NewFromFloat(s.cfg.PartialExitAtRMultiple))).Round(2)
				partialExitPct = decimal.NewFromFloat(s.cfg.PartialExitPct)
			}

			return &models.Signal{
				Symbol:           candle.Symbol,
				Type:             models.BuySignal,
				ProductType:      "MIS",
				Price:            closePrice,
				StopLoss:         stopLoss.Round(2),
				Target:           target.Round(2),
				PartialExitPrice: partialExitPrice,
				PartialExitPct:   partialExitPct,
				Metadata: map[string]string{
					"Strategy":      "ORB_Long",
					"RangeHigh":     state.RangeHigh.StringFixed(2),
					"RangeLow":      state.RangeLow.StringFixed(2),
					"VWAP":          currentVwap.StringFixed(2),
					"ATR":           atrVal.StringFixed(2),
					"ADX":           adxVal.StringFixed(2),
					"RSI":           rsiVal.StringFixed(2),
					"GapPct":        state.DayGapPct.StringFixed(2),
					"Volume":        volume.StringFixed(0),
					"AvgVol":        prevAvgVol.StringFixed(0),
					"RecentAvgVol":  prevRecentVol.StringFixed(0),
					"CloseStrength": closeStrength.StringFixed(2),
					"CandleTime":    candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	// SHORT Signal
	// Crossover: Close < RangeLow AND PrevClose >= RangeLow
	if closePrice.LessThan(state.RangeLow) && state.LastClose.GreaterThanOrEqual(state.RangeLow) {
		// Trend Filter: Price < VWAP AND RSI < threshold
		// Additional Filters: Bearish candle, Recent Volume Surge, Close Strength
		candleRange := candle.High.Sub(candle.Low)
		closeStrength := decimal.Zero
		if !candleRange.IsZero() {
			closeStrength = candle.High.Sub(closePrice).Div(candleRange)
		}

		recentVolSurge := volume.GreaterThan(prevRecentVol.Mul(decimal.NewFromFloat(1.2)))

		if gapAlignedShort && closePrice.LessThan(candle.Open) &&
			closePrice.LessThan(currentVwap) && volumeCondition && recentVolSurge &&
			rsiVal.LessThan(decimal.NewFromFloat(s.cfg.RSIShortThreshold)) &&
			closeStrength.GreaterThanOrEqual(decimal.NewFromFloat(s.cfg.BodyStrengthThreshold)) {

			// Structural SL: RangeHigh is the invalidation level for shorts.
			// Tighten with ATR-based SL if it gives a closer ceiling.
			stopLoss := state.RangeHigh

			var target decimal.Decimal
			if !atrVal.IsZero() {
				atrSL := closePrice.Add(atrVal.Mul(decimal.NewFromFloat(s.cfg.SLMultiplier)))
				if atrSL.LessThan(stopLoss) {
					stopLoss = atrSL
				}
				target = closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(s.cfg.TargetMultiplier)))
			} else {
				target = closePrice.Sub(state.RangeHigh.Sub(closePrice).Mul(decimal.NewFromFloat(2.0)))
			}

			// Floor the short stop at or above RangeHigh. The opening-range top is
			// the structural invalidation level; the ATR tighten above can lower the
			// stop down into the range, where a routine pullback would hit it. Applied
			// last so it overrides the ATR adjustment.
			if s.cfg.StopFloorAtRange != nil && *s.cfg.StopFloorAtRange {
				stopLoss = decimal.Max(stopLoss, state.RangeHigh)
			}

			// Persist the Traded flag before returning the signal so a crash/
			// restart between entry and the next save cannot fire a duplicate.
			state.Traded = true
			s.SaveState(candle.Symbol)

			var partialExitPrice, partialExitPct decimal.Decimal
			if s.cfg.PartialExitAtRMultiple > 0 && !atrVal.IsZero() {
				partialExitPrice = closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(s.cfg.PartialExitAtRMultiple))).Round(2)
				partialExitPct = decimal.NewFromFloat(s.cfg.PartialExitPct)
			}

			return &models.Signal{
				Symbol:           candle.Symbol,
				Type:             models.SellSignal,
				ProductType:      "MIS",
				Price:            closePrice,
				StopLoss:         stopLoss.Round(2),
				Target:           target.Round(2),
				PartialExitPrice: partialExitPrice,
				PartialExitPct:   partialExitPct,
				Metadata: map[string]string{
					"Strategy":      "ORB_Short",
					"RangeHigh":     state.RangeHigh.StringFixed(2),
					"RangeLow":      state.RangeLow.StringFixed(2),
					"VWAP":          currentVwap.StringFixed(2),
					"ATR":           atrVal.StringFixed(2),
					"ADX":           adxVal.StringFixed(2),
					"RSI":           rsiVal.StringFixed(2),
					"GapPct":        state.DayGapPct.StringFixed(2),
					"Volume":        volume.StringFixed(0),
					"AvgVol":        prevAvgVol.StringFixed(0),
					"RecentAvgVol":  prevRecentVol.StringFixed(0),
					"CloseStrength": closeStrength.StringFixed(2),
					"CandleTime":    candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	return nil
}
