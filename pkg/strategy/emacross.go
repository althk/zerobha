package strategy

import (
	"fmt"
	"strings"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// EMACross is a trend-following crossover system between a Fast EMA and a
// Slow EMA.
//
// Defaults:
//   - Fast EMA: 9
//   - Slow EMA: 21
//   - Timeframe: 1m
//   - ProductType: MIS (intraday) or CNC (overnight)
//   - AssetType: stocks or options (NIFTY weekly options via index signals)
//   - SL: 2x ATR
//   - TP: 5x ATR
//   - Exit: Opposite EMA cross OR SL/TP, whichever happens first.
//   - For CNC stocks: Long-only (short selling is suppressed).
type EMACross struct {
	cfg   config.EMACrossConfig
	state map[string]*emaCrossState
}

type emaCrossState struct {
	fastEMA *indicators.EMA
	slowEMA *indicators.EMA
	atr     *indicators.ATR

	prevFastEMA decimal.Decimal
	prevSlowEMA decimal.Decimal
	hasPrev     bool

	lastCandleTime time.Time
	lastDate       string

	openSide  models.SignalType
	openValid bool
}

// NewEMACrossStrategy constructs the EMA Cross strategy for the given symbols.
func NewEMACrossStrategy(symbols []string, cfg config.EMACrossConfig) *EMACross {
	s := &EMACross{
		cfg:   cfg,
		state: make(map[string]*emaCrossState, len(symbols)),
	}
	for _, sym := range symbols {
		s.state[sym] = s.newState()
	}
	return s
}

func (s *EMACross) newState() *emaCrossState {
	fastPeriod := s.cfg.FastPeriod
	if fastPeriod <= 0 {
		fastPeriod = 9
	}
	slowPeriod := s.cfg.SlowPeriod
	if slowPeriod <= 0 {
		slowPeriod = 21
	}
	atrPeriod := s.cfg.ATRPeriod
	if atrPeriod <= 0 {
		atrPeriod = 5
	}

	return &emaCrossState{
		fastEMA: indicators.NewEMA(fastPeriod),
		slowEMA: indicators.NewEMA(slowPeriod),
		atr:     indicators.NewATR(atrPeriod),
	}
}

func (s *EMACross) stateFor(symbol string) *emaCrossState {
	st, ok := s.state[symbol]
	if !ok {
		st = s.newState()
		s.state[symbol] = st
	}
	return st
}

func (s *EMACross) Name() string {
	return config.StrategyEMACross
}

func (s *EMACross) Init(provider core.DataProvider) error {
	return nil
}

// updateCandle updates indicators at most once per candle timestamp.
func (st *emaCrossState) updateCandle(candle models.Candle) {
	if !st.lastCandleTime.IsZero() && !candle.StartTime.After(st.lastCandleTime) {
		return
	}

	// Capture previous EMA values if ready
	if st.fastEMA.IsReady() && st.slowEMA.IsReady() {
		st.prevFastEMA = st.fastEMA.Value()
		st.prevSlowEMA = st.slowEMA.Value()
		st.hasPrev = true
	}

	st.fastEMA.Update(candle.Close)
	st.slowEMA.Update(candle.Close)
	st.atr.Update(candle)
	st.lastCandleTime = candle.StartTime

	dateStr := candle.StartTime.Format("2006-01-02")
	if dateStr != st.lastDate {
		st.lastDate = dateStr
	}
}

// ExitAdvice implements core.ExitAdvisor:
// For Long: if Slow EMA crosses above Fast EMA (Bearish cross).
// For Short: if Fast EMA crosses above Slow EMA (Bullish cross).
func (s *EMACross) ExitAdvice(candle models.Candle) *core.ExitAdvice {
	st := s.stateFor(candle.Symbol)
	st.updateCandle(candle)

	if !st.openValid || !st.hasPrev || !st.fastEMA.IsReady() || !st.slowEMA.IsReady() {
		return nil
	}

	currFast := st.fastEMA.Value()
	currSlow := st.slowEMA.Value()

	var reason string
	switch st.openSide {
	case models.BuySignal:
		// Exit Long if Fast EMA is now below Slow EMA
		if currFast.LessThan(currSlow) {
			reason = fmt.Sprintf("EMA bearish cross: fast EMA (%s) below slow EMA (%s)",
				currFast.StringFixed(2), currSlow.StringFixed(2))
		}
	case models.SellSignal:
		// Exit Short if Fast EMA is now above Slow EMA
		if currFast.GreaterThan(currSlow) {
			reason = fmt.Sprintf("EMA bullish cross: fast EMA (%s) above slow EMA (%s)",
				currFast.StringFixed(2), currSlow.StringFixed(2))
		}
	}

	if reason == "" {
		return nil
	}

	side := st.openSide
	st.openValid = false

	return &core.ExitAdvice{
		Symbol:  candle.Symbol,
		ForSide: side,
		Reason:  reason,
	}
}

// OnCandle evaluates new entry signals on candle close.
func (s *EMACross) OnCandle(candle models.Candle) *models.Signal {
	st := s.stateFor(candle.Symbol)
	st.updateCandle(candle)

	if !st.hasPrev || !st.fastEMA.IsReady() || !st.slowEMA.IsReady() || !st.atr.IsReady() {
		return nil
	}

	// Time window check
	h, m, _ := candle.StartTime.Clock()
	timeMin := h*60 + m
	if s.cfg.EntryStartMin > 0 && timeMin < s.cfg.EntryStartMin {
		return nil
	}
	if s.cfg.EntryCutoffMin > 0 && timeMin >= s.cfg.EntryCutoffMin {
		return nil
	}

	prevFast := st.prevFastEMA
	prevSlow := st.prevSlowEMA
	currFast := st.fastEMA.Value()
	currSlow := st.slowEMA.Value()

	isGoldenCross := prevFast.LessThanOrEqual(prevSlow) && currFast.GreaterThan(currSlow)
	isDeathCross := prevFast.GreaterThanOrEqual(prevSlow) && currFast.LessThan(currSlow)

	atrVal := st.atr.Value()
	if !atrVal.IsPositive() {
		return nil
	}

	slMult := s.cfg.SLATRMult
	if slMult <= 0 {
		slMult = 2.0
	}
	slDist := atrVal.Mul(decimal.NewFromFloat(slMult))

	var tpDist decimal.Decimal
	hasTP := s.cfg.TPATRMult > 0
	if hasTP {
		tpDist = atrVal.Mul(decimal.NewFromFloat(s.cfg.TPATRMult))
	}

	productType := strings.ToUpper(strings.TrimSpace(s.cfg.ProductType))
	if productType != "CNC" {
		productType = "MIS"
	}

	// Determine if shorting is allowed
	allowShort := productType == "MIS"
	if s.cfg.AllowShort != nil {
		allowShort = *s.cfg.AllowShort
	}
	// Per user rule: for stocks in CNC mode, strictly long-only
	if productType == "CNC" && strings.ToLower(s.cfg.AssetType) != "options" {
		allowShort = false
	}

	if isGoldenCross {
		if st.openValid && st.openSide == models.BuySignal {
			return nil // already long
		}

		st.openSide = models.BuySignal
		st.openValid = true

		sig := &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.BuySignal,
			Price:       candle.Close,
			StopLoss:    candle.Close.Sub(slDist),
			ProductType: productType,
			Metadata: map[string]string{
				"Strategy": config.StrategyEMACross,
				"Reason":   "GoldenCross",
				"FastEMA":  currFast.StringFixed(2),
				"SlowEMA":  currSlow.StringFixed(2),
			},
		}
		if hasTP {
			sig.Target = candle.Close.Add(tpDist)
		}
		if s.cfg.RiskPct > 0 {
			sig.RiskPct = decimal.NewFromFloat(s.cfg.RiskPct / 100.0)
		}
		return sig
	}

	if isDeathCross && allowShort {
		if st.openValid && st.openSide == models.SellSignal {
			return nil // already short
		}

		st.openSide = models.SellSignal
		st.openValid = true

		sig := &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.SellSignal,
			Price:       candle.Close,
			StopLoss:    candle.Close.Add(slDist),
			ProductType: productType,
			Metadata: map[string]string{
				"Strategy": config.StrategyEMACross,
				"Reason":   "DeathCross",
				"FastEMA":  currFast.StringFixed(2),
				"SlowEMA":  currSlow.StringFixed(2),
			},
		}
		if hasTP {
			sig.Target = candle.Close.Sub(tpDist)
		}
		if s.cfg.RiskPct > 0 {
			sig.RiskPct = decimal.NewFromFloat(s.cfg.RiskPct / 100.0)
		}
		return sig
	}

	return nil
}
