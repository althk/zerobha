package strategy

import (
	"log"
	"time"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/db"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

type ORBState struct {
	Symbol    string
	Vwap      *indicators.VWAP
	VolSma    *indicators.SMA // 9 period volume SMA
	Atr       *indicators.ATR // 14 period ATR
	Adx       *indicators.ADX // 9 period ADX
	Rsi       *indicators.RSI // 9 period RSI
	RangeHigh decimal.Decimal
	RangeLow  decimal.Decimal
	RangeSet  bool
	LastDate  string
	LastClose decimal.Decimal // Close price of the previous candle
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
}

func NewORBStrategy(symbols []string) *ORBStrategy {
	return &ORBStrategy{
		symbols: symbols,
		states:  make(map[string]*ORBState),
	}
}

func (s *ORBStrategy) Name() string {
	return "ORB_15min"
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
	for _, sym := range s.symbols {
		state := NewORBState(sym)
		s.states[sym] = state
		s.LoadState(sym)

		// Fetch 1 day of history for warmup (mainly for Volume SMA and ensuring we are ready)
		candles, err := provider.History(sym, "5minute", 1)
		if err != nil {
			log.Printf("WARNING: Failed to fetch history for %s: %v", sym, err)
			continue
		}

		for _, c := range candles {
			state.Vwap.Update(c)
			state.VolSma.Update(c.Volume)
			state.Atr.Update(c)
			state.Adx.Update(c)
			state.Rsi.Update(c.Close)
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

	// 1. Date Check for Reset
	// Assuming StartTime is effectively "Exchange Time" or local time appropriate for day checking
	currentDate := candle.StartTime.Format("2006-01-02")
	if state.LastDate != currentDate {
		state.RangeHigh = decimal.Zero
		state.RangeLow = decimal.Zero
		state.RangeSet = false
		state.LastDate = currentDate
		s.SaveState(candle.Symbol)
		// VWAP resets automatically based on its own date check, but we rely on its Update() method.
	}

	// 2. Capture Previous Volume Average (for "Avg of last 5 bars" check)
	// We want the average of the *previous* 5 bars to compare against current breakout volume.
	prevAvgVol := state.VolSma.Value() // Current Value of SMA (before this candle)

	// 3. Update Indicators
	currentVwap := state.Vwap.Update(candle)
	state.VolSma.Update(candle.Volume)
	atrVal := state.Atr.Update(candle)
	// Capture previous ADX before update
	prevAdx := state.Adx.Value()
	adxVal := state.Adx.Update(candle)
	rsiVal := state.Rsi.Update(candle.Close)

	// 4. Time Window Logic (9:15 - 9:30)
	// Convert to IST
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// Fallback to fixed offset +5:30
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	istTime := candle.StartTime.In(loc)
	h, m, _ := istTime.Clock()
	timeInMinutes := h*60 + m
	rangeStart := 9*60 + 15
	rangeEnd := 9*60 + 30

	// Check if this candle is WITHIN the Opening Range
	if timeInMinutes >= rangeStart && timeInMinutes < rangeEnd {
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
			s.SaveState(candle.Symbol)
			// log.Printf("[%s] ORB Range Set: High=%s, Low=%s", state.Symbol, state.RangeHigh, state.RangeLow)
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
	// Only consider valid trading hours (e.g. stop before close)
	// Only consider valid trading hours (e.g. stop before close)
	// Restrict entries to morning session (before 10:30 AM) for ORB
	// 10:30 = 10*60 + 30 = 630 minutes
	if timeInMinutes >= 10*60+30 {
		return nil
	}

	closePrice := candle.Close
	volume := candle.Volume

	// Entry Conditions
	// "Entry Trigger 5-min Candle Close outside Range"
	// Condition A: Volume > Avg(3) (User modified)
	volumeCondition := volume.GreaterThan(prevAvgVol)

	// Condition B: ADX > 20 (Up Trend)
	// Condition B: ADX > 20 (Up Trend) AND ADX rising
	if adxVal.LessThan(decimal.NewFromInt(20)) || adxVal.LessThanOrEqual(prevAdx) {
		return nil
	}

	// Condition C: Range Width Check
	// Avoid choppy/small ranges (Range < 1 * ATR) and over-extended ranges (Range > 3 * ATR)
	rangeSize := state.RangeHigh.Sub(state.RangeLow)
	if !atrVal.IsZero() {
		minRange := atrVal.Mul(decimal.NewFromFloat(1.0))
		maxRange := atrVal.Mul(decimal.NewFromFloat(3.0))

		if rangeSize.LessThan(minRange) || rangeSize.GreaterThan(maxRange) {
			return nil
		}
	}

	// LONG Signal
	// Crossover: Close > RangeHigh AND PrevClose <= RangeHigh
	if closePrice.GreaterThan(state.RangeHigh) && state.LastClose.LessThanOrEqual(state.RangeHigh) {
		// Trend Filter: Price > VWAP AND RSI > 50
		if closePrice.GreaterThan(currentVwap) && volumeCondition && rsiVal.GreaterThan(decimal.NewFromInt(50)) {
			// Stop Loss = Entry - 1 * ATR
			// Target = Entry + 2 * ATR
			stopLoss := state.RangeHigh.Add(state.RangeLow).Div(decimal.NewFromInt(2))
			target := closePrice.Add(closePrice.Sub(stopLoss).Mul(decimal.NewFromFloat(2.0))) // Default fallback

			if !atrVal.IsZero() {
				// Adjusted for High Beta: SL = 1.5 ATR, Target = 3.0 ATR (1:2 R:R)
				stopLoss = closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(1.5)))
				target = closePrice.Add(atrVal.Mul(decimal.NewFromFloat(3.0)))
			}

			return &models.Signal{
				Symbol:      candle.Symbol,
				Type:        models.BuySignal,
				ProductType: "MIS",
				Price:       closePrice,
				StopLoss:    stopLoss.Round(2),
				Target:      target.Round(2),
				Metadata: map[string]string{
					"Strategy":   "ORB_15min_Long",
					"RangeHigh":  state.RangeHigh.StringFixed(2),
					"RangeLow":   state.RangeLow.StringFixed(2),
					"VWAP":       currentVwap.StringFixed(2),
					"ATR":        atrVal.StringFixed(2),
					"ADX":        adxVal.StringFixed(2),
					"RSI":        rsiVal.StringFixed(2),
					"Volume":     volume.StringFixed(0),
					"AvgVol":     prevAvgVol.StringFixed(0),
					"CandleTime": candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	// SHORT Signal
	// Crossover: Close < RangeLow AND PrevClose >= RangeLow
	if closePrice.LessThan(state.RangeLow) && state.LastClose.GreaterThanOrEqual(state.RangeLow) {
		// Trend Filter: Price < VWAP AND RSI < 40
		if closePrice.LessThan(currentVwap) && volumeCondition && rsiVal.LessThan(decimal.NewFromInt(40)) {
			// Stop Loss = Entry + 1 * ATR
			// Target = Entry - 2 * ATR
			stopLoss := state.RangeHigh.Add(state.RangeLow).Div(decimal.NewFromInt(2))
			target := closePrice.Sub(stopLoss.Sub(closePrice).Mul(decimal.NewFromFloat(2.0))) // Default fallback

			if !atrVal.IsZero() {
				// Adjusted for High Beta: SL = 1.5 ATR, Target = 3.0 ATR (1:2 R:R)
				stopLoss = closePrice.Add(atrVal.Mul(decimal.NewFromFloat(1.5)))
				target = closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(3.0)))
			}

			return &models.Signal{
				Symbol:      candle.Symbol,
				Type:        models.SellSignal,
				ProductType: "MIS",
				Price:       closePrice,
				StopLoss:    stopLoss.Round(2),
				Target:      target.Round(2),
				Metadata: map[string]string{
					"Strategy":   "ORB_15min_Short",
					"RangeHigh":  state.RangeHigh.StringFixed(2),
					"RangeLow":   state.RangeLow.StringFixed(2),
					"VWAP":       currentVwap.StringFixed(2),
					"ATR":        atrVal.StringFixed(2),
					"ADX":        adxVal.StringFixed(2),
					"RSI":        rsiVal.StringFixed(2),
					"Volume":     volume.StringFixed(0),
					"AvgVol":     prevAvgVol.StringFixed(0),
					"CandleTime": candle.StartTime.Format("2006-01-02 15:04:05"),
				},
			}
		}
	}

	// Update LastClose for next iteration
	state.LastClose = candle.Close

	return nil
}
