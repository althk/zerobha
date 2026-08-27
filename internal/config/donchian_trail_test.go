package config

import "testing"

// trail_atr_mult = 0 is a meaningful setting — "no trail, keep the initial
// stop" — so it must survive LoadConfig. As a plain float64 it could not:
// zero was indistinguishable from absent, the 3.0 default was substituted,
// and a run configured to measure the trail-free case produced a trade list
// byte-identical to the default one with nothing in the output saying so.
func TestDonchianTrailATRMult(t *testing.T) {
	base := `
strategy = "donchian"
api_key = "k"
api_secret = "s"

[donchian]
`
	t.Run("explicit zero disables the trail", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"trail_atr_mult = 0.0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Donchian.TrailATRMult == nil {
			t.Fatal("explicit 0.0 was replaced by the default")
		}
		if got := cfg.Donchian.TrailMult(); got != 0 {
			t.Errorf("TrailMult() = %v, want 0", got)
		}
	})

	t.Run("absent takes the default", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := cfg.Donchian.TrailMult(); got != DefaultTrailATRMult {
			t.Errorf("TrailMult() = %v, want %v", got, DefaultTrailATRMult)
		}
	})

	t.Run("explicit value wins", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"trail_atr_mult = 3.5\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := cfg.Donchian.TrailMult(); got != 3.5 {
			t.Errorf("TrailMult() = %v, want 3.5", got)
		}
	})

	// A nil pointer on a hand-built config (no TOML involved) must not read as
	// "no trail" — the strategy would silently lose its only profit-taking exit.
	t.Run("zero-value config still trails", func(t *testing.T) {
		if got := (DonchianConfig{}).TrailMult(); got != DefaultTrailATRMult {
			t.Errorf("TrailMult() on zero value = %v, want %v", got, DefaultTrailATRMult)
		}
	})
}
