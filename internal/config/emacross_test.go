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
		if cfg.EMACross.SLATRMult != 3.0 {
			t.Errorf("expected SLATRMult 3.0, got %v", cfg.EMACross.SLATRMult)
		}
		if cfg.EMACross.ExitOnOppositeCross == nil || *cfg.EMACross.ExitOnOppositeCross {
			t.Error("expected the opposite-cross exit to default OFF")
		}
		if cfg.EMACross.TrailMult() != 3.0 {
			t.Errorf("expected default trail 3.0, got %v", cfg.EMACross.TrailMult())
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
		if cfg.EMACross.MinDaysToExpiry == nil || *cfg.EMACross.MinDaysToExpiry != 1 {
			t.Errorf("expected default MinDaysToExpiry 1, got %v", cfg.EMACross.MinDaysToExpiry)
		}
		cfg, err = LoadConfig(writeConfig(t, base+"min_days_to_expiry = 0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.EMACross.MinDaysToExpiry == nil || *cfg.EMACross.MinDaysToExpiry != 0 {
			t.Errorf("expected explicit MinDaysToExpiry 0 to survive, got %v", cfg.EMACross.MinDaysToExpiry)
		}
	})

	// The tp_rr / trail_atr_mult trap: an explicit 0 must turn the trail off
	// rather than being read as "absent" and replaced by the default.
	t.Run("trail_atr_mult = 0 disables the trail", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"trail_atr_mult = 0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.EMACross.TrailMult() != 0 {
			t.Errorf("explicit trail_atr_mult = 0 should disable the trail, got %v", cfg.EMACross.TrailMult())
		}
	})
}
