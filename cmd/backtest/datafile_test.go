package main

import "testing"

// The live symbol for an index is the Kite tradingsymbol, which carries a
// space ("NIFTY 50"), while the downloaders write "nifty50_real.csv". One
// universe CSV has to serve both the trader and the backtest, so the stem is
// derived by stripping spaces rather than by spelling the symbol differently.
func TestDataFileStem(t *testing.T) {
	cases := map[string]string{
		"NIFTY 50":   "nifty50",
		"NIFTY BANK": "niftybank",
		"SENSEX":     "sensex",
		"RELIANCE":   "reliance",
	}
	for sym, want := range cases {
		if got := dataFileStem(sym); got != want {
			t.Errorf("dataFileStem(%q) = %q, want %q", sym, got, want)
		}
	}
}
