// Package config provides configuration loading functionality.
package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// StrategySettings holds the fields common to every strategy that the engine
// needs to bootstrap: which symbols to load, how many, and the candle timeframe.
type StrategySettings struct {
	Timeframe string
	CSVFile   string
	Limit     int
}

type ORBConfig struct {
	Timeframe         string  `toml:"timeframe"`           // default "5m"
	CSVFile           string  `toml:"csv_file"`            // default "high_beta_stocks.csv"
	Limit             int     `toml:"limit"`               // default 50
	EntryWindowEnd    int     `toml:"entry_window_end"`    // minutes from midnight, default 630 (10:30 AM)
	RSILongThreshold  float64 `toml:"rsi_long_threshold"`  // default 50
	RSIShortThreshold float64 `toml:"rsi_short_threshold"` // default 40
	ADXThreshold      float64 `toml:"adx_threshold"`       // default 20
	MinRangeATR       float64 `toml:"min_range_atr"`       // default 1.0
	MaxRangeATR       float64 `toml:"max_range_atr"`       // default 3.0
	SLMultiplier      float64 `toml:"sl_multiplier"`       // default 1.5
	TargetMultiplier  float64 `toml:"target_multiplier"`   // default 3.0
	MaxConcurrent     int     `toml:"max_concurrent"`      // max simultaneous positions, default 5
	RelVolThreshold   float64 `toml:"rel_vol_threshold"`   // min ratio of opening-range vol to avg morning vol, default 1.5

	// VolThrustMult requires the breakout candle's volume to exceed
	// VolThrustMult × the trailing volume SMA. Default 1.0 preserves the
	// original "volume > avg" behavior; values >1 demand a genuine thrust.
	VolThrustMult float64 `toml:"vol_thrust_mult"` // default 1.0
	// MaxVWAPDistATR caps how far (in ATR units) the breakout close may be
	// from VWAP, to avoid chasing extended moves. 0 disables the filter.
	MaxVWAPDistATR float64 `toml:"max_vwap_dist_atr"` // default 0 (disabled)
	// ADXRisingEps relaxes the strict "ADX rising" requirement: a signal is
	// allowed when adx > prevADX - ADXRisingEps. 0 keeps the strict check.
	ADXRisingEps float64 `toml:"adx_rising_eps"` // default 0
	// OneTradePerDay blocks any further signals for a symbol once it has
	// fired one signal that day, capping the per-symbol whipsaw loss tail.
	OneTradePerDay *bool `toml:"one_trade_per_day"` // default true
	// StopFloorAtRange floors a long stop at RangeLow and caps a short stop
	// at RangeHigh, so the ATR stop never sits inside the opening range.
	StopFloorAtRange *bool `toml:"stop_floor_at_range"` // default true

	// BodyStrengthThreshold requires the breakout candle's body to be at
	// least this fraction of its full range (|close-open| / (high-low)).
	BodyStrengthThreshold float64 `toml:"body_strength_threshold"` // default 0.6
	// MaxGapPct rejects symbols that gapped more than this percent from the
	// previous day's close, avoiding exhausted overnight moves.
	MaxGapPct float64 `toml:"max_gap_pct"` // default 3.0
	// MaxVWAPDistPct caps how far (in percent) the breakout close may sit
	// from VWAP, to avoid chasing extended moves.
	MaxVWAPDistPct float64 `toml:"max_vwap_dist_pct"` // default 1.5

	// PartialExitAtRMultiple sets the ATR multiple at which half the position
	// is booked and the stop is moved to breakeven. 0 disables partial exits
	// (the position runs untouched to the full SL/target).
	PartialExitAtRMultiple float64 `toml:"partial_exit_at_r_multiple"` // default 0 (disabled)
	// PartialExitPct is the fraction of the position closed at the partial
	// exit level (e.g. 0.5 = sell half). Ignored when PartialExitAtRMultiple is 0.
	PartialExitPct float64 `toml:"partial_exit_pct"` // default 0.5
}

// DailyRevConfig holds parameters for the DailyReversal strategy (daily
// candles, multi-day holds). It is long-only by construction — see the note on
// the strategy type — so there are no short-side thresholds.
type DailyRevConfig struct {
	Timeframe string `toml:"timeframe"` // default "day"
	CSVFile   string `toml:"csv_file"`  // default "ind_nifty200list.csv"
	Limit     int    `toml:"limit"`     // default 200

	TrendPeriod   int `toml:"trend_period"`    // regime SMA, default 200
	RSIPeriod     int `toml:"rsi_period"`      // default 2
	ExitSMAPeriod int `toml:"exit_sma_period"` // reversion target SMA, default 5
	ATRPeriod     int `toml:"atr_period"`      // default 14

	RSIEntry float64 `toml:"rsi_entry"` // buy below this RSI, default 10
	StopATR  float64 `toml:"stop_atr"`  // stop distance in ATR, default 2.5
	// MinTargetATR rejects entries whose distance to the reversion target is
	// smaller than this many ATR — a target too close to cover costs.
	MinTargetATR float64 `toml:"min_target_atr"` // default 0.5
	// MinPrice skips low-priced names where tick and impact costs dominate.
	MinPrice float64 `toml:"min_price"` // default 50
	// MaxHoldDays force-closes a position after this many sessions. 0 disables
	// the time stop, which lets a stuck trade run to the end of the backtest.
	MaxHoldDays int `toml:"max_hold_days"` // default 10

	MaxConcurrent int `toml:"max_concurrent"` // default 5
}

// GapFadeConfig holds parameters for the GapFade strategy: buy an intraday
// recovery in a name that opened sharply lower, provided the gap is not
// explained by genuinely bad news or a collapsed quarter.
//
// It is long-only and intraday (MIS): the thesis is that an unexplained
// panic open mean-reverts within the session.
type GapFadeConfig struct {
	Timeframe string `toml:"timeframe"` // default "5m"
	CSVFile   string `toml:"csv_file"`  // default "ind_nifty200list.csv"
	Limit     int    `toml:"limit"`     // default 200

	// MinGapDownPct is the magnitude (positive percent) of the minimum
	// qualifying gap down, measured open vs previous close. Default 5.
	MinGapDownPct float64 `toml:"min_gap_down_pct"`
	// MaxGapDownPct rejects gaps so large they are usually a corporate action
	// (split, bonus, large special dividend) rather than a panic. Default 20.
	// Neither the CSV feed nor this strategy knows ex-dates, so the cap is the
	// only defence against fading a price adjustment that will never revert.
	MaxGapDownPct float64 `toml:"max_gap_down_pct"`

	// ObserveEndMin ends the "let the knife land" window, minutes from
	// midnight IST. Default 570 (09:30); its high becomes the reclaim level.
	ObserveEndMin int `toml:"observe_end_min"`
	// EntryWindowEnd is the last minute of the day an entry may trigger.
	// Default 660 (11:00) — later reclaims leave too little session to run.
	EntryWindowEnd int `toml:"entry_window_end"`

	ATRPeriod int `toml:"atr_period"` // default 14

	// SLATR sets the stop distance in ATR units below entry. Default 2.
	SLATR float64 `toml:"sl_atr"`
	// RewardRisk multiplies the realised stop distance to place the target.
	// Default 2 — the 1:2 risk-reward this strategy is specified around.
	RewardRisk float64 `toml:"reward_risk"`
	// MaxStopPct rejects an entry whose stop sits further than this percent
	// below entry. Default 5. It guards against the day-low floor widening the
	// stop (and with it the 1:2 target) to a distance the session cannot
	// travel; it cannot be much tighter than this, because on a 5%+ gap day
	// the distance from the panic low back to the reclaim level is itself
	// several percent. Position size shrinks with the wider stop anyway — the
	// sizer risks a fixed 1% of capital per trade.
	MaxStopPct float64 `toml:"max_stop_pct"`
	// StopAtDayLow floors the stop at the session low when the ATR stop would
	// otherwise sit above it, so the stop is never inside the panic range.
	// Default true.
	StopAtDayLow *bool `toml:"stop_at_day_low"`
	// RequireAboveVWAP demands the reclaim candle close above session VWAP.
	// Default true.
	RequireAboveVWAP *bool `toml:"require_above_vwap"`
	// MinPrice skips low-priced names where tick and impact costs dominate.
	MinPrice float64 `toml:"min_price"` // default 50

	// GateFailOpen decides what happens when the news/earnings gate errors
	// (network failure, expired Upstox token). Default false: no verdict, no
	// trade — the gate is the whole thesis, so trading blind would be trading
	// a different strategy than the one that was tested.
	GateFailOpen *bool `toml:"gate_fail_open"`

	MaxConcurrent int `toml:"max_concurrent"` // default 5
}

// DonchianConfig holds parameters for the Donchian intraday breakout strategy:
// a channel breakout computed on the NIFTY / SENSEX 5-minute **index** chart,
// intended for execution in weekly index options.
//
// The signal instrument and the traded instrument are deliberately different.
// The index says when to act; an option is what gets bought. Nothing here
// describes the option leg — strike selection, position size in lots and the
// premium P&L all live in cmd/optbt, which prices this strategy's trade list on
// real expired weekly-option candles.
//
// The zero-means-absent convention applies (see CLAUDE.md): setting a knob to 0
// in TOML gets the default back. Knobs whose zero is a meaningful setting are
// pointers (UseIgnition, AllowShort, ExitOnOppositeBreak) so that "absent" and
// "off" stay distinguishable.
//
// Three families of knob were REMOVED rather than defaulted off, because each
// was measured and found inapplicable or actively harmful. Read the Donchian
// sections of CLAUDE.md before reintroducing any of them:
//
//   - vol_mult / vol_avg_bars: an index publishes no volume at all, so the
//     filter was inapplicable rather than merely disabled.
//   - use_breakeven / breakeven_trigger_atr / breakeven_plus_points: BREAKEVEN+
//     truncates the right tail a breakout system lives on; removing it doubled
//     gross return per trade. It also fabricated profits whenever the parked
//     stop landed beyond the market.
//   - tp_rr: a target capped the winners in four independent tests and paid for
//     itself in none. The chandelier trail is the only profit-taking exit.
type DonchianConfig struct {
	Timeframe string `toml:"timeframe"` // default "5m"
	CSVFile   string `toml:"csv_file"`  // backtest universe, default "indices.csv"
	Limit     int    `toml:"limit"`     // default 2

	DonchianLookback int `toml:"donchian_lookback"` // channel bars, default 30
	ATRPeriod        int `toml:"atr_period"`        // default 9

	// MinATRPct is a volatility floor as a percent of price (0.03 = 0.03%),
	// skipping bars too dead for a breakout to pay for its costs. Default 0.03.
	// The stock-calibrated 0.15 rejects every bar on an index, whose 5-minute
	// ATR runs 0.076% (NIFTY) to 0.083% (SENSEX) of price.
	MinATRPct float64 `toml:"min_atr_pct"`
	// BreakoutBufferPoints requires the close to clear the channel by this many
	// rupees, not merely touch it. It is an absolute-points knob (not ATR) on
	// purpose - it is a noise/tick filter, and tick noise does not scale with
	// volatility the way the channel already does. Default 2.
	BreakoutBufferPoints float64 `toml:"breakout_buffer_points"`
	// UseIgnition demands the breakout bar carry real energy: its range
	// (high-low) must be at least IgnitionATRMult x ATR. A breakout that drifts
	// over the line on a doji is the one that gets given back. Default true.
	UseIgnition     *bool   `toml:"use_ignition"`
	IgnitionATRMult float64 `toml:"ignition_atr_mult"` // default 1.0

	// SLATRMult places the initial stop this many ATR from entry. Default 3.0.
	//
	// It was 1.3, which stopped out 82% of trades before the trend developed
	// while all of the profit sat in the ones that survived to run. At 3.0 the
	// chandelier trail binds first and the initial stop rarely fires at all —
	// stops of 3, 4 and 6 ATR produce identical results for that reason.
	SLATRMult float64 `toml:"sl_atr_mult"`
	// TrailATRMult is the chandelier trail distance in ATR, measured from the
	// highest high (long) or lowest low (short) since entry. The stop only
	// ratchets. Default 3.0; 0 disables trailing and leaves the initial stop.
	//
	// This is the only profit-taking exit the strategy has, so it is
	// load-bearing: 3 beats 2 on both indices, and every attempt to cap the
	// winning side ahead of it has cost more than it saved.
	TrailATRMult float64 `toml:"trail_atr_mult"`

	// EntryStartMin skips the opening auction noise; EntryCutoffMin is the last
	// minute a new entry may trigger; SquareOffMin is the hard flatten. All are
	// minutes from midnight IST: 570 = 09:30, 871 = 14:31, 897 = 14:57.
	//
	// SquareOffMin is earlier than the engine's global 15:13 MIS square-off and
	// is the binding one for this strategy - the wiring in cmd/backtest and
	// cmd/trader drives the flatten from it.
	EntryStartMin  int `toml:"entry_start_min"`
	EntryCutoffMin int `toml:"entry_cutoff_min"`
	SquareOffMin   int `toml:"squareoff_min"`

	// MaxCapitalPerTrade overrides [engine].max_capital_per_trade for this
	// strategy only. Zero means "absent" and the engine's value stands.
	//
	// It exists because the engine's cap is calibrated for cash equities and
	// this strategy trades indices, where the same number silently means
	// "no trades at all": quantity floors to zero for any instrument priced
	// above the cap and every signal vanishes, which reads as the strategy
	// finding no setups. SENSEX at ~78,000 against the 50,000 stock-sized cap
	// produced exactly zero trades. Raising the engine's cap globally would
	// instead rescale ORB and gapfade, whose recorded results were all
	// measured at the stock-sized value.
	MaxCapitalPerTrade int64 `toml:"max_capital_per_trade"`

	// RiskPct is the fraction of capital risked per trade, in percent.
	// Default 0.5 - half the 1% the other strategies use, because this one
	// runs both directions and takes more entries.
	RiskPct float64 `toml:"risk_pct"`
	// MaxDailyLossPct stops all new entries once the day's realised PnL is
	// below this percent of capital. Default 2.
	MaxDailyLossPct float64 `toml:"max_daily_loss_pct"`

	MaxConcurrent int `toml:"max_concurrent"` // default 5
	// MaxEntriesPerSymbol caps entries per symbol per day. Default 4.
	//
	// With a one-instrument universe this is the day's trade budget. A cap of 1
	// makes a marginal early entry consume the slot a better later setup needed,
	// so a filter change reshuffles which trade is taken rather than adding to
	// it — compare matched trade lists, never totals.
	MaxEntriesPerSymbol int `toml:"max_entries_per_symbol"`

	// TargetDelta is the |delta| of the option bought to express a signal.
	// Default 0.80. A long index view buys a call, a short view buys a put;
	// the strategy is never short an option.
	//
	// A delta target is not interchangeable with a fixed point offset. What
	// sets delta is the move in units of sigma*sqrt(T), so 150 points ITM is
	// ~0.95 delta on expiry morning and ~0.68 six days out. Holding the point
	// offset fixed sweeps delta across the week; holding delta fixed is what
	// this buys instead.
	TargetDelta float64 `toml:"target_delta"`

	// TargetDeltaNearExpiry is the |delta| used when time to expiry is <= 3 days.
	// Near expiry (Tue/Wed for Thu weekly expiry), theta accelerates. Target delta
	// increases (default 0.90) to lower extrinsic time value and avoid theta bleed.
	TargetDeltaNearExpiry float64 `toml:"target_delta_near_expiry"`

	// MinDaysToExpiry refuses entries when the nearest weekly expiry is closer
	// than this. Default 2 - skipping expiry day and the day before.
	//
	// It is the single largest improvement found in this strategy, and it has
	// a mechanism rather than a hindsight: as T collapses the target-delta
	// strike walks in towards spot, so near expiry it buys a nearly worthless
	// contract with enormous leverage and brutal decay per rupee of premium.
	// Measured at -1571 bps a trade on expiry day against -104 to +106 for the
	// rest of the week.
	// It is a pointer for the reason tp_rr was: 0 is a meaningful setting
	// ("trade expiry day too"), and with a plain int "absent from the TOML"
	// and "explicitly zero" are the same value, so honouring an explicit 0
	// means the default can never apply. That exact bug ran a whole backtest
	// without the target it was meant to be testing.
	MinDaysToExpiry *int `toml:"min_days_to_expiry"`

	// ADXThreshold gates entries to bars where ADX >= ADXThreshold, rejecting
	// breakouts that fire in low-energy sideways chop. 0 disables it.
	// Default 15 on ADXPeriod 14, the measured best of a 32-cell sweep.
	//
	// Keep it MILD. Out of sample the entire grid - filter off included - lands
	// between +1.1 and +2.2 bps, so the gate is worth ~0.2 bps per trade and is
	// not a lever. What it does buy is fewer trades for the same gross, which
	// matters against a flat per-order brokerage. Tightening it inverts the
	// two windows: at period 7 threshold 40 the tuning window reads +8.91 bps
	// and the held-out year -0.03, and every cell whose in-sample figure clears
	// ~7 bps is negative out of sample on both indices.
	ADXThreshold float64 `toml:"adx_threshold"`
	ADXPeriod    int     `toml:"adx_period"` // default 14

	// FallbackIV is the volatility used when the at-the-money premium cannot
	// be read. Default 0, which means "refuse to trade" - selecting a strike
	// against a stale constant picks the wrong contract silently, and not
	// trading is the better failure.
	FallbackIV float64 `toml:"fallback_iv"`

	// AllowShort enables the short side. Default true - the spec is symmetric,
	// and a bearish index signal is expressed by buying a put, never by selling
	// an option.
	AllowShort *bool `toml:"allow_short"`
	// ExitOnOppositeBreak closes a position at market when price closes beyond
	// the opposite Donchian band. Default true. There is no flip in v1: the
	// position is closed, and the same-day entry cap decides whether the
	// reverse trade may be taken.
	ExitOnOppositeBreak *bool `toml:"exit_on_opposite_break"`
}

// UpstoxConfig configures the policy of the Upstox-backed news and earnings
// gate. The credential itself is the top-level upstox_access_token key, not a
// field here — see Config.UpstoxAccessToken and the TOML placement warning
// there.
type UpstoxConfig struct {
	// ISINCSV is any NSE index constituent CSV carrying "Symbol" and
	// "ISIN Code" columns; it resolves a trading symbol to the ISIN that both
	// Upstox instrument keys and the fundamentals endpoints are keyed by.
	ISINCSV string `toml:"isin_csv"` // default "ind_nifty500list.csv"

	// NewsLookbackHours bounds how far back a headline still counts as an
	// explanation for today's gap. Default 48.
	NewsLookbackHours int `toml:"news_lookback_hours"`
	// BlockKeywords are matched case-insensitively against headline and
	// summary; any hit blocks the trade. Empty uses the built-in list.
	BlockKeywords []string `toml:"block_keywords"`
	// MaxProfitDropPct blocks the trade when the latest reported quarter's net
	// profit fell by more than this percent. Default 25.
	MaxProfitDropPct float64 `toml:"max_profit_drop_pct"`
	// MaxResultsAgeDays bounds how old the latest reported quarter may be
	// before the fundamentals are treated as stale and simply unavailable
	// (the entry then rests on the news check alone). Default 180.
	MaxResultsAgeDays int `toml:"max_results_age_days"`
	// TimeoutSeconds caps each HTTP call. Default 5: the gate runs on the
	// candle hot path, so a hung call must not stall the trading loop.
	TimeoutSeconds int `toml:"timeout_seconds"`
}

// RiskConfig holds parameters for the risk manager.
// MaxTradesPerStock is a cross-strategy cap; note that ORB's one_trade_per_day
// already enforces a per-symbol daily limit at the strategy level. Set
// MaxTradesPerStock to 0 to disable the risk-manager cap (recommended when
// one_trade_per_day is true, to avoid redundant and potentially conflicting checks).
type RiskConfig struct {
	MaxDailyLoss      int `toml:"max_daily_loss"`       // INR, default 5000
	MaxTradesPerDay   int `toml:"max_trades_per_day"`   // default 10
	MaxTradesPerStock int `toml:"max_trades_per_stock"` // default 0 (disabled); use one_trade_per_day for ORB
}

// EngineConfig holds capital-sizing and trade-gating parameters for the engine.
type EngineConfig struct {
	MinBalance         int `toml:"min_balance"`           // INR floor below which all signals are skipped, default 3000
	MinCapitalPerTrade int `toml:"min_capital_per_trade"` // INR floor per slot, default 30000
	MaxCapitalPerTrade int `toml:"max_capital_per_trade"` // INR cap per slot, default 50000
	TradeCutoffMin     int `toml:"trade_cutoff_min"`      // minutes from midnight, default 845 (14:05)
}

// PathsConfig locates the files the trader writes. Both default to paths
// relative to the working directory, which is what a local run wants.
//
// They are settable because a deployment's working directory is not the
// repository root. The container sets WORKDIR to the data volume so the
// relative database lands there — which silently put the log directory inside
// the data volume too, rather than the logs volume mounted beside it. The two
// destinations are independent, so they get independent knobs.
type PathsConfig struct {
	// DBPath is the SQLite file holding orders, trades, signals, equity
	// snapshots and paper-broker state. Default "zerobha.db".
	DBPath string `toml:"db_path"`
	// LogDir holds the daily log file and the order journal CSV. It is created
	// if missing. Default "logs".
	LogDir string `toml:"log_dir"`
}

// DefaultPathsConfig returns the relative locations used by a local run.
func DefaultPathsConfig() PathsConfig {
	return PathsConfig{
		DBPath: "zerobha.db",
		LogDir: "logs",
	}
}

type Config struct {
	Strategy  string `toml:"strategy"`
	APIKey    string `toml:"api_key"`
	APISecret string `toml:"api_secret"`
	// Paths holds the on-disk locations the trader writes to.
	Paths PathsConfig `toml:"paths"`
	// PaperTrading, when true, runs with simulated order fills and virtual balance
	// while consuming live market feeds and live option quotes.
	PaperTrading bool    `toml:"paper_trading"`
	PaperCapital float64 `toml:"paper_capital"`
	// UpstoxAccessToken authorises the read-only Upstox news and fundamentals
	// calls behind the GapFade gate. It is a long-lived (~1 year) read-only
	// token, unrelated to the Kite credentials above.
	//
	// Like api_key/api_secret this is a bare key: it MUST appear above the
	// first [section] header or TOML silently assigns it to that section.
	UpstoxAccessToken string         `toml:"upstox_access_token"`
	UptrendOnly       *bool          `toml:"uptrend_only"`
	Risk              RiskConfig     `toml:"risk"`
	Engine            EngineConfig   `toml:"engine"`
	ORB               ORBConfig      `toml:"orb"`
	DailyRev          DailyRevConfig `toml:"dailyrev"`
	GapFade           GapFadeConfig  `toml:"gapfade"`
	Donchian          DonchianConfig `toml:"donchian"`
	Upstox            UpstoxConfig   `toml:"upstox"`
}

// ActiveStrategySettings returns the Timeframe, CSVFile, and Limit the engine
// bootstraps from, taken from the section belonging to the selected strategy.
// Callers do not reach into the per-strategy configs directly.
func (c *Config) ActiveStrategySettings() StrategySettings {
	switch c.Strategy {
	case StrategyDailyRev:
		return StrategySettings{Timeframe: c.DailyRev.Timeframe, CSVFile: c.DailyRev.CSVFile, Limit: c.DailyRev.Limit}
	case StrategyGapFade:
		return StrategySettings{Timeframe: c.GapFade.Timeframe, CSVFile: c.GapFade.CSVFile, Limit: c.GapFade.Limit}
	case StrategyDonchian:
		return StrategySettings{Timeframe: c.Donchian.Timeframe, CSVFile: c.Donchian.CSVFile, Limit: c.Donchian.Limit}
	}
	return StrategySettings{Timeframe: c.ORB.Timeframe, CSVFile: c.ORB.CSVFile, Limit: c.ORB.Limit}
}

// Strategy names accepted by the `strategy` config key and the backtester's
// -strategy flag.
const (
	StrategyORB      = "orb"
	StrategyDailyRev = "dailyrev"
	StrategyGapFade  = "gapfade"
	StrategyDonchian = "donchian"
)

func DefaultGapFadeConfig() GapFadeConfig {
	return GapFadeConfig{
		Timeframe:        "5m",
		CSVFile:          "ind_nifty200list.csv",
		Limit:            200,
		MinGapDownPct:    5.0,
		MaxGapDownPct:    20.0,
		ObserveEndMin:    9*60 + 30,
		EntryWindowEnd:   11 * 60,
		ATRPeriod:        14,
		SLATR:            2.0,
		RewardRisk:       2.0,
		MaxStopPct:       5.0,
		StopAtDayLow:     boolPtr(true),
		RequireAboveVWAP: boolPtr(true),
		MinPrice:         50,
		GateFailOpen:     boolPtr(false),
		MaxConcurrent:    5,
	}
}

func DefaultDonchianConfig() DonchianConfig {
	return DonchianConfig{
		Timeframe:        "5m",
		CSVFile:          "indices.csv",
		Limit:            2,
		DonchianLookback: 30,
		ATRPeriod:        9,
		MinATRPct:        0.03,

		BreakoutBufferPoints: 2.0,
		UseIgnition:          boolPtr(true),
		IgnitionATRMult:      1.0,

		SLATRMult:    3.0,
		TrailATRMult: 3.0,

		EntryStartMin:  9*60 + 30,
		EntryCutoffMin: 14*60 + 31,
		SquareOffMin:   14*60 + 57,
		// Sized for the indices this strategy is specified on: enough for one
		// SENSEX unit at ~78,000 and still far below the 5L the backtester
		// funds an account with, above which SimBroker silently drops longs.
		MaxCapitalPerTrade:    150000,
		RiskPct:               0.5,
		MaxDailyLossPct:       2.0,
		MaxConcurrent:         5,
		MaxEntriesPerSymbol:   4,
		TargetDelta:           0.80,
		TargetDeltaNearExpiry: 0.90,
		MinDaysToExpiry:       intPtr(2),
		// Measured best of a 32-cell period x threshold sweep (2026-08-27).
		// A mild filter is the whole of the effect: it drops ~6% of entries and
		// still produces MORE total gross bps than no filter at all, in both
		// windows. Tightening it further only fits the tuning window - see the
		// ADX sweep section in CLAUDE.md.
		ADXThreshold:        15,
		ADXPeriod:           14,
		AllowShort:          boolPtr(true),
		ExitOnOppositeBreak: boolPtr(true),
	}
}

func DefaultUpstoxConfig() UpstoxConfig {
	return UpstoxConfig{
		ISINCSV:           "ind_nifty500list.csv",
		NewsLookbackHours: 48,
		MaxProfitDropPct:  25,
		MaxResultsAgeDays: 180,
		TimeoutSeconds:    5,
	}
}

func DefaultDailyRevConfig() DailyRevConfig {
	return DailyRevConfig{
		Timeframe:     "day",
		CSVFile:       "ind_nifty200list.csv",
		Limit:         200,
		TrendPeriod:   200,
		RSIPeriod:     2,
		ExitSMAPeriod: 5,
		ATRPeriod:     14,
		RSIEntry:      10,
		StopATR:       2.5,
		MinTargetATR:  0.5,
		MinPrice:      50,
		MaxHoldDays:   10,
		MaxConcurrent: 5,
	}
}

func DefaultRiskConfig() RiskConfig {
	return RiskConfig{
		MaxDailyLoss:      5000,
		MaxTradesPerDay:   10,
		MaxTradesPerStock: 0,
	}
}

func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		MinBalance:         3000,
		MinCapitalPerTrade: 30000,
		MaxCapitalPerTrade: 50000,
		TradeCutoffMin:     14*60 + 5,
	}
}

func DefaultORBConfig() ORBConfig {
	return ORBConfig{
		Timeframe:              "5m",
		CSVFile:                "high_beta_stocks.csv",
		Limit:                  50,
		EntryWindowEnd:         10*60 + 30,
		RSILongThreshold:       50,
		RSIShortThreshold:      40,
		ADXThreshold:           20,
		MinRangeATR:            1.0,
		MaxRangeATR:            3.0,
		SLMultiplier:           1.5,
		TargetMultiplier:       3.0,
		MaxConcurrent:          5,
		RelVolThreshold:        1.5,
		VolThrustMult:          1.0,
		MaxVWAPDistATR:         1.5,
		MaxVWAPDistPct:         1.5,
		BodyStrengthThreshold:  0.6,
		MaxGapPct:              3.0,
		ADXRisingEps:           0,
		OneTradePerDay:         boolPtr(true),
		StopFloorAtRange:       boolPtr(true),
		PartialExitAtRMultiple: 0,
		PartialExitPct:         0.5,
	}
}

func boolPtr(b bool) *bool { return &b }

func floatPtr(f float64) *float64 { return &f }

func intPtr(i int) *int { return &i }

func LoadConfig(path string) (*Config, error) {
	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("failed to load config file: %w", err)
	}

	if config.APIKey == "" || config.APISecret == "" {
		return nil, fmt.Errorf("api_key and api_secret are required in config file")
	}

	if config.UptrendOnly == nil {
		t := true
		config.UptrendOnly = &t
	}

	if config.Strategy == "" {
		config.Strategy = "orb"
	}

	// Risk defaults
	riskD := DefaultRiskConfig()
	if config.Risk.MaxDailyLoss == 0 {
		config.Risk.MaxDailyLoss = riskD.MaxDailyLoss
	}
	if config.Risk.MaxTradesPerDay == 0 {
		config.Risk.MaxTradesPerDay = riskD.MaxTradesPerDay
	}
	// MaxTradesPerStock intentionally defaults to 0 (disabled); no fill needed.

	// Engine defaults
	engD := DefaultEngineConfig()
	if config.Engine.MinBalance == 0 {
		config.Engine.MinBalance = engD.MinBalance
	}
	if config.Engine.MinCapitalPerTrade == 0 {
		config.Engine.MinCapitalPerTrade = engD.MinCapitalPerTrade
	}
	if config.Engine.MaxCapitalPerTrade == 0 {
		config.Engine.MaxCapitalPerTrade = engD.MaxCapitalPerTrade
	}
	if config.Engine.TradeCutoffMin == 0 {
		config.Engine.TradeCutoffMin = engD.TradeCutoffMin
	}

	// ORB defaults
	orbD := DefaultORBConfig()
	if config.ORB.Timeframe == "" {
		config.ORB.Timeframe = orbD.Timeframe
	}
	if config.ORB.CSVFile == "" {
		config.ORB.CSVFile = orbD.CSVFile
	}
	if config.ORB.Limit == 0 {
		config.ORB.Limit = orbD.Limit
	}
	if config.ORB.EntryWindowEnd == 0 {
		config.ORB.EntryWindowEnd = orbD.EntryWindowEnd
	}
	if config.ORB.RSILongThreshold == 0 {
		config.ORB.RSILongThreshold = orbD.RSILongThreshold
	}
	if config.ORB.RSIShortThreshold == 0 {
		config.ORB.RSIShortThreshold = orbD.RSIShortThreshold
	}
	if config.ORB.ADXThreshold == 0 {
		config.ORB.ADXThreshold = orbD.ADXThreshold
	}
	if config.ORB.MinRangeATR == 0 {
		config.ORB.MinRangeATR = orbD.MinRangeATR
	}
	if config.ORB.MaxRangeATR == 0 {
		config.ORB.MaxRangeATR = orbD.MaxRangeATR
	}
	if config.ORB.SLMultiplier == 0 {
		config.ORB.SLMultiplier = orbD.SLMultiplier
	}
	if config.ORB.TargetMultiplier == 0 {
		config.ORB.TargetMultiplier = orbD.TargetMultiplier
	}
	if config.ORB.MaxConcurrent == 0 {
		config.ORB.MaxConcurrent = orbD.MaxConcurrent
	}
	if config.ORB.RelVolThreshold == 0 {
		config.ORB.RelVolThreshold = orbD.RelVolThreshold
	}
	if config.ORB.VolThrustMult == 0 {
		config.ORB.VolThrustMult = orbD.VolThrustMult
	}
	// MaxVWAPDistATR and ADXRisingEps default to 0 (disabled/strict), so no fill needed.
	if config.ORB.MaxVWAPDistPct == 0 {
		config.ORB.MaxVWAPDistPct = orbD.MaxVWAPDistPct
	}
	if config.ORB.BodyStrengthThreshold == 0 {
		config.ORB.BodyStrengthThreshold = orbD.BodyStrengthThreshold
	}
	if config.ORB.MaxGapPct == 0 {
		config.ORB.MaxGapPct = orbD.MaxGapPct
	}
	if config.ORB.OneTradePerDay == nil {
		config.ORB.OneTradePerDay = orbD.OneTradePerDay
	}
	if config.ORB.StopFloorAtRange == nil {
		config.ORB.StopFloorAtRange = orbD.StopFloorAtRange
	}

	// DailyReversal defaults. Same zero-means-absent convention as above: a
	// knob set to 0 in TOML gets the default back, so disable a threshold with
	// an extreme value rather than 0 (see CLAUDE.md).
	drD := DefaultDailyRevConfig()
	if config.DailyRev.Timeframe == "" {
		config.DailyRev.Timeframe = drD.Timeframe
	}
	if config.DailyRev.CSVFile == "" {
		config.DailyRev.CSVFile = drD.CSVFile
	}
	if config.DailyRev.Limit == 0 {
		config.DailyRev.Limit = drD.Limit
	}
	if config.DailyRev.TrendPeriod == 0 {
		config.DailyRev.TrendPeriod = drD.TrendPeriod
	}
	if config.DailyRev.RSIPeriod == 0 {
		config.DailyRev.RSIPeriod = drD.RSIPeriod
	}
	if config.DailyRev.ExitSMAPeriod == 0 {
		config.DailyRev.ExitSMAPeriod = drD.ExitSMAPeriod
	}
	if config.DailyRev.ATRPeriod == 0 {
		config.DailyRev.ATRPeriod = drD.ATRPeriod
	}
	if config.DailyRev.RSIEntry == 0 {
		config.DailyRev.RSIEntry = drD.RSIEntry
	}
	if config.DailyRev.StopATR == 0 {
		config.DailyRev.StopATR = drD.StopATR
	}
	if config.DailyRev.MinTargetATR == 0 {
		config.DailyRev.MinTargetATR = drD.MinTargetATR
	}
	if config.DailyRev.MinPrice == 0 {
		config.DailyRev.MinPrice = drD.MinPrice
	}
	if config.DailyRev.MaxHoldDays == 0 {
		config.DailyRev.MaxHoldDays = drD.MaxHoldDays
	}
	if config.DailyRev.MaxConcurrent == 0 {
		config.DailyRev.MaxConcurrent = drD.MaxConcurrent
	}

	// GapFade defaults. Same zero-means-absent convention as above: a knob set
	// to 0 in TOML gets the default back.
	gfD := DefaultGapFadeConfig()
	if config.GapFade.Timeframe == "" {
		config.GapFade.Timeframe = gfD.Timeframe
	}
	if config.GapFade.CSVFile == "" {
		config.GapFade.CSVFile = gfD.CSVFile
	}
	if config.GapFade.Limit == 0 {
		config.GapFade.Limit = gfD.Limit
	}
	if config.GapFade.MinGapDownPct == 0 {
		config.GapFade.MinGapDownPct = gfD.MinGapDownPct
	}
	if config.GapFade.MaxGapDownPct == 0 {
		config.GapFade.MaxGapDownPct = gfD.MaxGapDownPct
	}
	if config.GapFade.ObserveEndMin == 0 {
		config.GapFade.ObserveEndMin = gfD.ObserveEndMin
	}
	if config.GapFade.EntryWindowEnd == 0 {
		config.GapFade.EntryWindowEnd = gfD.EntryWindowEnd
	}
	if config.GapFade.ATRPeriod == 0 {
		config.GapFade.ATRPeriod = gfD.ATRPeriod
	}
	if config.GapFade.SLATR == 0 {
		config.GapFade.SLATR = gfD.SLATR
	}
	if config.GapFade.RewardRisk == 0 {
		config.GapFade.RewardRisk = gfD.RewardRisk
	}
	if config.GapFade.MaxStopPct == 0 {
		config.GapFade.MaxStopPct = gfD.MaxStopPct
	}
	if config.GapFade.MinPrice == 0 {
		config.GapFade.MinPrice = gfD.MinPrice
	}
	if config.GapFade.MaxConcurrent == 0 {
		config.GapFade.MaxConcurrent = gfD.MaxConcurrent
	}
	if config.GapFade.StopAtDayLow == nil {
		config.GapFade.StopAtDayLow = gfD.StopAtDayLow
	}
	if config.GapFade.RequireAboveVWAP == nil {
		config.GapFade.RequireAboveVWAP = gfD.RequireAboveVWAP
	}
	if config.GapFade.GateFailOpen == nil {
		config.GapFade.GateFailOpen = gfD.GateFailOpen
	}

	// Donchian defaults.
	dcD := DefaultDonchianConfig()
	if config.Donchian.Timeframe == "" {
		config.Donchian.Timeframe = dcD.Timeframe
	}
	if config.Donchian.CSVFile == "" {
		config.Donchian.CSVFile = dcD.CSVFile
	}
	if config.Donchian.Limit == 0 {
		config.Donchian.Limit = dcD.Limit
	}
	if config.Donchian.DonchianLookback == 0 {
		config.Donchian.DonchianLookback = dcD.DonchianLookback
	}
	if config.Donchian.ATRPeriod == 0 {
		config.Donchian.ATRPeriod = dcD.ATRPeriod
	}
	if config.Donchian.MinATRPct == 0 {
		config.Donchian.MinATRPct = dcD.MinATRPct
	}
	if config.Donchian.BreakoutBufferPoints == 0 {
		config.Donchian.BreakoutBufferPoints = dcD.BreakoutBufferPoints
	}
	if config.Donchian.IgnitionATRMult == 0 {
		config.Donchian.IgnitionATRMult = dcD.IgnitionATRMult
	}
	if config.Donchian.UseIgnition == nil {
		config.Donchian.UseIgnition = dcD.UseIgnition
	}
	if config.Donchian.SLATRMult == 0 {
		config.Donchian.SLATRMult = dcD.SLATRMult
	}
	if config.Donchian.TrailATRMult == 0 {
		config.Donchian.TrailATRMult = dcD.TrailATRMult
	}
	if config.Donchian.EntryStartMin == 0 {
		config.Donchian.EntryStartMin = dcD.EntryStartMin
	}
	if config.Donchian.EntryCutoffMin == 0 {
		config.Donchian.EntryCutoffMin = dcD.EntryCutoffMin
	}
	if config.Donchian.SquareOffMin == 0 {
		config.Donchian.SquareOffMin = dcD.SquareOffMin
	}
	if config.Donchian.MaxCapitalPerTrade == 0 {
		config.Donchian.MaxCapitalPerTrade = dcD.MaxCapitalPerTrade
	}
	if config.Donchian.RiskPct == 0 {
		config.Donchian.RiskPct = dcD.RiskPct
	}
	if config.Donchian.MaxDailyLossPct == 0 {
		config.Donchian.MaxDailyLossPct = dcD.MaxDailyLossPct
	}
	if config.Donchian.MaxConcurrent == 0 {
		config.Donchian.MaxConcurrent = dcD.MaxConcurrent
	}
	if config.Donchian.TargetDelta == 0 {
		config.Donchian.TargetDelta = dcD.TargetDelta
	}
	if config.Donchian.TargetDeltaNearExpiry == 0 {
		config.Donchian.TargetDeltaNearExpiry = dcD.TargetDeltaNearExpiry
	}
	// nil -> default; an explicit min_days_to_expiry = 0 survives.
	if config.Donchian.MinDaysToExpiry == nil {
		config.Donchian.MinDaysToExpiry = dcD.MinDaysToExpiry
	}
	if config.Donchian.ADXPeriod == 0 {
		config.Donchian.ADXPeriod = dcD.ADXPeriod
	}
	if config.Donchian.MaxEntriesPerSymbol == 0 {
		config.Donchian.MaxEntriesPerSymbol = dcD.MaxEntriesPerSymbol
	}
	if config.Donchian.AllowShort == nil {
		config.Donchian.AllowShort = dcD.AllowShort
	}
	if config.Donchian.ExitOnOppositeBreak == nil {
		config.Donchian.ExitOnOppositeBreak = dcD.ExitOnOppositeBreak
	}

	// Upstox gate defaults. AccessToken and BlockKeywords are intentionally
	// left as given: an empty token means "no gate configured", and an empty
	// keyword list falls back to the built-in one inside the gate.
	upD := DefaultUpstoxConfig()
	if config.Upstox.ISINCSV == "" {
		config.Upstox.ISINCSV = upD.ISINCSV
	}
	if config.Upstox.NewsLookbackHours == 0 {
		config.Upstox.NewsLookbackHours = upD.NewsLookbackHours
	}
	if config.Upstox.MaxProfitDropPct == 0 {
		config.Upstox.MaxProfitDropPct = upD.MaxProfitDropPct
	}
	if config.Upstox.MaxResultsAgeDays == 0 {
		config.Upstox.MaxResultsAgeDays = upD.MaxResultsAgeDays
	}
	if config.Upstox.TimeoutSeconds == 0 {
		config.Upstox.TimeoutSeconds = upD.TimeoutSeconds
	}

	pathD := DefaultPathsConfig()
	if config.Paths.DBPath == "" {
		config.Paths.DBPath = pathD.DBPath
	}
	if config.Paths.LogDir == "" {
		config.Paths.LogDir = pathD.LogDir
	}

	if config.PaperCapital == 0 {
		config.PaperCapital = 1000000.0 // 10 Lakhs INR default virtual capital
	}

	return &config, nil
}
