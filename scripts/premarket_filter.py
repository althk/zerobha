"""
Pre-market stock filter for ORB strategy.

Filters the input universe (high_beta_stocks.csv or ind_nifty500list.csv) down to
stocks that are liquid, volatile, and respond well to market direction.

Filters applied in order:
  1. Price between ₹100 and ₹5,000
  2. ADTV (avg daily traded value) > ₹50 Cr over last 20 days
  3. ATR(14) as % of closing price > 1.5%
  4. Beta > 1.2

Output: filtered_watchlist.csv  (symbol, beta, last_price, atr_pct, adtv_cr)
        sorted by atr_pct descending

Run before market open (~8:30 AM IST):
    python scripts/premarket_filter.py
    python scripts/premarket_filter.py --input ind_nifty500list.csv --output filtered_watchlist.csv
"""

import argparse
import concurrent.futures
import os
import sys
from datetime import datetime, timedelta

import numpy as np
import pandas as pd
import yfinance as yf

ADTV_MIN_CR = 50          # ₹ crore
ATR_PCT_MIN = 1.5         # percent of close
BETA_MIN = 1.2
PRICE_MIN = 100
PRICE_MAX = 5000
HISTORY_DAYS = 20         # calendar days to look back for ADTV / ATR
BETA_WINDOW_DAYS = 60     # calendar days for beta calculation


def compute_atr(df: pd.DataFrame, period: int = 14) -> float:
    """Return the most recent ATR(period) value from a daily OHLCV DataFrame."""
    if len(df) < period + 1:
        return 0.0
    high = df["High"]
    low = df["Low"]
    close = df["Close"]
    prev_close = close.shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()], axis=1
    ).max(axis=1)
    atr = tr.rolling(period).mean().iloc[-1]
    return float(atr) if not np.isnan(atr) else 0.0


def compute_beta(stock_returns: pd.Series, market_returns: pd.Series) -> float | None:
    """Return beta of stock relative to market, or None if insufficient data."""
    data = pd.concat([stock_returns, market_returns], axis=1, join="inner")
    data.columns = ["stock", "market"]
    if len(data) < 30:
        return None
    cov = data["stock"].cov(data["market"])
    var = data["market"].var()
    if var == 0:
        return None
    return float(cov / var)


def process_symbol(args):
    """Download data and compute metrics for a single symbol. Returns dict or None."""
    sym, start_adtv, start_beta, end_str, market_returns = args
    try:
        df = yf.download(sym, start=start_beta, end=end_str, progress=False, auto_adjust=True)
        if df.empty or len(df) < 15:
            return None

        # Flatten MultiIndex columns that yfinance sometimes returns
        if isinstance(df.columns, pd.MultiIndex):
            df.columns = df.columns.get_level_values(0)

        last_price = float(df["Close"].iloc[-1])

        # --- Filter 1: Price range (fast, skip download cost already paid) ---
        if not (PRICE_MIN <= last_price <= PRICE_MAX):
            return {"symbol": sym, "filtered_by": "price", "last_price": last_price}

        # --- ADTV over last 20 trading days ---
        df_adtv = df[df.index >= start_adtv]
        if df_adtv.empty:
            return None
        adtv = float((df_adtv["Close"] * df_adtv["Volume"]).mean()) / 1e7  # in ₹ crore

        # --- ATR(14) as % of close ---
        atr = compute_atr(df)
        atr_pct = (atr / last_price * 100) if last_price > 0 else 0.0

        # --- Beta ---
        stock_returns = df["Close"].pct_change().dropna()
        beta = compute_beta(stock_returns, market_returns)

        return {
            "symbol": sym,
            "last_price": round(last_price, 2),
            "adtv_cr": round(adtv, 2),
            "atr_pct": round(atr_pct, 2),
            "beta": round(beta, 2) if beta is not None else None,
        }
    except Exception as e:
        print(f"  WARNING: {sym}: {e}", file=sys.stderr)
        return None


def main():
    parser = argparse.ArgumentParser(description="Pre-market ORB stock filter")
    parser.add_argument(
        "--input",
        default="high_beta_stocks.csv",
        help="Input CSV with a 'symbol' or 'Symbol' column (default: high_beta_stocks.csv)",
    )
    parser.add_argument(
        "--output",
        default="filtered_watchlist.csv",
        help="Output CSV path (default: filtered_watchlist.csv)",
    )
    parser.add_argument(
        "--workers",
        type=int,
        default=min(os.cpu_count() or 4, 8),
        help="Parallel download workers",
    )
    parser.add_argument(
        "--adtv-min",
        type=float,
        default=ADTV_MIN_CR,
        help=f"Min ADTV in ₹ crore (default: {ADTV_MIN_CR})",
    )
    parser.add_argument(
        "--atr-pct-min",
        type=float,
        default=ATR_PCT_MIN,
        help=f"Min ATR%% of close (default: {ATR_PCT_MIN})",
    )
    parser.add_argument(
        "--beta-min",
        type=float,
        default=BETA_MIN,
        help=f"Min beta (default: {BETA_MIN})",
    )
    args = parser.parse_args()

    # --- Load input universe ---
    if not os.path.exists(args.input):
        print(f"ERROR: Input file not found: {args.input}")
        sys.exit(1)

    df_in = pd.read_csv(args.input)
    # Normalise column name
    col_map = {c.lower(): c for c in df_in.columns}
    sym_col = col_map.get("symbol")
    if sym_col is None:
        print(f"ERROR: No 'Symbol' column found in {args.input}. Columns: {list(df_in.columns)}")
        sys.exit(1)

    symbols_raw = df_in[sym_col].dropna().tolist()
    symbols = [s if str(s).endswith(".NS") else f"{s}.NS" for s in symbols_raw]
    print(f"Input universe: {len(symbols)} stocks from {args.input}")

    # --- Date ranges ---
    end_date = datetime.now()
    end_str = (end_date + timedelta(days=1)).strftime("%Y-%m-%d")
    start_beta = (end_date - timedelta(days=BETA_WINDOW_DAYS)).strftime("%Y-%m-%d")
    start_adtv = end_date - timedelta(days=HISTORY_DAYS + 10)  # a bit of buffer

    # --- Fetch Nifty 50 for beta ---
    print("Fetching Nifty 50 index data for beta calculation...")
    nifty = yf.download("^NSEI", start=start_beta, end=end_str, progress=False, auto_adjust=True)
    if nifty.empty:
        print("ERROR: Failed to fetch Nifty 50 data. Check internet connection.")
        sys.exit(1)
    if isinstance(nifty.columns, pd.MultiIndex):
        nifty.columns = nifty.columns.get_level_values(0)
    market_returns = nifty["Close"].pct_change().dropna()

    # --- Parallel processing ---
    tasks = [(sym, start_adtv, start_beta, end_str, market_returns) for sym in symbols]
    results = []
    print(f"Processing {len(symbols)} stocks with {args.workers} workers...")
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as executor:
        for r in executor.map(process_symbol, tasks):
            if r:
                results.append(r)

    # Separate filtered-early (price) from candidates
    candidates = [r for r in results if "filtered_by" not in r]
    price_filtered = [r for r in results if r.get("filtered_by") == "price"]

    print(f"\n=== Filter Pipeline ===")
    print(f"  Total downloaded:      {len(results)}")
    print(f"  Failed price filter:   {len(price_filtered)}")

    df = pd.DataFrame(candidates)
    if df.empty:
        print("No candidates after price filter.")
        sys.exit(0)

    after_price = len(df)

    # Filter 2: ADTV
    df = df[df["adtv_cr"] >= args.adtv_min]
    print(f"  After ADTV >{args.adtv_min}Cr:    {len(df)}  (dropped {after_price - len(df)})")
    after_adtv = len(df)

    # Filter 3: ATR%
    df = df[df["atr_pct"] >= args.atr_pct_min]
    print(f"  After ATR% >{args.atr_pct_min}%:     {len(df)}  (dropped {after_adtv - len(df)})")
    after_atr = len(df)

    # Filter 4: Beta
    df = df[df["beta"].notna() & (df["beta"] >= args.beta_min)]
    print(f"  After Beta >{args.beta_min}:       {len(df)}  (dropped {after_atr - len(df)})")

    if df.empty:
        print("\nNo stocks survived all filters. Consider relaxing thresholds.")
        sys.exit(0)

    # Sort by ATR% descending
    df = df.sort_values("atr_pct", ascending=False).reset_index(drop=True)

    # Strip .NS suffix so the output matches the format the Go code expects
    df["symbol"] = df["symbol"].str.replace(r"\.NS$", "", regex=True)

    output_cols = ["symbol", "beta", "last_price", "atr_pct", "adtv_cr"]
    df[output_cols].to_csv(args.output, index=False)

    print(f"\nTop 10 stocks by ATR%:")
    print(df[output_cols].head(10).to_string(index=False))
    print(f"\nSaved {len(df)} stocks to {args.output}")
    print(f"Set  csv_file = \"{args.output}\"  in config.toml to use this watchlist.")


if __name__ == "__main__":
    main()
