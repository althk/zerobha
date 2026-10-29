package kiteauth

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenFreshUntilTheNext0600IST(t *testing.T) {
	cases := []struct {
		issued, now string
		fresh       bool
	}{
		{"2026-10-29T08:00:00+05:30", "2026-10-29T15:30:00+05:30", true},
		{"2026-10-29T08:00:00+05:30", "2026-10-30T05:59:00+05:30", true},
		{"2026-10-29T08:00:00+05:30", "2026-10-30T06:00:00+05:30", false},
		// Issued before the morning reset: dies at 06:00 the same day.
		{"2026-10-29T05:30:00+05:30", "2026-10-29T06:30:00+05:30", false},
		// A UTC timestamp is converted to IST, not compared as wall time.
		{"2026-10-29T02:30:00Z", "2026-10-30T00:29:00Z", true},
	}
	for _, c := range cases {
		issued, _ := time.Parse(time.RFC3339, c.issued)
		now, _ := time.Parse(time.RFC3339, c.now)
		got := Token{AccessToken: "x", IssuedAt: issued}.Fresh(now)
		if got != c.fresh {
			t.Errorf("issued %s, now %s: fresh = %v, want %v", c.issued, c.now, got, c.fresh)
		}
	}
}

func TestTokenFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokenFileName)
	issued := time.Date(2026, 10, 29, 8, 0, 0, 0, ist)
	now := issued.Add(time.Hour)

	if _, err := LoadToken(path, "key", now); !errors.Is(err, ErrNoToken) {
		t.Fatalf("missing file: err = %v, want ErrNoToken", err)
	}
	if err := SaveToken(path, Token{APIKey: "key", AccessToken: "tok", IssuedAt: issued}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadToken(path, "key", now)
	if err != nil || got.AccessToken != "tok" {
		t.Fatalf("LoadToken = %+v, %v", got, err)
	}
	if _, err := LoadToken(path, "other-key", now); !errors.Is(err, ErrNoToken) {
		t.Errorf("other app key: err = %v, want ErrNoToken", err)
	}
	if _, err := LoadToken(path, "key", now.Add(24*time.Hour)); !errors.Is(err, ErrNoToken) {
		t.Errorf("next day: err = %v, want ErrNoToken", err)
	}
}
