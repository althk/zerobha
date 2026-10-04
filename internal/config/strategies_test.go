package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestActiveStrategies(t *testing.T) {
	t.Run("single strategy key still works", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, `
api_key = "k"
api_secret = "s"
strategy = "emacross"
`))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.ActiveStrategies(); !reflect.DeepEqual(got, []string{"emacross"}) {
			t.Errorf("ActiveStrategies = %v, want [emacross]", got)
		}
	})

	t.Run("list wins over the single key, order kept", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, `
api_key = "k"
api_secret = "s"
strategy = "orb"
strategies = ["emacross", "orb"]
`))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.ActiveStrategies(); !reflect.DeepEqual(got, []string{"emacross", "orb"}) {
			t.Errorf("ActiveStrategies = %v, want [emacross orb]", got)
		}
		if got := cfg.StrategySettingsFor("emacross").Timeframe; got != cfg.EMACross.Timeframe {
			t.Errorf("emacross timeframe = %q, want its own section's %q", got, cfg.EMACross.Timeframe)
		}
	})

	for _, tc := range []struct{ list, want string }{
		{`["emacross", "orbb"]`, "unknown strategy"},
		{`["orb", "orb"]`, "listed twice"},
	} {
		_, err := LoadConfig(writeConfig(t, `
api_key = "k"
api_secret = "s"
strategies = `+tc.list+`
`))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("strategies = %s: err = %v, want %q", tc.list, err, tc.want)
		}
	}
}

func TestAllocation(t *testing.T) {
	load := func(body string) error {
		_, err := LoadConfig(writeConfig(t, `
api_key = "k"
api_secret = "s"
strategies = ["emacross", "orb"]
`+body))
		return err
	}
	if err := load("\n[allocation]\nemacross = 80\norb = 20\n"); err != nil {
		t.Errorf("80/20 should load, got %v", err)
	}
	if err := load(""); err != nil {
		t.Errorf("no allocation should load, got %v", err)
	}
	for _, tc := range []struct{ body, want string }{
		{"\n[allocation]\nemacross = 80\n", `no share for running strategy "orb"`},
		{"\n[allocation]\nemacross = 80\norb = 30\n", "more than the account"},
		{"\n[allocation]\nemacross = 80\norb = 20\ndonchian = 1\n", "not a running strategy"},
		{"\n[allocation]\nemacross = 100\norb = 0\n", "want a percent"},
	} {
		if err := load(tc.body); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.body, err, tc.want)
		}
	}
}
