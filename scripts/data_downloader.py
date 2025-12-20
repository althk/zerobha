import concurrent.futures
import yfinance as yf
import pandas as pd
import time
import argparse
import os


def download_stock(symbol, args):
    if not symbol.endswith(".NS") and not symbol.endswith(".BO") and not symbol.startswith("^"):
        symbol = symbol + ".NS"

    print(f"Downloading data for {symbol} @ {args.interval}...")

    # Fetch data
    try:
        if args.start and args.end:
            df = yf.download(symbol, start=args.start, end=args.end, interval=args.interval,
                             progress=False, auto_adjust=True, multi_level_index=False)
        else:
            df = yf.download(symbol, period=args.period, interval=args.interval,
                             progress=False, auto_adjust=True, multi_level_index=False)
    except Exception as e:
        print(f"Error downloading {symbol}: {e}")
        return

    if df.empty:
        print(f"No data for {symbol}")
        return

    # Format for Zerobha
    # 1. Reset index to get Timestamp as column
    df.reset_index(inplace=True)

    # 2. Rename columns to lowercase matches
    df.rename(columns={
        "Datetime": "timestamp",
        "Date": "timestamp",
        "Open": "open",
        "High": "high",
        "Low": "low",
        "Close": "close",
        "Volume": "volume"
    }, inplace=True)

    # 3. Format timestamp to RFC3339 (Go standard)
    if 'timestamp' in df.columns:
        # Check if it's already datetime or string
        if not pd.api.types.is_datetime64_any_dtype(df['timestamp']):
            df['timestamp'] = pd.to_datetime(df['timestamp'])

        df['timestamp'] = df['timestamp'].dt.strftime('%Y-%m-%dT%H:%M:%S%z')
    else:
        print(f"Missing timestamp for {symbol}")
        return

    # 4. Save
    clean_sym = symbol.replace(".NS", "").lower()
    # Ensure directory exists
    os.makedirs("test/data/{}".format(args.interval), exist_ok=True)
    output_file = f"test/data/{args.interval}/{clean_sym}_real.csv"
    df.to_csv(output_file, index=False)
    print(f"Saved {len(df)} candles to {output_file}")


def main():
    parser = argparse.ArgumentParser(
        description="Download historical data for stocks")
    parser.add_argument("--stocks_csv", type=str, default="ind_nifty50list.csv",
                        help="Path to CSV file containing stock symbols (column 'symbol')")
    parser.add_argument("--limit", type=int, default=500,
                        help="Number of stocks to download (default: 100)")
    parser.add_argument("--interval", type=str, default="1h",
                        help="Data interval (1h, 1d)")
    parser.add_argument("--period", type=str, default="59d",
                        help="Data period (e.g. 59d, 2y)")
    parser.add_argument("--start", type=str, help="Start date (YYYY-MM-DD)")
    parser.add_argument("--end", type=str, help="End date (YYYY-MM-DD)")
    args = parser.parse_args()

    stocks = []
    stocks_csv = "ind_nifty50list.csv"
    if args.stocks_csv and os.path.exists(args.stocks_csv):
        stocks_csv = args.stocks_csv
    else:
        print("No CSV provided. Using nifty50 list.")

    print(f"Reading stocks from {stocks_csv}...")
    df = pd.read_csv(stocks_csv)
    if 'symbol' in df.columns:
        stocks = df['symbol'].tolist()
    elif 'Symbol' in df.columns:
        stocks = df['Symbol'].tolist()
    else:
        print("Error: CSV must have a 'symbol' or 'Symbol' column")
        return

    # Apply limit
    if args.limit > 0:
        stocks = stocks[:args.limit]
        print(f"Limiting to top {args.limit} stocks.")

    # Use ProcessPoolExecutor for concurrent downloads to avoid yfinance thread safety issues
    with concurrent.futures.ProcessPoolExecutor(max_workers=5) as executor:
        futures = [executor.submit(download_stock, symbol, args)
                   for symbol in stocks]
        for future in concurrent.futures.as_completed(futures):
            try:
                future.result()
            except Exception as e:
                print(f"Thread raised an exception: {e}")


if __name__ == "__main__":
    main()
