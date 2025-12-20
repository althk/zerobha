package strategy

import (
	"log"
	"math"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// EmaTrendRsiState holds the indicators for a single symbol
type EmaTrendRsiState struct {
	Symbol     string
	Ema21      *indicators.EMA // 21 Period
	Rsi14      *indicators.RSI // 14 Period
	Atr        *indicators.ATR // 14 Period
	EmaHistory []decimal.Decimal
	PrevRsi    decimal.Decimal
}

func NewEmaTrendRsiState(symbol string) *EmaTrendRsiState {
	return &EmaTrendRsiState{
		Symbol:     symbol,
		Ema21:      indicators.NewEMA(21),
		Rsi14:      indicators.NewRSI(14),
		Atr:        indicators.NewATR(14),
		EmaHistory: make([]decimal.Decimal, 0, 25),
		PrevRsi:    decimal.Zero,
	}
}

func (s *EmaTrendRsiState) CalculateTrendAngle(currentEma decimal.Decimal) float64 {
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

type EmaTrendRsi struct {
	symbols []string
	states  map[string]*EmaTrendRsiState
}

func NewEmaTrendRsi(symbols []string) *EmaTrendRsi {
	return &EmaTrendRsi{
		symbols: symbols,
		states:  make(map[string]*EmaTrendRsiState),
	}
}

func (s *EmaTrendRsi) Name() string {
	return "EMA_Trend_RSI"
}

func (s *EmaTrendRsi) Init(provider core.DataProvider) error {
	log.Println("Initializing Strategy: Fetching History...")

	for _, sym := range s.symbols {
		// 1. Create State
		state := NewEmaTrendRsiState(sym)
		s.states[sym] = state

		// 2. Fetch History (Last 15 days for warmup)
		candles, err := provider.History(sym, "60minute", 15)
		if err != nil {
			log.Printf("WARNING: Failed to fetch history for %s: %v", sym, err)
			continue
		}

		// 3. Replay Candles to Warmup
		for _, c := range candles {
			state.Ema21.Update(c.Close)
			rsiVal := state.Rsi14.Update(c.Close)
			state.Atr.Update(c)

			// Store PrevRsi
			state.PrevRsi = rsiVal

			// Maintain History for Angle Calculation
			if state.Ema21.IsReady() {
				state.EmaHistory = append(state.EmaHistory, state.Ema21.Value())
				if len(state.EmaHistory) > 21 {
					state.EmaHistory = state.EmaHistory[1:]
				}
			}
		}
		log.Printf("Warmed up %s with %d candles. EMA21: %s", sym, len(candles), state.Ema21.Value().StringFixed(2))
	}
	return nil
}

func (s *EmaTrendRsi) OnCandle(candle models.Candle) *models.Signal {
	// 1. Get State
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewEmaTrendRsiState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	// Capture Previous RSI before update
	prevRsi := state.PrevRsi

	// 2. Update Indicators
	ema21 := state.Ema21.Update(candle.Close)
	rsiVal := state.Rsi14.Update(candle.Close)
	atrVal := state.Atr.Update(candle)

	// Update State PrevRsi for next candle
	state.PrevRsi = rsiVal

	// Update History
	if state.Ema21.IsReady() {
		state.EmaHistory = append(state.EmaHistory, ema21)
		if len(state.EmaHistory) > 21 {
			state.EmaHistory = state.EmaHistory[1:]
		}
	}

	// 3. Wait for warm-up
	if !state.Ema21.IsReady() || atrVal.IsZero() || rsiVal.IsZero() {
		return nil
	}

	// 4. Logic Implementation

	// Condition A: Trend Angle > 35 degrees
	angle := state.CalculateTrendAngle(ema21)
	goodAngle := angle > 35

	// Condition B: RSI Crosses Above 50
	// We need to check if it WAS <= 50 and NOW > 50
	rsiCross := prevRsi.LessThanOrEqual(decimal.NewFromInt(50)) && rsiVal.GreaterThan(decimal.NewFromInt(50))

	if goodAngle && rsiCross {
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
				"EMA21":      ema21.StringFixed(2),
				"ATR":        atrVal.StringFixed(2),
				"RSI":        rsiVal.StringFixed(2),
				"PrevRSI":    prevRsi.StringFixed(2),
				"TrendAngle": decimal.NewFromFloat(angle).StringFixed(2),
			},
		}
	}

	return nil
}
