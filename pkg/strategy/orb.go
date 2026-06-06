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
	AvgMorningVol decimal.Decimal // Avg opening-range (9:15–9:30) volume from historical days
	RangeVolume   decimal.Decimal // Accumulated volume during today's opening range
	RelVolSkip    bool            // True if today's opening-range volume failed the RelVol filter
	Traded        bool            // True once a signal has fired for this symbol today (one-trade-per-day guard)

	// Rolling baseline of opening-range volume, updated as each day's range
	// locks. This lets the strategy self-seed AvgMorningVol when Init() was not
	// called (e.g. the backtest harness feeds candles directly), and keeps the
	// baseline adapting over time in live trading.
	MorningVolSum  decimal.Decimal // Sum of past days' opening-range volumes
	MorningVolDays int64           // Count of past days contributing to MorningVolSum
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

	// Verify date match (don't load old state)
	// We might need to check today's date?
	// The implementation in OnCandle checks LastDate != currentDate and resets.
	// So if we load old date, it will just get reset on first candle. Perfect.

	state.RangeHigh = pState.RangeHigh
	state.RangeLow = pState.RangeLow
	state.RangeSet = pState.RangeSet
	state.LastDate = pState.LastDate
	state.LastClose = pState.LastClose
	state.RelVolSkip = pState.RelVolSkip
	state.RangeVolume = pState.RangeVolume
	state.Traded = pState.Traded
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

		// Accumulate opening-range volume per historical day to compute AvgMorningVol
		morningVolByDay := make(map[string]decimal.Decimal)
		var lastDate string
		var lastClose decimal.Decimal
		for _, c := range candles {
			state.Vwap.Update(c)
			state.VolSma.Update(c.Volume)
			state.VolSma5.Update(c.Volume)
			state.Atr.Update(c)
			state.Adx.Update(c)
			state.Rsi.Update(c.Close)

			// Capture PrevDayClose: The close of the last candle of the previous day
			istTime := c.StartTime.In(loc)
			dateKey := istTime.Format("2006-01-02")

			// If we see a new date, the previous date's last candle was the close
			if lastDate != "" && dateKey != lastDate {
				state.PrevDayClose = lastClose
			}
			lastDate = dateKey
			lastClose = c.Close

			h, m, _ := istTime.Clock()
			tMin := h*60 + m
			if tMin >= 9*60+15 && tMin < 9*60+30 {
				morningVolByDay[dateKey] = morningVolByDay[dateKey].Add(c.Volume)
			}
		}

		// Fallback for PrevDayClose if only one day of history (though we requested 10)
		if state.PrevDayClose.IsZero() && lastClose.GreaterThan(decimal.Zero) {
			state.PrevDayClose = lastClose
		}

		if len(morningVolByDay) > 0 {
			total := decimal.Zero
			for _, v := range morningVolByDay {
				total = total.Add(v)
			}
			// Seed both the average and the rolling-baseline accumulators so the
			// per-day folding in OnCandle extends this history rather than
			// overwriting it.
			state.MorningVolSum = total
			state.MorningVolDays = int64(len(morningVolByDay))
			state.AvgMorningVol = total.Div(decimal.NewFromInt(state.MorningVolDays))
			log.Printf("[%s] AvgMorningVol (9:15-9:30) over %d days: %s", sym, len(morningVolByDay), state.AvgMorningVol.StringFixed(0))
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

	// 1. Date Check for Reset
	currentDate := candle.StartTime.Format("2006-01-02")
	if state.LastDate != currentDate {
		state.RangeHigh = decimal.Zero
		state.RangeLow = decimal.Zero
		state.RangeSet = false
		state.LastDate = currentDate
		state.RangeVolume = decimal.Zero
		state.RelVolSkip = false
		state.Traded = false
		s.SaveState(candle.Symbol)
	}

	// Skip for the day if RelVol filter already fired, or if we've already
	// taken our one allowed trade for this symbol today.
	if state.RelVolSkip {
		return nil
	}
	if state.Traded && s.cfg.OneTradePerDay != nil && *s.cfg.OneTradePerDay {
		return nil
	}

	// 3. Update Indicators
	currentVwap := state.Vwap.Update(candle)
	prevAvgVol := state.VolSma.Value()
	state.VolSma.Update(candle.Volume)
	prevRecentVol := state.VolSma5.Value()
	state.VolSma5.Update(candle.Volume)
	atrVal := state.Atr.Update(candle)
	prevAdx := state.Adx.Value()
	adxVal := state.Adx.Update(candle)
	rsiVal := state.Rsi.Update(candle.Close)

	// 4. Time Window Logic (9:15 - 9:30)
	istTime := candle.StartTime.In(istLocation)
	h, m, _ := istTime.Clock()
	timeInMinutes := h*60 + m
	rangeStart := 9*60 + 15
	rangeEnd := 9*60 + 30

	// Check if this candle is WITHIN the Opening Range
	if timeInMinutes >= rangeStart && timeInMinutes < rangeEnd {
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
	if timeInMinutes >= rangeEnd && !state.RangeSet {
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

			// Fold today's opening-range volume into the rolling baseline so the
			// average adapts and self-seeds even without Init().
			if state.RangeVolume.GreaterThan(decimal.Zero) {
				state.MorningVolSum = state.MorningVolSum.Add(state.RangeVolume)
				state.MorningVolDays++
				state.AvgMorningVol = state.MorningVolSum.Div(decimal.NewFromInt(state.MorningVolDays))
			}

			s.SaveState(candle.Symbol)

			if state.RelVolSkip {
				return nil
			}
		} else {
			// No data in range?
			return nil
		}
	}

	// If range is not set or we are somehow before start (shouldn't happen with logic above), return
	if !state.RangeSet {
		return nil
	}

	// 5. Gap Check — evaluate once on the first post-range candle using that candle's open
	if !state.GapChecked && !state.PrevDayClose.IsZero() {
		state.GapChecked = true
		gapPct := candle.Open.Sub(state.PrevDayClose).Div(state.PrevDayClose).Abs().Mul(decimal.NewFromInt(100))
		if gapPct.GreaterThan(decimal.NewFromFloat(s.cfg.MaxGapPct)) {
			log.Printf("[%s] Gap filter: Gap=%.2f%% > %.2f%% — skipping today", state.Symbol, gapPct.InexactFloat64(), s.cfg.MaxGapPct)
			state.RelVolSkip = true
			return nil
		}
	}

	// 6. Breakout Logic
	// Restrict entries to morning session (configurable, default 10:30 AM)
	if timeInMinutes >= s.cfg.EntryWindowEnd {
		return nil
	}

	// One signal per symbol per day — prevent false re-entries after pullbacks
	if state.BreakoutFired {
		return nil
	}

	// VWAP Distance Check
	vwapDistPct := candle.Close.Sub(currentVwap).Div(currentVwap).Abs().Mul(decimal.NewFromInt(100))
	if vwapDistPct.GreaterThan(decimal.NewFromFloat(s.cfg.MaxVWAPDistPct)) {
		return nil
	}

	closePrice := candle.Close
	volume := candle.Volume

	// Warmup guard: never trade on a half-warmed indicator set.
	if !state.Atr.IsReady() || rsiVal.IsZero() || adxVal.IsZero() {
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

	// LONG Signal
	// Crossover: Close > RangeHigh AND PrevClose <= RangeHigh
	if closePrice.GreaterThan(state.RangeHigh) && state.LastClose.LessThanOrEqual(state.RangeHigh) {
		// Trend Filter: Price > VWAP AND RSI > threshold
		// Additional Filters: Recent Volume Surge AND Body Strength
		bodySize := candle.High.Sub(candle.Low)
		bodyStrength := decimal.Zero
		if !bodySize.IsZero() {
			bodyStrength = closePrice.Sub(candle.Low).Div(bodySize)
		}

		recentVolSurge := volume.GreaterThan(prevRecentVol.Mul(decimal.NewFromFloat(1.2)))

		if closePrice.GreaterThan(currentVwap) && volumeCondition && recentVolSurge &&
			rsiVal.GreaterThan(decimal.NewFromFloat(s.cfg.RSILongThreshold)) &&
			bodyStrength.GreaterThanOrEqual(decimal.NewFromFloat(s.cfg.BodyStrengthThreshold)) {

			// Structural SL: Max(RangeMid, RangeLow, Entry - SLMultiplier*ATR)
			rangeMid := state.RangeHigh.Add(state.RangeLow).Div(decimal.NewFromInt(2))
			stopLoss := rangeMid
			if state.RangeLow.GreaterThan(stopLoss) {
				stopLoss = state.RangeLow
			}

			// Floor the long stop at RangeLow: the bottom of the opening range is
			// the structural invalidation level. An ATR stop sitting above it would
			// be hit by a routine pullback into the range. min() widens to RangeLow.
			if s.cfg.StopFloorAtRange != nil && *s.cfg.StopFloorAtRange {
				stopLoss = decimal.Min(stopLoss, state.RangeLow)
			}

			state.Traded = true

			return &models.Signal{
				Symbol:      candle.Symbol,
				Type:        models.BuySignal,
				ProductType: "MIS",
				Price:       closePrice,
				StopLoss:    stopLoss.Round(2),
				Target:      target.Round(2),
				Metadata: map[string]string{
					"Strategy":     "ORB_Long",
					"RangeHigh":    state.RangeHigh.StringFixed(2),
					"RangeLow":     state.RangeLow.StringFixed(2),
					"VWAP":         currentVwap.StringFixed(2),
					"ATR":          atrVal.StringFixed(2),
					"ADX":          adxVal.StringFixed(2),
					"RSI":          rsiVal.StringFixed(2),
					"Volume":       volume.StringFixed(0),
					"AvgVol":       prevAvgVol.StringFixed(0),
					"RecentAvgVol": prevRecentVol.StringFixed(0),
					"BodyStrength": bodyStrength.StringFixed(2),
					"CandleTime":   candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	// SHORT Signal
	// Crossover: Close < RangeLow AND PrevClose >= RangeLow
	if closePrice.LessThan(state.RangeLow) && state.LastClose.GreaterThanOrEqual(state.RangeLow) {
		// Trend Filter: Price < VWAP AND RSI < threshold
		// Additional Filters: Recent Volume Surge AND Body Strength
		bodySize := candle.High.Sub(candle.Low)
		bodyStrength := decimal.Zero
		if !bodySize.IsZero() {
			bodyStrength = candle.High.Sub(closePrice).Div(bodySize)
		}

		recentVolSurge := volume.GreaterThan(prevRecentVol.Mul(decimal.NewFromFloat(1.2)))

		if closePrice.LessThan(currentVwap) && volumeCondition && recentVolSurge &&
			rsiVal.LessThan(decimal.NewFromFloat(s.cfg.RSIShortThreshold)) &&
			bodyStrength.GreaterThanOrEqual(decimal.NewFromFloat(s.cfg.BodyStrengthThreshold)) {

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

			// Cap the short stop at RangeHigh: the top of the opening range is the
			// structural invalidation level. An ATR stop below it would be hit by a
			// routine pullback into the range. max() widens to RangeHigh.
			if s.cfg.StopFloorAtRange != nil && *s.cfg.StopFloorAtRange {
				stopLoss = decimal.Max(stopLoss, state.RangeHigh)
			}

			state.Traded = true

			return &models.Signal{
				Symbol:      candle.Symbol,
				Type:        models.SellSignal,
				ProductType: "MIS",
				Price:       closePrice,
				StopLoss:    stopLoss.Round(2),
				Target:      target.Round(2),
				Metadata: map[string]string{
					"Strategy":     "ORB_Short",
					"RangeHigh":    state.RangeHigh.StringFixed(2),
					"RangeLow":     state.RangeLow.StringFixed(2),
					"VWAP":         currentVwap.StringFixed(2),
					"ATR":          atrVal.StringFixed(2),
					"ADX":          adxVal.StringFixed(2),
					"RSI":          rsiVal.StringFixed(2),
					"Volume":       volume.StringFixed(0),
					"AvgVol":       prevAvgVol.StringFixed(0),
					"RecentAvgVol": prevRecentVol.StringFixed(0),
					"BodyStrength": bodyStrength.StringFixed(2),
					"CandleTime":   candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	return nil
}
