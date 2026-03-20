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
}

func NewORBState(symbol string) *ORBState {
	return &ORBState{
		Symbol:    symbol,
		Vwap:      indicators.NewVWAP(),
		VolSma:    indicators.NewSMA(9),
		Atr:       indicators.NewATR(14),
		Adx:       indicators.NewADX(9),
		Rsi:       indicators.NewRSI(9),
		RangeHigh: decimal.Zero,
		RangeLow:  decimal.Zero,
		RangeSet:  false,
		LastClose: decimal.Zero,
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
		"RangeHigh": state.RangeHigh,
		"RangeLow":  state.RangeLow,
		"RangeSet":  state.RangeSet,
		"LastDate":  state.LastDate,
		"LastClose": state.LastClose,
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
		RangeHigh decimal.Decimal
		RangeLow  decimal.Decimal
		RangeSet  bool
		LastDate  string
		LastClose decimal.Decimal
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
	log.Printf("LOADED STATE for %s: High=%s Low=%s Set=%v", symbol, state.RangeHigh, state.RangeLow, state.RangeSet)
}

func (s *ORBStrategy) Init(provider core.DataProvider) error {
	log.Println("Initializing ORB Strategy...")

	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}

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
		for _, c := range candles {
			state.Vwap.Update(c)
			state.VolSma.Update(c.Volume)
			state.Atr.Update(c)
			state.Adx.Update(c)
			state.Rsi.Update(c.Close)

			istTime := c.StartTime.In(loc)
			h, m, _ := istTime.Clock()
			tMin := h*60 + m
			if tMin >= 9*60+15 && tMin < 9*60+30 {
				dateKey := c.StartTime.Format("2006-01-02")
				morningVolByDay[dateKey] = morningVolByDay[dateKey].Add(c.Volume)
			}
		}

		if len(morningVolByDay) > 0 {
			total := decimal.Zero
			for _, v := range morningVolByDay {
				total = total.Add(v)
			}
			state.AvgMorningVol = total.Div(decimal.NewFromInt(int64(len(morningVolByDay))))
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
		s.SaveState(candle.Symbol)
	}

	// Skip for the day if RelVol filter already fired
	if state.RelVolSkip {
		return nil
	}

	// 2. Capture Previous Volume Average (for "Avg of last 5 bars" check)
	prevAvgVol := state.VolSma.Value()

	// 3. Update Indicators
	currentVwap := state.Vwap.Update(candle)
	state.VolSma.Update(candle.Volume)
	atrVal := state.Atr.Update(candle)
	prevAdx := state.Adx.Value()
	adxVal := state.Adx.Update(candle)
	rsiVal := state.Rsi.Update(candle.Close)

	// 4. Time Window Logic (9:15 - 9:30)
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	istTime := candle.StartTime.In(loc)
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

			// Relative Volume filter: opening-range volume must be >= threshold × AvgMorningVol
			if !state.AvgMorningVol.IsZero() {
				threshold := state.AvgMorningVol.Mul(decimal.NewFromFloat(s.cfg.RelVolThreshold))
				if state.RangeVolume.LessThan(threshold) {
					log.Printf("[%s] RelVol filter: RangeVol=%s < %.1fx AvgMorningVol=%s — skipping today",
						state.Symbol, state.RangeVolume.StringFixed(0), s.cfg.RelVolThreshold, state.AvgMorningVol.StringFixed(0))
					state.RelVolSkip = true
					s.SaveState(candle.Symbol)
					return nil
				}
			}

			s.SaveState(candle.Symbol)
		} else {
			// No data in range?
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

	closePrice := candle.Close
	volume := candle.Volume

	// Entry Conditions
	// Condition A: Volume > Avg(9)
	volumeCondition := volume.GreaterThan(prevAvgVol)

	// Condition B: ADX > threshold AND ADX rising
	if adxVal.LessThan(decimal.NewFromFloat(s.cfg.ADXThreshold)) || adxVal.LessThanOrEqual(prevAdx) {
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

	// LONG Signal
	// Crossover: Close > RangeHigh AND PrevClose <= RangeHigh
	if closePrice.GreaterThan(state.RangeHigh) && state.LastClose.LessThanOrEqual(state.RangeHigh) {
		// Trend Filter: Price > VWAP AND RSI > threshold
		if closePrice.GreaterThan(currentVwap) && volumeCondition && rsiVal.GreaterThan(decimal.NewFromFloat(s.cfg.RSILongThreshold)) {
			stopLoss := state.RangeHigh.Add(state.RangeLow).Div(decimal.NewFromInt(2))
			target := closePrice.Add(closePrice.Sub(stopLoss).Mul(decimal.NewFromFloat(2.0))) // Default fallback

			if !atrVal.IsZero() {
				stopLoss = closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(s.cfg.SLMultiplier)))
				target = closePrice.Add(atrVal.Mul(decimal.NewFromFloat(s.cfg.TargetMultiplier)))
			}

			return &models.Signal{
				Symbol:      candle.Symbol,
				Type:        models.BuySignal,
				ProductType: "MIS",
				Price:       closePrice,
				StopLoss:    stopLoss.Round(2),
				Target:      target.Round(2),
				Metadata: map[string]string{
					"Strategy":      "ORB_Long",
					"RangeHigh":     state.RangeHigh.StringFixed(2),
					"RangeLow":      state.RangeLow.StringFixed(2),
					"VWAP":          currentVwap.StringFixed(2),
					"ATR":           atrVal.StringFixed(2),
					"ADX":           adxVal.StringFixed(2),
					"RSI":           rsiVal.StringFixed(2),
					"Volume":        volume.StringFixed(0),
					"AvgVol":        prevAvgVol.StringFixed(0),
					"RangeVolume":   state.RangeVolume.StringFixed(0),
					"AvgMorningVol": state.AvgMorningVol.StringFixed(0),
					"CandleTime":    candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	// SHORT Signal
	// Crossover: Close < RangeLow AND PrevClose >= RangeLow
	if closePrice.LessThan(state.RangeLow) && state.LastClose.GreaterThanOrEqual(state.RangeLow) {
		// Trend Filter: Price < VWAP AND RSI < threshold
		if closePrice.LessThan(currentVwap) && volumeCondition && rsiVal.LessThan(decimal.NewFromFloat(s.cfg.RSIShortThreshold)) {
			stopLoss := state.RangeHigh.Add(state.RangeLow).Div(decimal.NewFromInt(2))
			target := closePrice.Sub(stopLoss.Sub(closePrice).Mul(decimal.NewFromFloat(2.0))) // Default fallback

			if !atrVal.IsZero() {
				stopLoss = closePrice.Add(atrVal.Mul(decimal.NewFromFloat(s.cfg.SLMultiplier)))
				target = closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(s.cfg.TargetMultiplier)))
			}

			return &models.Signal{
				Symbol:      candle.Symbol,
				Type:        models.SellSignal,
				ProductType: "MIS",
				Price:       closePrice,
				StopLoss:    stopLoss.Round(2),
				Target:      target.Round(2),
				Metadata: map[string]string{
					"Strategy":      "ORB_Short",
					"RangeHigh":     state.RangeHigh.StringFixed(2),
					"RangeLow":      state.RangeLow.StringFixed(2),
					"VWAP":          currentVwap.StringFixed(2),
					"ATR":           atrVal.StringFixed(2),
					"ADX":           adxVal.StringFixed(2),
					"RSI":           rsiVal.StringFixed(2),
					"Volume":        volume.StringFixed(0),
					"AvgVol":        prevAvgVol.StringFixed(0),
					"RangeVolume":   state.RangeVolume.StringFixed(0),
					"AvgMorningVol": state.AvgMorningVol.StringFixed(0),
					"CandleTime":    candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	return nil
}
