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
