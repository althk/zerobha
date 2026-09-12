package main

import (
	"testing"
	"time"
)

// The data tree names the same bar size two ways: test/data/5m (Yahoo, Go
// duration syntax) and test/data/5minute (Kite/Upstox naming). parseDuration
// used to fall back to one hour for the second spelling, which stamped every
// Candle.EndTime an hour late and — because Engine.Execute gates entries on
// EndTime — moved the effective entry cutoff an hour earlier than configured.
func TestParseDurationUnderstandsBothTimeframeSpellings(t *testing.T) {
	cases := map[string]time.Duration{
		"5m":       5 * time.Minute,
		"5minute":  5 * time.Minute,
		"15minute": 15 * time.Minute,
		"1minute":  time.Minute,
		"day":      24 * time.Hour,
		"1d":       24 * time.Hour,
	}
	for tf, want := range cases {
		if got := parseDuration(tf); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", tf, got, want)
		}
	}
}

// Every config file spells the bar size "5m" (cmd/trader parses it as a Go
// duration) while the Kite/Upstox data lands in test/data/5minute, so the
// data lookup has to try both spellings.
func TestTimeframeDirsTriesBothSpellings(t *testing.T) {
	cases := map[string][]string{
		"5m":      {"5m", "5minute"},
		"5minute": {"5minute", "5m"},
		"1m":      {"1m", "1minute", "minute"},
		"day":     {"day", "1d"},
		"weird":   {"weird"},
	}
	for tf, want := range cases {
		got := timeframeDirs(tf)
		if len(got) != len(want) {
			t.Errorf("timeframeDirs(%q) = %v, want %v", tf, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("timeframeDirs(%q) = %v, want %v", tf, got, want)
				break
			}
		}
	}
}
