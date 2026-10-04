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
