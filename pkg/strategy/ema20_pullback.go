package strategy

import (
	"log"
	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

type EMA20PullbackState struct {
	Symbol      string
	Ema20       *indicators.EMA
	Ema50       *indicators.EMA
	Sma200      *indicators.SMA
	Atr         *indicators.ATR
	LastClose   decimal.Decimal
	candleCount int
}

func NewEMA20PullbackState(symbol string) *EMA20PullbackState {
	return &EMA20PullbackState{
		Symbol: symbol,
		Ema20:  indicators.NewEMA(20),
		Ema50:  indicators.NewEMA(50),
		Sma200: indicators.NewSMA(200),
		Atr:    indicators.NewATR(14),
	}
}

// EMA20Pullback is a daily swing strategy that enters on a pullback to the 20 EMA.
// Entry: close crosses back above 20 EMA after touching it (pullback touch), confirmed by trend filters.
// Trend filters: 50 EMA > 200 SMA (medium-term uptrend), 20 EMA > 50 EMA (short-term uptrend).
// Risk: SL = SLMultiplier×ATR(14) below entry; TP = TPMultiplier×ATR(14) above
// entry (defaults 3×/4× ATR ≈ 1:1.33 RR). The pullback reference EMA, SL, and TP
// multipliers are all configurable via [ema20_pullback] in config.toml.
type EMA20Pullback struct {
	symbols []string
	states  map[string]*EMA20PullbackState
	cfg     config.EMA20PullbackConfig
}

func NewEMA20Pullback(symbols []string, cfg config.EMA20PullbackConfig) *EMA20Pullback {
	return &EMA20Pullback{
		symbols: symbols,
		states:  make(map[string]*EMA20PullbackState),
		cfg:     cfg,
	}
}

func (s *EMA20Pullback) Name() string {
	return "EMA20_Pullback"
}

func (s *EMA20Pullback) Init(provider core.DataProvider) error {
	log.Println("Initializing EMA20 Pullback Strategy...")

	for _, sym := range s.symbols {
		state := NewEMA20PullbackState(sym)
		s.states[sym] = state

		// 200 days of daily history to warm up SMA200 and EMAs
		candles, err := provider.History(sym, "day", 250)
		if err != nil {
			log.Printf("WARNING: Failed to fetch daily history for %s: %v", sym, err)
			continue
		}

		for _, c := range candles {
			state.Ema20.Update(c.Close)
			state.Ema50.Update(c.Close)
			state.Sma200.Update(c.Close)
			state.Atr.Update(c)
			state.candleCount++
		}

		log.Printf("Warmed up %s with %d candles. EMA20=%s EMA50=%s SMA200=%s",
			sym, len(candles),
			state.Ema20.Value().StringFixed(2),
			state.Ema50.Value().StringFixed(2),
			state.Sma200.Value().StringFixed(2),
		)
	}
	return nil
}

func (s *EMA20Pullback) OnCandle(candle models.Candle) *models.Signal {
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewEMA20PullbackState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	prevClose := state.LastClose
	defer func() {
		state.LastClose = candle.Close
		state.candleCount++
	}()

	// Update all indicators with the current candle
	ema20 := state.Ema20.Update(candle.Close)
	ema50 := state.Ema50.Update(candle.Close)
	sma200 := state.Sma200.Update(candle.Close)
	atr := state.Atr.Update(candle)

	// Wait until SMA200 is fully warmed up (200 candles seen)
	if state.candleCount < 200 || atr.IsZero() || prevClose.IsZero() {
		return nil
	}

	// Trend filter 1: medium-term uptrend (50 EMA > 200 SMA)
	if !ema50.GreaterThan(sma200) {
		return nil
	}

	// Trend filter 2: short-term uptrend (20 EMA > 50 EMA)
	if !ema20.GreaterThan(ema50) {
		return nil
	}

	// Pullback entry: previous close was at or below the pullback EMA
	// (touched/crossed below), current close crosses back above it — the bounce.
	// The pullback reference EMA is configurable (20 = shallower/faster,
	// 50 = deeper/less frequent); trend filters above are unchanged.
	pullbackEMA := ema20
	if s.cfg.PullbackEMA == 50 {
		pullbackEMA = ema50
	}
	touchedEMA := prevClose.LessThanOrEqual(pullbackEMA)
	crossedAbove := candle.Close.GreaterThan(pullbackEMA)

	if !touchedEMA || !crossedAbove {
		return nil
	}

	slMultiplier := decimal.NewFromFloat(s.cfg.SLMultiplier)
	tpMultiplier := decimal.NewFromFloat(s.cfg.TPMultiplier)

	stopLoss := candle.Close.Sub(atr.Mul(slMultiplier))
	target := candle.Close.Add(atr.Mul(tpMultiplier))

	return &models.Signal{
		Symbol:      candle.Symbol,
		Type:        models.BuySignal,
		ProductType: "CNC",
		Price:       candle.Close,
		StopLoss:    stopLoss.Round(2),
		Target:      target.Round(2),
		Metadata: map[string]string{
			"Strategy":  s.Name(),
			"EMA20":     ema20.StringFixed(2),
			"EMA50":     ema50.StringFixed(2),
			"SMA200":    sma200.StringFixed(2),
			"ATR":       atr.StringFixed(2),
			"PrevClose": prevClose.StringFixed(2),
		},
	}
}
