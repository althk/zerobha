package strategy

import (
	"fmt"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// DailyReversal is a short-term mean-reversion strategy for daily candles.
//
// The rule set is the classic short-horizon reversal (Jegadeesh 1990, Lehmann
// 1990) in its Connors RSI(2) formulation: inside a longer-term uptrend, buy
// sharp short-term weakness and exit when price reverts to its short moving
// average.
//
//	regime : close > SMA(TrendPeriod)      — only fade dips inside an uptrend
//	entry  : RSI(RSIPeriod) < RSIEntry     — short-term oversold
//	target : SMA(ExitSMAPeriod) at entry   — the reversion objective
//	stop   : entry − StopATR × ATR
//	time   : exit after MaxHoldDays sessions if neither level is touched
//
// LONG ONLY, deliberately. The Indian cash segment does not permit carrying a
// short equity position overnight — a short must be squared off intraday or
// expressed in futures. A multi-day hold therefore has no symmetric short
// side, so the mirror-image "sell overbought" leg is intentionally absent
// rather than merely unimplemented.
//
// Unlike ORB this strategy holds across sessions, so it emits CNC signals and
// must not be squared off at 15:13. Daily candles are stamped 00:00:00+05:30,
// which sits before the engine's intraday cutoffs, so both the trade cutoff
// and the square-off pass over it.
type DailyReversal struct {
	symbols []string
	cfg     config.DailyRevConfig

	state map[string]*dailyRevState
}

// dailyRevState is the per-symbol streaming indicator set. Each symbol owns its
// own indicators; the backtester runs symbols independently, and the live
// engine feeds one candle stream per symbol.
type dailyRevState struct {
	rsi      *indicators.RSI
	atr      *indicators.ATR
	trendSMA *indicators.SMA
	exitSMA  *indicators.SMA

	bars int // candles seen, used to gate on indicator warmup

	// lastSignalDate suppresses a duplicate entry when the same session's
	// candle is replayed (the live aggregator can emit a final and a revised
	// candle for the same period).
	lastSignalDate string
}

// NewDailyReversalStrategy builds the strategy over the given symbols.
func NewDailyReversalStrategy(symbols []string, cfg config.DailyRevConfig) *DailyReversal {
	s := &DailyReversal{
		symbols: symbols,
		cfg:     cfg,
		state:   make(map[string]*dailyRevState, len(symbols)),
	}
	for _, sym := range symbols {
		s.state[sym] = s.newState()
	}
	return s
}

func (s *DailyReversal) newState() *dailyRevState {
	return &dailyRevState{
		rsi:      indicators.NewRSI(s.cfg.RSIPeriod),
		atr:      indicators.NewATR(s.cfg.ATRPeriod),
		trendSMA: indicators.NewSMA(s.cfg.TrendPeriod),
		exitSMA:  indicators.NewSMA(s.cfg.ExitSMAPeriod),
	}
}

// Name returns the identifier used in logs and reports.
func (s *DailyReversal) Name() string { return "DailyReversal" }

// Init is a no-op: the strategy self-seeds from the replayed candle stream.
// The backtester never calls Init (see CLAUDE.md), so warmup must come from
// the candles themselves, gated by the bars counter.
func (s *DailyReversal) Init(_ core.DataProvider) error { return nil }

// OnCandle consumes one daily candle and returns an entry signal or nil.
func (s *DailyReversal) OnCandle(candle models.Candle) *models.Signal {
	st, ok := s.state[candle.Symbol]
	if !ok {
		st = s.newState()
		s.state[candle.Symbol] = st
	}

	// Zero-volume candles are real on Kite (halted or illiquid symbols) and
	// carry a degenerate range. Feed nothing to the indicators and take no
	// signal, so a halt cannot manufacture an oversold reading.
	if candle.Volume.IsZero() || candle.High.Equal(candle.Low) {
		return nil
	}

	rsi := st.rsi.Update(candle.Close)
	atr := st.atr.Update(candle)
	trend := st.trendSMA.Update(candle.Close)
	exitMA := st.exitSMA.Update(candle.Close)
	st.bars++

	// Warm up every indicator before trusting any of them. The trend SMA is
	// the longest, so it sets the boundary.
	if st.bars <= s.cfg.TrendPeriod {
		return nil
	}
	if atr.IsZero() || trend.IsZero() {
		return nil
	}

	date := candle.StartTime.Format("2006-01-02")
	if date == st.lastSignalDate {
		return nil
	}

	// Regime filter: only buy dips inside a longer-term uptrend. Without it
	// this rule set buys every downtrend on the way down.
	if !candle.Close.GreaterThan(trend) {
		return nil
	}

	// Short-term oversold.
	if !rsi.LessThan(decimal.NewFromFloat(s.cfg.RSIEntry)) {
		return nil
	}

	// Price floor: sub-₹N names carry tick and impact costs this backtest does
	// not model. 0 disables the filter.
	if s.cfg.MinPrice > 0 && candle.Close.LessThan(decimal.NewFromFloat(s.cfg.MinPrice)) {
		return nil
	}

	entry := candle.Close
	target := exitMA
	stop := entry.Sub(atr.Mul(decimal.NewFromFloat(s.cfg.StopATR)))

	// The reversion objective must sit above the entry, or there is nothing to
	// revert to.
	if !target.GreaterThan(entry) {
		return nil
	}

	// Minimum edge filter. A target a few paise above entry cannot clear
	// costs; every intraday variant of this system died on exactly that. The
	// gap to the short MA must be worth at least MinTargetATR of ATR.
	minGap := atr.Mul(decimal.NewFromFloat(s.cfg.MinTargetATR))
	if target.Sub(entry).LessThan(minGap) {
		return nil
	}

	if !stop.IsPositive() {
		return nil
	}

	st.lastSignalDate = date

	metadata := map[string]string{
		"Strategy": s.Name(),
		"RSI":      rsi.StringFixed(2),
		"ATR":      atr.StringFixed(2),
		"TrendSMA": trend.StringFixed(2),
		"Reason":   fmt.Sprintf("RSI(%d)=%s below %.1f in uptrend", s.cfg.RSIPeriod, rsi.StringFixed(1), s.cfg.RSIEntry),
	}
	// Hand the simulator a hard time stop. Without it an entry that never
	// touches either level is held to the end of the backtest, which silently
	// converts a short-horizon rule into a buy-and-hold.
	if s.cfg.MaxHoldDays > 0 {
		metadata[models.MetaExitOnOrAfter] = holdDeadline(candle.StartTime, s.cfg.MaxHoldDays)
	}

	return &models.Signal{
		Symbol:      candle.Symbol,
		Type:        models.BuySignal,
		Price:       entry,
		StopLoss:    stop,
		Target:      target,
		ProductType: "CNC",
		Metadata:    metadata,
	}
}

// holdDeadline returns the calendar date on or after which the position must be
// closed. MaxHoldDays counts trading sessions; weekends are added back so the
// deadline lands the intended number of sessions ahead rather than expiring
// early across a weekend. Exchange holidays make this an approximation that
// errs toward holding slightly longer, never shorter.
func holdDeadline(entry time.Time, sessions int) string {
	d := entry
	remaining := sessions
	for remaining > 0 {
		d = d.AddDate(0, 0, 1)
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			remaining--
		}
	}
	return d.Format("2006-01-02")
}
