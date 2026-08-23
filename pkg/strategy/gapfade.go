package strategy

import (
	"fmt"
	"log"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// GapFade buys the intraday recovery in a stock that opened sharply lower.
//
// The thesis is that a large gap down splits into two populations: one carries
// new information (a fraud probe, a collapsed quarter, a guidance cut) and
// keeps going, the other is an overnight liquidity panic that mean-reverts
// during the session. Price alone cannot separate them, so this strategy asks
// an external source — the injected core.NewsGate — whether the gap is
// explained, and only fades the ones that are not.
//
//	qualify : open <= prevClose × (1 − MinGapDownPct), and the gap is not so
//	          large it is really a corporate action (MaxGapDownPct)
//	observe : 09:15 → ObserveEndMin, let the selling exhaust; the window's
//	          high becomes the reclaim level and its low anchors the stop
//	entry   : first candle up to EntryWindowEnd that closes above the observe
//	          high on a bullish body, and (optionally) above session VWAP
//	gate    : NewsGate.Assess must allow the symbol, evaluated once per day
//	stop    : entry − SLATR × ATR, floored at the session low
//	target  : entry + RewardRisk × (entry − stop)  →  1:2 by default
//
// LONG ONLY and MIS: the engine squares off at 15:13, so an entry that neither
// stops out nor reaches target exits at the square-off price.
//
// IMPORTANT — the gate is not backtestable. Upstox serves current news and
// current fundamentals; there is no as-of-date history, so a backtest can only
// run with gate == nil, i.e. fading every qualifying gap regardless of cause.
// That is a strictly different (and worse) strategy than the live one, and its
// results are a lower bound on the gated version at best, not an estimate of
// it. See CLAUDE.md before drawing any conclusion from a gapfade backtest.
type GapFade struct {
	symbols []string
	cfg     config.GapFadeConfig
	gate    core.NewsGate

	state map[string]*gapFadeState
}

// gapFadeState is the per-symbol session state. Everything except the
// indicators resets on the date change.
type gapFadeState struct {
	atr  *indicators.ATR
	vwap *indicators.VWAP

	lastDate  string
	lastClose decimal.Decimal // close of the previous candle, seen so far
	prevClose decimal.Decimal // close of the previous trading session

	gapPct      decimal.Decimal // signed gap % of today's open vs prevClose
	qualified   bool            // today's gap is in the fadeable band
	done        bool            // today is finished: traded, or gate blocked
	observeHigh decimal.Decimal // highest high of the observation window
	dayLow      decimal.Decimal // lowest low of the session so far
}

// NewGapFadeStrategy builds the strategy over the given symbols. gate may be
// nil, which disables the news/earnings check and fades every qualifying gap —
// see the warning on the type before doing that anywhere but a backtest.
func NewGapFadeStrategy(symbols []string, cfg config.GapFadeConfig, gate core.NewsGate) *GapFade {
	s := &GapFade{
		symbols: symbols,
		cfg:     cfg,
		gate:    gate,
		state:   make(map[string]*gapFadeState, len(symbols)),
	}
	for _, sym := range symbols {
		s.state[sym] = s.newState()
	}
	return s
}

func (s *GapFade) newState() *gapFadeState {
	return &gapFadeState{
		atr:  indicators.NewATR(s.cfg.ATRPeriod),
		vwap: indicators.NewVWAP(),
	}
}

// Name returns the identifier used in logs and reports.
func (s *GapFade) Name() string { return "GapFade" }

// Init is a no-op: the strategy self-seeds from the candle stream. The
// backtester never calls Init (see CLAUDE.md), so the ATR warms up from the
// replayed candles and the first sessions of any run take no trades.
func (s *GapFade) Init(_ core.DataProvider) error { return nil }

// OnCandle consumes one intraday candle and returns an entry signal or nil.
func (s *GapFade) OnCandle(candle models.Candle) *models.Signal {
	st, ok := s.state[candle.Symbol]
	if !ok {
		st = s.newState()
		s.state[candle.Symbol] = st
	}

	// Record this candle's close as the previous close for the next one, no
	// matter which branch below returns. On the first candle of a session it
	// still holds the last close of the previous session, which is what the
	// gap is measured against.
	defer func() { st.lastClose = candle.Close }()

	istTime := candle.StartTime.In(istLocation)
	h, m, _ := istTime.Clock()
	minuteOfDay := h*60 + m

	if date := istTime.Format("2006-01-02"); date != st.lastDate {
		s.rollSession(st, candle, date)
	}

	// Zero-volume candles are real on Kite (halted or illiquid symbols) and
	// carry a degenerate range that would poison ATR and VWAP. The session
	// roll above still ran, so the gap is measured even when the opening print
	// is a halt; this candle simply contributes nothing further.
	if candle.Volume.IsZero() || candle.High.Equal(candle.Low) {
		return nil
	}

	atr := st.atr.Update(candle)
	vwap := st.vwap.Update(candle)

	if st.dayLow.IsZero() || candle.Low.LessThan(st.dayLow) {
		st.dayLow = candle.Low
	}

	if !st.qualified || st.done {
		return nil
	}

	// Observation window: let the knife land. Its high is the level a
	// recovery has to reclaim.
	if minuteOfDay < s.cfg.ObserveEndMin {
		if candle.High.GreaterThan(st.observeHigh) {
			st.observeHigh = candle.High
		}
		return nil
	}

	if minuteOfDay > s.cfg.EntryWindowEnd {
		st.done = true
		return nil
	}

	if st.observeHigh.IsZero() || atr.IsZero() {
		return nil
	}

	// Reclaim: close above the observation high on an up candle. The body
	// check rejects a wick that pokes through the level and closes back down.
	if !candle.Close.GreaterThan(st.observeHigh) || !candle.Close.GreaterThan(candle.Open) {
		return nil
	}

	// Session VWAP is the day's average trade price; closing above it says
	// buyers have taken back the whole session, not just the last few bars.
	// VWAP can be zero only if every candle so far had zero volume, in which
	// case the filter is meaningless rather than failed — guard the division
	// by treating it as not-yet-available.
	if s.cfg.RequireAboveVWAP != nil && *s.cfg.RequireAboveVWAP {
		if vwap.IsZero() || !candle.Close.GreaterThan(vwap) {
			return nil
		}
	}

	entry := candle.Close
	if s.cfg.MinPrice > 0 && entry.LessThan(decimal.NewFromFloat(s.cfg.MinPrice)) {
		st.done = true
		return nil
	}

	stop := entry.Sub(atr.Mul(decimal.NewFromFloat(s.cfg.SLATR)))
	// Floor the stop at the session low: a stop sitting inside the panic range
	// is inside the noise that created the opportunity.
	if s.cfg.StopAtDayLow != nil && *s.cfg.StopAtDayLow && !st.dayLow.IsZero() && st.dayLow.LessThan(stop) {
		stop = st.dayLow
	}
	if !stop.IsPositive() || !stop.LessThan(entry) {
		return nil
	}

	risk := entry.Sub(stop)
	if s.cfg.MaxStopPct > 0 {
		maxRisk := entry.Mul(decimal.NewFromFloat(s.cfg.MaxStopPct / 100))
		if risk.GreaterThan(maxRisk) {
			log.Printf("[%s] GapFade: stop %.2f%% below entry exceeds max %.2f%% — skipping",
				candle.Symbol, risk.Div(entry).Mul(decimal.NewFromInt(100)).InexactFloat64(), s.cfg.MaxStopPct)
			st.done = true
			return nil
		}
	}
	target := entry.Add(risk.Mul(decimal.NewFromFloat(s.cfg.RewardRisk)))

	// Consult the news/earnings gate last, once the price setup is confirmed,
	// so a live run makes at most one API call per symbol per day and only for
	// symbols it would otherwise trade.
	verdict, ok := s.consultGate(candle)
	if !ok {
		st.done = true
		return nil
	}

	st.done = true

	return &models.Signal{
		Symbol:      candle.Symbol,
		Type:        models.BuySignal,
		Price:       entry,
		StopLoss:    stop,
		Target:      target,
		ProductType: "MIS",
		Metadata: map[string]string{
			"Strategy":    s.Name(),
			"GapPct":      st.gapPct.StringFixed(2),
			"ATR":         atr.StringFixed(2),
			"ObserveHigh": st.observeHigh.StringFixed(2),
			"DayLow":      st.dayLow.StringFixed(2),
			"VWAP":        vwap.StringFixed(2),
			"Gate":        verdict,
			"Reason": fmt.Sprintf("Gap %.2f%% reclaimed %s; gate: %s",
				st.gapPct.InexactFloat64(), st.observeHigh.StringFixed(2), verdict),
		},
	}
}

// rollSession resets per-day state and decides whether today's open qualifies
// as a fadeable gap down. It runs on the first candle of each session, whose
// Open is the day's true opening print.
func (s *GapFade) rollSession(st *gapFadeState, candle models.Candle, date string) {
	if !st.lastClose.IsZero() {
		st.prevClose = st.lastClose
	}

	st.lastDate = date
	st.qualified = false
	st.done = false
	st.observeHigh = decimal.Zero
	st.dayLow = decimal.Zero
	st.gapPct = decimal.Zero

	// VWAP is session-anchored: rebuild it so yesterday's volume never leaks
	// into today's average.
	st.vwap = indicators.NewVWAP()

	if st.prevClose.IsZero() {
		return
	}

	st.gapPct = candle.Open.Sub(st.prevClose).Div(st.prevClose).Mul(decimal.NewFromInt(100))
	gapDown := st.gapPct.Neg() // positive magnitude for a gap down

	if gapDown.LessThan(decimal.NewFromFloat(s.cfg.MinGapDownPct)) {
		return
	}
	if s.cfg.MaxGapDownPct > 0 && gapDown.GreaterThan(decimal.NewFromFloat(s.cfg.MaxGapDownPct)) {
		log.Printf("[%s] GapFade: gap %.2f%% exceeds max %.2f%% (likely a corporate action) — skipping today",
			candle.Symbol, gapDown.InexactFloat64(), s.cfg.MaxGapDownPct)
		return
	}

	st.qualified = true
	log.Printf("[%s] GapFade: qualifying gap %.2f%% (prev close %s, open %s) — watching for reclaim",
		candle.Symbol, st.gapPct.InexactFloat64(), st.prevClose.StringFixed(2), candle.Open.StringFixed(2))
}

// consultGate asks the injected NewsGate whether this gap is explained by bad
// information. It returns the verdict reason and whether to proceed.
//
// With no gate injected the setup is taken unconditionally — correct for a
// backtest, which has no as-of-date news, and flagged in the metadata so the
// trade log says which regime produced it. When a gate errors, GateFailOpen
// decides: by default the trade is skipped, because an unverified gap fade is
// not the strategy that was specified.
func (s *GapFade) consultGate(candle models.Candle) (string, bool) {
	if s.gate == nil {
		return "no gate (ungated backtest)", true
	}

	verdict, err := s.gate.Assess(candle.Symbol, candle.StartTime)
	if err != nil {
		failOpen := s.cfg.GateFailOpen != nil && *s.cfg.GateFailOpen
		log.Printf("[%s] GapFade: news gate failed (%v); fail_open=%t", candle.Symbol, err, failOpen)
		if !failOpen {
			return "", false
		}
		return fmt.Sprintf("gate error, failed open: %v", err), true
	}
	if !verdict.Allow {
		log.Printf("[%s] GapFade: news gate blocked entry — %s", candle.Symbol, verdict.Reason)
		return "", false
	}
	return verdict.Reason, true
}
