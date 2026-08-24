package strategy

import (
	"fmt"
	"log"
	"strings"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

// Donchian is an intraday channel-breakout system on 5-minute bars, long and
// short, squared off before the close.
//
//	channel : highest high / lowest low of the last DonchianLookback bars,
//	          EXCLUDING the bar being evaluated (see indicators.Donchian)
//	entry   : close clears the channel by BreakoutBufferPoints, inside the
//	          entry window, on a bar that passes the volume, ignition and
//	          volatility filters
//	stop    : entry ∓ SLATRMult × ATR
//	protect : a
//	          chandelier trail at TrailATRMult × ATR from the best price
//	          reached since entry — both applied by the broker, not here
//	exit    : trail or stop, a close beyond the
//	          OPPOSITE band (via core.ExitAdvisor), and the hard square-off
//
// The channel is deliberately NOT reset at the session boundary. With a
// 4-bar lookback a reset would leave the first bars of every session with no
// channel at all, and the entry window opens at 09:30 — 3 bars in. Carrying
// yesterday's last bars means the first breakout of the day is measured against
// the level that actually held into the close, which is the level traders are
// watching.
//
// The signal is computed on the NIFTY / SENSEX index chart and is intended for
// execution in weekly index options: the index says when, the option is what
// gets bought. Nothing here knows about options — it emits a plain directional
// signal, and SetContracts injects the lot size and exchange whatever the
// traded instrument turns out to be.
//
// An index publishes no volume, which the strategy detects rather than being
// told (see sawVolume), which is why there is no volume filter here at all —
// on an index it would be inapplicable rather than merely disabled.
type Donchian struct {
	cfg config.DonchianConfig

	// contracts maps trading symbol to its lot size and exchange. Empty for
	// cash equities, where the sizer works in single shares and orders route
	// to the default exchange.
	contracts map[string]ContractSpec

	state map[string]*donchianState

	// optionExec, when set, makes the strategy trade a weekly option on the
	// signal instrument rather than the instrument itself. See
	// donchian_options.go — it changes both what a signal names and who owns
	// the stop. nil is the backtest path and the recorded index-leg results.
	optionExec OptionExecutor
}

// ContractSpec is what a strategy needs to know about a derivative contract to
// size and route an order for it: the lot it trades in, and the exchange the
// order goes to. It is injected rather than looked up so the strategy stays
// free of any dependency on the instrument dump.
type ContractSpec struct {
	LotSize  int
	Exchange string
}

// donchianState is the per-symbol state. The indicators run continuously across
// sessions; only the day counters and the position view reset on the date change.
type donchianState struct {
	channel *indicators.Donchian
	atr     *indicators.ATR

	lastDate     string
	entriesToday int

	// sawVolume records whether this instrument has ever reported a positive
	// volume. An index never does — the exchanges publish no volume for one —
	// and treating that as a halted instrument would reject every bar and
	// produce a backtest of exactly zero trades. A cash equity sets it on its
	// first real bar, so a later zero-volume bar is still recognised as the
	// halt it is.
	sawVolume bool

	// openSide is the side of the position this strategy believes it holds,
	// and openValid says whether it holds one at all. It is a belief, not a
	// fact: the engine drops signals for risk limits and concurrency caps
	// without telling the strategy, and stops fire without telling it either.
	// Everything downstream treats it that way — an exit advice for a position
	// that does not exist is a no-op at the engine.
	openSide  models.SignalType
	openValid bool

	// exitAdvisedAt marks the bar on which an opposite-band exit was advised,
	// so the same bar cannot also open the reverse position. v1 has no flip:
	// the break closes the trade and the next bar starts fresh.
	exitAdvisedAt time.Time

	// leg is the option position this signal is being expressed through, and
	// carries the index-side stop and trail that decide when to close it. nil
	// whenever option execution is off, which is every backtest.
	leg *optionLeg
}

// NewDonchianStrategy builds the strategy over the given symbols.
func NewDonchianStrategy(symbols []string, cfg config.DonchianConfig) *Donchian {
	s := &Donchian{
		cfg:       cfg,
		contracts: make(map[string]ContractSpec),
		state:     make(map[string]*donchianState, len(symbols)),
	}
	for _, sym := range symbols {
		s.state[sym] = s.newState()
	}
	return s
}

// SetContracts injects the lot size and exchange per trading symbol, so signals
// are sized in whole lots and routed to the right segment. Symbols absent from
// the map size in single units and route to the default exchange, which is
// correct for cash equities and wrong for futures — the live wiring fills this
// from the Kite instrument dump.
func (s *Donchian) SetContracts(specs map[string]ContractSpec) {
	for sym, spec := range specs {
		s.contracts[sym] = spec
	}
}

func (s *Donchian) newState() *donchianState {
	return &donchianState{
		channel: indicators.NewDonchian(s.cfg.DonchianLookback),
		atr:     indicators.NewATR(s.cfg.ATRPeriod),
	}
}

// ingest folds one bar into the indicators and returns their state as it stood
// BEFORE that bar: the channel excluding it, and the ATR including it.
//
// That ordering is the whole game. A channel that included the current bar
// could never be broken by it. ATR is the exception — it is a volatility scale
// for sizing the stop, not a level to be crossed, so the most recent bar
// belongs in it.
func (st *donchianState) ingest(candle models.Candle) (upper, lower, atr decimal.Decimal, ready bool) {
	// Readiness is captured BEFORE the update, to match the values. On the bar
	// that fills the window the channel reports ready afterwards while the
	// values handed back still describe an unfilled window — i.e. zeros. Asking
	// afterwards makes the first breakout of every symbol a comparison against
	// a channel of [0, 0], which any close clears.
	ready = st.channel.IsReady()
	upper, lower = st.channel.Update(candle)
	atr = st.atr.Update(candle)
	return upper, lower, atr, ready
}

// Name returns the identifier used in logs and reports.
func (s *Donchian) Name() string { return "Donchian" }

// Init warms the indicators from historical bars so a live session can trade
// from its first candle rather than spending the first ~70 minutes filling a
// 4-bar channel and a 14-bar ATR.
//
// Failures are reported but not fatal, and the caller treats them that way: a
// symbol whose history cannot be fetched simply warms up from the live stream
// instead, which costs it the early session, not correctness.
//
// The backtester never calls Init (see CLAUDE.md) — there the warm-up comes
// from the replayed candles, so the first bars of any run take no trades.
func (s *Donchian) Init(provider core.DataProvider) error {
	if provider == nil {
		return nil
	}

	// Enough bars to fill both windows, and enough calendar days to find them
	// across a weekend or a holiday.
	needed := s.cfg.DonchianLookback + s.cfg.ATRPeriod
	var failures []string

	for symbol, st := range s.state {
		candles, err := provider.History(symbol, s.cfg.Timeframe, 5)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", symbol, err))
			continue
		}
		if len(candles) > needed {
			candles = candles[len(candles)-needed:]
		}
		for _, c := range candles {
			if c.Volume.IsZero() || c.High.Equal(c.Low) {
				continue
			}
			st.ingest(c)
		}
		log.Printf("[%s] Donchian: warmed up on %d historical bars", symbol, len(candles))
	}

	if len(failures) > 0 {
		return fmt.Errorf("warm-up incomplete for %d symbol(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func (s *Donchian) stateFor(symbol string) *donchianState {
	st, ok := s.state[symbol]
	if !ok {
		st = s.newState()
		s.state[symbol] = st
	}
	return st
}

// ExitAdvice implements core.ExitAdvisor: a close beyond the opposite band says
// the move that justified the position has reversed, and v1 gets out at market
// rather than waiting for the trail.
//
// It reads the channel without ingesting the candle — OnCandle does that a
// moment later, and the engine calls this first precisely so the exit is
// evaluated against the same channel the entry would be.
func (s *Donchian) ExitAdvice(candle models.Candle) *core.ExitAdvice {
	st := s.stateFor(candle.Symbol)

	// The index-side stop and trail come first: when the position lives in an
	// option they are the strategy's own responsibility, because no resting
	// order on the option can express a level on the index.
	if advice := s.optionExitAdvice(candle, st); advice != nil {
		return advice
	}

	if s.cfg.ExitOnOppositeBreak == nil || !*s.cfg.ExitOnOppositeBreak {
		return nil
	}
	if !st.openValid || !st.channel.IsReady() {
		return nil
	}

	upper, lower := st.channel.Value()
	var reason string
	switch {
	case st.openSide == models.BuySignal && candle.Close.LessThan(lower):
		reason = fmt.Sprintf("close %s below Donchian lower %s", candle.Close.StringFixed(2), lower.StringFixed(2))
	case st.openSide == models.SellSignal && candle.Close.GreaterThan(upper):
		reason = fmt.Sprintf("close %s above Donchian upper %s", candle.Close.StringFixed(2), upper.StringFixed(2))
	default:
		return nil
	}

	// With option execution on, the instrument to close is the contract, not
	// the index the signal was computed on.
	symbol, side := st.optionSymbolFor(candle.Symbol)

	st.openValid = false
	st.exitAdvisedAt = candle.StartTime
	st.leg = nil

	return &core.ExitAdvice{
		Symbol:  symbol,
		ForSide: side,
		Reason:  "Donchian opposite break: " + reason,
	}
}

// OnCandle consumes one bar and returns an entry signal or nil.
func (s *Donchian) OnCandle(candle models.Candle) *models.Signal {
	st := s.stateFor(candle.Symbol)

	istTime := candle.StartTime.In(istLocation)
	if date := istTime.Format("2006-01-02"); date != st.lastDate {
		st.lastDate = date
		st.entriesToday = 0
		// Everything here is MIS; an option leg cannot survive the square-off
		// either, and a stale one would close a position that no longer exists.
		st.leg = nil
		// A position cannot survive the session: everything here is MIS and
		// the square-off flattens it. Believing otherwise would suppress the
		// next day's first entry.
		st.openValid = false
	}

	if candle.Volume.IsPositive() {
		st.sawVolume = true
	}

	// Zero-volume candles are real on Kite (halted or illiquid names) and carry
	// a degenerate range that would poison ATR, the volume baseline and the
	// channel alike. Drop them before any indicator sees them — but only for
	// instruments that report volume at all, or an index would lose every bar.
	if (st.sawVolume && candle.Volume.IsZero()) || candle.High.Equal(candle.Low) {
		return nil
	}

	upper, lower, atr, channelReady := st.ingest(candle)

	if !channelReady || !atr.IsPositive() {
		return nil
	}

	h, m, _ := istTime.Clock()
	minuteOfDay := h*60 + m
	if minuteOfDay < s.cfg.EntryStartMin || minuteOfDay > s.cfg.EntryCutoffMin {
		return nil
	}
	if st.entriesToday >= s.cfg.MaxEntriesPerSymbol {
		return nil
	}
	// No flip in v1: the bar that broke the opposite band closes the trade and
	// does nothing else.
	if candle.StartTime.Equal(st.exitAdvisedAt) {
		return nil
	}

	buffer := decimal.NewFromFloat(s.cfg.BreakoutBufferPoints)
	side := models.BuySignal
	switch {
	case candle.Close.GreaterThan(upper.Add(buffer)):
		side = models.BuySignal
	case candle.Close.LessThan(lower.Sub(buffer)):
		if s.cfg.AllowShort == nil || !*s.cfg.AllowShort {
			return nil
		}
		side = models.SellSignal
	default:
		return nil
	}

	if !s.passesFilters(candle, atr) {
		return nil
	}

	signal := s.buildSignal(candle, side, atr, upper, lower)
	if signal == nil {
		return nil
	}

	// With option execution on, the index signal is a view, not an order: it
	// is translated into the weekly contract that expresses it, and the stop
	// and trail move from the broker to this strategy. The translation can
	// decline — too close to expiry, no strike listed, no premium — and a
	// declined entry must not consume the day's entry budget or leave the
	// strategy believing it holds a position.
	if s.optionExec != nil {
		optionSignal := s.buildOptionSignal(candle, st, signal, atr)
		if optionSignal == nil {
			return nil
		}
		st.entriesToday++
		st.openSide = side
		st.openValid = true
		return optionSignal
	}

	st.entriesToday++
	st.openSide = side
	st.openValid = true

	log.Printf("[%s] Donchian: %s breakout close=%s channel=[%s, %s] ATR=%s → SL %s trail %s",
		candle.Symbol, signal.Type, candle.Close.StringFixed(2),
		lower.StringFixed(2), upper.StringFixed(2), atr.StringFixed(2),
		signal.StopLoss.StringFixed(2), signal.TrailDistance.StringFixed(2))

	return signal
}

// passesFilters applies the two quality gates that separate a breakout with
// energy behind it from a drift over the line.
func (s *Donchian) passesFilters(candle models.Candle, atr decimal.Decimal) bool {

	// Ignition: the bar's own range must be a real fraction of recent ATR.
	// A breakout on a doji is the one that gets given back on the next bar.
	if s.cfg.UseIgnition != nil && *s.cfg.UseIgnition {
		if candle.High.Sub(candle.Low).LessThan(atr.Mul(decimal.NewFromFloat(s.cfg.IgnitionATRMult))) {
			return false
		}
	}

	// Volatility floor: in a name whose ATR is a rounding error on its price,
	// the stop distance cannot cover the round-trip cost.
	if s.cfg.MinATRPct > 0 && candle.Close.IsPositive() {
		atrPct := atr.Div(candle.Close).Mul(decimal.NewFromInt(100))
		if atrPct.LessThan(decimal.NewFromFloat(s.cfg.MinATRPct)) {
			return false
		}
	}

	return true
}

// buildSignal turns a confirmed breakout into the order parameters: the initial
// stop, the optional target, and the two protective-stop instructions the broker
// applies as price moves.
func (s *Donchian) buildSignal(candle models.Candle, side models.SignalType, atr, upper, lower decimal.Decimal) *models.Signal {
	entry := candle.Close
	risk := atr.Mul(decimal.NewFromFloat(s.cfg.SLATRMult))
	if !risk.IsPositive() {
		return nil
	}

	trail := atr.Mul(decimal.NewFromFloat(s.cfg.TrailATRMult))

	// No target and no breakeven: the chandelier trail is the only exit that
	// takes profit. Both alternatives were measured and both cost more than
	// they saved — see CLAUDE.md. Signal.Target is left zero deliberately, and
	// the simulator guards against reading that as "target at price 0".
	var stop decimal.Decimal
	if side == models.BuySignal {
		stop = entry.Sub(risk)
	} else {
		stop = entry.Add(risk)
	}

	// A stop at or below zero would make the sizer's risk-per-unit meaningless.
	if !stop.IsPositive() {
		return nil
	}

	if s.cfg.TrailATRMult <= 0 {
		trail = decimal.Zero
	}

	return &models.Signal{
		Symbol:        candle.Symbol,
		Type:          side,
		Price:         entry,
		StopLoss:      stop,
		TrailDistance: trail,
		RiskPct:       decimal.NewFromFloat(s.cfg.RiskPct / 100),
		LotSize:       s.contracts[candle.Symbol].LotSize,
		Exchange:      s.contracts[candle.Symbol].Exchange,
		ProductType:   "MIS",
		Metadata: map[string]string{
			"Strategy":      s.Name(),
			"ATR":           atr.StringFixed(2),
			"ChannelUpper":  upper.StringFixed(2),
			"ChannelLower":  lower.StringFixed(2),
			"TrailDistance": trail.StringFixed(2),
			"Reason": fmt.Sprintf("Donchian(%d) breakout, close %s cleared [%s, %s] by %.2f pts",
				s.cfg.DonchianLookback, entry.StringFixed(2),
				lower.StringFixed(2), upper.StringFixed(2), s.cfg.BreakoutBufferPoints),
		},
	}
}
