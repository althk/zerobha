#!/usr/bin/env python3
"""
Find High Beta Stocks.

Calculates beta and 1-month relative strength for stocks, optionally filtered
by industry sectors. Outputs a ranked CSV for use as a trading watchlist.
"""
import yfinance as yf
import pandas as pd
import numpy as np
import argparse
import json
import os
import concurrent.futures
from datetime import datetime, timedelta

# Default list of some Nifty 500 stocks (Placeholder)
DEFAULT_STOCKS = [
    "RELIANCE.NS", "TCS.NS", "INFY.NS", "HDFCBANK.NS", "ICICIBANK.NS",
    "ADANIENT.NS", "ADANIPORTS.NS", "TATAMOTORS.NS", "SUNPHARMA.NS",
    "SBIN.NS", "BHARTIARTL.NS", "ITC.NS", "KOTAKBANK.NS", "LICI.NS",
    "BAJFINANCE.NS", "MARUTI.NS", "HCLTECH.NS", "ASIANPAINT.NS",
    "AXISBANK.NS", "TITAN.NS", "ULTRACEMCO.NS", "WIPRO.NS",
    "ONGC.NS", "NTPC.NS", "POWERGRID.NS", "JSWSTEEL.NS",
    "TATASTEEL.NS", "LTIM.NS", "COALINDIA.NS", "HINDUNILVR.NS",
    "IDEA.NS", "YESBANK.NS", "PNB.NS", "IDFCFIRSTB.NS", "BHEL.NS",
    "ZEEL.NS", "DLF.NS", "VEDL.NS", "SAIL.NS", "NATIONALUM.NS"
]


def calculate_beta(stock_returns, market_returns):
    """Calculate beta of a stock relative to the market."""
    data = pd.concat([stock_returns, market_returns], axis=1, join='inner')
    data.columns = ['Stock', 'Market']

    if len(data) < 30:
        return None

    covariance = data['Stock'].cov(data['Market'])
    variance = data['Market'].var()

    if variance == 0:
        return None

    return covariance / variance


def process_symbol(args):
    """
    Worker: download data, calculate beta and RS for a single symbol.
    args: (symbol, industry, start_str, end_str, market_returns, market_perf_1m)
    """
    sym, industry, start_str, end_str, market_returns, market_perf_1m = args
    try:
        stock_data = yf.download(sym, start=start_str,
                                 end=end_str, progress=False, auto_adjust=True)

        if stock_data.empty:
            return None

        close = stock_data['Close']
        if hasattr(close, 'iloc') and len(close.shape) > 1:
            close = close.iloc[:, 0]

        stock_returns = close.pct_change().dropna()
        beta = calculate_beta(stock_returns, market_returns)

        last_price = close.iloc[-1]
        if hasattr(last_price, 'iloc'):
            last_price = last_price.iloc[0]

        # 1-month relative strength (stock return - benchmark return)
        rs_1m = None
        if len(close) >= 21 and market_perf_1m is not None:
            stock_perf_1m = (close.iloc[-1] / close.iloc[-21]) - 1
            rs_1m = float(round((stock_perf_1m - market_perf_1m) * 100, 2))

        if beta is not None:
            return {
                "symbol": sym,
                "industry": industry,
                "beta": float(round(beta, 2)),
                "last_price": float(round(last_price, 2)),
                "rs_1m": rs_1m,
            }
    except Exception:
        return None
    return None


def main():
    parser = argparse.ArgumentParser(description="Find High Beta Stocks")
    parser.add_argument("--symbols", type=str, default="ind_nifty500list.csv",
                        help="Path to CSV file containing symbols (column name 'Symbol')")
    parser.add_argument(
        "--output", type=str, default="high_beta_stocks.csv", help="Output CSV file")
    parser.add_argument("--start-date", type=str,
                        help="Start date (YYYY-MM-DD)")
    parser.add_argument("--end-date", type=str, help="End date (YYYY-MM-DD)")
    parser.add_argument("--workers", type=int,
                        default=os.cpu_count(), help="Number of worker processes")
    parser.add_argument("--sectors", type=str, default=None,
                        help="Comma-separated list of industries to filter "
                             "(e.g. 'Healthcare,Metals & Mining')")
    parser.add_argument("--min-beta", type=float, default=None,
                        help="Minimum beta threshold (e.g. 1.2)")
    parser.add_argument("--limit", type=int, default=None,
                        help="Max number of stocks to output")
    args = parser.parse_args()

    # Determine date range
    end_date = datetime.now()
    if args.end_date:
        end_date = datetime.strptime(args.end_date, "%Y-%m-%d")

    start_date = end_date - timedelta(days=60)
    if args.start_date:
        start_date = datetime.strptime(args.start_date, "%Y-%m-%d")

    print(
        f"Calculating Beta from {start_date.strftime('%Y-%m-%d')} to {end_date.strftime('%Y-%m-%d')}")

    # Parse sector filter
    sector_filter = None
    if args.sectors:
        sector_filter = [s.strip() for s in args.sectors.split(',')]
        print(f"Filtering to industries: {sector_filter}")

    # Load symbols
    symbols_with_industry = []
    if args.symbols and os.path.exists(args.symbols):
        try:
            df = pd.read_csv(args.symbols)
            has_industry = 'Industry' in df.columns

            if sector_filter and has_industry:
                df = df[df['Industry'].isin(sector_filter)]
                print(f"  {len(df)} stocks match sector filter")

            for _, row in df.iterrows():
                sym = row['Symbol']
                sym = sym if sym.endswith('.NS') else f"{sym}.NS"
                industry = row['Industry'] if has_industry else ''
                symbols_with_industry.append((sym, industry))
        except Exception as e:
            print(f"Error reading symbols file: {e}")
            return
    else:
        print("Using default sample list (Nifty 500 subset)...")
        symbols_with_industry = [(s, '') for s in DEFAULT_STOCKS]

    print(f"Fetching market data (^NSEI)...")
    start_str = start_date.strftime("%Y-%m-%d")
    end_str = (end_date + timedelta(days=1)).strftime("%Y-%m-%d")

    market_data = yf.download("^NSEI", start=start_str,
                              end=end_str, progress=False, auto_adjust=True)
    if market_data.empty:
        print("Error: No market data fetched.")
        return

    market_close = market_data['Close']
    if hasattr(market_close, 'iloc') and len(market_close.shape) > 1:
        market_close = market_close.iloc[:, 0]

    market_returns = market_close.pct_change().dropna()

    # 1-month market performance for RS calculation
    market_perf_1m = None
    if len(market_close) >= 21:
        market_perf_1m = (market_close.iloc[-1] / market_close.iloc[-21]) - 1

    results = []
    print(f"Processing {len(symbols_with_industry)} stocks using {args.workers} workers...")

    tasks = [(sym, ind, start_str, end_str, market_returns, market_perf_1m)
             for sym, ind in symbols_with_industry]

    with concurrent.futures.ProcessPoolExecutor(max_workers=args.workers) as executor:
        for result in executor.map(process_symbol, tasks):
            if result:
                results.append(result)

    # Apply min-beta filter
    if args.min_beta is not None:
        results = [r for r in results if r['beta'] >= args.min_beta]

    # Sort by beta descending
    results.sort(key=lambda x: x['beta'], reverse=True)

    # Apply limit
    if args.limit:
        results = results[:args.limit]

    # Save to file
    if args.output.endswith('.csv'):
        df_results = pd.DataFrame(results)
        df_results.to_csv(args.output, index=False)
    else:
        with open(args.output, "w") as f:
            json.dump(results, f, indent=2)

    print(f"\nTop 10 High Beta Stocks:")
    for item in results[:10]:
        rs_str = f"  RS_1M: {item['rs_1m']:+.2f}%" if item.get('rs_1m') is not None else ""
        print(f"  {item['symbol']}: beta={item['beta']}{rs_str}")

    print(f"\nSaved {len(results)} stocks to {args.output}")


if __name__ == "__main__":
    main()
