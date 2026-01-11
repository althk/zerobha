import pandas as pd
import yfinance as yf
from datetime import datetime, timedelta


def calculate_sector_momentum(sectors, benchmark='^NSEI'):
    """
    Calculates the Composite Relative Strength Score for Indian sectors.
    Weightage: 1-Month (40%), 3-Month (30%), 6-Month (30%)
    Default Benchmark: Nifty 50 (^NSEI)
    """

    # Define time windows (approximate trading days)
    windows = {
        '1M': 21,
        '3M': 63,
        '6M': 126
    }

    # Calculate required lookback (add buffer for weekends/holidays)
    max_lookback = max(windows.values()) + 20
    start_date = (datetime.now() -
                  timedelta(days=int(max_lookback * 1.5))).strftime('%Y-%m-%d')

    # 1. Fetch Data
    tickers = [benchmark] + list(sectors.values())
    print(
        f"Fetching NSE data for {len(tickers)} symbols starting from {start_date}...")

    try:
        data = yf.download(tickers, start=start_date,
                           interval='1d', auto_adjust=True)['Close']
    except Exception as e:
        print(f"Error fetching data: {e}")
        return pd.DataFrame()

    # Handle missing values (forward fill then backward fill for any gaps at the start)
    data = data.ffill().bfill()

    results = []

    # 2. Process Benchmark Performance
    bench_perf = {}
    for label, days in windows.items():
        if len(data) <= days:
            continue
        current_price = data[benchmark].iloc[-1]
        past_price = data[benchmark].iloc[-(days + 1)]
        bench_perf[label] = (current_price / past_price) - 1

    # 3. Process Sector Performance
    for sector_name, ticker in sectors.items():
        if ticker not in data.columns:
            print(f"Skipping {ticker}: No data found.")
            continue

        sector_prices = data[ticker]

        # Calculate RS for each window
        rs_scores = {}
        valid_sector = True
        for label, days in windows.items():
            if len(sector_prices) <= days:
                valid_sector = False
                break
            s_current = sector_prices.iloc[-1]
            s_past = sector_prices.iloc[-(days + 1)]
            s_perf = (s_current / s_past) - 1

            # Relative Strength = Sector % - Benchmark %
            rs_scores[label] = s_perf - bench_perf.get(label, 0)

        if not valid_sector:
            continue

        # 4. Calculate Weighted Composite Score
        # Formula: (0.40 * 1M_RS) + (0.30 * 3M_RS) + (0.30 * 6M_RS)
        composite_score = (
            (0.40 * rs_scores['1M']) +
            (0.30 * rs_scores['3M']) +
            (0.30 * rs_scores['6M'])
        ) * 100  # Scale for readability

        # 5. Trend Confirmation (Short Term > Medium Term)
        is_improving = rs_scores['1M'] > rs_scores['3M']

        results.append({
            'Sector': sector_name,
            'Ticker': ticker,
            '1M_RS (%)': round(rs_scores['1M'] * 100, 2),
            '3M_RS (%)': round(rs_scores['3M'] * 100, 2),
            '6M_RS (%)': round(rs_scores['6M'] * 100, 2),
            'Composite_Score': round(composite_score, 4),
            'Status': 'LEADERSHIP' if composite_score > 0 and is_improving else
                      'ACCELERATING' if is_improving else 'LAGGING'
        })

    # Create DataFrame and Rank
    df = pd.DataFrame(results)
    if not df.empty:
        df = df.sort_values(by='Composite_Score',
                            ascending=False).reset_index(drop=True)
    return df


if __name__ == "__main__":
    # Standard NSE Sectoral Indices
    # Note: Yahoo Finance symbols for NSE indices often start with ^
    nse_sectors = {
        'Nifty Bank': '^NSEBANK',
        'Nifty IT': '^CNXIT',
        'Nifty FMCG': '^CNXFMCG',
        'Nifty Pharma': '^CNXPHARMA',
        'Nifty Auto': '^CNXAUTO',
        'Nifty Metal': '^CNXMETAL',
        'Nifty Reality': '^CNXREALTY',
        'Nifty Media': '^CNXMEDIA',
        'Nifty Energy': '^CNXENERGY',
        'Nifty Infra': '^CNXINFRA',
        'Nifty PSU Bank': '^CNXPSUBANK',
        'Nifty Private Bank': 'NIFTY_PVT_BANK.NS'  # Alternative symbol format
    }

    # Benchmark: Nifty 50
    report = calculate_sector_momentum(nse_sectors, benchmark='^NSEI')

    if not report.empty:
        print("\n--- INDIAN SECTOR RELATIVE STRENGTH RANKING (vs NIFTY 50) ---")
        print(report.to_string(index=False))

        print("\nInterpretation Guide:")
        print(
            "1. LEADERSHIP: Outperforming the market and momentum is picking up (Buy Zone).")
        print("2. ACCELERATING: Momentum is improving relative to mid-term, watch for breakout.")
        print("3. LAGGING: Underperforming or losing steam. Avoid for momentum plays.")
    else:
        print("No data could be retrieved. Please check your internet connection or ticker symbols.")
