package strategy

import (
	"log"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// DonchianBreakoutState holds the indicators for a single symbol
type DonchianBreakoutState struct {
	Symbol       string
	EmaTrend     *indicators.EMA     // 50 Period (Trend Filter)
	EmaLongTerm  *indicators.EMA     // 200 Period (Long Term Trend Filter)
	DonchianHigh *indicators.Highest // 20 Period (Upper Channel)
	Atr          *indicators.ATR     // 14 Period (Stop Loss)
}

func NewDonchianBreakoutState(symbol string) *DonchianBreakoutState {
	return &DonchianBreakoutState{
		Symbol:       symbol,
		EmaTrend:     indicators.NewEMA(50),
		EmaLongTerm:  indicators.NewEMA(200),
		DonchianHigh: indicators.NewHighest(20),
		Atr:          indicators.NewATR(14),
	}
}

// DonchianBreakout implements a Trend Following Breakout Strategy
// Recommended Timeframe: Daily
// Holding Period: 2-3 Weeks
// Risk/Reward: 1:2
type DonchianBreakout struct {
	symbols []string
	states  map[string]*DonchianBreakoutState
}

func NewDonchianBreakout(symbols []string) *DonchianBreakout {
	return &DonchianBreakout{
		symbols: symbols,
		states:  make(map[string]*DonchianBreakoutState),
	}
}

func (s *DonchianBreakout) Name() string {
	return "Donchian_Breakout_20"
}

func (s *DonchianBreakout) Init(provider core.DataProvider) error {
	log.Println("Initializing Donchian Strategy: Fetching History...")

	for _, sym := range s.symbols {
		// 1. Create State
		state := NewDonchianBreakoutState(sym)
		s.states[sym] = state

		// 2. Fetch History (200 days for EMA200)
		// We use "day" timeframe as this is a Swing strategy
		candles, err := provider.History(sym, "day", 200)
		if err != nil {
			// Fallback to 60minute if day not available, but warn
			log.Printf("WARNING: Failed to fetch daily history for %s: %v. Trying 60minute...", sym, err)
			candles, err = provider.History(sym, "60minute", 200) // 200 hours is not enough for EMA200 but better than nothing
			if err != nil {
				log.Printf("ERROR: Failed to fetch history for %s: %v", sym, err)
				continue
			}
		}

		// 3. Replay Candles to Warmup
		for _, c := range candles {
			state.EmaTrend.Update(c.Close)
			state.EmaLongTerm.Update(c.Close)
			state.DonchianHigh.Update(c.High)
			state.Atr.Update(c)
		}
		log.Printf("Warmed up %s with %d candles. EMA200: %s", sym, len(candles), state.EmaLongTerm.Value().StringFixed(2))
	}
	return nil
}

func (s *DonchianBreakout) OnCandle(candle models.Candle) *models.Signal {
	// 1. Get State
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewDonchianBreakoutState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	// 2. Check Breakout Condition BEFORE updating Donchian with current candle
	// We want to know if the current Close broke the PREVIOUS 20-day High.
	// state.DonchianHigh.Value() currently holds the Max(High) of the previous window.

	// Ensure indicators are ready
	if !state.EmaLongTerm.IsReady() || !state.DonchianHigh.IsReady() {
		// Update and return
		state.EmaTrend.Update(candle.Close)
		state.EmaLongTerm.Update(candle.Close)
		state.DonchianHigh.Update(candle.High)
		state.Atr.Update(candle)
		return nil
	}

	prevHigh := state.DonchianHigh.Value()

	// 3. Update Indicators with current candle
	ema50 := state.EmaTrend.Update(candle.Close)
	ema200 := state.EmaLongTerm.Update(candle.Close)
	atrVal := state.Atr.Update(candle)

	// Update Donchian LAST so we don't compare against self for the breakout check.
	// We want to check if THIS candle is the breakout, we compare THIS Close to PREV High.
	state.DonchianHigh.Update(candle.High)

	// 4. Logic Implementation

	// Condition A: Long Term Uptrend (EMA 50 > EMA 200)
	isTrendUp := ema50.GreaterThan(ema200)

	// Condition B: Price above EMA 50
	priceAboveTrend := candle.Close.GreaterThan(ema50)

	// Condition C: Breakout (Close > Previous 20-day High)
	isBreakout := candle.Close.GreaterThan(prevHigh)

	if isTrendUp && priceAboveTrend && isBreakout {
		// VALID SIGNAL

		// 5. Calculate Stops (ATR Based)
		// Stop Loss = Close - 2 * ATR
		riskBuffer := atrVal.Mul(decimal.NewFromInt(2))
		stopLoss := candle.Close.Sub(riskBuffer)

		// Target = Close + 2 * Risk (1:2 RR)
		// Risk = 2 * ATR. Target Distance = 4 * ATR.
		target := candle.Close.Add(riskBuffer.Mul(decimal.NewFromInt(2)))

		return &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.BuySignal,
			ProductType: "CNC", // Delivery
			Price:       candle.Close,
			StopLoss:    stopLoss.Floor(),
			Target:      target.Floor(),
			Metadata: map[string]string{
				"Strategy":     s.Name(),
				"EMA50":        ema50.StringFixed(2),
				"EMA200":       ema200.StringFixed(2),
				"DonchianHigh": prevHigh.StringFixed(2),
				"ATR":          atrVal.StringFixed(2),
			},
		}
	}

	return nil
}
