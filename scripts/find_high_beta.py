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
    # Align data (inner join on dates)
    data = pd.concat([stock_returns, market_returns], axis=1, join='inner')
    data.columns = ['Stock', 'Market']

    if len(data) < 30:  # Need enough data points
        return None

    # Calculate Covariance and Variance
    covariance = data['Stock'].cov(data['Market'])
    variance = data['Market'].var()

    if variance == 0:
        return None

    beta = covariance / variance
    return beta


def process_symbol(args):
    """
    Worker function to download data and calculate beta for a single symbol.
    args: (symbol, start_str, end_str, market_returns)
    """
    sym, start_str, end_str, market_returns = args
    try:
        # Download stock data
        stock_data = yf.download(sym, start=start_str,
                                 end=end_str, progress=False, auto_adjust=True)

        if stock_data.empty:
            return None

        # Calculate Returns
        stock_returns = stock_data['Close'].pct_change().dropna()

        # Calculate Beta
        beta = calculate_beta(stock_returns, market_returns)

        # Get Last Price
        last_price = stock_data['Close'].iloc[-1]
        if hasattr(last_price, 'iloc'):
            last_price = last_price.iloc[0]

        if beta is not None:
            return {
                "symbol": sym,
                "beta": float(round(beta, 2)),
                "last_price": float(round(last_price, 2))
            }
    except Exception as e:
        # print(f"Failed {sym}: {e}") # Reduce noise in multiprocessing
        return None
    return None


def main():
    parser = argparse.ArgumentParser(description="Find High Beta Stocks")
    parser.add_argument("--symbols", type=str, default="ind_nifty500list.csv",
                        help="Path to CSV file containing symbols (column name 'Symbol')")
    parser.add_argument(
        "--output", type=str, default="high_beta_stocks.csv", help="Output JSON/CSV file")
    parser.add_argument("--start-date", type=str,
                        help="Start date (YYYY-MM-DD)")
    parser.add_argument("--end-date", type=str, help="End date (YYYY-MM-DD)")
    parser.add_argument("--workers", type=int,
                        default=os.cpu_count(), help="Number of worker processes")
    args = parser.parse_args()

    # Determine date range
    end_date = datetime.now()
    if args.end_date:
        end_date = datetime.strptime(args.end_date, "%Y-%m-%d")

    start_date = end_date - timedelta(days=60)  # Default to 60 days back
    if args.start_date:
        start_date = datetime.strptime(args.start_date, "%Y-%m-%d")

    print(
        f"Calculating Beta using data from {start_date.strftime('%Y-%m-%d')} to {end_date.strftime('%Y-%m-%d')}")

    # Load symbols
    symbols = []
    if args.symbols and os.path.exists(args.symbols):
        try:
            df = pd.read_csv(args.symbols)
            symbols = df['Symbol'].tolist()
            # Ensure .NS suffix
            symbols = [s if s.endswith('.NS') else f"{s}.NS" for s in symbols]
        except Exception as e:
            print(f"Error reading symbols file: {e}")
            return
    else:
        print("Using default sample list (Nifty 500 subset)...")
        symbols = DEFAULT_STOCKS

    print(f"Fetching market data (^NSEI)...")
    start_str = start_date.strftime("%Y-%m-%d")
    end_str = (end_date + timedelta(days=1)).strftime("%Y-%m-%d")

    # Fetch Market Data ONCE
    market_data = yf.download("^NSEI", start=start_str,
                              end=end_str, progress=False, auto_adjust=True)
    if market_data.empty:
        print("Error: No market data fetched. Check your date range.")
        return

    # Pre-calculate market returns to pass to workers
    market_returns = market_data['Close'].pct_change().dropna()

    results = []
    print(f"Processing {len(symbols)} stocks using {args.workers} workers...")

    # Prepare arguments for multiprocessing
    # We pass market_returns to each worker. DataFrame/Series pickle overhead is acceptable for this size.
    tasks = [(sym, start_str, end_str, market_returns) for sym in symbols]

    with concurrent.futures.ProcessPoolExecutor(max_workers=args.workers) as executor:
        # Map returns results in order
        for result in executor.map(process_symbol, tasks):
            if result:
                results.append(result)
            # Progress indicator could be added here

    # Sort by Beta descending
    results.sort(key=lambda x: x['beta'], reverse=True)

    # Save to file
    if args.output.endswith('.csv'):
        df_results = pd.DataFrame(results)
        df_results.to_csv(args.output, index=False)
    else:
        with open(args.output, "w") as f:
            json.dump(results, f, indent=2)

    print(f"\nTop 10 High Beta Stocks:")
    for item in results[:10]:
        print(f"{item['symbol']}: {item['beta']}")

    print(f"\nSaved full list to {args.output}")


if __name__ == "__main__":
    main()
