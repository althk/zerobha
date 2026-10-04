package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/pkg/broker"
	"zerobha/pkg/db"
	"zerobha/pkg/strategy"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// defaultSquareOffMin is the account-wide MIS flatten, 15:13 IST.
const defaultSquareOffMin = 15*60 + 13

// liveStrategies are the strategies cmd/trader can run. dailyrev is CNC
// research whose positions the square-off would close the same day, and
// srlevels has nothing measured that justifies option execution; both are
// backtest-only.
var liveStrategies = map[string]bool{
	config.StrategyORB:      true,
	config.StrategyGapFade:  true,
	config.StrategyDonchian: true,
	config.StrategyEMACross: true,
}

// checkLiveStrategies fails before the interactive login, rather than after
// it, on a strategy the trader cannot run or a timeframe it cannot parse.
func checkLiveStrategies(cfg *config.Config) error {
	for _, name := range cfg.ActiveStrategies() {
		if !liveStrategies[name] {
			return fmt.Errorf("live trading supports strategy %q, %q, %q or %q, got %q (backtest-only: go run ./cmd/backtest -strategy %s)",
				config.StrategyORB, config.StrategyGapFade, config.StrategyDonchian, config.StrategyEMACross, name, name)
		}
		if _, err := time.ParseDuration(cfg.StrategySettingsFor(name).Timeframe); err != nil {
			return fmt.Errorf("%s: invalid timeframe: %w", name, err)
		}
	}
	return nil
}

// strategyDeps is what building a live strategy needs from the trader.
type strategyDeps struct {
	cfg     *config.Config
	store   *db.Store
	isPaper bool
	im      *broker.InstrumentManager
	kc      *kiteconnect.Client
}

// liveRunner is one configured strategy, ready to trade: the engine runner
// plus what the trader wires around it (its candle timeframe, watchlist and
// flatten time).
type liveRunner struct {
	runner       *core.Runner
	timeframe    time.Duration
	watchlist    []string
	squareOffMin int
}

// buildRunner constructs the named strategy with its own entry settings. Each
// setting a strategy used to impose on the whole engine — its entry cutoff,
// capital cap and uptrend exemption — is the runner's now, so strategies
// sharing an account do not overwrite one another's.
func buildRunner(name string, d strategyDeps) (*liveRunner, error) {
	cfg := d.cfg
	ss := cfg.StrategySettingsFor(name)
	tf, err := time.ParseDuration(ss.Timeframe)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid timeframe: %w", name, err)
	}
	watchlist := loadWatchlist(name, ss)

	r := &core.Runner{
		Timeframe:      tf.String(),
		Symbols:        make(map[string]bool, len(watchlist)),
		TradeCutoffMin: cfg.Engine.TradeCutoffMin,
		UptrendOnly:    *cfg.UptrendOnly,
	}
	for _, sym := range watchlist {
		r.Symbols[sym] = true
	}
	squareOffMin := defaultSquareOffMin

	switch name {
	case config.StrategyORB:
		orb := strategy.NewORBStrategy(watchlist, cfg.ORB)
		// Inject DB (Manual Dependency Injection) so opening ranges and
		// per-day state survive a mid-session restart.
		orb.SetDB(d.store)
		r.Strategy = orb
		r.MaxConcurrent = cfg.ORB.MaxConcurrent

	case config.StrategyGapFade:
		gate, err := buildUpstoxGate(cfg)
		if err != nil {
			return nil, fmt.Errorf("gapfade needs the Upstox news/earnings gate: %w", err)
		}
		r.Strategy = strategy.NewGapFadeStrategy(watchlist, cfg.GapFade, gate)
		r.MaxConcurrent = cfg.GapFade.MaxConcurrent

	case config.StrategyDonchian:
		dc := strategy.NewDonchianStrategy(watchlist, cfg.Donchian)
		dc.SetDB(d.store, d.isPaper)
		minDTE := 2
		if cfg.Donchian.MinDaysToExpiry != nil {
			minDTE = *cfg.Donchian.MinDaysToExpiry
		}
		dc.SetOptionExecution(buildOptionExecutor("Donchian", optionExecParams{
			TargetDelta:           cfg.Donchian.TargetDelta,
			TargetDeltaNearExpiry: cfg.Donchian.TargetDeltaNearExpiry,
			MinDaysToExpiry:       minDTE,
			FallbackIV:            cfg.Donchian.FallbackIV,
		}, d.im, d.kc))
		r.Strategy = dc
		r.MaxConcurrent = cfg.Donchian.MaxConcurrent
		// The engine's global cutoff is earlier than Donchian's own
		// last-entry time, so it has to give way to it.
		r.TradeCutoffMin = cfg.Donchian.EntryCutoffMin + 1
		// The engine's cap is sized for cash equities, and an index priced
		// above it is not traded smaller, it is not traded at all.
		r.MaxCapitalPerTrade = cfg.Donchian.MaxCapitalPerTrade
		// The NIFTY uptrend filter blocks every signal on a down day, not just
		// the longs. On a symmetric long/short strategy it turns off half the
		// strategy on exactly the days the short side exists to trade.
		if r.UptrendOnly {
			log.Println("Donchian: disabling the NIFTY uptrend filter — it would gate the short side out of existence")
			r.UptrendOnly = false
		}
		squareOffMin = cfg.Donchian.SquareOffMin
		log.Printf("Donchian: [donchian] max_daily_loss_pct is not used live; the daily limit is [risk] max_daily_loss, shared by every strategy")

	case config.StrategyEMACross:
		ec := strategy.NewEMACrossStrategy(watchlist, cfg.EMACross)
		ec.SetDB(d.store, d.isPaper)
		// asset_type = "options" expresses the INDEX signal through a weekly
		// contract; without this the strategy would send MIS orders for
		// "NIFTY 50" itself, which no exchange accepts.
		if strings.EqualFold(cfg.EMACross.AssetType, "options") {
			minDTE := 0
			if cfg.EMACross.MinDaysToExpiry != nil {
				minDTE = *cfg.EMACross.MinDaysToExpiry
			}
			nearExpiry := cfg.EMACross.TargetDeltaNearExpiry
			if nearExpiry == 0 {
				nearExpiry = cfg.EMACross.TargetDelta
			}
			ec.SetOptionExecution(buildOptionExecutor("EMACross", optionExecParams{
				TargetDelta:           cfg.EMACross.TargetDelta,
				TargetDeltaNearExpiry: nearExpiry,
				MinDaysToExpiry:       minDTE,
				FallbackIV:            cfg.EMACross.FallbackIV,
			}, d.im, d.kc))
		}
		r.Strategy = ec
		r.MaxConcurrent = cfg.EMACross.MaxConcurrent
		// +1 so a bar STARTING at the cutoff minute is still admitted: the
		// engine gates on EndTime, and the backtester uses the same +1.
		if cfg.EMACross.EntryCutoffMin > 0 {
			r.TradeCutoffMin = cfg.EMACross.EntryCutoffMin + 1
		}
		r.MaxCapitalPerTrade = cfg.EMACross.MaxCapitalPerTrade
		if r.UptrendOnly && (cfg.EMACross.AllowShort == nil || *cfg.EMACross.AllowShort) {
			log.Println("EMACross: disabling the NIFTY uptrend filter for symmetric long/short trading")
			r.UptrendOnly = false
		}
		if cfg.EMACross.SquareOffMin > 0 {
			squareOffMin = cfg.EMACross.SquareOffMin
		}

	default:
		return nil, fmt.Errorf("strategy %q is not supported live", name)
	}

	if r.MaxCapitalPerTrade > 0 {
		log.Printf("%s: max capital per trade Rs%d (overrides [engine] Rs%d)",
			r.Name(), r.MaxCapitalPerTrade, int64(cfg.Engine.MaxCapitalPerTrade))
	}
	return &liveRunner{runner: r, timeframe: tf, watchlist: watchlist, squareOffMin: squareOffMin}, nil
}

// loadWatchlist reads a strategy's symbol CSV, capped at its limit.
func loadWatchlist(name string, ss config.StrategySettings) []string {
	watchlist, err := loadSymbolsFromCSV(ss.CSVFile)
	if err != nil {
		log.Printf("WARNING: %s: failed to load %s: %v. Using fallback list.", name, ss.CSVFile, err)
		watchlist = []string{"IDEA", "CANBK", "LTF", "NBCC", "RELIANCE"}
	}
	if ss.Limit > 0 && len(watchlist) > ss.Limit {
		log.Printf("%s: limiting watchlist to top %d symbols.", name, ss.Limit)
		watchlist = watchlist[:ss.Limit]
	}
	log.Printf("%s: %d symbols on %s candles", name, len(watchlist), ss.Timeframe)
	return watchlist
}

// squareOffPlan splits the day's flattening between the account and the
// strategies: the account-wide square-off runs at the latest strategy's time,
// and a strategy that flattens earlier closes only its own positions then.
// With one strategy that is exactly its own time, as before.
func squareOffPlan(runners []*liveRunner) (globalMin int, early map[int][]string) {
	globalMin = 0
	for _, lr := range runners {
		if lr.squareOffMin > globalMin {
			globalMin = lr.squareOffMin
		}
	}
	if globalMin == 0 {
		globalMin = defaultSquareOffMin
	}
	early = map[int][]string{}
	for _, lr := range runners {
		if lr.squareOffMin < globalMin {
			early[lr.squareOffMin] = append(early[lr.squareOffMin], lr.runner.Name())
		}
	}
	return globalMin, early
}
