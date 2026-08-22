# Zerobha

Intraday algorithmic trading system for NSE equities via Zerodha Kite. Go core
(live trader, backtester, data tools) plus Python scripts for stock screening.

**ORB (Opening Range Breakout) is the only strategy.** Other strategies existed
and were removed in `6f8ef50` after backtests showed no edge — see
[Findings](#findings-do-not-re-derive) before proposing to add them back.

## Commands

```bash
go build ./... && go vet ./... && go test ./...

# Backtest (reads test/data/<timeframe>/<symbol>_real.csv)
go run ./cmd/backtest -csv high_beta_100.csv -timeframe 5minute -cost-bps 3

# Download history from Kite (interactive login, historical add-on required)
go run ./cmd/histdl -csv ind_nifty200list.csv -interval 5minute -days 400

# Live trading (interactive Kite login; refuses to start outside 08:55–15:30 IST)
go run ./cmd/trader -config config.local.toml
```

Python scripts need the venv: `& 'C:\Users\Harish Kotha\.venv\Scripts\Activate.ps1'`

## Architecture

Live: Kite WebSocket ticks → `internal/core/aggregator.go` (builds candles) →
`Engine.Execute` → `Strategy.OnCandle` → `Signal` → risk checks → sizing →
`Broker.PlaceOrder`. Backtest replaces the feed with CSV replay and the broker
with `pkg/broker/sim.go`; the engine and strategy code are identical.

| Path | Role |
|---|---|
| `internal/core/engine.go` | Orchestration, capital allocation, GTT/partial exits, `SquareOff()` |
| `internal/core/interfaces.go` | `Strategy`, `Broker`, `DataProvider` contracts |
| `internal/core/sizer.go` | Position sizing: 1% risk per trade, capped by leverage/capital |
| `internal/config/config.go` | TOML config, defaults, `ActiveStrategySettings()` |
| `pkg/strategy/orb.go` | The strategy |
| `pkg/broker/zerodha.go`, `sim.go` | Live and simulated brokers |
| `pkg/indicators/` | EMA, SMA, ATR, ADX, RSI, VWAP — all streaming/O(1) |
| `cmd/histdl/` | Kite historical downloader |
| `scripts/*.py` | Watchlist building, beta/sector screening, Yahoo data download |

A strategy implements `Name() / Init(DataProvider) / OnCandle(Candle) *Signal`.
Optional `SetDB(*db.Store)` gets injected by the trader for state that must
survive a mid-session restart.

## Configuration

- `config.toml` — committed template, empty credentials.
- `config.local.toml` — real credentials and tuned values. Gitignored via `*.local*`.
  This is the backtester's **default** `-config`.

**`api_key`/`api_secret` must appear above the first `[section]` header.** TOML
assigns bare keys to the preceding table, so credentials placed after `[risk]`
silently become `engine.api_key` and the config fails to load. This was a real
bug in `config.toml`, fixed in `6f8ef50`.

**Setting a config knob to `0` does not disable it.** `LoadConfig` treats zero as
"absent" and substitutes the default. To effectively disable a threshold, pass an
extreme value (`rel_vol_threshold = 0.01`, `max_gap_pct = 99`).

## Market data

`test/data/<timeframe>/<symbol>_real.csv`, columns
`timestamp,open,high,low,close,volume` with RFC3339 IST timestamps. The whole
tree is gitignored — never commit candles.

The directory name must match the `-timeframe` flag exactly. Two sources coexist:

| Dir | Source | Coverage |
|---|---|---|
| `test/data/5m/` | `scripts/data_downloader.py` (Yahoo) | **60 days max, rolling** |
| `test/data/5minute/` | `cmd/histdl` (Kite) | Years; 400 days ≈ 271 sessions |

Yahoo's 60-day intraday cap is permanent and cannot be worked around by waiting —
at ORB's trade rate (~0.2–0.6 trades/day across 100–200 symbols) that yields
under 20 trades, which cannot distinguish an edge from noise. **Use `histdl` for
anything you intend to draw a conclusion from.**

Currently on disk: 271 sessions (2025-07-18 → 2026-08-21) of 5-minute candles for
`high_beta_100.csv` and the Nifty 200.

## Evaluating a strategy

`-cost-bps 3` applies 3 bps to *both legs'* notional, ≈ real Zerodha intraday
costs (brokerage + STT + exchange + GST + stamp). At ORB's typical ₹40–50k
position that is **₹24–32 per round trip**; at ₹190k positions it is ~₹113.
Always quote results net of costs — gross numbers have repeatedly been positive
for strategies that lose money.

`-trades-csv <path>` dumps every trade with pre-cost PnL for offline analysis
(per-trade edge, t-stat, regime splits, drawdown).

Rules learned the hard way:

- **Sample size.** 42 sessions is not enough. ORB looked like a winner on 42
  sessions (t = 2.31) and was indistinguishable from zero on 271 (t = 0.59).
  Target ≥250 sessions and ≥100 trades; treat t < 2 as "no result".
- **Multiple testing.** Trying 14 parameter variants on one window guarantees a
  best-looking one. The variant that topped a 42-session sweep flipped sign on
  271 sessions. Re-test any promising variant on a different sample before
  believing it.
- **Selection lookahead.** `watchlist.csv` and `high_beta_*.csv` are built from
  price data. Building them with data that overlaps the backtest window leaks the
  future — use `build_watchlist.py --as-of-date <start of test window>`.
- **Refresh all symbols before comparing.** Stale CSVs from earlier runs silently
  mix date ranges. Verify spans (first/last date per symbol) before trusting a
  cross-symbol result; a run where 45 of 100 symbols covered a different period
  produced entirely void numbers once.

## Findings (do not re-derive)

All net of 3 bps costs. High-beta = 100 names, β 1.4–2.1; both universes 271
sessions unless noted.

| Test | Trades | Net | Edge/trade | t |
|---|---|---|---|---|
| ORB tuned, high-beta 100 | 52 | +₹2,062 | +₹40 | 0.59 |
| ORB shipped defaults, Nifty 200 | 160 | +₹361 | +₹2 | 0.07 |
| ORB tuned, Nifty 200 | 125 | −₹2,817 | −₹23 | −0.58 |
| ORB, rel-vol filter disabled | 1,058 | −₹50,058 | −₹47 | −3.52 |
| CPR-VWAP, high-beta 100 (42 sessions) | 234 | −₹27,604 | −₹118 | −2.71 |
| Intraday breakout, high-beta 100 (42 sessions) | 1,377 | −₹104,114 | −₹76 | −4.23 |

- **No configuration of any strategy here has shown a real edge.** Best t-stat on
  an adequate sample is 1.01.
- **Loosening ORB's filters to get more trades always destroys the edge.** The
  available edge is roughly fixed; relaxing `rel_vol_threshold` (the binding
  filter — 1.5 → 1.0 roughly triples trade count) spreads it thinner and adds
  drawdown. Breadth (more symbols) scales trade count at constant edge/trade;
  filter relaxation does not.
- **`config.local.toml`'s ORB tuning is overfit** to its original watchlist. It
  beats shipped defaults on high-beta names and loses to them on Nifty 200.
- **The deleted intraday-breakout strategy matched a driftless random walk**:
  61.0% of entries touched +1 ATR before a 1.5-ATR stop against 60.0% theoretical;
  28.8% reached +3 ATR against 33.3% theoretical.
- ORB's `min_range_atr`, both RSI thresholds, and `one_trade_per_day` never bind
  on these universes — relaxing them changes nothing.

If pursuing this further: the cost structure argues for lower-frequency, larger-
target trades (daily bars, multi-day holds), where ₹24–32 of cost is negligible
rather than 100%+ of expectancy. `histdl -interval day -days 1800` pulls 7 years
of daily candles quickly.

## Gotchas

- **Zero-volume candles are real.** Kite emits them for halted/illiquid symbols;
  Yahoo omits them. They make session VWAP zero — guard any division by VWAP or
  candle range. This crashed both backtest and (latently) the live trader; see
  the guard in `orb.go` and `orb_zerovolume_test.go`.
- **The backtester never calls `Strategy.Init()`.** Strategies must self-seed from
  the replayed candle stream. ORB's `AvgMorningVol` baseline does this, so its
  first few sessions of any backtest are effectively skipped.
- `-start` in the backtester is a *warmup boundary*, not a filter: earlier candles
  are fed to the strategy but not traded. `-end` excludes candles outright.
- All session logic is IST (`istLocation` in `orb.go`, `pkg/nseutils`). Candle
  timestamps carry `+05:30`; convert before comparing clock times.
- Signals are `MIS`. The engine applies leverage from `zerodha-mis-margins.csv`
  and squares off at 15:13 IST; `SquareOff()` only touches MIS orders/positions,
  so a `CNC` signal would be held overnight.
- Kite login is interactive (browser → callback on **:9880**) for both `trader`
  and `histdl`. `histdl` caches the token in `.kite_token.json` (gitignored) for
  the day; the trader does not.
- `MaxConcurrent` gates every signal — if it is left at 0 for a strategy, the
  engine silently drops all trades.

## Conventions

- Conventional commits (`feat:`, `fix:`, `refactor:`), body explains *why*.
- Add a regression test for every bug fix; verify it fails without the fix.
- Prefer streaming/O(1) indicators updated once per candle; strategies own their
  per-symbol state and reset session state on date change.
- `decimal.Decimal` for all prices/quantities — never float64.
