package strategy

import (
	"log"
	"math"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// EmaTrendAngleState holds the indicators for a single symbol
type EmaTrendAngleState struct {
	Symbol     string
	Ema21      *indicators.EMA // 21 Period
	Atr        *indicators.ATR // 14 Period
	EmaHistory []decimal.Decimal
}

func NewEmaTrendAngleState(symbol string) *EmaTrendAngleState {
	return &EmaTrendAngleState{
		Symbol:     symbol,
		Ema21:      indicators.NewEMA(21),
		Atr:        indicators.NewATR(14),
		EmaHistory: make([]decimal.Decimal, 0, 15), // Keep enough for 10 period lookback
	}
}

func (s *EmaTrendAngleState) CalculateTrendAngle(currentEma decimal.Decimal) float64 {
	// We need 10 periods of history.
	// EmaHistory stores previous EMAs.
	if len(s.EmaHistory) < 10 {
		return 0.0
	}

	// Get the EMA value from 10 candles ago
	// EmaHistory is [t-10, t-9, ..., t-1]
	oldEma := s.EmaHistory[len(s.EmaHistory)-10]
	if oldEma.IsZero() {
		return 0.0
	}
	// Normalized Slope = (Change / OldValue) * 100 * 5 (Multiplier for sensitivity)
	change := currentEma.Sub(oldEma).Div(oldEma).Mul(decimal.NewFromInt(100))
	slope := change.Mul(decimal.NewFromInt(5)).InexactFloat64()
	angle := math.Atan(slope) * (180 / math.Pi)
	return angle
}

type EmaTrendAngle struct {
	symbols   []string
	states    map[string]*EmaTrendAngleState
	Timeframe string
}

func NewEmaTrendAngle(symbols []string, timeframe string) *EmaTrendAngle {
	return &EmaTrendAngle{
		symbols:   symbols,
		states:    make(map[string]*EmaTrendAngleState),
		Timeframe: timeframe,
	}
}

func (s *EmaTrendAngle) Name() string {
	return "EMA_Trend_Angle"
}

func (s *EmaTrendAngle) Init(provider core.DataProvider) error {
	log.Println("Initializing Strategy: Fetching History...")

	for _, sym := range s.symbols {
		// 1. Create State
		state := NewEmaTrendAngleState(sym)
		s.states[sym] = state

		// 2. Fetch History (Last 50 candles for warmup)
		// Using configured timeframe
		candles, err := provider.History(sym, s.Timeframe, 50)
		if err != nil {
			log.Printf("WARNING: Failed to fetch history for %s: %v", sym, err)
			continue
		}

		// 3. Replay Candles to Warmup
		for _, c := range candles {
			state.Ema21.Update(c.Close)
			state.Atr.Update(c)

			// Maintain History for Angle Calculation
			if state.Ema21.IsReady() {
				state.EmaHistory = append(state.EmaHistory, state.Ema21.Value())
				if len(state.EmaHistory) > 14 {
					state.EmaHistory = state.EmaHistory[1:] // Keep last 14
				}
			}
		}
		log.Printf("Warmed up %s with %d candles. EMA21: %s", sym, len(candles), state.Ema21.Value().StringFixed(2))
	}
	return nil
}

func (s *EmaTrendAngle) OnCandle(candle models.Candle) *models.Signal {
	// 1. Get State
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewEmaTrendAngleState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	// 2. Update Indicators
	ema21 := state.Ema21.Update(candle.Close)
	atrVal := state.Atr.Update(candle)

	// 3. Wait for warm-up
	if !state.Ema21.IsReady() || atrVal.IsZero() {
		// Update history to keep it flowing
		if state.Ema21.IsReady() {
			state.EmaHistory = append(state.EmaHistory, ema21)
			if len(state.EmaHistory) > 14 {
				state.EmaHistory = state.EmaHistory[1:]
			}
		}
		return nil
	}

	// 4. Time Window Check (9:30 AM - 2:30 PM)
	// 4. Time Window Check (9:30 AM - 2:30 PM)
	// Only applies if we are trading intraday on lower timeframes.
	// For now, this logic is specific to intraday but we can leave it or make it smarter.
	// Since we are moving to higher timeframes (1h, 1d), this check might skip valid candles.
	// TODO: Make this smarter based on s.Timeframe.
	// For "1h", the candle start times are e.g., 09:15, 10:15...
	// 09:15 < 930 -> SKIP
	// 10:15 > 930 -> OK
	// ...
	// 15:15 > 1430 -> SKIP
	// If Timeframe is "1d", hour will be 0 or 9 (depending on data source).
	// If 1d, we should skip this check entirely.
	if s.Timeframe == "1d" {
		// No time check for Daily
	} else {
		hour := candle.StartTime.Hour()
		minute := candle.StartTime.Minute()
		timeVal := hour*100 + minute
		if timeVal < 930 || timeVal > 1430 {
			return nil
		}
	}

	// 5. Logic Implementation

	// Condition A: Trend Angle > 25 degrees
	angle := state.CalculateTrendAngle(ema21)
	goodAngle := angle > 45.0

	// Condition B: Candle crosses up and closes above EMA 21
	// We check if Open < EMA and Close > EMA (Body Cross)
	crossUp := candle.Open.LessThan(ema21) && candle.Close.GreaterThan(ema21)

	var signal *models.Signal

	if goodAngle && crossUp {
		// VALID SIGNAL FOUND

		// 5. Calculate Dynamic Stops
		// Stop Loss: Low - ATR
		stopLoss := candle.Low.Sub(atrVal)

		// Target: Close + ATR * 2
		target := candle.Close.Add(atrVal.Mul(decimal.NewFromFloat(5)))

		signal = &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.BuySignal,
			Price:       candle.Close,
			StopLoss:    stopLoss.Floor(),
			Target:      target.Floor(),
			ProductType: "CNC", // Delivery order
			Metadata: map[string]string{
				"Strategy":   s.Name(),
				"EMA21":      ema21.StringFixed(2),
				"ATR":        atrVal.StringFixed(2),
				"TrendAngle": decimal.NewFromFloat(angle).StringFixed(2),
				"CandleTime": candle.StartTime.Format("2006-01-02 15:04:05"),
			},
		}
	}

	// Update History for next candle
	state.EmaHistory = append(state.EmaHistory, ema21)
	if len(state.EmaHistory) > 10 {
		state.EmaHistory = state.EmaHistory[1:]
	}

	return signal
}
