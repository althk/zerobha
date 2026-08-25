# Zerobha

Intraday algorithmic trading system for NSE equities via Zerodha Kite. Go core
(live trader, backtester, data tools) plus Python scripts for stock screening.

**ORB (Opening Range Breakout) is the primary strategy**, plus `dailyrev`, a
daily short-term reversal added to test the lower-frequency thesis, and
`gapfade`, an intraday gap-down recovery trade gated on Upstox news and
earnings, and `donchian`, an intraday channel breakout specified on NSE stock
futures (long and short). Other
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

# Download index history from Upstox (no login, no token, no entitlement)
go run ./cmd/idxdl -index nifty  -from 2023-09-01 -to 2026-08-21
go run ./cmd/idxdl -index sensex -from 2023-09-01 -to 2026-08-21

# Donchian channel breakout on the INDEX chart (the signal leg)
go run ./cmd/backtest -strategy donchian -csv indices.csv   -timeframe 5minute -limit 2 -cost-bps 0 -trades-csv idx.csv

# Price that trade list on real expired weekly-option premium candles
# (needs upstox_access_token + an Upstox Plus plan; caches to test/data/options)
go run ./cmd/optbt -trades idx.csv -underlying nifty -itm 150   -cost-bps 30 -spread-pct 0.25 -split 2025-09-01

# Same, choosing the strike by delta and skipping expiry day and the day before
# (-lots matters: brokerage is flat per order, so the edge improves to ~10 lots)
go run ./cmd/optbt -trades idx.csv -underlying nifty -delta 0.8   -lots 10 -min-dte 2 -spread-pct 0.25 -split 2025-09-01

# Daily short-term reversal (CNC, multi-day holds - note the higher cost-bps)
go run ./cmd/backtest -strategy dailyrev -csv ind_nifty200list.csv   -timeframe day -limit 200 -cost-bps 11

# Download history from Kite (interactive login, historical add-on required)
go run ./cmd/histdl -csv ind_nifty200list.csv -interval 5minute -days 400

# Live trading (interactive Kite login; refuses to start outside 08:55–15:30 IST)
go run ./cmd/trader -config config.local.toml

# Paper trading: same code path, real quotes and candles, simulated fills
go run ./cmd/trader -config config.local.toml -paper
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
| `pkg/strategy/donchian.go` | Intraday channel breakout, long+short, futures |
| `pkg/broker/futures.go` | NFO stock-futures universe: rollover + liquidity gate |
| `pkg/indicators/donchian.go` | Donchian channel, excluding the current bar |
| `pkg/upstox/` | Read-only Upstox client + the news/earnings `core.NewsGate` |
| `pkg/broker/zerodha.go`, `sim.go` | Live and simulated brokers |
| `pkg/broker/paper.go` | Paper broker: live data, simulated fills, own resting stops |
| `pkg/indicators/` | EMA, SMA, ATR, ADX, RSI, VWAP — all streaming/O(1) |
| `cmd/histdl/` | Kite historical downloader |
| `cmd/idxdl/` | Upstox index-history downloader (anonymous, no entitlement) |
| `cmd/optbt/` | Prices an index trade list on real expired weekly-option candles |
| `pkg/options/` | Black-Scholes, strike selection by delta, expiry calendar |
| `pkg/broker/optionchain.go` | The Kite instrument dump as an `options.Chain` |
| `pkg/strategy/donchian_options.go` | Index signal -> option order, index-driven exits |
| `scripts/*.py` | Watchlist building, beta/sector screening, Yahoo data download |

A strategy implements `Name() / Init(DataProvider) / OnCandle(Candle) *Signal`.
Optional `SetDB(*db.Store)` gets injected by the trader for state that must
survive a mid-session restart.

`core.ExitAdvisor` is the third strategy-side hook, alongside `SetDB` and
`SetContracts`: an optional interface whose `ExitAdvice(candle)` closes an open
position for a reason no resting order can express (Donchian's opposite-band
break). The engine type-asserts for it, calls it **before** every entry gate —
the entry cutoff and risk limits decide whether a position may be *opened*, not
whether one may be *closed* — and acts only if a position on the named side
exists. Closing goes through `core.PositionCloser` when the broker implements it
(the simulator does), otherwise via a counter market order.

`core.NewsGate` is the second injection point: `Assess(symbol, asOf)` returns a
`GateVerdict{Allow, Reason}`. `gapfade` consults it before entering; the trader
injects `pkg/upstox.Gate`, the backtester injects nil. Live trading supports `orb`, `gapfade` and `donchian`; `dailyrev` is
backtest-only (CNC positions would be squared off the same day).

`donchian` is the one strategy whose **signal instrument and traded instrument
differ**: the signal is computed on the NIFTY / SENSEX index chart and the
position is taken in a weekly option. `strategy.OptionExecutor` is the seam.
`cmd/backtest` injects nothing and trades the index directly — which is what
every recorded donchian result measures — while `cmd/trader` injects
`broker.OptionExecutor`, so the same code path either trades the index or
translates each signal into a contract.

That split has one consequence that shapes the design: **no resting order on an
option can express a level on the index.** A stop at "NIFTY below 24,200" is not
a premium level — it moves with time, volatility and the index itself. So when
option execution is on, the strategy stops delegating its stop and trail to the
broker, tracks them itself against the index, and closes the option at market
through `core.ExitAdvisor`. The engine needed no changes for this: it already
calls `ExitAdvice` ahead of every entry gate, and the sizer already rounds to
whole lots from `Signal.LotSize`.

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
| Donchian breakout, Nifty 200 (cash) | 26,904 | −₹976,256 | −₹36 | −22.46 |
| CPR-VWAP, high-beta 100 (42 sessions) | 234 | −₹27,604 | −₹118 | −2.71 |
| Intraday breakout, high-beta 100 (42 sessions) | 1,377 | −₹104,114 | −₹76 | −4.23 |

- **No configuration of any strategy here has shown a real edge.** Best t-stat on
  an adequate sample is 1.01. `donchian` is the largest sample and the clearest
  result: t = −22.46 over 26,904 trades.
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

### Intraday gap fade (`gapfade`) — added 2026-08-24, no edge

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

Ungated runs, 271 sessions, 3 bps. The second widens the gap band to 3% and
shortens the observation window to a single candle (09:15–09:20), which is what
lifts the sample from 52 trades to 380:

| Universe | gap | observe | Trades | Net | Edge/trade | Win% | PF | t |
|---|---|---|---|---|---|---|---|---|
| Nifty 200 | ≥5% | 09:30 | 52 | +₹1,471 | +₹28 | 46.2% | 1.27 | 0.65 |
| Nifty 500 (257 with data) | ≥3% | 09:20 | 380 | +₹1,618 | +₹4.3 | 45.3% | 1.03 | 0.21 |

**The 380-trade run fails the drift benchmark outright.** Its gross per-trade
return is **−2.8 bps**, while buying *every* ≤−3% gap-down name at the 09:20
close and holding to 15:10 returns **+13.7 bps** gross over the same window
(n = 533, t = 1.37). The reclaim + VWAP filter subtracts ~16 bps against random
entry into the same population, then pays ~6 bps of costs on top. Selection
alpha is negative — the same verdict as `dailyrev`, reached at t = 0.21 instead
of t = 4.49 only because the sample is smaller.

Note the rupee PnL is positive (+₹1,618) while the return is negative: the
sizer put more capital behind the winners. Rupee totals are not the measure
here; per-trade return against benchmark is.

**The 1:2 target is structurally unreachable intraday.** Of 380 trades, 22 hit
target, 75 hit the stop, and **283 (74%) died at the 15:13 square-off**. The
stop is floored at the session low, which on a gap-down day sits a median 2.2%
below entry, so the 1:2 target is a median **+4.4%** move required before
15:13 from a ~10:00 entry. What the backtest actually measures is therefore
"buy the reclaim, exit at the close" — the target barely participates. Any
future work here should either cap the stop far tighter (breaking the day-low
floor) or drop the 1:2 framing for a VWAP/close-based exit.

The gated version cannot be measured this way at all. Measuring it means
recording gate verdicts live, forward, from the day the strategy runs.

### Intraday Donchian breakout (`donchian`) — added 2026-08-24, no edge

Spec: `donchian_intraday_spec.md`, with the knob values the user supplied on
top of it (they win wherever the two disagree; where the panel was silent the
spec's values stand).

Rule set: channel = highest high / lowest low of the last 4 completed 5-minute
bars **excluding the current one**; enter when the close clears it by 2 points,
between 09:30 and 14:31, on a bar whose volume ≥ 1.2x its 4-bar average, whose
range ≥ 1 ATR ("ignition"), in a name whose ATR ≥ 0.15% of price. Stop 1.3 ATR;
BREAKEVEN+ parks the stop at entry ±10 points once 1 ATR in profit; chandelier
trail 3 ATR from the best price since entry; no target (`tp_rr = 0` — the trail
is the exit); close beyond the opposite band exits at market; flatten 14:57.
Long and short, MIS, one entry per symbol per day, 0.5% risk per trade.

**Three things about this strategy are not testable on the data here**, and all
three flatter the backtest:

1. **It is specified on stock futures; the repo has only cash candles.** No
   basis, no roll cost, no lot-size granularity (the backtester sizes to the
   exact rupee, live sizing rounds down to whole lots and often to zero), and
   spot liquidity rather than the futures book. The backtester prints a banner
   saying so on every donchian run.
2. **The universe is built live, from the Kite instrument dump** (current-month
   stock futures, index futures excluded, rolled `rollover_days_before` expiry,
   prior-day volume ≥ `min_daily_volume`). A backtest uses whatever CSV it is
   given, so it does not test the rollover or the liquidity gate — those have
   unit tests in `pkg/broker/futures_test.go` instead.
3. **The daily-loss kill switch never trips in a backtest.** `Engine.Execute`
   never feeds realised PnL back to the risk manager (there is a TODO on the
   line), so `max_daily_loss_pct` is carried but inert. It is real in live
   trading only to the extent the risk manager's own PnL tracking is.

`cmd/trader` also force-disables the NIFTY uptrend filter for this strategy.
That filter blocks *every* signal on a down day, not just longs, so on a
symmetric long/short breakout it switches off the short side on exactly the days
the short side exists for.

Full run, Nifty 200 cash candles, 271 sessions, 3 bps both legs, shipped
defaults above (per-trade dump: `-trades-csv donchian_trades.csv`):

| Trades | Net | Edge/trade | Win% | PF | t |
|---|---|---|---|---|---|
| 26,904 | −₹976,256 | −₹36.29 | 48.6% | 0.71 | **−22.46** |

Decisively negative on a large sample, and the reason is structural rather than
marginal. **Gross return is +0.9 bps per trade** (t = 3.22) against a ~6 bps
round trip, so the rules find almost exactly nothing and then pay costs on it.
The payoff is symmetric — average win 34.0 bps, average loss 34.4 bps, at a
48.7% hit rate — which is the same pathology CLAUDE.md already records for
shortening ORB's target: **BREAKEVEN+ at 1 ATR truncates the right tail that a
breakout system needs to pay for its stops**, while the stop stays a full 1.3
ATR away. 87% of trades exit at the stop; the trail almost never gets to work.

Relaxing the volume filter (2026-08-24, same 271 sessions): `vol_mult`
1.5 → 1.2 **and** `vol_avg_bars` 20 → 4 together.

| Config | Trades | Net | Edge/trade | Gross/trade | PF | t |
|---|---|---|---|---|---|---|
| vol 1.5 / 20 bars (original default) | 26,904 | −₹976,256 | −₹36.29 | +0.9 bps | 0.71 | −22.46 |
| vol 1.2 / 4 bars (**shipped default**) | 30,419 | −₹1,144,503 | −₹37.62 | +0.7 bps | 0.69 | −25.65 |

Worse, and matching the two trade lists on (symbol, entry time) says why: the
13,270 entries the looser filter **admits** are worse than the ones it keeps
(−₹39.34 vs −₹36.30, gross 0.5 vs 0.9 bps). Note this is not ORB's "mild
relaxation is free" result — a 4-bar baseline is a different knob from the
multiplier, and it is the weaker filter of the two: on a breakout bar the last
four bars are already rising, so the bar is being compared against itself.

**The churn is far larger than the net trade count suggests** — 13,270 entries
gained against 9,755 *lost*, for a net of +3,515. With `max_entries_per_symbol
= 1`, a marginal early entry consumes the day's only slot and displaces the
better setup that would have come later, and the displaced trades were as good
as the ones kept (−₹36.26, gross 0.9 bps). Any filter change here is therefore
not additive; it reshuffles which trade of the day gets taken. Compare matched
trade lists, never just the totals.

Disabling BREAKEVEN+ (2026-08-24, `use_breakeven = false`, same 1.2/4 volume
filter). It only touches exits, so the entry list is **identical** to the row
above — a clean controlled comparison:

| Config | Trades | Net | Edge/trade | Gross/trade | Win% | PF | t |
|---|---|---|---|---|---|---|---|
| breakeven ON | 30,419 | −₹1,144,503 | −₹37.62 | +0.7 bps | 51.3% | 0.69 | −25.65 |
| breakeven OFF | 30,419 | −₹973,056 | −₹31.99 | **+1.5 bps** | 34.5% | 0.79 | −15.06 |

This confirms the truncation diagnosis. Removing the breakeven **doubles gross
return per trade** and restores the right tail: average win 34.1 → 63.3 bps,
95th percentile 59.7 → 120.9 bps, largest win 1008 → 1609 bps. The win rate
collapses from 51% to 35% and that is the correct direction — a breakout system
is supposed to be mostly small losses paid for by a few large winners, and the
exit reasons flip with it (SL-HIT 25,816 → 14,743; opposite-band exits 4,545 →
14,862).

**It is still not tradeable, and the gap is now purely cost.** Gross +1.5 bps
against a ~6 bps intraday round trip. Nothing in the exit logic can close a 4x
gap; only a larger per-trade move can, which means a longer `donchian_lookback`
(4 bars = 20 minutes of range is noise, not a level), a higher timeframe, or
accepting far fewer trades. Do not read the improvement from −₹37.62 to −₹31.99
as progress towards profitability — it is progress towards a payoff shape whose
gross edge is still a quarter of its costs.

Adding a 1:2 target (2026-08-24, `tp_rr = 2`), tested at both volume settings:

| Config | Trades | Net | Edge/trade | Gross/trade | Win% | PF |
|---|---|---|---|---|---|---|
| no target, vol 1.2 | 30,419 | −₹973,056 | −₹31.99 | **+1.5 bps** | 31.2% | 0.79 |
| tp_rr 2, vol 1.2 | 30,419 | −₹1,125,085 | −₹36.99 | +0.7 bps | 32.7% | 0.76 |
| no target, vol 1.0 | 31,162 | −₹990,646 | −₹31.79 | +1.4 bps | 31.4% | 0.79 |
| tp_rr 2, vol 1.0 | 31,162 | −₹1,134,089 | −₹36.39 | +0.8 bps | 32.8% | 0.76 |

**The target halves gross edge, and the two volume settings confirm it
independently.** 7,542 trades reach it; the win rate gains ~1.5 points and buys
nothing. This is the third independent confirmation of one mechanism in this
strategy — BREAKEVEN+, then the 1:2 target, against a stop that stays at 1R.
**Every cap on the winning side costs more than it saves.** `vol_mult` 1.2 → 1.0
is roughly neutral on both (−₹31.99 vs −₹31.79; −₹36.99 vs −₹36.39).

Best configuration found: **no target, no breakeven, trail only** — and the
exit side is now exhausted. Nothing there can quadruple per-trade magnitude,
which is what closing a 1.5-vs-6 bps gap requires.

The drift benchmark is not the binding question here (this one runs both
directions — 15,491 long, 11,413 short — and its gross is ~0 either way), but
before believing any *positive* variant of this strategy, apply it anyway.

Next experiment, given the above: a longer `donchian_lookback`. Every exit
variant has now been tried and the payoff shape is fixed; what is missing is
per-trade magnitude, and 4 bars on a 5-minute chart is 20 minutes of range —
noise rather than a level. Expect far fewer trades, and apply the sample-size
and multiple-testing rules before believing any of it.

**The stop-advance rules will manufacture profit if you let them.** The very
first run of this strategy showed +₹238k on 10 symbols, PF 2.09, with nearly every
winner exiting at exactly entry + 10 points. The cause: BREAKEVEN+ parks the
stop at entry + 10 while the ATR trigger is ~4 points, so the stop landed above
the market and the next bar "filled" it at a price that never printed. The same
hole existed in the chandelier trail whenever a bar gave its high back. Capping
every stop advance at the bar's close (`broker.stopNoBetterThanMarket`, and the
same rule in `Engine.stopAfterTick` for live ticks) turned the same run into
**−₹49k, PF 0.78**. If a future change makes this strategy look good, check
this invariant first.

### Two config traps this strategy walked into (2026-08-24)

**A `float64` knob cannot distinguish "absent" from "0".** `tp_rr`'s default was
2 in `DefaultDonchianConfig`, and the fill block skipped it so an explicit
`tp_rr = 0.0` would survive — which meant a config omitting the key (every
backtest: `config.local.toml` has no `[donchian]` section) kept the zero value
and the default of 2 never applied. A whole backtest ran without the target it
was meant to be testing, and nothing in the output said so; it was caught only
because the exit-reason histogram had no TARGET-HIT rows. `TPRR` is now
`*float64`: nil → default, explicit 0.0 → no target. **Any knob whose zero is a
meaningful setting must be a pointer**, not a float with a special case.

**Check an indicator's readiness against the values you actually read.**
`donchian.ingest` asked `channel.IsReady()` after ingesting the bar while using
the channel values from before it, so on the bar that fills the window the
strategy compared a close against `[0, 0]` and every close clears that. It never
fired in a backtest — `ATR(14)` is not ready until bar 14, long after the 4-bar
channel fills, so `atr.IsPositive()` masked it — but it would bite immediately
for any `atr_period <= donchian_lookback`. Regression test:
`TestDonchianDoesNotTradeTheBarThatFillsTheChannel`.

**Strategy tests must pin the knobs they assert on.** These tests originally
inherited `DefaultDonchianConfig`, so tuning a default flipped three of them
into asserting the opposite of their own comments. `donchianTestConfig` now sets
every value it depends on explicitly.

### Donchian on the index, for option execution (2026-08-24)

The plan: compute the channel on the NIFTY/SENSEX 5-minute **index** chart and
execute via a ~150-point ITM weekly option, rather than on stock futures.

**Data is fully available and costs nothing to re-fetch.** `cmd/idxdl` pulls
index history from Upstox's v3 `historical-candle` endpoint, which serves
indices **anonymously** — no token, no entitlement. 2023-09-01 → 2026-08-21 is
~55,000 five-minute bars per index (~733 sessions), already on disk as
`test/data/5minute/nifty50_real.csv` and `sensex_real.csv`.

The **expired-option** endpoints need an Upstox Plus plan (`isPlusPlan` in the
token's JWT claims) and were verified working once the account was upgraded:
99 NIFTY weekly expiries and 98 SENSEX, back to 2024-10, with full chains
(NIFTY 105 CE strikes, lot 65; SENSEX 134 CE strikes, lot 20) and real 5-minute
premium candles. `pkg/upstox/history.go` wraps all three calls. Note the
account's *extended* token is restricted to a configured static IP for
**account** endpoints (`/user/profile` → UDAPI1221), but market-data and
expired-instrument endpoints are not IP-bound.

Three knobs are stock-calibrated and silently produce **zero trades** on an
index until changed:

| Knob | Stock value | Why it fails on an index |
|---|---|---|
| `min_atr_pct` | 0.15 | Index 5-min ATR is 0.076% (NIFTY) / 0.083% (SENSEX) of price, p90 0.11% — the floor rejects every bar |
| `vol_mult` | 1.2 | An index publishes no volume at all; the filter is *inapplicable*, not failed. The strategy now detects this itself (`sawVolume`) |
| `breakout_buffer_points` | 2.0 | 2 points is 0.11 ATR on NIFTY but 0.03 ATR on SENSEX — an absolute-points buffer is not comparable across instruments of different scale |

Result, trail-only config (no target, no breakeven), gross and **before any
costs**, 2023-09 → 2026-08:

| Index | Trades | Gross/trade | t | Win% | PF |
|---|---|---|---|---|---|
| NIFTY 50 | 730 | **−0.5 bps** | −0.70 | 35.2% | 0.93 |
| SENSEX | 730 | **−0.5 bps** | −0.69 | 33.3% | 0.93 |

**Two independent indices, ~733 sessions each, agree to the decimal: the signal
has no edge before costs.** This is the cheapest possible test of the option
plan and it settles it — an option is a levered, cost-laden claim on the same
move. A ~150-point ITM weekly runs roughly 0.65 delta, so it captures about
two-thirds of an index move while paying the full bid-ask spread, theta over
the hold, and per-order brokerage plus STT. Executing a signal that is flat
gross on the underlying can only lose after that. **Build the option-execution
layer to measure drag, not to look for an edge.**

### Donchian lookback sweep — the lever is dead (2026-08-24)

`donchian_lookback` was the last untested knob and the only one that changes
per-trade magnitude rather than redistributing it. Swept 4 → 60 on the index
charts, trail-only config, **gross** per-trade bps, selection window
2023-09..2025-08 with 2025-09..2026-08 held out.

| look | NIFTY IS | NIFTY OOS | SENSEX IS | SENSEX OOS | BANKNIFTY IS | BANKNIFTY OOS |
|---|---|---|---|---|---|---|
| 4 | +0.01 | −1.65 | +0.34 | −2.35 | +0.16 | −4.03 |
| 12 | +3.87 (t 2.49) | +0.39 | +2.72 (t 2.13) | −0.27 | — | — |
| 20 | +4.39 (t 2.79) | +0.10 | +3.62 (t 2.62) | +0.73 | +0.14 | +0.11 |
| 30 | +4.95 (t 2.93) | **+2.02** (t 1.15) | +4.21 (t 2.81) | +0.84 | +0.20 | +0.61 |
| 40 | +5.25 (t 2.86) | +1.80 | +4.70 (t 2.91) | −0.36 | +1.31 | −2.31 |
| 60 | +5.42 (t 2.65) | +0.51 | +5.20 (t 2.91) | −0.53 | +2.09 | −1.03 |

**In-sample, longer is monotonically better on both NIFTY and SENSEX, reaching
t ≈ 2.9. None of it survives.** Out of sample the whole curve collapses to
noise, and BANKNIFTY — which took no part in the selection — never shows the
pattern at all (+0.20 bps at lookback 30 against NIFTY's +4.95).

NIFTY and SENSEX are not independent evidence: they are the same large-cap
Indian market and move together, so agreeing in-sample is close to one
observation, not two. BANKNIFTY is the useful control and it says no.

**Read the direction of the effect, not its level.** At lookback 4 all three
instruments are clearly negative out of sample (−1.65, −2.35, −4.03 bps);
lengthening the channel walks that back toward zero and stops there. The knob
removes a cost of trading noise; it does not create an edge. Best out-of-sample
figure anywhere in the grid is NIFTY at 30: +2.02 bps, t = 1.15 — not
significant, and below the cost of trading an index option even if it were.

**What the sweep actually closed was one STRUCTURE, not the strategy.** Every
run above held the same skeleton fixed — one entry per instrument per day, no
flip, no re-entry, exit on the opposite band of the same channel, flat by 14:57
— because those came from the v1 spec's non-goals. Parameters were swept; the
skeleton never was. See the next section: two of those fixed choices were doing
most of the damage.

### The stop and the entry cap were the damage (2026-08-24)

Exit-reason breakdown, NIFTY lookback 30, gross bps:

| Exit | n | mean bps | median hold |
|---|---|---|---|
| SL-HIT | 495 | −3.51 | 110 min |
| EOD-SQUAREOFF | 91 | **+44.84** | 330 min |
| opposite break | 18 | +3.77 | 285 min |

**All of the money is in trades that survive long enough to run**, and only 91
of 604 got there — the 1.3 ATR initial stop killed 82% of them first. That is
the signature of a trend system strangled by a tight stop, not of a signal with
no edge.

Widening the stop and allowing re-entry improves **both** windows, which is the
opposite of what overfitting looks like:

| SL (ATR) | entries/day | IS bps | t | OOS bps | t |
|---|---|---|---|---|---|
| 1.3 | 1 | +4.95 | 2.93 | +2.02 | 1.15 |
| 3.0 | 4 | **+6.15** | 3.51 | **+3.95** | 1.88 |

Stops of 3, 4 and 6 ATR are identical because the 3-ATR chandelier trail is
binding — the initial stop never fires. So the honest reading is that the
initial stop was pure damage and the trail alone is the better exit.

**And the cost comparison used everywhere above is WRONG for options.** Index
bps were being compared against an equity-style round trip of ~6 bps. An option
is leveraged: measured on 2026-08-17, NIFTY spot 24,306, the ~150-point ITM
weekly call (24150 CE) traded at a premium of 184.05 with 156 intrinsic and
**28 of time value**, lot 65, so one lot costs ₹11,963. At ~0.65 delta the
leverage is `delta x spot / premium` = **85.8x**, and +3.95 index bps becomes
**+339 bps on the premium**.

That does not automatically make it profitable — the same denominator works
against you. Theta on 28 points of time value in expiry week can cost several
hundred bps of premium over a two-hour hold, and the bid-ask plus brokerage,
STT, exchange and GST run roughly 85-140 bps round trip. Both the edge and the
costs are large numbers on the premium scale, so **the sign cannot be argued,
it has to be measured on real option candles** — which is exactly what the
expired-contract data provides. Do not repeat the "index gross is below a 6 bps
round trip" reasoning; it does not apply to a leveraged instrument.

### The option-execution leg, measured (2026-08-24)

`cmd/optbt` closes the loop the index work was pointing at: it takes the
backtester's index trade list and prices every trade on the **real premium
candles of the weekly option that was actually listed at the time**. Nearest
weekly expiry on or after the entry date, listed strike closest to
`spot -/+ itm`, call for a long signal and put for a short (never short an
option), entry and exit at the close of the option's own 5-minute bar matching
the index bar the backtest used. Contracts cache to `test/data/options/`, so a
re-run costs nothing.

Two limits it cannot remove: an intrabar index stop prices at the **end** of the
bar it fired in rather than at the stop level (pessimistic for stops, and the
closest honest mapping bar data allows), and expired-contract history begins
**2024-10**, so of 837 NIFTY index trades only 544 could be priced. Unpriced
trades are reported, never silently dropped.

Signal leg, full entry window, gross, 733 sessions 2023-09 → 2026-08, config
lookback 30 / SL 3 ATR / trail-only / 4 entries per day:

| Index | Trades | Gross/trade | t | IS 2023-09..2025-08 | OOS 2025-09..2026-08 |
|---|---|---|---|---|---|
| NIFTY 50 | 837 | +4.1 bps | 3.53 | +5.56 (t 3.67) | +1.32 (t 0.75) |
| SENSEX | 859 | +3.8 bps | 3.52 | +4.85 (t 3.54) | +1.70 (t 1.00) |

Option leg, ~150 points ITM, costs 30 bps/leg (₹20/order brokerage + STT +
exchange + GST on premium) and a 0.25% half-spread each way:

| Index | n | Index bps | Premium GROSS | Premium NET | t | Net/lot | PF |
|---|---|---|---|---|---|---|---|
| NIFTY | 544 | +3.75 | +169.2 bps | +58.2 bps | 0.45 | −₹66 | 0.95 |
| SENSEX | 569 | +3.63 | +259.7 bps | +147.9 bps | 0.76 | +₹68 | 1.07 |
| NIFTY OOS | 281 | +1.36 | −39.4 bps | **−148.8 bps** | −0.86 | −₹265 | 0.83 |
| SENSEX OOS | 294 | +1.70 | +64.5 bps | **−45.8 bps** | −0.17 | +₹7 | 1.01 |

**Theta is not what kills this, and that is the one genuinely new fact here.**
Over a ~2 hour hold in a ~150-point ITM weekly, gross premium return is strongly
positive (+169 / +260 bps) — delta gain beats decay comfortably. What closes the
gap is the ~110 bps round-trip friction, and what decides it is that the index
edge itself does not survive out of sample. Full sample lands at t = 0.45 and
0.76; the held-out year is negative on both.

The leverage arithmetic from the previous section is confirmed rather than
refuted: +3.75 index bps really does become +169 premium bps (a ~45x realised
multiple on this sample). The problem is that the *dispersion* is multiplied
too, so a t of 3.5 on the index becomes a t of 0.45 on the option. **Leverage
does not manufacture significance — it amplifies edge and noise together.** Any
future index signal must clear significance on the index chart first; the option
wrapper cannot rescue one that does not.

Two knobs were swept and are settled:

- **Strike depth is not a lever** — see the delta sweep below. ATM (`-itm 0`)
  is the one clearly bad choice (gross **−15.3 bps**, net −124.9); everything
  from 0.6 delta to 0.95 delta lands within noise of zero. `-itm 600` and beyond
  look better but only 235 of 543 (and 19 of 543) trades can be priced, because
  the chain does not list strikes that deep every week — that is a biased
  subsample of wide-chain weeks, not a result.
- **Bid-ask.** Immaterial. Halving the assumed half-spread from 0.25% to 0.05%
  moves NIFTY from +58.2 to +98.6 bps and SENSEX from +147.9 to +188.7 — about
  10 bps per 0.05%, nowhere near the gap. The verdict is not spread-sensitive,
  so do not spend effort refining the spread assumption.

### Strike depth: delta targeting, and why it changes nothing (2026-08-24)

A fixed point offset is not a fixed delta. 150 points ITM on a NIFTY weekly is
~0.95 delta on expiry morning and ~0.68 six days out, because what sets delta is
the move in units of `sigma*sqrt(T)`, not in points. `optbt -delta 0.8` therefore
selects the strike properly: it backs the implied vol out of the **at-the-money
premium that actually traded in the entry bar** (bisection on Black-Scholes,
`cmd/optbt/blackscholes.go`), inverts `N(d1) = target` for the strike, and snaps
to the nearest listed one. Vol is measured, never assumed — weekly index IV runs
18.4% in the first half of this sample and 15.8% in the second, and a constant
would put the realised delta systematically off in one half or the other. Skew is
ignored (ATM vol applied to an ITM strike), which biases the strike slightly and
the P&L not at all, since the P&L still comes from that contract's real candles.

NIFTY, 542-544 priced trades, 30 bps/leg + 0.25% half-spread. "Realised delta"
and "time value" are measured at entry, averaged over the trades actually taken:

| Strike rule | Realised delta | Pts ITM | Time value | NET bps | t | OOS NET bps |
|---|---|---|---|---|---|---|
| `-itm 0` (ATM) | ~0.5 | 0 | all of it | −124.9 | −0.52 | −432.5 |
| `-delta 0.60` | 0.60 | 73 | 53% | −35.2 | −0.17 | −413.2 |
| `-delta 0.70` | 0.70 | 153 | 27% | +15.5 | 0.09 | −334.7 |
| `-itm 150` | **0.73** | 149 | 26% | +58.2 | 0.45 | −148.8 |
| `-delta 0.80` | 0.80 | 246 | 12% | −2.2 | −0.02 | −265.4 |
| `-delta 0.90` | 0.90 | 374 | 5% | −7.3 | −0.07 | −221.6 |
| `-delta 0.95` | 0.95 | 480 | 2% | +16.5 | 0.19 | −168.3 |

SENSEX at `-delta 0.80`: 551 trades, gross +248.7, net **+137.0 bps**, t = 0.99,
+₹229/lot, PF 1.18 — and OOS **−40.3 bps**, t = −0.21. Same verdict as NIFTY.

**Deeper strikes cut theta and leverage in the same proportion, so they cancel.**
That is the mechanism, and it is why the whole column sits on zero. Going from
0.6 to 0.95 delta drops time value from 53% of premium to 2% — theta almost
vanishes — but the premium paid rises from 156 to 490, so the same index move
buys proportionally less. Meanwhile **costs are a percentage of premium, so they
are ~110 bps at every strike**: nothing about strike selection changes the toll.
Only true ATM breaks the symmetry, and it breaks it the wrong way.

`-itm 150` topping the table (+58.2 against +15.5 for the 0.70 delta that buys an
almost identical 149-vs-153 point strike) is noise, not a finding: t = 0.45
against t = 0.09, and both are negative in the held-out year. Do not read the
ordering of this column as a ranking.

**The conclusion is the one from the section above, now with the strike question
closed: the option wrapper is not where this is won or lost.** Every strike rule
tested is statistically zero on the full sample and negative out of sample,
because the index signal underneath is +1.3 to +1.7 bps at t < 1 in that window.
Fix the signal or drop it; there is nothing left to tune on the execution side.

### ATR 9, and the break-even number that settles this (2026-08-24)

Config changes tested: `use_ignition` on, volume filter off, `atr_period`
14 -> 9. Two of the three were already in force on an index run — the sweeps
always passed `use_ignition = true`, and `passesFilters` skips the volume gate
entirely on an instrument that has never reported a volume (`sawVolume`), which
is every index. **Only the ATR change was live.** It helps, and `trail_atr_mult`
3 beats the new default of 2 on both indices, so the trail stays at 3.

Index signal, gross bps per trade, lookback 30 / SL 3 ATR / trail-only:

| ATR | trail | Index | n | All | t | IS | OOS | t |
|---|---|---|---|---|---|---|---|---|
| 9 | 3.0 | NIFTY | 825 | **+4.86** | 4.04 | +6.46 | +1.74 | 0.97 |
| 9 | 3.0 | SENSEX | 855 | **+4.13** | 3.71 | +5.53 | +1.47 | 0.86 |
| 14 | 3.0 | NIFTY | 837 | +4.13 | 3.53 | +5.56 | +1.32 | 0.75 |
| 14 | 3.0 | SENSEX | 859 | +3.77 | 3.52 | +4.85 | +1.70 | 1.00 |
| 9 | 2.0 | NIFTY | 955 | +2.76 | 3.46 | +3.74 | +0.82 | 0.68 |
| 9 | 2.0 | SENSEX | 993 | +2.15 | 2.85 | +3.01 | +0.50 | 0.44 |

A shorter ATR tightens the entry (ignition needs range >= 1 ATR) and the exits
scale with it. It is a real improvement and it does not change the verdict.

**The full per-trade accounting, which is the thing to keep.** Every priced
NIFTY trade decomposed into what the index move earns on the premium, what decay
takes, and what costs take — the three sum to the measured net exactly:

| | n | Leverage | Index move gives | Decay takes | Costs take | Net |
|---|---|---|---|---|---|---|
| All | 537 | 92.7x | +358 bps | −176 | −111 | **+70** |
| IS 2023-09..2025-08 | 258 | 88.1x | +655 | −227 | −113 | +315 |
| OOS 2025-09..2026-08 | 279 | 96.8x | +82 | −129 | −109 | **−157** |

This corrects the earlier "theta is not what kills this" reading. Decay is not
negligible: it takes **176 bps per trade**, about half of what the index move
delivers. The earlier statement was true only in the weak sense that gross stays
positive.

**The break-even threshold is ~3.5 index bps per trade at one lot, ~3.1 at ten**
(see the position-size section below for where the difference comes from). Decay
plus costs come to ~287 bps of premium against the 358 the index move delivers,
so break-even needs 4.41 x 287/358 = 3.5 bps of index move in the trade's favour,
on average, every trade. That single number is what any future version of this
has to clear:

- Full sample the signal gives **+4.4 bps** — it clears, and the option makes
  +65 bps net at one lot, +102 at ten. Real, and t = 0.76, so not
  distinguishable from luck.
- Last year the signal gives **+1.7 bps** — it does not clear, and the option
  loses 156 bps net at one lot, 124 at ten.

So the strategy is not "obviously unprofitable". It is *marginally* profitable
in the window it was tuned on and marginally unprofitable outside it, and the
margin either way is smaller than the noise. Improving it means moving index bps
per trade clearly above 3.1 and keeping it there out of sample — not adjusting
the option leg, which is now fully swept (strike depth, delta target, spread,
costs) and has nothing left in it.

### Position size is a real parameter, and the cost sheet is pinned (2026-08-24)

`optbt` prices **`-lots` lots per trade, default 1**, and that default was doing
real damage to the numbers above it. Brokerage is a flat Rs20 per executed order,
so at one NIFTY lot (65 units, ~Rs13,600 of premium) the Rs40 round trip is ~29 bps
all by itself — a quarter of total costs. It amortises away with size:

| Lots | Capital/trade | Costs | NET bps | OOS NET bps | Net rupees/trade |
|---|---|---|---|---|---|
| 1 | Rs13,613 | 116 bps | +65.2 | −155.8 | +Rs11 |
| 2 | Rs27,225 | 95 | +85.8 | −138.2 | +Rs70 |
| 5 | Rs68,063 | 83 | +98.1 | −127.6 | +Rs246 |
| 10 | Rs136,127 | 79 | +102.2 | −124.1 | +Rs539 |
| 25 | Rs340,317 | 76 | +104.7 | −122.0 | +Rs1,419 |
| 50 | Rs680,635 | 76 | +105.5 | −121.3 | +Rs2,885 |

**Essentially all of the benefit is captured by 5-10 lots**; past that the flat
charge is already negligible and the curve is flat. Size is worth ~40 bps per
trade and it does not change the verdict — the held-out year stays negative at
every size, because size cannot touch decay (177 bps) or the spread.

**At any size worth trading, the bid-ask is the dominant cost.** The statutory
charges bottom out under 20 bps for a round trip (STT 15 + txn 7 + stamp + GST),
while a 0.25% half-spread on both legs is ~50 bps. This reverses the earlier
"spread is immaterial" note, which was measured at one lot where brokerage
swamped everything: **at 10 lots the spread is roughly two-thirds of all cost**,
and it is the one cost assumption still worth tightening with real bid-ask data.

The rates are no longer transcribed from a rate card. `costs_test.go` pins the
sheet to a worked example from Zerodha's own brokerage calculator (equity
options, buy 100 / sell 110 / qty 400: brokerage 40, STT 66, txn 29.85, GST
12.59, SEBI 0.08, stamp 1, total 149.52) and reproduces it to within a rupee.
That caught a stale **STT rate of 0.10% when the real one is 0.15%**, which had
been understating every round trip by ~15%. An out-of-date statutory rate is
invisible in a backtest — it just quietly shifts every net figure — so it gets a
test rather than a comment.

Final figures, ATR 9 / trail 3 / lookback 30, 10 lots, 150 points ITM, corrected
costs, 0.25% half-spread:

| Index | n | Index bps | Gross | NET | t | Rupees/trade | Capital |
|---|---|---|---|---|---|---|---|
| NIFTY all | 537 | +4.41 | +181.0 | +102.2 | 0.76 | +Rs539 | Rs136k |
| NIFTY IS | 258 | +7.30 | +428.1 | +347.0 | 1.68 | +Rs3,148 | Rs130k |
| NIFTY OOS | 279 | +1.74 | −47.5 | **−124.1** | −0.71 | −Rs1,873 | Rs142k |
| SENSEX all | 563 | +4.10 | +278.4 | +196.1 | 0.96 | +Rs1,084 | Rs84k |
| SENSEX IS | 268 | +7.00 | +478.8 | +394.7 | 1.45 | +Rs2,085 | Rs81k |
| SENSEX OOS | 295 | +1.47 | +96.3 | **+15.6** | 0.05 | +Rs174 | Rs87k |

SENSEX out of sample is the first non-negative held-out number this strategy has
produced — and at t = 0.05 it is exactly zero, not a signal. Read the pair
together: NIFTY −124 and SENSEX +16 in the same window is one flat result
sampled twice, not disagreement between two markets.

**`optbt` retries remote calls with backoff.** A burst of Upstox 429s previously
surfaced as hundreds of "unpriced" trades, which reads as missing data rather
than throttling — one SENSEX run silently priced 136 of 855 trades that way and
looked like a real (and much better) result. Skip reasons now carry the HTTP
status.

### `tp_rr = 2` on the index, last 12 months only (2026-08-24)

Window 2025-08-24 -> 2026-08-21 (`-start` warms the strategy on earlier candles
without trading them), ATR 9 / trail 3 / lookback 30 / SL 3 ATR / 4 entries a
day. Target on and off, run as a matched pair.

Index signal, gross bps per trade:

| Index | tp_rr | n | Gross bps | t | TARGET-HIT |
|---|---|---|---|---|---|
| NIFTY | 2.0 | 290 | +1.23 | 0.76 | 25 |
| NIFTY | none | 286 | **+1.76** | 1.00 | — |
| SENSEX | 2.0 | 302 | +1.52 | 0.92 | 27 |
| SENSEX | none | 300 | +1.49 | 0.88 | — |

Priced on options, 10 lots, 0.25% half-spread:

| Index | Strike | tp_rr | n | Gross | NET bps | t |
|---|---|---|---|---|---|---|
| NIFTY | 150 pts (0.74 delta) | 2.0 | 288 | −50.7 | −127.3 | −0.73 |
| NIFTY | 150 pts (0.74 delta) | none | 284 | −44.9 | **−121.6** | −0.71 |
| SENSEX | 0.74 delta | 2.0 | 301 | +48.6 | −29.7 | −0.13 |
| SENSEX | 0.74 delta | none | 299 | +77.5 | **−1.0** | −0.00 |

**The target is inert here, and very slightly negative.** Only ~9% of trades
reach it (25 of 290, 27 of 302), and both indices come out marginally worse with
it than without on the option leg. This is the same direction as the stock-futures
result already recorded above ("the target halves gross edge"), just much weaker,
because a 1:2 target against a 3-ATR stop is a far bigger move than the 1.3-ATR
version and almost never gets hit. Keep `tp_rr` unset.

Two measurement notes this run surfaced:

- **150 points is not a comparable strike across indices.** On NIFTY at ~24,300
  it is a 0.62% move and buys 0.74 delta; on SENSEX at ~78,000 it is 0.19% and
  buys **0.59** delta — a different instrument with three times the time value
  (59% of premium against 24%). Any cross-index comparison must use `-delta`,
  not `-itm`.
- **Rupee totals and per-trade bps disagree in sign here**, and bps is the one to
  read. With `-lots` fixed, capital per trade still varies with the premium, so
  the rupee total is dominated by high-premium days. SENSEX shows −29.7 bps and
  +Rs702 per trade simultaneously. Same trap already recorded for the sizer in the
  gapfade section.

The 12-month window is the held-out one, and the answer there is unchanged by the
target: NIFTY loses ~122 bps a trade, SENSEX is flat at 0. The index signal
delivers +1.2 to +1.8 bps against a break-even need of ~3.1.

### Skipping expiry week — the first real improvement (2026-08-24)

**`-min-dte 2` drops trades whose nearest weekly expiry is under two days away**,
i.e. expiry day and the day before. It is the largest single improvement found
anywhere in this work, and unlike the parameter sweeps it has a mechanism rather
than a hindsight.

Why a *fixed delta* is the wrong thing to buy near expiry: what sets delta is the
move in units of `sigma*sqrt(T)`, so as T collapses the 0.75-delta strike walks
in towards spot. On expiry morning it sits ~30 points ITM on NIFTY — a nearly
worthless option with a tiny premium, enormous leverage and brutal decay per
rupee of premium. Those days then dominate any bps-weighted average. `-itm 150`
accidentally avoided this by going *deeper* as expiry approached; delta targeting
walks straight into it. Exit-day breakdown, NIFTY 0.75 delta, last 12 months:

| DTE | n | net premium bps |
|---|---|---|
| 0 | 63 | **−1571** |
| 1 | 53 | −51 |
| 4-6 | 160 | −104 to +106 |

Dropping DTE 0 and 1 turns NIFTY 0.75 delta from **−336 bps to +37**, and 0.90
delta from −219 to +25, on the same trade list.

**Read the two windows the right way round.** `-min-dte 2` was chosen by looking
at 2025-09..2026-08, so that window is the *selection* set and 2024-10..2025-08
(the earliest option data available) is the clean test of the rule. All figures
`-lots 10`, ATR 9 / trail 3 / lookback 30 / SL 3, no target, no breakeven:

| Index | delta | half-spread | Clean 2024-10..2025-08 | t | Selection 2025-09..2026-08 | t |
|---|---|---|---|---|---|---|
| NIFTY | 0.80 | 0.25% | **+190.1** | 1.12 | +2.9 | 0.02 |
| NIFTY | 0.80 | 0.50% | +139.2 | — | −47.0 | −0.32 |
| NIFTY | 0.80 | 1.00% | +38.3 | — | −146.1 | −1.01 |
| NIFTY | 0.90 | 0.25% | +145.4 | 1.10 | +21.1 | 0.18 |
| NIFTY | 0.90 | 1.00% | −5.7 | — | −128.2 | −1.13 |
| SENSEX | 0.80 | 0.25% | +353.4 | 2.13 | +21.3 | 0.16 |
| SENSEX | 0.80 | 1.00% | +199.2 | 1.22 | −128.0 | −0.97 |
| SENSEX | 0.90 | 0.25% | +600.0 | **4.20** | +145.8 | 1.32 |
| SENSEX | 0.90 | 1.00% | +442.1 | 3.14 | −5.3 | −0.05 |

The clean window is positive in every cell but one, which is the first time
anything in this project has managed that. Note the selection window is the
*weaker* of the two — the opposite of what overfitting looks like, so `-min-dte`
is probably a real effect rather than a fitted one.

**Everything now turns on the bid-ask, which is the one input we do not have.**
At a 0.25% half-spread every configuration is positive; at 1.00% almost all are
negative. Premium candles are last-traded prices and carry no spread, so this
cannot be resolved from the data on disk. **Getting real bid-ask quotes for ITM
weeklies is now the highest-value next step in this entire project** — it decides
the sign, and nothing else left to tune does.

**Do not trust the SENSEX 0.90 cell, despite it being the best-looking number
here.** Two defects, both of which flatter it:

1. **Liquidity survivorship.** At 0.90 delta, 51 trades are dropped as "option
   did not trade in one of those bars" against 15 at 0.80. A 1,522-point ITM
   SENSEX weekly does not print every five minutes, so the surviving trades are
   selected on the contract having been liquid at both ends — exactly the
   condition that fails when you need it.
2. **The spread assumption is least plausible precisely there.** That contract
   carries **13.7 points of time value on a 1,535 premium (1%)**, so a 0.25%
   half-spread is 7.7 points round trip — 56% of all the time value in the
   instrument. Deep ITM weeklies have the widest spreads, not the narrowest.

The defensible configuration is therefore **~0.80 delta with `-min-dte 2`**,
not the deepest strike the table rewards.

### Config cleanup (2026-08-24)

`DonchianConfig` lost three families of knob, all measured rather than assumed.
They are gone from the struct, the defaults, `LoadConfig` and the strategy —
read this before reintroducing any of them:

| Removed | Why |
|---|---|
| `vol_mult`, `vol_avg_bars` | An index publishes no volume, so the filter was *inapplicable*, not disabled. It never ran on any index backtest. |
| `use_breakeven`, `breakeven_trigger_atr`, `breakeven_plus_points` | BREAKEVEN+ truncates the right tail a breakout system lives on; removing it doubled gross return per trade. It also fabricated profits when the parked stop landed beyond the market. |
| `tp_rr` | A target capped the winners in four independent tests and paid for itself in none. Latest: on the index over 12 months it was hit by only ~9% of trades and was slightly negative on both indices. |

Defaults now carry the measured-best values rather than the v1 spec's:
`donchian_lookback` 4 -> **30**, `atr_period` 14 -> **9**, `sl_atr_mult` 1.3 ->
**3.0**, `trail_atr_mult` 2.0 -> **3.0**, `max_entries_per_symbol` -> **4**,
`csv_file` -> `indices.csv`, `limit` -> 2. The chandelier trail is now the only
profit-taking exit the strategy has.

The generic BREAKEVEN+ machinery in `internal/core/engine.go`, `pkg/broker/sim.go`
and the `Signal`/`Order` fields is **kept**: it is a Signal-level capability with
its own regression tests, including the ones guarding the fabricated-profit bug.
Donchian simply no longer sets those fields, and `TestDonchianEmitsNoTarget`
asserts that it does not.

### Spreads in ticks, not percent — and what that changes (2026-08-24)

Quoted option spreads are absolute, and modelling them as a percentage of
premium is wrong in both directions: it over-charges cheap options and
under-charges expensive ones. `optbt -spread-ticks` takes the full bid-ask in
ticks and charges half on each leg, using the contract's own tick size.

Published near-week spreads: ATM 1-2 ticks, near ITM 2-5, deep ITM 20-100+
(a tick is Rs0.05). The 0.25% half-spread used in the sections above works out at
**28 ticks on a 280-premium NIFTY contract but 154 ticks on a 1,535-premium
SENSEX one** — so SENSEX had been penalised roughly 8x too heavily.

`-lots 10`, `-min-dte 2`, ATR 9 / trail 3 / lookback 30 / SL 3, net premium bps.
"Clean" is 2024-10..2025-08 (never used to pick `-min-dte`); "selection" is
2025-09..2026-08 (where it was picked):

| Index | delta | ticks | n | Clean | Selection | t |
|---|---|---|---|---|---|---|
| NIFTY | 0.80 | 5 | 316 | +233.6 | +45.1 | 0.31 |
| NIFTY | 0.80 | 20 | 316 | +211.0 | +21.2 | 0.14 |
| NIFTY | 0.80 | 50 | 316 | +165.9 | −26.6 | −0.18 |
| NIFTY | 0.80 | 100 | 316 | +91.2 | −105.5 | −0.72 |
| NIFTY | 0.90 | 20 | 316 | +174.7 | +48.5 | 0.42 |
| SENSEX | 0.80 | 20 | 321 | +396.1 | +61.7 | 0.46 |
| SENSEX | 0.80 | 100 | 321 | +359.5 | +22.3 | 0.17 |
| SENSEX | 0.90 | 20 | 285 | +646.2 | +189.4 | 1.71 |

**SENSEX is far less spread-sensitive than NIFTY** — 5 to 100 ticks costs it only
~37 bps against NIFTY's ~142 — because the same absolute spread is a much
smaller fraction of a 900-1,500 rupee premium than of a 210-280 one. That is a
genuine structural advantage of the larger-priced index, not a modelling
artefact, and it is the opposite of what the percentage model implied.

Still nothing clears t = 2, and the SENSEX 0.90 liquidity-survivorship caveat
from the previous section stands (285 priced trades against 321 at 0.80).

### Live option execution (2026-08-24)

Built: expiry awareness, the option execution layer, and the `cmd/trader` wiring.

**`pkg/options`** is shared by the backtest and the trader, deliberately — a
strike chosen in `cmd/optbt` is chosen by the same code that chooses it live,
rather than by two lookalike implementations that drift apart. It holds
Black-Scholes (price, delta, implied vol by bisection, `StrikeForDelta`), the
expiry calendar helpers, and `Selector`, which:

- takes the nearest expiry on or after today and **refuses if it is under
  `MinDaysToExpiry` away** (`ErrTooCloseToExpiry` — a routine no-trade, not an
  error);
- backs the volatility out of the **at-the-money premium trading in that bar**
  rather than a config constant, because weekly index IV ran 18.4% in the first
  half of the sample and 15.8% in the second;
- inverts `N(d1) = target` for the strike and snaps to the nearest listed one;
- **refuses to trade at all when the ATM premium cannot be read**, unless
  `fallback_iv` is set deliberately. Selecting against a stale constant buys the
  wrong contract silently; not trading is the better failure.

**Chains.** `broker.KiteChain` exposes the Kite instrument dump as an
`options.Chain` + `options.Quoter`. `FetchInstruments` now pulls **BSE and BFO**
as well as NSE/NFO — without them SENSEX options are simply absent and the
strategy silently never trades. The chain maps the feed symbol onto the
derivative `Name` (`NIFTY 50` -> `NIFTY`, `SENSEX` -> `SENSEX`) and fails loudly
on an unknown underlying rather than returning an empty list.

**Exits.** `donchian_options.go` keeps the index-side stop and chandelier trail
per open leg and closes the **contract** when the index closes through them.
Exits are evaluated on bar closes, not ticks: a live index stop would need
tick-by-tick evaluation of one instrument to fire a market order in another, and
the round trip through the option book makes the difference largely notional. It
is also the pessimistic choice. The opposite-band exit closes the contract too —
closing the index symbol would be a no-op at the engine and would leave the real
position open.

**Sizing.** `options.PremiumStop` converts the index stop into an approximate
premium level for the sizer only (risk per unit has to be in rupees of premium).
Delta is a local approximation; because delta falls as the option goes out of
the money, the realised premium loss is smaller than the linear estimate, so the
sizing errs conservative.

**Config.** `target_delta` (0.80), `min_days_to_expiry` (2, a `*int` so an
explicit 0 survives — the `tp_rr` trap), `fallback_iv` (0 = refuse).
`cmd/trader` logs all three at start-up and warns loudly if
`min_days_to_expiry = 0`.

**Verified:** the index-leg backtest is **byte-identical** before and after this
work (same trade list, same prices), so every recorded index result still holds.
What is NOT verified is anything that needs a live session: order placement into
NFO/BFO, real fills, and whether the ATM quote is readable fast enough on the
candle hot path. Those are untested until the first live run.

## Paper trading

`-paper`, or `paper_trading = true`, swaps `broker.PaperAdapter` in for the
Zerodha adapter. Quotes, candles and the tick feed stay real; only the fills are
simulated, against `paper_capital` (default Rs10L).

**The paper broker holds its own stops and targets, and this is the point of it.**
Live, a position is protected by a GTT resting at the exchange, and ORB, gapfade
and Donchian's futures mode exit *solely* through that GTT. A paper broker that
only recorded fills would run every position to the 15:13 square-off and would
therefore measure a different strategy from the one being run. So:

- `PlaceOrder` arms a resting stop/target whenever the order carries a stop, and
  returns a synthetic `GTTTriggerID` — which is what makes the engine arm its
  tick-driven breakeven and chandelier monitor, exactly as a real trigger id does.
- The engine still decides *where* the stop belongs (`stopAfterTick`, then
  `ModifyPositionStop`); the broker only decides *when* it is hit. Same split as
  live, one implementation of the rules.
- `core.TickObserver` is how prices reach it. The engine forwards every tick.
- **The broker subscribes the instruments it holds positions in.** The trader
  subscribes its watchlist once, in `OnConnect`; an option contract is chosen at
  signal time and is not in that list, so nothing would ever tick for the
  instrument the position actually lives in. `broker.TickSubscriber` (implemented
  by `kiteTickFeed` in `cmd/trader`) adds it on fill and drops it on close. This
  is about the websocket *subscription*, not data availability — chains and
  quotes are fetchable either way.
- A quote monitor still polls anything the feed misses, so a subscription that
  fails degrades a stop's resolution rather than removing it.
- Subscriptions do not survive a Kite reconnect, and `OnConnect` only restores
  the watchlist — `resubscribeAll` and `PaperAdapter.ResyncFeed` put the held
  contracts back.

Other things it models rather than stubs, each of which was wrong in the first
version and would have silently distorted results:

| Behaviour | Why it matters |
|---|---|
| Blocks **margin**, not notional, using the engine's leverage map | Debiting full notional rejects leveraged positions the real account takes |
| A short blocks margin too | Crediting the short's notional let the balance grow with every short and inflated the next trade's size |
| Signed cost basis on reduce/reverse | The unsigned version turned a 400 average into 1250 on a partial exit, and negative on a flip |
| Closed positions stay in the book at qty 0 with realised PnL | `computeSummary` splits realised from unrealised on exactly that; dropping them reported realised PnL as zero all session |
| `ClosePosition` enforces `forSide` | A long-exit advice must not close a short in the same symbol — this is the path *every* Donchian option exit takes |
| State persists per trading date | A container restart mid-session otherwise comes back flat with full capital while positions are open |

**Paper and live rows are never mixed.** `is_paper` is written on orders, trades
and equity snapshots, and `GetTradeHistory`/`GetEquitySnapshots` require the
mode. Both modes share `zerobha.db`, so an unscoped query would fold simulated
fills into the live track record's win rate, profit factor, Sharpe and drawdown.

**What paper still cannot tell you**: slippage, queue position, partial fills,
and whether a real order would have been accepted at all. Fills are at the
observed price, and a stop fills at the tick that crossed it.

## Gotchas

- **A zero `Target` used to mean "target at price 0".** `sim.go` compared
  `candle.High >= o.Target` unguarded, so any signal without a target exited on
  its first bar at a price of zero — and the mirror case, a zero `StopLoss` on a
  short. Both are guarded now (`sim_protective_test.go` covers them); a strategy
  exiting purely on a trail leaves `Target` zero legitimately.
- **A stop may never be placed past the market.** For a long that means at or
  below the last price, for a short at or above it. Both the simulator and the
  live tick path cap stop advances at the market for this reason — see the
  donchian section above for what the uncapped version produced.
- **The engine's `trade_cutoff_min` gates every signal**, and at its 14:05
  default it silently truncates any strategy whose own entry window runs later.
  Both `cmd/backtest` and `cmd/trader` raise it from `donchian.entry_cutoff_min`;
  a new strategy with a late window has to do the same.
- **`-timeframe 5minute` silently truncated every entry window by an hour** until
  2026-08-24. The data tree names the same bar size two ways — `test/data/5m`
  (Yahoo, Go duration syntax) and `test/data/5minute` (Kite/Upstox naming) — and
  `parseDuration` fed the string to `time.ParseDuration`, which rejects
  `"5minute"`, then **fell back to one hour**. Every `Candle.EndTime` landed an
  hour late, and `Engine.Execute` gates entries on `EndTime`, so the effective
  cutoff ran an hour EARLY: a donchian index run configured to enter until 14:31
  actually stopped at 13:30. It now switches on the known spellings and
  `log.Fatal`s on an unrecognised one rather than guessing
  (`cmd/backtest/duration_test.go`). **Every 5-minute result recorded above this
  line was produced with the shortened window** — the ORB, gapfade and donchian
  numbers in [Findings](#findings-do-not-re-derive) all lost their 13:30–14:31
  entries. It does not change any of those verdicts (all were decisively
  negative or flatly zero), but re-derive before quoting a trade count.
- **A percentage bid-ask model is wrong for options.** Quoted spreads are
  absolute (ticks), so a percent-of-premium assumption over-charges cheap
  contracts and under-charges expensive ones — it penalised SENSEX roughly 8x
  too heavily against NIFTY. Use `optbt -spread-ticks`. This reverses the
  earlier "spread is immaterial" note twice over: immaterial at one lot where
  flat brokerage swamped it, dominant at ten lots under the percentage model,
  and materially different per index once quoted properly.
- **A paper broker that only records fills is not a paper broker.** Every
  strategy here except Donchian's option mode exits through a stop resting at
  the broker, so simulated positions with no resting stop run to the square-off
  and measure something else entirely. `PaperAdapter` holds them; see the paper
  trading section.
- **A relative write path resolves against WORKDIR, not against a `VOLUME`.**
  The Dockerfile declared `/app/logs` and `/app/data` as volumes but set
  `WORKDIR /app/data`, so the trader's relative `logs/...` resolved to
  `/app/data/logs` — a directory nothing creates. `os.OpenFile` failed, the
  error was discarded (`logFile, _ :=`), and because `os.Stdout` comes first in
  the `MultiWriter` the process logged normally, so `docker logs` looked
  healthy while the daily log and the order journal were written nowhere and
  the backup uploaded an empty logs volume. WORKDIR is now `/app` and both
  destinations are explicit `[paths]` knobs (`db_path`, `log_dir`), each
  relative and landing on its own volume.
- **`-strategy` is not a `cmd/trader` flag.** It defines only `-config` and
  `-paper`; the strategy comes from the config's `strategy` key. The Dockerfile
  passed `-strategy donchian` for four commits, which makes `flag.Parse` exit 2
  before the trader starts.
- **`cmd/backtest` ignored the `[engine]` section** until 2026-08-24, while
  `cmd/trader` applied it — so one config file meant different capital
  allocation and a different entry cutoff in backtest than in live. Fixed.
- **A long whose notional exceeds the cash balance is silently dropped.**
  `SimBroker.PlaceOrder` models spot cash: a BUY needs the full amount and is
  refused as "insufficient funds", while a SELL never checks. Raise
  `max_capital_per_trade` too far and a long/short strategy quietly becomes
  short-only — a SENSEX index run produced 396 shorts and zero longs this way.
  Keep per-trade notional below the ₹5L the backtester starts with, and check
  the LONG/SHORT split of any two-sided result before reading it.
- **An instrument priced above `max_capital_per_trade` is untradeable and says
  nothing about it.** Quantity floors to zero and every signal vanishes, which
  reads as "the strategy found no setups". SENSEX at 78,000 against the
  stock-sized 50,000 cap produced exactly zero trades.
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
