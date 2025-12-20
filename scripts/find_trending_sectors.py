import yfinance as yf
import pandas as pd
import numpy as np
import argparse
import os
from datetime import datetime, timedelta

def calculate_momentum_score(industry_index):
    """
    Calculates momentum score based on:
    Score = 0.4 * (1W Return) + 0.4 * (1M Return) + 0.2 * (3M Return)
    """
    # Ensure we have enough data
    if len(industry_index) < 90: # Approx 3 months trading days
        return -np.inf

    current_price = industry_index.iloc[-1]

    # 1 Week Return (approx 5 trading days)
    price_1w_ago = industry_index.iloc[-5] if len(industry_index) >= 5 else industry_index.iloc[0]
    ret_1w = (current_price - price_1w_ago) / price_1w_ago

    # 1 Month Return (approx 21 trading days)
    price_1m_ago = industry_index.iloc[-21] if len(industry_index) >= 21 else industry_index.iloc[0]
    ret_1m = (current_price - price_1m_ago) / price_1m_ago

    # 3 Month Return (approx 63 trading days)
    price_3m_ago = industry_index.iloc[-63] if len(industry_index) >= 63 else industry_index.iloc[0]
    ret_3m = (current_price - price_3m_ago) / price_3m_ago

    score = (0.4 * ret_1w) + (0.4 * ret_1m) + (0.2 * ret_3m)
    return score

def check_trend_filter(industry_index):
    """
    Checks if the industry index is above its 20-day SMA.
    """
    if len(industry_index) < 20:
        return False

    sma_20 = industry_index.rolling(window=20).mean().iloc[-1]
    current_price = industry_index.iloc[-1]

    return current_price > sma_20

def main():
    parser = argparse.ArgumentParser(description="Find Trending Industries")
    parser.add_argument("--input", type=str, default="ind_nifty500list.csv", help="Path to Nifty 500 CSV")
    parser.add_argument("--output", type=str, default="trending_sector_stocks.csv", help="Output CSV file")
    parser.add_argument("--as-of-date", type=str, help="As-of date for analysis (YYYY-MM-DD)")
    args = parser.parse_args()

    if not os.path.exists(args.input):
        print(f"Error: Input file {args.input} not found.")
        return

    print(f"Loading stocks from {args.input}...")
    df = pd.read_csv(args.input)

    # Ensure necessary columns exist
    if 'Industry' not in df.columns or 'Symbol' not in df.columns:
        print("Error: CSV must contain 'Industry' and 'Symbol' columns.")
        return

    # Group stocks by Industry
    industries = df['Industry'].unique()
    print(f"Found {len(industries)} industries.")

    # Prepare to download data
    if args.as_of_date:
        end_date = datetime.strptime(args.as_of_date, "%Y-%m-%d")
    else:
        end_date = datetime.now()

    start_date = end_date - timedelta(days=180) # Approx 6 months

    start_str = start_date.strftime("%Y-%m-%d")
    end_str = (end_date + timedelta(days=1)).strftime("%Y-%m-%d")

    print(f"Analyzing data from {start_str} to {end_date.strftime('%Y-%m-%d')}...")

    industry_scores = []

    # Download data for all stocks at once to optimize (or batch if too large)
    # yfinance can handle multiple tickers
    all_symbols = [s + ".NS" for s in df['Symbol'].tolist()]

    print("Downloading historical data for all stocks (this may take a while)...")
    # Using threads=True for faster download
    data = yf.download(all_symbols, start=start_str, end=end_str, group_by='ticker', progress=True, threads=True, auto_adjust=True)

    print("\nCalculating Industry Indices and Scores...")

    for industry in industries:
        industry_stocks = df[df['Industry'] == industry]['Symbol'].tolist()
        industry_stocks_ns = [s + ".NS" for s in industry_stocks]

        # Collect close prices for stocks in this industry
        industry_prices = pd.DataFrame()

        for sym in industry_stocks_ns:
            try:
                # Handle multi-level columns from yfinance if multiple tickers downloaded
                if isinstance(data.columns, pd.MultiIndex):
                    if sym in data.columns.levels[0]:
                        series = data[sym]['Close']
                    else:
                        continue
                else:
                    # Fallback if single ticker (unlikely here but good practice)
                    if sym == data.name: # data.name might not be available like this, but for bulk download structure is different
                         series = data['Close']
                    else:
                         continue

                if not series.empty:
                    # Normalize to 100 at start to give equal weight
                    first_valid = series.first_valid_index()
                    if first_valid:
                        normalized = (series / series.loc[first_valid]) * 100
                        industry_prices[sym] = normalized
            except Exception as e:
                # print(f"Skipping {sym}: {e}")
                pass

        if industry_prices.empty:
            continue

        # Create Equal-Weighted Industry Index
        industry_index = industry_prices.mean(axis=1).dropna()

        if industry_index.empty:
            continue

        # Calculate Score
        score = calculate_momentum_score(industry_index)

        # Check Trend Filter
        is_trending = check_trend_filter(industry_index)

        if is_trending:
            industry_scores.append({
                'Industry': industry,
                'Score': score,
                'StockCount': len(industry_prices.columns)
            })

    # Sort by Score
    industry_scores.sort(key=lambda x: x['Score'], reverse=True)

    print("\nTop Trending Industries:")
    top_industries = []
    for i, item in enumerate(industry_scores[:5]): # Top 5
        print(f"{i+1}. {item['Industry']} (Score: {item['Score']:.4f}, Stocks: {item['StockCount']})")
        top_industries.append(item['Industry'])

    if not top_industries:
        print("No industries met the trending criteria.")
        return

    # Filter original CSV
    filtered_df = df[df['Industry'].isin(top_industries)]

    print(f"\nFiltering stocks... Retained {len(filtered_df)} stocks from {len(df)}.")
    filtered_df.to_csv(args.output, index=False)
    print(f"Saved filtered list to {args.output}")

if __name__ == "__main__":
    main()
