# Intraday Donchian Breakout — NSE Stock Futures (Spec v1)

## Overview
Intraday breakout system on 5-min candles. Long and short. Universe = all NSE stock futures. All positions squared off before close (MIS). Signal engine is broker-agnostic; execution via KiteConnect.

## Universe
- Source: Kite instruments dump (daily, pre-market). Filter: `segment = NFO-FUT`, current-month expiry, **exclude index futures** (NIFTY, BANKNIFTY, FINNIFTY, MIDCPNIFTY, NIFTYNXT50).
- Liquidity gate: prior-day futures volume ≥ `min_daily_volume` contracts (skip illiquid names).
- Rollover: switch to next-month contract on expiry day − `rollover_days_before`.

## Data
- 5-min OHLCV of the **futures contract** (not spot), via Kite websocket ticks aggregated locally (historical API for warm-up bars).
- Warm-up: fetch last `donchian_lookback + atr_period` completed bars at startup.

## Indicators (on 5-min bars)
- Donchian upper = highest HIGH of last `donchian_lookback` bars, **excluding current bar**.
- Donchian lower = lowest LOW of last `donchian_lookback` bars, excluding current bar.
- ATR = ATR(`atr_period`).

## Entry (evaluated on bar close only)
- **Long**: bar CLOSE > Donchian upper. **Short**: bar CLOSE < Donchian lower.
- Filters (all must pass):
  - Time window: `entry_start_time` ≤ now ≤ `entry_cutoff_time`.
  - Volume: breakout bar volume ≥ `vol_mult` × avg volume of last `vol_avg_bars` bars.
  - Volatility floor: ATR / price ≥ `min_atr_pct` (skips dead names).
  - No open position in symbol; entries taken so far in symbol < `max_entries_per_symbol`.
  - Open positions count < `max_concurrent_positions`; daily loss limit not hit; margin available.
- Order: MARKET, product MIS, exchange NFO.

## Exit (checked every tick; SL rests at broker)
- Initial SL: entry ∓ `sl_atr_mult` × ATR (placed as SL-M immediately after fill).
- Trailing (chandelier): highest high (long) / lowest low (short) since entry ∓ `trail_atr_mult` × ATR; SL only ratchets, never loosens; broker SL-M modified on change.
- Optional target: `tp_rr` × initial risk (0 = disabled).
- Opposite Donchian breakout while in position → exit at market (no flip in v1).
- Hard square-off: cancel all, exit all at `squareoff_time`.

## Risk & Sizing
- Risk per trade = `risk_pct` × capital. Lots = floor(risk_amount / (SL distance × lot_size)); skip trade if < 1 lot.
- Stop all new entries when realized day PnL ≤ −`max_daily_loss_pct` × capital.

## Config Knobs (defaults)
| Knob | Default | Notes |
|---|---|---|
| `donchian_lookback` | 20 | bars (5-min) |
| `atr_period` | 14 | bars |
| `vol_mult` | 1.5 | breakout volume multiple |
| `vol_avg_bars` | 20 | volume baseline |
| `min_atr_pct` | 0.15% | ATR/price floor |
| `min_daily_volume` | 500000 | prior-day futures contracts×qty |
| `sl_atr_mult` | 1.5 | initial stop |
| `trail_atr_mult` | 2.0 | chandelier trail |
| `tp_rr` | 0 (off) | target in R multiples |
| `entry_start_time` | 09:35 | skip opening noise |
| `entry_cutoff_time` | 14:45 | last new entry |
| `squareoff_time` | 15:12 | hard exit |
| `risk_pct` | 0.5% | per trade |
| `max_daily_loss_pct` | 2% | kill switch |
| `max_concurrent_positions` | 5 | portfolio cap |
| `max_entries_per_symbol` | 1 | per day |
| `rollover_days_before` | 1 | contract switch |

## Non-Goals (v1)
No flip-on-reverse, no pyramiding, no spot-vs-futures basis logic, no overnight carry.
