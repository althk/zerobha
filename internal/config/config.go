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
	Strategy    string       `toml:"strategy"`
	APIKey      string       `toml:"api_key"`
	APISecret   string       `toml:"api_secret"`
	UptrendOnly *bool        `toml:"uptrend_only"`
	Risk        RiskConfig   `toml:"risk"`
	Engine      EngineConfig `toml:"engine"`
	ORB         ORBConfig    `toml:"orb"`
}

// ActiveStrategySettings returns the Timeframe, CSVFile, and Limit the engine
// bootstraps from. ORB is the only strategy, so these come from its section;
// the accessor stays so callers don't reach into ORBConfig directly.
func (c *Config) ActiveStrategySettings() StrategySettings {
	return StrategySettings{Timeframe: c.ORB.Timeframe, CSVFile: c.ORB.CSVFile, Limit: c.ORB.Limit}
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

	return &config, nil
}
