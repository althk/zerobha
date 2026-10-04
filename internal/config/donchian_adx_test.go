package config

import "testing"

// adx_threshold's default of 15 never applied to a config without the key:
// LoadConfig's zero test cannot tell an absent key from an explicit 0, and 0
// is a real setting (gate off), so the field was left alone - and a backtest
// reading a config with no [donchian] section ran with the gate silently off.
func TestDonchianADXThresholdDefaultApplies(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       float64
	}{
		{"no section", "", 15},
		{"section without the key", "\n[donchian]\nlimit = 2\n", 15},
		{"explicit zero turns the gate off", "\n[donchian]\nadx_threshold = 0.0\n", 0},
		{"explicit value", "\n[donchian]\nadx_threshold = 20.0\n", 20},
	} {
		cfg, err := LoadConfig(writeConfig(t, "api_key = \"k\"\napi_secret = \"s\"\n"+tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cfg.Donchian.ADXThreshold != tc.want {
			t.Errorf("%s: adx_threshold = %v, want %v", tc.name, cfg.Donchian.ADXThreshold, tc.want)
		}
	}
}
