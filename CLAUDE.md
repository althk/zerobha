# Zerobha

Intraday algorithmic trading system for NSE equities via Zerodha Kite. Go core
(live trader, backtester, data tools) plus Python scripts for stock screening.

**ORB (Opening Range Breakout) is the primary strategy**, plus `dailyrev`, a
daily short-term reversal added to test the lower-frequency thesis, and
`gapfade`, an intraday gap-down recovery trade gated on Upstox news and
earnings. Other
strategies existed and were removed in `6f8ef50` after backtests showed no edge
— see [Findings](#findings-do-not-re-derive) before proposing to add them back.
`dailyrev` also has no edge once benchmarked; it is kept as the worked example
of the benchmark discipline, not as a candidate to trade.

## Commands

```bash
go build ./... && go vet ./... && go test ./...

# Backtest (reads test/data/<timeframe>/<symbol>_real.csv)
go run ./cmd/backtest -csv high_beta_100.csv -timeframe 5minute -cost-bps 3

# Gap fade (intraday MIS; UNGATED in backtest - see the gapfade section)
go run ./cmd/backtest -strategy gapfade -csv ind_nifty200list.csv   -timeframe 5minute -limit 200 -cost-bps 3

# Daily short-term reversal (CNC, multi-day holds - note the higher cost-bps)
go run ./cmd/backtest -strategy dailyrev -csv ind_nifty200list.csv   -timeframe day -limit 200 -cost-bps 11

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
| `pkg/strategy/orb.go` | The intraday strategy |
| `pkg/strategy/dailyrev.go` | Daily short-term reversal (CNC, multi-day holds) |
| `pkg/strategy/gapfade.go` | Intraday gap-down recovery, gated on news/earnings |
| `pkg/upstox/` | Read-only Upstox client + the news/earnings `core.NewsGate` |
| `pkg/broker/zerodha.go`, `sim.go` | Live and simulated brokers |
| `pkg/indicators/` | EMA, SMA, ATR, ADX, RSI, VWAP — all streaming/O(1) |
| `cmd/histdl/` | Kite historical downloader |
| `scripts/*.py` | Watchlist building, beta/sector screening, Yahoo data download |

A strategy implements `Name() / Init(DataProvider) / OnCandle(Candle) *Signal`.
Optional `SetDB(*db.Store)` gets injected by the trader for state that must
survive a mid-session restart.

`core.NewsGate` is the second injection point: `Assess(symbol, asOf)` returns a
`GateVerdict{Allow, Reason}`. `gapfade` consults it before entering; the trader
injects `pkg/upstox.Gate`, the backtester injects nil. Live trading supports
`orb` and `gapfade`; `dailyrev` is backtest-only (CNC positions would be
squared off the same day).

## Configuration

- `config.toml` — committed template, empty credentials.
- `config.local.toml` — real credentials and tuned values. Gitignored via `*.local*`.
  This is the backtester's **default** `-config`.

**`api_key`/`api_secret` must appear above the first `[section]` header.** TOML
assigns bare keys to the preceding table, so credentials placed after `[risk]`
silently become `engine.api_key` and the config fails to load. This was a real
bug in `config.toml`, fixed in `6f8ef50`.

`upstox_access_token` is a bare key too, and TOML needs the value **quoted** —
an unquoted JWT fails to parse at the first `.`. It is a long-lived (~1 year)
read-only token, unrelated to the Kite credentials; the `[upstox]` section holds
only the gate's policy knobs.

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

**MIS and CNC have completely different cost structures — do not reuse
`-cost-bps 3` for a delivery strategy.** Intraday STT is 0.025% sell-side only;
delivery STT is 0.1% on *both* legs. With stamp duty and exchange charges a CNC
round trip is **≈22 bps (`-cost-bps 11`), roughly 4x intraday**. Costing
`dailyrev` at 3 bps overstated its net PnL by 42% (₹129k → ₹75k).

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
- **Benchmark against unconditional drift, always.** A long-only strategy in a
  rising market produces a large, highly significant t-stat while having no
  skill whatsoever. Before believing any long-only result, compute the mean
  N-day forward return of the same universe over the same window and compare
  it to the strategy's *gross* per-trade return. `dailyrev` hit t = 4.49 on
  5,016 trades and still failed this test. A t-stat measures "different from
  zero", which is the wrong null — the null is the market.
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
- **Relaxing `rel_vol_threshold` is not uniformly bad — the magnitude decides.**
  Disabling it (1.5 → ~1.0, tripling trade count) destroys the edge: −₹47/trade
  over 1,058 trades. But a *mild* 1.5 → 1.2 roughly doubles entries at
  approximately constant edge/trade, on both universes independently
  (high-beta +₹61 → +₹55; Nifty 200 −₹34 → +₹1). Do not generalize from the
  disabled-filter row to any relaxation.
- **Shortening ORB's target destroys expectancy.** Four independent comparisons
  agree: TP 5 → 4 ATR costs ~₹29/trade
  on high-beta and ~₹9/trade on Nifty 200 with the **win rate unchanged in both**
  — trades that reached 4 ATR were mostly going to reach 5, so the cut only
  truncates the right tail that pays for the stops. 1.5 SL : 1.5 TP (1:1) lifts
  the win rate just 1.4 points (52.9% → 54.3%) and turns +₹61/trade into
  −₹9/trade. The expectancy lives entirely in a few large winners; **keep
  `target_multiplier` at 5.**
- **`config.local.toml`'s ORB tuning is overfit** to its original watchlist. It
  beats shipped defaults on high-beta names and loses to them on Nifty 200.
- **The deleted intraday-breakout strategy matched a driftless random walk**:
  61.0% of entries touched +1 ATR before a 1.5-ATR stop against 60.0% theoretical;
  28.8% reached +3 ATR against 33.3% theoretical.
- ORB's `min_range_atr`, both RSI thresholds, and `one_trade_per_day` never bind
  on these universes — relaxing them changes nothing.

### ORB exit/filter sweep (2026-08-22, 270 sessions 2025-07-21 → 2026-08-21)

Trade counts here are **distinct entries** (partial-exit rows folded back into
their parent trade), so they run lower than the row counts the backtester prints
and lower than the table above. All other ORB knobs at `config.local.toml` values.

| Universe | rel_vol | SL:TP | Entries | Net | Edge/trade | Win% | PF | t |
|---|---|---|---|---|---|---|---|---|
| high-beta 100 | 1.5 | 2:5 | 34 | +₹2,062 | +₹61 | 52.9% | 1.26 | 0.53 |
| high-beta 100 | 1.2 | 2:5 | 64 | +₹3,533 | +₹55 | 57.8% | 1.25 | 0.73 |
| high-beta 100 | 1.2 | 2:4 | 64 | +₹1,693 | +₹26 | 57.8% | 1.12 | 0.38 |
| high-beta 100 | 1.5 | 2:4 | 34 | +₹746 | +₹22 | 52.9% | 1.09 | 0.21 |
| high-beta 100 | 1.5 | 1.5:1.5 | 35 | −₹322 | −₹9 | 54.3% | 0.96 | −0.11 |
| Nifty 200 | 1.5 | 2:5 | 83 | −₹2,817 | −₹34 | 53.0% | 0.87 | −0.54 |
| Nifty 200 | 1.2 | 2:5 | 138 | +₹81 | +₹1 | 55.1% | 1.00 | 0.01 |
| Nifty 200 | 1.2 | 2:4 | 138 | −₹1,148 | −₹8 | 55.1% | 0.97 | −0.18 |

Best combination found is **rel_vol 1.2 with 2:5**, and both of its legs replicate
across the two universes. It still does not constitute an edge: t = 0.73 on
high-beta (the universe with known selection lookahead — `high_beta_100.csv` was
built from price data overlapping this window) and t = 0.01 on Nifty 200, which
has no such leak and says flatly zero.

### Daily short-term reversal (`dailyrev`) — tested 2026-08-22, no edge

Rule set: close > SMA(200), RSI(2) < 10, target = SMA(5) at entry, stop = 2.5
ATR, 10-session time stop. Long only — the Indian cash segment does not permit
carrying a short equity position overnight, so this rule set has no symmetric
short side. Universe Nifty 200, 1,223 sessions (2021-09-17 → 2026-08-21),
5,016 trades, avg hold 7 calendar days, avg notional only ₹6,726 (the 1%-risk
sizer against a wide daily ATR stop produces small positions).

| Costing | Net | Edge/trade | Win% | PF | t |
|---|---|---|---|---|---|
| 3 bps (wrong — intraday) | +₹129,159 | +₹25.7 | 69.6% | 1.29 | 7.73 |
| 11 bps (correct — delivery) | +₹75,060 | +₹15.0 (22.2 bps) | 69.6% | 1.16 | 4.49 |

**t = 4.49 and it is still not an edge.** The unconditional mean 5-day forward
return of the same 200 names over the same window is **+51.8 bps gross**. The
strategy's gross per-trade return is 22.2 + 22 = **44.2 bps** — *below* random
entry. Buying any of these names on any day and holding a week beat the
strategy's carefully filtered entries, which then paid costs on top. Selection
alpha is negative; 100% of the positive PnL is market drift.

The yearly split makes the same point — the strategy's edge tracks the market's
drift almost exactly, and both decay to negative together:

| Year | Strategy edge/trade | t | Unconditional 5-day drift |
|---|---|---|---|
| 2022 | +₹31.4 | 3.07 | +75.4 bps |
| 2023 | +₹35.9 | 5.64 | +88.9 bps |
| 2024 | +₹11.4 | 1.93 | +57.3 bps |
| 2025 | +₹8.5 | 1.20 | +24.9 bps |
| 2026 | −₹24.5 | −2.25 | +11.2 bps |

Two further biases, both inflating the result that already fails: the universe
is *today's* Nifty 200 membership applied to 2021 (index-membership lookahead,
the CLAUDE.md selection-lookahead trap), and entries fill at the signal bar's
close, which requires MOC execution.

Also note the backtester runs each symbol against its own ₹5L account, so 200
symbols can hold positions simultaneously with no shared capital constraint.
Average concurrent exposure was ~29 positions; a single account with
`max_concurrent = 5` would take a fraction of these signals. **The ₹75k total is
not reachable in one account** — only the per-trade edge is meaningful, and it
is negative against benchmark.

**`partial_exit_at_r_multiple` must stay below `target_multiplier`** or the
partial book is effectively dead — at 2.0 against a 1.5 target it fired on only
5 of 35 trades, and only where a single candle spanned both levels (`sim.go`
checks the partial before the target).

If pursuing this further: the cost structure argues for lower-frequency, larger-
target trades (daily bars, multi-day holds), where ₹24–32 of cost is negligible
rather than 100%+ of expectancy. `histdl -interval day -days 1800` pulls 7 years
of daily candles quickly.

### Intraday gap fade (`gapfade`) — added 2026-08-24, unvalidated

Rule set: open <= −5% vs previous close (and > −20%, above which it is usually
a corporate action); observe 09:15–09:30 and take its high as the reclaim
level; enter up to 11:00 on the first candle closing above that high, above
its own open, and above session VWAP; stop = 2 ATR below entry floored at the
session low (rejected if wider than 5% of price); target = 2 x the realised
stop distance (1:2). MIS, long only, one entry per symbol per day.

Before entering, the strategy consults `core.NewsGate`. The live gate
(`pkg/upstox`) blocks when a headline from the last 48h matches a damaging
keyword, or the latest reported quarter is loss-making or down more than 25%
year-on-year. The premise is that big gaps split into informed (keep going)
and panic (revert), and price alone cannot tell them apart.

**The gate is not backtestable.** Upstox's news and fundamentals endpoints
serve current state with no as-of-date parameter, so a backtest can only run
ungated — fading every qualifying gap, informed ones included. The backtester
prints a banner saying so on every gapfade run. Treat any backtest number as
the no-information floor, never as an estimate of the gated strategy.

First ungated run, Nifty 200, 271 sessions, 3 bps:

| Trades | Net | Edge/trade | Win% | PF | t |
|---|---|---|---|---|---|
| 52 | +₹1,471 | +₹28 | 46.2% | 1.27 | 0.65 |

t = 0.65 on 52 trades is no result, exactly as CLAUDE.md predicts for this
sample size — 5% gaps are rare enough that 271 sessions x 200 names produces
only ~52 setups. Two things must happen before this is believed:

1. **Sample.** Pull more history (`histdl -interval 5minute -days 1800`) and/or
   widen the universe; target >=100 trades.
2. **Benchmark.** It is long-only, so a positive number proves nothing on its
   own. Compare the gross per-trade return against the unconditional mean
   intraday (open-to-1513) return of gap-down names over the same window —
   the same test `dailyrev` failed at t = 4.49.

The gated version cannot be measured this way at all. Measuring it means
recording gate verdicts live, forward, from the day the strategy runs.

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
