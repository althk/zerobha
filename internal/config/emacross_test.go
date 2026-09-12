package config

import "testing"

func TestEMACrossConfig_LoadConfig(t *testing.T) {
	base := `
strategy = "emacross"
api_key = "k"
api_secret = "s"

[emacross]
`

	t.Run("default configuration normalizes negative TP to 0", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.EMACross.TPATRMult != 0 {
			t.Errorf("expected default TPATRMult to be normalized to 0, got %v", cfg.EMACross.TPATRMult)
		}
		if cfg.EMACross.SLATRMult != 2.0 {
			t.Errorf("expected SLATRMult 2.0, got %v", cfg.EMACross.SLATRMult)
		}
		if cfg.EMACross.FastPeriod != 9 || cfg.EMACross.SlowPeriod != 21 {
			t.Errorf("expected Fast=9 Slow=21, got Fast=%d Slow=%d", cfg.EMACross.FastPeriod, cfg.EMACross.SlowPeriod)
		}
	})

	t.Run("explicit negative TP normalizes to 0", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"tp_atr_mult = -1.0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.EMACross.TPATRMult != 0 {
			t.Errorf("expected explicit -1.0 TP to normalize to 0, got %v", cfg.EMACross.TPATRMult)
		}
	})

	t.Run("explicit positive TP is preserved", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"tp_atr_mult = 5.0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.EMACross.TPATRMult != 5.0 {
			t.Errorf("expected explicit 5.0 TP to be preserved, got %v", cfg.EMACross.TPATRMult)
		}
	})

	t.Run("min_days_to_expiry: absent takes the default, explicit 0 survives", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.EMACross.MinDaysToExpiry == nil || *cfg.EMACross.MinDaysToExpiry != 0 {
			t.Errorf("expected default MinDaysToExpiry 0, got %v", cfg.EMACross.MinDaysToExpiry)
		}
		cfg, err = LoadConfig(writeConfig(t, base+"min_days_to_expiry = 2\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.EMACross.MinDaysToExpiry == nil || *cfg.EMACross.MinDaysToExpiry != 2 {
			t.Errorf("expected explicit MinDaysToExpiry 2, got %v", cfg.EMACross.MinDaysToExpiry)
		}
	})
}
