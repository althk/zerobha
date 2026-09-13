#!/usr/bin/env python3
"""
Rank a universe by beta computed from the daily candles already on disk.

Beta is cov(stock, benchmark) / var(benchmark) over daily close-to-close
returns in the window [as-of minus `--months`, as-of), i.e. strictly BEFORE
the as-of date. Build the list with `--as-of` equal to the first session of
the backtest and there is no selection lookahead (the CLAUDE.md trap the
Yahoo-built high_beta_*.csv files fall into).

    python scripts/beta_from_candles.py --as-of 2025-07-18 --months 3 --top 50 \
        --output high_beta_50_2025-07-18.csv

Reads test/data/day/<symbol>_real.csv for every symbol in --symbols and the
benchmark (default nifty50). Symbols without enough sessions in the window are
reported and skipped, never silently ranked on a partial sample.
"""
import argparse
import os
import sys

import pandas as pd

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DAY_DIR = os.path.join(ROOT, "test", "data", "day")


INTRADAY_DIR = os.path.join(ROOT, "test", "data", "5minute")


def load_closes(stem: str) -> pd.Series:
    """Daily closes for a symbol: the daily file, or the last 5-minute close of
    each session when only intraday history exists (the indices from idxdl)."""
    path = os.path.join(DAY_DIR, f"{stem.lower()}_real.csv")
    intraday = False
    if not os.path.exists(path):
        path = os.path.join(INTRADAY_DIR, f"{stem.lower()}_real.csv")
        intraday = True
    if not os.path.exists(path):
        return pd.Series(dtype=float)
    df = pd.read_csv(path, usecols=["timestamp", "close"])
    idx = pd.to_datetime(df["timestamp"], utc=True).dt.tz_convert("Asia/Kolkata").dt.normalize()
    s = pd.Series(df["close"].values, index=idx.dt.tz_localize(None)).sort_index()
    if intraday:
        s = s.groupby(level=0).last()
    return s


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--symbols", default="ind_nifty200list.csv", help="universe CSV with a Symbol column")
    ap.add_argument("--benchmark", default="nifty50", help="benchmark file stem under test/data/day")
    ap.add_argument("--as-of", required=True, help="first backtest session, YYYY-MM-DD; the window ends the day before")
    ap.add_argument("--months", type=int, default=3, help="lookback window length in calendar months")
    ap.add_argument("--top", type=int, default=50, help="how many names to keep, highest beta first")
    ap.add_argument("--min-sessions", type=int, default=40, help="minimum overlapping sessions to rank a name")
    ap.add_argument("--output", required=True)
    args = ap.parse_args()

    end = pd.Timestamp(args.as_of)
    start = end - pd.DateOffset(months=args.months)
    print(f"Beta window: [{start.date()}, {end.date()}) on daily returns vs {args.benchmark}")

    bench = load_closes(args.benchmark)
    if bench.empty:
        print(f"benchmark {args.benchmark} not found under {DAY_DIR}", file=sys.stderr)
        return 1
    bench = bench[(bench.index >= start) & (bench.index < end)]
    bench_ret = bench.pct_change().dropna()
    print(f"Benchmark sessions in window: {len(bench)}")

    uni = pd.read_csv(args.symbols)
    sym_col = next(c for c in uni.columns if c.lower() == "symbol")
    ind_col = next((c for c in uni.columns if c.lower() == "industry"), None)

    rows, skipped = [], []
    for _, r in uni.iterrows():
        sym = str(r[sym_col]).replace(".NS", "").upper()
        closes = load_closes(sym)
        closes = closes[(closes.index >= start) & (closes.index < end)]
        ret = closes.pct_change().dropna()
        both = pd.concat([ret, bench_ret], axis=1, join="inner").dropna()
        if len(both) < args.min_sessions:
            skipped.append((sym, len(both)))
            continue
        var = both.iloc[:, 1].var()
        if var == 0:
            skipped.append((sym, len(both)))
            continue
        beta = both.iloc[:, 0].cov(both.iloc[:, 1]) / var
        rows.append({
            "symbol": sym,
            "industry": r[ind_col] if ind_col else "",
            "beta": round(float(beta), 3),
            "sessions": len(both),
            "as_of": end.strftime("%Y-%m-%d"),
        })

    out = pd.DataFrame(rows).sort_values("beta", ascending=False).head(args.top)
    out.to_csv(args.output, index=False)
    print(f"Ranked {len(rows)} names, skipped {len(skipped)}; wrote top {len(out)} to {args.output}")
    for s, n in skipped:
        print(f"  skipped {s}: {n} sessions")
    print(out.head(10).to_string(index=False))
    print("  ...")
    print(out.tail(3).to_string(index=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
