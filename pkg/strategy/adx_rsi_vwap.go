package strategy

import (
	"log"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// AdxRsiVwapState holds the indicators for a single symbol
type AdxRsiVwapState struct {
	Symbol  string
	Adx     *indicators.ADX  // 14 Period
	Rsi     *indicators.RSI  // 14 Period
	Vwap    *indicators.VWAP // Daily VWAP
	PrevAdx decimal.Decimal
}

func NewAdxRsiVwapState(symbol string) *AdxRsiVwapState {
	return &AdxRsiVwapState{
		Symbol:  symbol,
		Adx:     indicators.NewADX(14),
		Rsi:     indicators.NewRSI(14),
		Vwap:    indicators.NewVWAP(),
		PrevAdx: decimal.Zero,
	}
}

type AdxRsiVwap struct {
	symbols []string
	states  map[string]*AdxRsiVwapState
}

func NewAdxRsiVwap(symbols []string) *AdxRsiVwap {
	return &AdxRsiVwap{
		symbols: symbols,
		states:  make(map[string]*AdxRsiVwapState),
	}
}

func (s *AdxRsiVwap) Name() string {
	return "ADX_RSI_VWAP"
}

func (s *AdxRsiVwap) Init(provider core.DataProvider) error {
	log.Println("Initializing Strategy: Fetching History...")

	for _, sym := range s.symbols {
		// 1. Create State
		state := NewAdxRsiVwapState(sym)
		s.states[sym] = state

		// 2. Fetch History (Last 5 days for warmup)
		// We need enough data for ADX(14) and RSI(14) to stabilize.
		candles, err := provider.History(sym, "5minute", 5)
		if err != nil {
			log.Printf("WARNING: Failed to fetch history for %s: %v", sym, err)
			continue
		}

		// 3. Replay Candles to Warmup
		for _, c := range candles {
			adxVal := state.Adx.Update(c)
			state.Rsi.Update(c.Close)
			state.Vwap.Update(c)

			// Store PrevAdx
			state.PrevAdx = adxVal
		}
		log.Printf("Warmed up %s with %d candles. ADX: %s", sym, len(candles), state.Adx.Value().StringFixed(2))
	}
	return nil
}

func (s *AdxRsiVwap) OnCandle(candle models.Candle) *models.Signal {
	// 1. Get State
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewAdxRsiVwapState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	// Capture Previous ADX before update
	prevAdx := state.PrevAdx

	// 2. Update Indicators
	currentAdx := state.Adx.Update(candle)
	currentRsi := state.Rsi.Update(candle.Close)
	currentVwap := state.Vwap.Update(candle)
	plusDI := state.Adx.PlusDI()
	minusDI := state.Adx.MinusDI()

	// Update State PrevAdx for next candle
	state.PrevAdx = currentAdx

	// 3. Wait for warm-up (simple check if values are non-zero)
	if currentAdx.IsZero() || currentRsi.IsZero() {
		return nil
	}

	// Debug Log
	// log.Printf("ERROR DEBUG %s: ADX=%s, RSI=%s, VWAP=%s, +DI=%s, -DI=%s",
	// 	candle.StartTime.Format("15:04"), currentAdx.StringFixed(2), currentRsi.StringFixed(2),
	// 	currentVwap.StringFixed(2), plusDI.StringFixed(2), minusDI.StringFixed(2))

	// 4. Logic Implementation

	// Time Window Check (09:30 - 15:00)
	// Assuming candle.StartTime has correct time
	h, m, _ := candle.StartTime.Clock()
	timeInMinutes := h*60 + m
	startTime := 9*60 + 30
	endTime := 15 * 60

	if timeInMinutes < startTime || timeInMinutes > endTime {
		return nil
	}

	adxThreshold := decimal.NewFromInt(25)

	// VWAP Check (Handle 0 VWAP for Index data)
	vwapLong := true
	vwapShort := true
	if !currentVwap.IsZero() {
		vwapLong = candle.Close.GreaterThan(currentVwap)
		vwapShort = candle.Close.LessThan(currentVwap)
	}

	// LONG ENTRY LOGIC
	if currentAdx.GreaterThan(adxThreshold) && currentAdx.GreaterThan(prevAdx) && // Rising Strength
		plusDI.GreaterThan(minusDI) && // Bullish Direction
		vwapLong && // Volume Support
		currentRsi.LessThan(decimal.NewFromInt(75)) { // Not Overbought

		// Entry at High + Buffer (using 0.05% buffer or fixed tick size? Let's use a small fixed buffer or just High)
		// User said: Place BUY_STOP_ORDER at (Signal_Candle_High + Buffer)
		// For backtest simplicity, we signal ENTRY at Close, but in real execution it would be a stop order.
		// However, the backtester usually executes at Close or Open of next candle.
		// Let's assume we signal entry now.

		buffer := candle.Close.Mul(decimal.NewFromFloat(0.0005)) // 0.05% buffer
		entryPrice := candle.High.Add(buffer)
		stopLoss := candle.Low

		risk := entryPrice.Sub(stopLoss)
		target := entryPrice.Add(risk.Mul(decimal.NewFromInt(2))) // 1:2 RR

		return &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.BuySignal,
			ProductType: "MIS",
			Price:       entryPrice, // Signal Price (Trigger Price)
			StopLoss:    stopLoss.Floor(),
			Target:      target.Floor(),
			Metadata: map[string]string{
				"Strategy": "ADX_RSI_VWAP",
				"ADX":      currentAdx.StringFixed(2),
				"RSI":      currentRsi.StringFixed(2),
				"VWAP":     currentVwap.StringFixed(2),
				"PlusDI":   plusDI.StringFixed(2),
				"MinusDI":  minusDI.StringFixed(2),
			},
		}
	}

	// SHORT ENTRY LOGIC
	if currentAdx.GreaterThan(adxThreshold) && currentAdx.GreaterThan(prevAdx) && // Rising Strength
		minusDI.GreaterThan(plusDI) && // Bearish Direction
		vwapShort && // Volume Support
		currentRsi.GreaterThan(decimal.NewFromInt(25)) { // Not Oversold

		buffer := candle.Close.Mul(decimal.NewFromFloat(0.0005)) // 0.05% buffer
		entryPrice := candle.Low.Sub(buffer)
		stopLoss := candle.High

		risk := stopLoss.Sub(entryPrice)
		target := entryPrice.Sub(risk.Mul(decimal.NewFromInt(2))) // 1:2 RR

		return &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.SellSignal,
			ProductType: "MIS",
			Price:       entryPrice,
			StopLoss:    stopLoss.Floor(),
			Target:      target.Floor(),
			Metadata: map[string]string{
				"Strategy": "ADX_RSI_VWAP",
				"ADX":      currentAdx.StringFixed(2),
				"RSI":      currentRsi.StringFixed(2),
				"VWAP":     currentVwap.StringFixed(2),
				"PlusDI":   plusDI.StringFixed(2),
				"MinusDI":  minusDI.StringFixed(2),
			},
		}
	}

	return nil
}
