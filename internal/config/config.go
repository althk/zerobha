// Package config provides configuration loading functionality.
package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

type ORBConfig struct {
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
}

type Config struct {
	Strategy  string    `toml:"strategy"`
	CSVFile   string    `toml:"csv_file"`
	Limit     int       `toml:"limit"`
	Timeframe string    `toml:"timeframe"`
	APIKey    string    `toml:"api_key"`
	APISecret string    `toml:"api_secret"`
	ORB       ORBConfig `toml:"orb"`
}

func DefaultORBConfig() ORBConfig {
	return ORBConfig{
		EntryWindowEnd:    10*60 + 30,
		RSILongThreshold:  50,
		RSIShortThreshold: 40,
		ADXThreshold:      20,
		MinRangeATR:       1.0,
		MaxRangeATR:       3.0,
		SLMultiplier:      1.5,
		TargetMultiplier:  3.0,
		MaxConcurrent:     5,
		RelVolThreshold:   1.5,
	}
}

func LoadConfig(path string) (*Config, error) {
	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("failed to load config file: %w", err)
	}

	// Validate required fields
	if config.APIKey == "" || config.APISecret == "" {
		return nil, fmt.Errorf("api_key and api_secret are required in config file")
	}

	// Set defaults if empty
	if config.Strategy == "" {
		config.Strategy = "orb"
	}
	if config.CSVFile == "" {
		config.CSVFile = "high_beta_stocks.csv"
	}
	if config.Limit == 0 {
		config.Limit = 50
	}
	if config.Timeframe == "" {
		config.Timeframe = "5m"
	}

	// ORB defaults
	defaults := DefaultORBConfig()
	if config.ORB.EntryWindowEnd == 0 {
		config.ORB.EntryWindowEnd = defaults.EntryWindowEnd
	}
	if config.ORB.RSILongThreshold == 0 {
		config.ORB.RSILongThreshold = defaults.RSILongThreshold
	}
	if config.ORB.RSIShortThreshold == 0 {
		config.ORB.RSIShortThreshold = defaults.RSIShortThreshold
	}
	if config.ORB.ADXThreshold == 0 {
		config.ORB.ADXThreshold = defaults.ADXThreshold
	}
	if config.ORB.MinRangeATR == 0 {
		config.ORB.MinRangeATR = defaults.MinRangeATR
	}
	if config.ORB.MaxRangeATR == 0 {
		config.ORB.MaxRangeATR = defaults.MaxRangeATR
	}
	if config.ORB.SLMultiplier == 0 {
		config.ORB.SLMultiplier = defaults.SLMultiplier
	}
	if config.ORB.TargetMultiplier == 0 {
		config.ORB.TargetMultiplier = defaults.TargetMultiplier
	}
	if config.ORB.MaxConcurrent == 0 {
		config.ORB.MaxConcurrent = defaults.MaxConcurrent
	}
	if config.ORB.RelVolThreshold == 0 {
		config.ORB.RelVolThreshold = defaults.RelVolThreshold
	}

	return &config, nil
}
