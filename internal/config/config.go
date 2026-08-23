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

type Config struct {
	Strategy  string `toml:"strategy"`
	APIKey    string `toml:"api_key"`
	APISecret string `toml:"api_secret"`
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
	}
	return StrategySettings{Timeframe: c.ORB.Timeframe, CSVFile: c.ORB.CSVFile, Limit: c.ORB.Limit}
}

// Strategy names accepted by the `strategy` config key and the backtester's
// -strategy flag.
const (
	StrategyORB      = "orb"
	StrategyDailyRev = "dailyrev"
	StrategyGapFade  = "gapfade"
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

	return &config, nil
}
