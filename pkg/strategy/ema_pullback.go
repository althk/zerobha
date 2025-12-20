package strategy

import (
	"log"
	"math"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// StrategyState holds the indicators for a single symbol
type StrategyState struct {
	Symbol     string
	EmaFast    *indicators.EMA // 20 Period
	EmaSlow    *indicators.EMA // 50 Period
	VolSma     *indicators.SMA // 20 Period Volume
	Atr        *indicators.ATR // 14 Period
	Adx        *indicators.ADX // 14 Period
	EmaHistory []decimal.Decimal
}

func NewStrategyState(symbol string) *StrategyState {
	return &StrategyState{
		Symbol:     symbol,
		EmaFast:    indicators.NewEMA(20),
		EmaSlow:    indicators.NewEMA(50),
		VolSma:     indicators.NewSMA(20),
		Atr:        indicators.NewATR(14),
		Adx:        indicators.NewADX(14),
		EmaHistory: make([]decimal.Decimal, 0, 25),
	}
}

func (s *StrategyState) CalculateTrendAngle(currentEma decimal.Decimal) float64 {
	if len(s.EmaHistory) < 21 {
		return 0.0
	}
	oldEma := s.EmaHistory[0] // 20 bars ago (since we keep 21)
	if oldEma.IsZero() {
		return 0.0
	}
	// Normalized Slope = (Change / OldValue) * 100 / Period
	change := currentEma.Sub(oldEma).Div(oldEma).Mul(decimal.NewFromInt(100))
	slope := change.Div(decimal.NewFromInt(20)).InexactFloat64()
	return math.Atan(slope) * (180 / math.Pi)
}

type EmaPullback struct {
	symbols []string
	states  map[string]*StrategyState
}

func NewEmaPullback(symbols []string) *EmaPullback {
	return &EmaPullback{
		symbols: symbols,
		states:  make(map[string]*StrategyState),
	}
}

func (s *EmaPullback) Name() string {
	return "EMA_Pullback_20_50"
}

func (s *EmaPullback) Init(provider core.DataProvider) error {
	log.Println("Initializing Strategy: Fetching History...")

	for _, sym := range s.symbols {
		// 1. Create State
		state := NewStrategyState(sym)
		s.states[sym] = state

		// 2. Fetch History (Last 15 days for 70-period High)
		candles, err := provider.History(sym, "60minute", 15)
		if err != nil {
			log.Printf("WARNING: Failed to fetch history for %s: %v", sym, err)
			continue
		}

		// 3. Replay Candles to Warmup
		for _, c := range candles {
			state.EmaFast.Update(c.Close)
			state.EmaSlow.Update(c.Close)
			state.VolSma.Update(c.Volume)
			state.Atr.Update(c)
			state.Adx.Update(c)

			// Maintain History for Angle Calculation
			if state.EmaFast.IsReady() {
				state.EmaHistory = append(state.EmaHistory, state.EmaFast.Value())
				if len(state.EmaHistory) > 21 {
					state.EmaHistory = state.EmaHistory[1:]
				}
			}
		}
		log.Printf("Warmed up %s with %d candles. EMA50: %s", sym, len(candles), state.EmaSlow.Value().StringFixed(2))
	}
	return nil
}

func (s *EmaPullback) OnCandle(candle models.Candle) *models.Signal {
	// 1. Get State
	state, ok := s.states[candle.Symbol]
	if !ok {
		// Should not happen if initialized correctly, but lazy init just in case
		state = NewStrategyState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	// 2. Update Indicators
	ema20 := state.EmaFast.Update(candle.Close)
	ema50 := state.EmaSlow.Update(candle.Close)
	volAvg := state.VolSma.Update(candle.Volume)
	atrVal := state.Atr.Update(candle)
	adxVal := state.Adx.Update(candle)

	// Update History
	if state.EmaFast.IsReady() {
		state.EmaHistory = append(state.EmaHistory, ema20)
		if len(state.EmaHistory) > 21 {
			state.EmaHistory = state.EmaHistory[1:]
		}
	}

	// 3. Wait for warm-up
	if ema50.IsZero() || atrVal.IsZero() {
		return nil
	}

	// 4. Logic Implementation

	// Condition A: Uptrend
	isUptrend := candle.Close.GreaterThan(ema50) && ema20.GreaterThan(ema50)

	if isUptrend {
		// Condition B: The Pullback
		touched20 := candle.Low.LessThanOrEqual(ema20)
		held20 := candle.Close.GreaterThan(ema20)

		// Condition C: Volume Confirmation (Optional/Removed for now)
		_ = candle.Volume.GreaterThan(volAvg)

		// Condition D: Trend Strength (ADX > 25)
		strongTrend := adxVal.GreaterThan(decimal.NewFromInt(25))

		// Condition E: Trend Angle > 20 degrees
		angle := state.CalculateTrendAngle(ema20)
		goodAngle := angle > 20

		if touched20 && held20 && strongTrend && goodAngle {
			// VALID SIGNAL FOUND

			// 5. Calculate Dynamic Stops
			riskBuffer := atrVal.Mul(decimal.NewFromInt(2))
			stopLoss := candle.Low.Sub(riskBuffer)

			riskPerShare := candle.Close.Sub(stopLoss)
			target := candle.Close.Add(riskPerShare.Mul(decimal.NewFromInt(2)))

			return &models.Signal{
				Symbol:      candle.Symbol,
				Type:        models.BuySignal,
				ProductType: "CNC", // Swing
				Price:       candle.Close,
				StopLoss:    stopLoss.Floor(),
				Target:      target.Floor(),
				Metadata: map[string]string{
					"Strategy":   s.Name(),
					"EMA20":      ema20.StringFixed(2),
					"ATR":        atrVal.StringFixed(2),
					"VolSMA":     volAvg.StringFixed(2),
					"Volume":     candle.Volume.StringFixed(0),
					"ADX":        adxVal.StringFixed(2),
					"TrendAngle": decimal.NewFromFloat(angle).StringFixed(2),
				},
			}
		}
	}

	return nil
}
