package config

import "testing"

// Three srlevels knobs have a meaningful zero — no target, no room gate, no
// per-level cap — so each has to survive LoadConfig rather than being replaced
// by its default. As plain values they could not: zero is indistinguishable
// from absent, and a run configured to switch one off would silently measure
// the default instead, with nothing in the output saying so. That has now
// happened twice in this repo (tp_rr, trail_atr_mult); this pins the third.
func TestSRLevelsZeroMeaningKnobs(t *testing.T) {
	base := `
strategy = "srlevels"
api_key = "k"
api_secret = "s"

[srlevels]
`

	t.Run("explicit zero target survives", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"tp_atr_mult = 0.0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.SRLevels.TPATRMult == nil {
			t.Fatal("explicit 0.0 was replaced by the default")
		}
		if got := cfg.SRLevels.TPMult(); got != 0 {
			t.Errorf("TPMult() = %v, want 0", got)
		}
	})

	t.Run("explicit zero trail survives", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"trail_atr_mult = 0.0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.SRLevels.TrailATRMult == nil {
			t.Fatal("explicit 0.0 was replaced by the default")
		}
		if got := cfg.SRLevels.SRTrailMult(); got != 0 {
			t.Errorf("SRTrailMult() = %v, want 0", got)
		}
	})

	t.Run("explicit zero room gate survives", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"min_room_atr = 0.0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := cfg.SRLevels.RoomATR(); got != 0 {
			t.Errorf("RoomATR() = %v, want 0", got)
		}
	})

	t.Run("explicit zero per-level cap survives", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base+"max_entries_per_level = 0\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := cfg.SRLevels.EntriesPerLevel(); got != 0 {
			t.Errorf("EntriesPerLevel() = %v, want 0 (uncapped)", got)
		}
	})

	t.Run("absent takes the defaults", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, base))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := cfg.SRLevels.TPMult(); got != DefaultSRTPATRMult {
			t.Errorf("TPMult() = %v, want %v", got, DefaultSRTPATRMult)
		}
		if got := cfg.SRLevels.RoomATR(); got != DefaultSRMinRoomATR {
			t.Errorf("RoomATR() = %v, want %v", got, DefaultSRMinRoomATR)
		}
		if got := cfg.SRLevels.EntriesPerLevel(); got != DefaultSRMaxEntriesPerLevel {
			t.Errorf("EntriesPerLevel() = %v, want %v", got, DefaultSRMaxEntriesPerLevel)
		}
		if got := cfg.SRLevels.SRTrailMult(); got != DefaultSRTrailATRMult {
			t.Errorf("SRTrailMult() = %v, want %v", got, DefaultSRTrailATRMult)
		}
	})

	// A nil pointer on a hand-built config (no TOML involved) must not read as
	// "off" — the strategy would silently lose its target and its per-level cap.
	t.Run("zero-value config takes the defaults", func(t *testing.T) {
		var c SRLevelsConfig
		if got := c.TPMult(); got != DefaultSRTPATRMult {
			t.Errorf("TPMult() on zero value = %v, want %v", got, DefaultSRTPATRMult)
		}
		if got := c.RoomATR(); got != DefaultSRMinRoomATR {
			t.Errorf("RoomATR() on zero value = %v, want %v", got, DefaultSRMinRoomATR)
		}
		if got := c.EntriesPerLevel(); got != DefaultSRMaxEntriesPerLevel {
			t.Errorf("EntriesPerLevel() on zero value = %v, want %v", got, DefaultSRMaxEntriesPerLevel)
		}
		// The trail is the only profit-taking exit this strategy has; a nil
		// pointer reading as "no trail" would silently remove it.
		if got := c.SRTrailMult(); got != DefaultSRTrailATRMult {
			t.Errorf("SRTrailMult() on zero value = %v, want %v", got, DefaultSRTrailATRMult)
		}
	})
}

// The engine bootstraps its universe from whichever section the selected
// strategy owns. A missing case here reads as "the strategy found no symbols",
// which looks like a data problem rather than a wiring one.
func TestSRLevelsActiveStrategySettings(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
strategy = "srlevels"
api_key = "k"
api_secret = "s"

[srlevels]
csv_file = "indices.csv"
limit = 2
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := cfg.ActiveStrategySettings()
	if got.CSVFile != "indices.csv" || got.Limit != 2 || got.Timeframe != "5m" {
		t.Errorf("ActiveStrategySettings() = %+v, want the srlevels section", got)
	}
}
