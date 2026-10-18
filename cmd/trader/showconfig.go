package main

import (
	"fmt"
	"io"
	"log"
	"sort"
	"strings"

	"zerobha/internal/config"
)

// printConfig writes a readable summary of the effective configuration (after
// defaults) to w: mode, capital, strategies, allocation, engine and risk
// limits, then each active strategy's own section. Credentials are reported
// as set or missing, never printed.
func printConfig(w io.Writer, cfg *config.Config, configPath string, paper bool) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	rs := func(n int) string {
		if n == 0 {
			return "off"
		}
		return fmt.Sprintf("Rs%d", n)
	}

	p("=== ZEROBHA CONFIG (%s) ===", configPath)
	if paper {
		p("Mode              : PAPER (simulated fills, live data)")
		p("Capital           : Rs%.0f (paper_capital)   option spread %.0f ticks", cfg.PaperCapital, cfg.PaperOptionSpreadTicks)
	} else {
		p("Mode              : LIVE (real orders)")
		p("Capital           : the Kite account balance at start-up")
	}
	p("Strategies        : %s (priority order)", strings.Join(cfg.ActiveStrategies(), ", "))
	if len(cfg.Allocation) > 0 {
		names := make([]string, 0, len(cfg.Allocation))
		for n := range cfg.Allocation {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = fmt.Sprintf("%s %.0f%%", n, cfg.Allocation[n])
		}
		p("Allocation        : %s", strings.Join(parts, ", "))
	} else {
		p("Allocation        : none (every strategy shares the whole balance)")
	}
	p("Uptrend only      : %v", *cfg.UptrendOnly)

	e := cfg.Engine
	maxConc := fmt.Sprint(e.MaxConcurrent)
	if e.MaxConcurrent == 0 {
		maxConc = "sum of the strategies' caps"
	}
	p("")
	p("--- ENGINE ---")
	p("  Capital/Trade   : Rs%d – Rs%d   Min Balance: Rs%d", e.MinCapitalPerTrade, e.MaxCapitalPerTrade, e.MinBalance)
	p("  Max Concurrent  : %s   Trade Cutoff: %02d:%02d", maxConc, e.TradeCutoffMin/60, e.TradeCutoffMin%60)

	r := cfg.Risk
	p("--- RISK (account-wide) ---")
	p("  Max Daily Loss  : %s   Max Monthly Loss: %s", rs(r.MaxDailyLoss), rs(r.MaxMonthlyLoss))
	p("  Max Loss/Stock  : %s (per underlying, per day)", rs(r.MaxLossPerStockPerDay))
	p("  Max Trades      : %d per day, %d per stock (0 = off)", r.MaxTradesPerDay, r.MaxTradesPerStock)

	p("--- PATHS / CREDENTIALS ---")
	p("  DB / Logs       : %s / %s", cfg.Paths.DBPath, cfg.Paths.LogDir)
	p("  Kite key/secret : %s / %s   Upstox token: %s", setOrMissing(cfg.APIKey), setOrMissing(cfg.APISecret), setOrMissing(cfg.UpstoxAccessToken))
	p("  Telegram        : token %s, chat %s", setOrMissing(cfg.Telegram.Token), setOrMissing(cfg.Telegram.ChatID))
	p("")

	// The per-strategy detail already exists as log lines; route them here
	// without timestamps.
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(w)
	log.SetFlags(0)
	for _, name := range cfg.ActiveStrategies() {
		logStrategyConfig(cfg, name)
		p("")
	}
	log.SetOutput(prevOut)
	log.SetFlags(prevFlags)
}

func setOrMissing(s string) string {
	if strings.TrimSpace(s) == "" {
		return "MISSING"
	}
	return "set"
}
