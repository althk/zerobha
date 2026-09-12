package strategy

import (
	"strings"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/db"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// Exit reasons are constant strings so a -trades-csv dump groups by them; the
// EMA values that used to be embedded belong in the log line, not the reason.
const (
	emaExitBearishCross = "EMA bearish cross"
	emaExitBullishCross = "EMA bullish cross"
)

// EMACross is a trend-following crossover system between a fast EMA and a
// slow EMA.
//
// Defaults (see config.DefaultEMACrossConfig, measured values):
//   - Fast EMA 9, slow EMA 21, ATR(5), 5-minute bars, entries 10:00-15:00
//   - ProductType MIS (intraday) or CNC (overnight)
//   - AssetType "stocks" (trade the signal instrument) or "options" (index
//     signal expressed through a weekly option, live only — see option_leg.go)
//   - SL 3 ATR, 3 ATR chandelier trail, no target, no opposite-cross exit:
//     the position runs on the trail and the next cross enters once flat
//   - Optional: exit_on_opposite_cross, min_sep_atr, adx_threshold,
//     max_entries_per_symbol — all measured, all off by default
//   - CNC stocks are long-only: the cash segment cannot carry a short overnight
type EMACross struct {
	cfg   config.EMACrossConfig
	state map[string]*emaCrossState
	// optionExec, when set, makes the strategy trade a weekly option on the
	// signal instrument instead of the instrument itself. nil is the backtest
	// path and what every recorded index-leg result measures.
	optionExec OptionExecutor
	declines   declineRecorder
}

type emaCrossState struct {
	fastEMA *indicators.EMA
	slowEMA *indicators.EMA
	atr     *indicators.ATR
	adx     *indicators.ADX // nil unless an ADX threshold is configured

	prevFastEMA decimal.Decimal
	prevSlowEMA decimal.Decimal
	hasPrev     bool

	lastCandleTime time.Time
	lastDate       string
	entriesToday   int

	openSide  models.SignalType
	openValid bool
	// leg is the option position the current signal is expressed through,
	// nil when trading the signal instrument directly.
	leg *optionLeg
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

// SetOptionExecution switches the strategy from trading the signal instrument
// to trading a weekly option on it. Passing nil (the default, and what
// cmd/backtest does) leaves it trading the index symbol directly.
func (s *EMACross) SetOptionExecution(exec OptionExecutor) {
	s.optionExec = exec
}

// SetDB lets the strategy record the entries its option layer declined, so
// the dashboard's signal funnel shows them. paper scopes the rows.
func (s *EMACross) SetDB(store *db.Store, paper bool) {
	s.declines = declineRecorder{store: store, paper: paper}
}

// OpenLegs implements core.LegReporter.
func (s *EMACross) OpenLegs() []core.OpenLeg {
	var out []core.OpenLeg
	for symbol, st := range s.state {
		if st.leg != nil {
			out = append(out, st.leg.report(symbol))
		}
	}
	return out
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

	st := &emaCrossState{
		fastEMA: indicators.NewEMA(fastPeriod),
		slowEMA: indicators.NewEMA(slowPeriod),
		atr:     indicators.NewATR(atrPeriod),
	}
	if s.cfg.ADXThreshold > 0 {
		adxPeriod := s.cfg.ADXPeriod
		if adxPeriod <= 0 {
			adxPeriod = 14
		}
		st.adx = indicators.NewADX(adxPeriod)
	}
	return st
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

func (s *EMACross) productType() string {
	pt := strings.ToUpper(strings.TrimSpace(s.cfg.ProductType))
	if pt != "CNC" {
		pt = "MIS"
	}
	return pt
}

// updateCandle updates indicators at most once per candle timestamp. Both
// ExitAdvice and OnCandle call it for the same bar; the second call is a no-op.
func (s *EMACross) updateCandle(st *emaCrossState, candle models.Candle) {
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
	if st.adx != nil {
		st.adx.Update(candle)
	}
	st.lastCandleTime = candle.StartTime

	dateStr := candle.StartTime.In(istLocation).Format("2006-01-02")
	if dateStr != st.lastDate {
		st.lastDate = dateStr
		st.entriesToday = 0
		// An MIS position cannot survive the session: the square-off flattens
		// it, and an option leg with it. Believing otherwise would suppress
		// the next day's first entry on the same side and, worse, "close" a
		// contract that no longer exists.
		if s.productType() == "MIS" {
			st.openValid = false
			st.leg = nil
		}
	}
}

// ExitAdvice implements core.ExitAdvisor.
//
// With option execution on, the index-side stop comes first: no resting order
// on the option can express a level on the index, so the strategy tracks it
// itself and closes the contract when the index closes through it.
//
// Then the crossover exit: a long is closed once the fast EMA is below the slow
// (bearish cross), a short once it is above (bullish cross). The instrument
// closed is the contract when a leg is open, the signal instrument otherwise.
func (s *EMACross) ExitAdvice(candle models.Candle) *core.ExitAdvice {
	st := s.stateFor(candle.Symbol)
	s.updateCandle(st, candle)

	if leg := st.leg; leg != nil {
		if hit, reason := leg.stopHit(candle); hit {
			st.leg = nil
			st.openValid = false
			return leg.closeAdvice("EMACross index stop: " + reason)
		}
	}

	if s.cfg.ExitOnOppositeCross != nil && !*s.cfg.ExitOnOppositeCross {
		return nil
	}
	if !st.openValid || !st.hasPrev || !st.fastEMA.IsReady() || !st.slowEMA.IsReady() {
		return nil
	}

	currFast := st.fastEMA.Value()
	currSlow := st.slowEMA.Value()

	var reason string
	switch st.openSide {
	case models.BuySignal:
		if currFast.LessThan(currSlow) {
			reason = emaExitBearishCross
		}
	case models.SellSignal:
		if currFast.GreaterThan(currSlow) {
			reason = emaExitBullishCross
		}
	}
	if reason == "" {
		return nil
	}

	st.openValid = false
	if leg := st.leg; leg != nil {
		st.leg = nil
		return leg.closeAdvice(reason)
	}
	return &core.ExitAdvice{
		Symbol:  candle.Symbol,
		ForSide: st.openSide,
		Reason:  reason,
	}
}

// OnCandle evaluates new entry signals on candle close.
func (s *EMACross) OnCandle(candle models.Candle) *models.Signal {
	st := s.stateFor(candle.Symbol)
	s.updateCandle(st, candle)

	if !st.hasPrev || !st.fastEMA.IsReady() || !st.slowEMA.IsReady() || !st.atr.IsReady() {
		return nil
	}

	// Time window check
	h, m, _ := candle.StartTime.In(istLocation).Clock()
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
	if !isGoldenCross && !isDeathCross {
		return nil
	}

	atrVal := st.atr.Value()
	if !atrVal.IsPositive() {
		return nil
	}

	// Entry filters. A cross with the EMAs still on top of each other is the
	// signature of chop; a separation floor demands the cross came with
	// momentum. ADX rejects low-energy sideways sessions outright.
	if s.cfg.MinSepATR > 0 {
		sep := currFast.Sub(currSlow).Abs()
		if sep.LessThan(atrVal.Mul(decimal.NewFromFloat(s.cfg.MinSepATR))) {
			return nil
		}
	}
	if s.cfg.ADXThreshold > 0 && st.adx != nil {
		if st.adx.Value().LessThan(decimal.NewFromFloat(s.cfg.ADXThreshold)) {
			return nil
		}
	}
	if s.cfg.MaxEntriesPerSymbol > 0 && st.entriesToday >= s.cfg.MaxEntriesPerSymbol {
		return nil
	}

	productType := s.productType()

	// Determine if shorting is allowed
	allowShort := productType == "MIS"
	if s.cfg.AllowShort != nil {
		allowShort = *s.cfg.AllowShort
	}
	// CNC stocks are strictly long-only: the cash segment does not carry a
	// short overnight. A CNC option is a bought put, which is fine.
	if productType == "CNC" && strings.ToLower(s.cfg.AssetType) != "options" {
		allowShort = false
	}

	side := models.BuySignal
	if isDeathCross {
		if !allowShort {
			return nil
		}
		side = models.SellSignal
	}
	// A cross always alternates with the previous one, so this only guards a
	// same-side re-entry while a position from that side is still believed
	// open (e.g. a CNC long carried across sessions).
	if st.openValid && st.openSide == side {
		return nil
	}
	// No entry while an option leg is open. Trading the index directly, the
	// engine refuses a second position in the same symbol; a call and a put
	// are different symbols, so nothing downstream would stop the strategy
	// buying a put on top of an open call and losing the call's index stop.
	if st.leg != nil {
		return nil
	}

	signal := s.buildSignal(candle, side, atrVal, productType, currFast, currSlow)

	// With option execution on, the index signal is a view, not an order: it
	// is translated into the weekly contract that expresses it, and the stop
	// moves from the broker to this strategy. The translation can decline —
	// too close to expiry, no strike listed, no premium — and a declined
	// entry must not leave the strategy believing it holds a position.
	if s.optionExec != nil {
		optionSignal, leg := buildOptionLeg(s.optionExec, "EMACross", candle, signal, atrVal, s.declines)
		if optionSignal == nil {
			return nil
		}
		st.leg = leg
		st.openSide = side
		st.openValid = true
		st.entriesToday++
		return optionSignal
	}

	st.openSide = side
	st.openValid = true
	st.entriesToday++
	return signal
}

func (s *EMACross) buildSignal(candle models.Candle, side models.SignalType, atrVal decimal.Decimal,
	productType string, currFast, currSlow decimal.Decimal) *models.Signal {

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

	reason := "GoldenCross"
	stop := candle.Close.Sub(slDist)
	target := candle.Close.Add(tpDist)
	if side == models.SellSignal {
		reason = "DeathCross"
		stop = candle.Close.Add(slDist)
		target = candle.Close.Sub(tpDist)
	}

	sig := &models.Signal{
		Symbol:      candle.Symbol,
		Type:        side,
		Price:       candle.Close,
		StopLoss:    stop,
		ProductType: productType,
		Metadata: map[string]string{
			"Strategy": config.StrategyEMACross,
			"Reason":   reason,
			"FastEMA":  currFast.StringFixed(2),
			"SlowEMA":  currSlow.StringFixed(2),
		},
	}
	if hasTP {
		sig.Target = target
	}
	if trail := s.cfg.TrailMult(); trail > 0 {
		sig.TrailDistance = atrVal.Mul(decimal.NewFromFloat(trail))
	}
	if s.cfg.RiskPct > 0 {
		sig.RiskPct = decimal.NewFromFloat(s.cfg.RiskPct / 100.0)
	}
	return sig
}
