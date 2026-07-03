package strategy

import (
	"log"
	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// The setup requires a pullback of 3-8 sessions, so we keep at least this much
// recent committed daily history (high/low/close/volume) to evaluate swing points.
const (
	pullbackMinSessions = 3
	pullbackMaxSessions = 8

	// dateLayout is the day-bucket key derived from a candle's StartTime; a change
	// in this value marks a new trading session (day rollover).
	dateLayout = "2006-01-02"
)

// dayBar is a compact snapshot of a completed daily candle, retained in a small
// rolling window so the strategy can reason about the recent swing high, the
// pullback swing low, and prior-day levels.
type dayBar struct {
	High   decimal.Decimal
	Low    decimal.Decimal
	Close  decimal.Decimal
	Volume decimal.Decimal
}

type EMA20PullbackState struct {
	Symbol string

	// --- Committed daily indicators (advanced once per completed day) ---------
	Ema20     *indicators.EMA
	Ema50     *indicators.EMA
	Sma200    *indicators.SMA
	Atr       *indicators.ATR
	VolSma    *indicators.SMA // 20-day average volume
	ema20Prev decimal.Decimal // committed EMA20 one session ago (to confirm it is rising)

	// recent retains the last (pullbackMaxSessions+2) *completed* daily bars,
	// oldest first (today is never included here — it lives in the forming bar).
	recent      []dayBar
	candleCount int // number of committed days seen

	// --- Forming (today's, in-progress) day, built up from intraday candles ----
	curDate   string // date key of the day currently forming
	formHigh  decimal.Decimal
	formLow   decimal.Decimal
	formClose decimal.Decimal
	formVol   decimal.Decimal // cumulative volume so far today
	formSeen  bool            // any intraday candle accumulated into the forming day

	signaledDate string // date key on which we already emitted a signal (one trade/day)
}

func NewEMA20PullbackState(symbol string) *EMA20PullbackState {
	return &EMA20PullbackState{
		Symbol: symbol,
		Ema20:  indicators.NewEMA(20),
		Ema50:  indicators.NewEMA(50),
		Sma200: indicators.NewSMA(200),
		Atr:    indicators.NewATR(14),
		VolSma: indicators.NewSMA(20),
	}
}

// pushBar appends a completed daily bar to the rolling window, trimming it to
// the small fixed capacity we need for swing analysis.
func (st *EMA20PullbackState) pushBar(b dayBar) {
	st.recent = append(st.recent, b)
	if len(st.recent) > pullbackMaxSessions+2 {
		st.recent = st.recent[len(st.recent)-(pullbackMaxSessions+2):]
	}
}

// commitDay folds the just-finished forming day into the committed daily
// indicators and the rolling window. Called when a new day rolls over (or a day
// is finalized during warmup).
func (st *EMA20PullbackState) commitDay(b dayBar) {
	st.ema20Prev = st.Ema20.Value()
	st.Ema20.Update(b.Close)
	st.Ema50.Update(b.Close)
	st.Sma200.Update(b.Close)
	st.Atr.Update(models.Candle{High: b.High, Low: b.Low, Close: b.Close})
	st.VolSma.Update(b.Volume)
	st.pushBar(b)
	st.candleCount++
}

// EMA20Pullback is a daily swing strategy that buys a controlled pullback to a
// rising 20 EMA inside an established uptrend.
//
// Data model: the strategy reasons on the DAILY timeframe but is driven by
// intraday (e.g. 5m) candles so it can enter early, the moment an intraday bar
// confirms the breakout — rather than waiting for the 3:30pm daily close. The
// daily indicators (EMA20/50, SMA200, ATR, 20-day volume avg) are committed once
// per *completed* day; today's forming bar (running high/low/close, cumulative
// volume) is used only to evaluate the trigger and is never folded into the
// indicators until the day completes.
//
// Setup (evaluated against committed daily bars):
//   - Pullback of 3-8 sessions to a rising 20 EMA.
//   - Pullback volume below the 20-day average volume (no distribution).
//   - Pullback depth <= 1.5x ATR(14) from the swing high; no close below the 50 EMA.
//
// Trigger (intraday, against the forming day):
//   - Today's price trades above the prior day's high, with today's cumulative
//     volume >= 1.3x the 20-day average.
//   - Skip if the entry is more than 0.5x ATR extended above the 20 EMA (chasing).
//
// Stop:
//   - Initial SL = min(pullback swing low - 0.25x ATR, entry - 2x ATR), but never
//     tighter than 1x ATR below entry. A hard SL order is always carried.
//
// Exit:
//   - Scale out 50% at +2R; the runner is managed by the execution layer (the
//     current broker takes a full exit at +2R — see ScaleOut metadata).
//
// Trend filters: 50 EMA > 200 SMA (medium-term uptrend), 20 EMA > 50 EMA
// (short-term uptrend). The SL/TP ATR multipliers remain configurable via
// [ema20_pullback] in config.toml, but the new stop construction above takes
// precedence over the legacy single-multiplier stop.
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

		// 200+ days of daily history to warm up SMA200 and EMAs. Warmup uses
		// completed daily bars directly — each is committed immediately.
		candles, err := provider.History(sym, "day", 250)
		if err != nil {
			log.Printf("WARNING: Failed to fetch daily history for %s: %v", sym, err)
			continue
		}

		for _, c := range candles {
			state.commitDay(dayBar{High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume})
		}
		if len(candles) > 0 {
			state.curDate = candles[len(candles)-1].StartTime.Format(dateLayout)
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

// OnCandle is driven by intraday candles. It commits the prior day on rollover,
// accumulates the current intraday candle into the forming day, and evaluates the
// setup/trigger against committed daily indicators plus the forming day.
func (s *EMA20Pullback) OnCandle(candle models.Candle) *models.Signal {
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewEMA20PullbackState(candle.Symbol)
		s.states[candle.Symbol] = state
	}

	date := candle.StartTime.Format(dateLayout)

	// Day rollover: a new date means the previous forming day is complete. Commit
	// it into the daily indicators/window before starting the new day.
	if date != state.curDate {
		if state.formSeen {
			state.commitDay(dayBar{
				High:   state.formHigh,
				Low:    state.formLow,
				Close:  state.formClose,
				Volume: state.formVol,
			})
		}
		// Start a fresh forming day.
		state.curDate = date
		state.formHigh = candle.High
		state.formLow = candle.Low
		state.formClose = candle.Close
		state.formVol = candle.Volume
		state.formSeen = true
	} else {
		// Same day: extend the forming bar with this intraday candle.
		if !state.formSeen {
			state.formHigh = candle.High
			state.formLow = candle.Low
			state.formVol = candle.Volume
			state.formSeen = true
		} else {
			if candle.High.GreaterThan(state.formHigh) {
				state.formHigh = candle.High
			}
			if candle.Low.LessThan(state.formLow) {
				state.formLow = candle.Low
			}
			state.formVol = state.formVol.Add(candle.Volume)
		}
		state.formClose = candle.Close
	}

	// At most one entry per day.
	if state.signaledDate == state.curDate {
		return nil
	}

	// Committed indicator values (as of the last *completed* day — today is NOT
	// folded in).
	ema20 := state.Ema20.Value()
	ema50 := state.Ema50.Value()
	sma200 := state.Sma200.Value()
	atr := state.Atr.Value()
	volAvg := state.VolSma.Value()

	// Prior day = last committed bar.
	if len(state.recent) == 0 {
		return nil
	}
	prevBar := state.recent[len(state.recent)-1]

	// Wait until SMA200 is fully warmed up (200 committed days) and supporting
	// series are ready.
	if state.candleCount < 200 || atr.IsZero() || volAvg.IsZero() || prevBar.High.IsZero() {
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

	// Setup filter: the 20 EMA must be rising (pullback to a *rising* 20 EMA).
	if !state.ema20Prev.IsZero() && !ema20.GreaterThan(state.ema20Prev) {
		return nil
	}

	// Locate the pullback: a 3-8 session decline from a recent swing high into the
	// 20 EMA. The swing high is found across committed bars; today's forming day
	// is the breakout/trigger session.
	swingHigh, swingLow, sessions, ok := state.pullbackSwing()
	if !ok {
		return nil
	}
	if sessions < pullbackMinSessions || sessions > pullbackMaxSessions {
		return nil
	}

	// Setup filter: pullback depth must be <= 1.5x ATR from the swing high.
	maxDepth := atr.Mul(decimal.NewFromFloat(1.5))
	if swingHigh.Sub(swingLow).GreaterThan(maxDepth) {
		return nil
	}

	// Setup filter: no committed close below the 50 EMA anywhere in the pullback,
	// and today's price must also hold above it.
	if state.formClose.LessThan(ema50) {
		return nil
	}
	for _, b := range state.pullbackBars(sessions) {
		if b.Close.LessThan(ema50) {
			return nil
		}
	}

	// Setup filter: pullback volume should be quiet (below the 20-day average)
	// across the committed pullback window — no distribution while pulling back.
	for _, b := range state.pullbackBars(sessions) {
		if b.Volume.GreaterThan(volAvg) {
			return nil
		}
	}

	// Trigger: today trades above the prior day's high (use the forming high so we
	// fire the moment price prints through it intraday).
	if !state.formHigh.GreaterThan(prevBar.High) {
		return nil
	}

	// Trigger: breakout volume confirmation — today's cumulative volume >= 1.3x the
	// 20-day average. Naturally satisfied mid/late session on genuine breakouts.
	volThreshold := volAvg.Mul(decimal.NewFromFloat(1.3))
	if state.formVol.LessThan(volThreshold) {
		return nil
	}

	// Entry is the current intraday price.
	entry := state.formClose

	// Trigger filter: skip chasing — entry must not be more than 0.5x ATR extended
	// above the 20 EMA.
	maxExtension := atr.Mul(decimal.NewFromFloat(0.5))
	if entry.Sub(ema20).GreaterThan(maxExtension) {
		return nil
	}

	// --- Risk construction -------------------------------------------------
	// Initial SL = min(swingLow - 0.25xATR, entry - 2xATR), never tighter than
	// 1x ATR below entry.
	swingLowStop := swingLow.Sub(atr.Mul(decimal.NewFromFloat(0.25)))
	atrStop := entry.Sub(atr.Mul(decimal.NewFromFloat(2)))
	stopLoss := decimal.Min(swingLowStop, atrStop)

	// Never tighter than 1x ATR below entry (clamp stops that sit too close).
	minStop := entry.Sub(atr)
	if stopLoss.GreaterThan(minStop) {
		stopLoss = minStop
	}

	// R is the per-share risk; the scale-out and target are derived from it.
	risk := entry.Sub(stopLoss)
	if risk.LessThanOrEqual(decimal.Zero) {
		return nil
	}

	// Scale out 50% at +2R. The execution layer manages the partial; we expose the
	// level via metadata and set Target to the +2R price so the existing
	// single-exit broker still takes profit there.
	scaleOutPrice := entry.Add(risk.Mul(decimal.NewFromInt(2)))
	target := scaleOutPrice

	// Mark today as signaled so we don't re-fire on later intraday candles.
	state.signaledDate = state.curDate

	return &models.Signal{
		Symbol:      candle.Symbol,
		Type:        models.BuySignal,
		ProductType: "CNC",
		Price:       entry,
		StopLoss:    stopLoss.Round(2),
		Target:      target.Round(2),
		Metadata: map[string]string{
			"Strategy":         s.Name(),
			"EMA20":            ema20.StringFixed(2),
			"EMA50":            ema50.StringFixed(2),
			"SMA200":           sma200.StringFixed(2),
			"ATR":              atr.StringFixed(2),
			"VolAvg20":         volAvg.StringFixed(0),
			"VolToday":         state.formVol.StringFixed(0),
			"PrevHigh":         prevBar.High.StringFixed(2),
			"SwingHigh":        swingHigh.StringFixed(2),
			"SwingLow":         swingLow.StringFixed(2),
			"PullbackSessions": decimal.NewFromInt(int64(sessions)).String(),
			"RiskPerShare":     risk.StringFixed(2),
			// Scale-out plan: sell 50% at +2R, let the remainder run.
			"ScaleOut":      "true",
			"ScaleOutR":     "2",
			"ScaleOutQtyPc": "50",
			"ScaleOutPrice": scaleOutPrice.Round(2).String(),
		},
	}
}

// pullbackSwing scans the committed daily window (completed days only, excluding
// today) and returns the swing high, the pullback swing low reached since that
// high, and the number of sessions since the high (counting today's forming day
// as the trigger session). ok is false when there is not enough history.
func (st *EMA20PullbackState) pullbackSwing() (swingHigh, swingLow decimal.Decimal, sessions int, ok bool) {
	n := len(st.recent)
	if n == 0 {
		return decimal.Zero, decimal.Zero, 0, false
	}

	// Find the highest high in the committed window; treat it as the swing high the
	// pullback descended from.
	hiIdx := 0
	swingHigh = st.recent[0].High
	for i := 1; i < n; i++ {
		if st.recent[i].High.GreaterThan(swingHigh) {
			swingHigh = st.recent[i].High
			hiIdx = i
		}
	}

	// Sessions elapsed since the swing high: committed bars after it, plus today
	// (the forming/trigger session).
	sessions = (n - 1 - hiIdx) + 1

	// Swing low across the pullback (from the swing-high bar to the last committed
	// bar). Today's forming low is intentionally excluded — the stop references the
	// completed pullback structure.
	swingLow = st.recent[hiIdx].Low
	for i := hiIdx; i < n; i++ {
		if st.recent[i].Low.LessThan(swingLow) {
			swingLow = st.recent[i].Low
		}
	}

	return swingHigh, swingLow, sessions, true
}

// pullbackBars returns the committed bars that make up the pullback (the most
// recent `sessions-1` completed bars; today's forming day is the trigger and is
// handled separately by the caller).
func (st *EMA20PullbackState) pullbackBars(sessions int) []dayBar {
	count := sessions - 1
	if count <= 0 || count > len(st.recent) {
		count = len(st.recent)
	}
	return st.recent[len(st.recent)-count:]
}
