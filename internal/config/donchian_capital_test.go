package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes a throwaway TOML file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// The engine's cap is sized for cash equities. An index priced above it is not
// traded smaller, it is not traded at all — quantity floors to zero and every
// signal vanishes silently. Donchian therefore carries its own cap.
func TestDonchianMaxCapitalPerTrade(t *testing.T) {
	t.Run("explicit value wins", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, `
strategy = "donchian"
api_key = "k"
api_secret = "s"

[engine]
max_capital_per_trade = 50000

[donchian]
max_capital_per_trade = 250000
`))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Donchian.MaxCapitalPerTrade != 250000 {
			t.Errorf("donchian cap = %d, want 250000", cfg.Donchian.MaxCapitalPerTrade)
		}
		if cfg.Engine.MaxCapitalPerTrade != 50000 {
			t.Errorf("engine cap = %v, want it left alone at 50000", cfg.Engine.MaxCapitalPerTrade)
		}
	})

	t.Run("absent takes the donchian default, not the engine's", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, `
strategy = "donchian"
api_key = "k"
api_secret = "s"

[engine]
max_capital_per_trade = 50000

[donchian]
donchian_lookback = 30
`))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		want := DefaultDonchianConfig().MaxCapitalPerTrade
		if cfg.Donchian.MaxCapitalPerTrade != want {
			t.Errorf("donchian cap = %d, want the default %d", cfg.Donchian.MaxCapitalPerTrade, want)
		}
	})

	// The default has to clear the priciest index this strategy trades, or the
	// SENSEX side silently disappears; and stay under the 5L the backtester
	// funds an account with, above which SimBroker refuses longs and a
	// long/short strategy quietly becomes short-only.
	t.Run("default clears SENSEX and stays under the sim account", func(t *testing.T) {
		const sensexPrice = 78000
		const simAccount = 500000

		got := DefaultDonchianConfig().MaxCapitalPerTrade
		if got <= sensexPrice {
			t.Errorf("default cap %d does not afford one SENSEX unit at ~%d", got, sensexPrice)
		}
		if got >= simAccount {
			t.Errorf("default cap %d is not safely below the %d sim account", got, simAccount)
		}
	})
}
