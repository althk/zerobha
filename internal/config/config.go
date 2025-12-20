// Package config provides configuration loading functionality.
package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Strategy  string `toml:"strategy"`
	CSVFile   string `toml:"csv_file"`
	Limit     int    `toml:"limit"`
	Timeframe string `toml:"timeframe"`
	APIKey    string `toml:"api_key"`
	APISecret string `toml:"api_secret"`
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

	return &config, nil
}
