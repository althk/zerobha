#!/usr/bin/env python3
"""
Sector Momentum Analyzer.

Calculates Relative Rotation Graph (RRG) style analysis for NSE sectoral indices
vs Nifty 50 benchmark. Outputs ranked sectors with quadrant classification.

Weightage: 1W (30%), 1M (40%), 3M (30%)
"""
import argparse
import pandas as pd
import yfinance as yf
from datetime import datetime, timedelta

# Sector index to Nifty 500 industry mapping.
# Used by build_watchlist.py to filter stocks belonging to top sectors.
SECTOR_INDUSTRY_MAP = {
    'Nifty Bank': ['Financial Services'],
    'Nifty IT': ['Information Technology'],
    'Nifty FMCG': ['Fast Moving Consumer Goods'],
    'Nifty Pharma': ['Healthcare'],
    'Nifty Auto': ['Automobile and Auto Components'],
    'Nifty Metal': ['Metals & Mining'],
    'Nifty Reality': ['Realty'],
    'Nifty Media': ['Media Entertainment & Publication'],
    'Nifty Energy': ['Oil Gas & Consumable Fuels', 'Power'],
    'Nifty Infra': ['Construction', 'Capital Goods'],
    'Nifty PSU Bank': ['Financial Services'],
    'Nifty Private Bank': ['Financial Services'],
    'Nifty Consumption': ['Consumer Durables', 'Consumer Services'],
    'Nifty Commodities': ['Chemicals', 'Forest Materials'],
}

# Default NSE sectoral indices (Yahoo Finance tickers)
NSE_SECTORS = {
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
    'Nifty Private Bank': 'NIFTY_PVT_BANK.NS',
}

# Weights for composite score
WEIGHT_1W = 0.30
WEIGHT_1M = 0.40
WEIGHT_3M = 0.30

# Trading days per period
WINDOWS = {
    '1W': 5,
    '1M': 21,
    '3M': 63,
}


def classify_quadrant(rs_ratio_positive, rs_momentum_positive):
    """
    RRG-style quadrant classification.

    rs_ratio_positive: composite score > 0 (outperforming benchmark)
    rs_momentum_positive: short-term RS improving vs medium-term (momentum rising)

    Quadrants:
      Leading    = strong RS + rising momentum  (best to trade)
      Weakening  = strong RS + falling momentum (take profits)
      Lagging    = weak RS + falling momentum   (avoid)
      Improving  = weak RS + rising momentum    (watch for entry)
    """
    if rs_ratio_positive and rs_momentum_positive:
        return 'LEADING'
    elif rs_ratio_positive and not rs_momentum_positive:
        return 'WEAKENING'
    elif not rs_ratio_positive and rs_momentum_positive:
        return 'IMPROVING'
    else:
        return 'LAGGING'


def calculate_sector_momentum(sectors, benchmark='^NSEI'):
    """
    Calculates Composite Relative Strength Score for NSE sectors.

    Uses 1W/1M/3M windows with RRG quadrant classification and
    momentum-of-momentum detection.
    """
    weights = {'1W': WEIGHT_1W, '1M': WEIGHT_1M, '3M': WEIGHT_3M}

    # Need enough history for 3M + buffer for previous period (for momentum-of-momentum)
    max_lookback = WINDOWS['3M'] + WINDOWS['1M'] + 20
    start_date = (datetime.now() -
                  timedelta(days=int(max_lookback * 1.5))).strftime('%Y-%m-%d')

    tickers = [benchmark] + list(sectors.values())
    print(f"Fetching data for {len(tickers)} symbols from {start_date}...")

    try:
        data = yf.download(tickers, start=start_date,
                           interval='1d', auto_adjust=True)['Close']
    except Exception as e:
        print(f"Error fetching data: {e}")
        return pd.DataFrame()

    data = data.ffill().bfill()

    # Benchmark performance for each window
    bench_perf = {}
    bench_perf_prev = {}  # Previous period (for momentum-of-momentum)
    for label, days in WINDOWS.items():
        if len(data) <= days:
            continue
        current = data[benchmark].iloc[-1]
        past = data[benchmark].iloc[-(days + 1)]
        bench_perf[label] = (current / past) - 1

        # Previous period: shift back by 1W (5 days) to compare
        if len(data) > days + 5:
            prev_current = data[benchmark].iloc[-6]  # 1 week ago
            prev_past = data[benchmark].iloc[-(days + 6)]
            bench_perf_prev[label] = (prev_current / prev_past) - 1

    results = []

    for sector_name, ticker in sectors.items():
        if ticker not in data.columns:
            print(f"  Skipping {ticker}: no data")
            continue

        prices = data[ticker]

        # Current RS for each window
        rs_scores = {}
        rs_scores_prev = {}  # RS from 1 week ago (for momentum-of-momentum)
        valid = True

        for label, days in WINDOWS.items():
            if len(prices) <= days:
                valid = False
                break

            current = prices.iloc[-1]
            past = prices.iloc[-(days + 1)]
            perf = (current / past) - 1
            rs_scores[label] = perf - bench_perf.get(label, 0)

            # Previous period RS (1 week ago snapshot)
            if len(prices) > days + 5:
                prev_current = prices.iloc[-6]
                prev_past = prices.iloc[-(days + 6)]
                prev_perf = (prev_current / prev_past) - 1
                rs_scores_prev[label] = prev_perf - bench_perf_prev.get(label, 0)

        if not valid:
            continue

        # Composite score
        composite = sum(weights[k] * rs_scores[k] for k in WINDOWS) * 100

        # Previous composite (for momentum-of-momentum)
        prev_composite = None
        if len(rs_scores_prev) == len(WINDOWS):
            prev_composite = sum(weights[k] * rs_scores_prev[k] for k in WINDOWS) * 100

        # RS momentum: is short-term RS improving vs medium-term?
        rs_momentum_positive = rs_scores['1W'] > rs_scores['1M']

        # Momentum-of-momentum: is the composite score itself rising?
        mom_of_mom = None
        if prev_composite is not None:
            mom_of_mom = composite - prev_composite

        # RRG quadrant
        quadrant = classify_quadrant(composite > 0, rs_momentum_positive)

        results.append({
            'Sector': sector_name,
            'Ticker': ticker,
            '1W_RS': round(rs_scores['1W'] * 100, 2),
            '1M_RS': round(rs_scores['1M'] * 100, 2),
            '3M_RS': round(rs_scores['3M'] * 100, 2),
            'Composite': round(composite, 2),
            'Mom_of_Mom': round(mom_of_mom, 2) if mom_of_mom is not None else None,
            'Quadrant': quadrant,
        })

    df = pd.DataFrame(results)
    if not df.empty:
        df = df.sort_values('Composite', ascending=False).reset_index(drop=True)
    return df


def main():
    parser = argparse.ArgumentParser(
        description='NSE Sector Momentum Analyzer (RRG-style)')
    parser.add_argument('--output', type=str, default='top_sectors.csv',
                        help='Output CSV path (default: top_sectors.csv)')
    parser.add_argument('--benchmark', type=str, default='^NSEI',
                        help='Benchmark ticker (default: ^NSEI)')
    parser.add_argument('--top', type=int, default=4,
                        help='Number of top sectors to highlight (default: 4)')
    args = parser.parse_args()

    report = calculate_sector_momentum(NSE_SECTORS, benchmark=args.benchmark)

    if report.empty:
        print("No data retrieved. Check internet connection or ticker symbols.")
        return

    # Save full report
    report.to_csv(args.output, index=False)
    print(f"\nFull report saved to {args.output}")

    # Display
    print(f"\n{'='*80}")
    print(f"SECTOR RELATIVE STRENGTH RANKING (vs Nifty 50)")
    print(f"Weights: 1W={WEIGHT_1W*100:.0f}% | 1M={WEIGHT_1M*100:.0f}% | 3M={WEIGHT_3M*100:.0f}%")
    print(f"{'='*80}\n")

    for _, row in report.iterrows():
        quad_icon = {
            'LEADING': '+',
            'IMPROVING': '~',
            'WEAKENING': '-',
            'LAGGING': 'x',
        }.get(row['Quadrant'], '?')

        mom = f"  MoM: {row['Mom_of_Mom']:+.2f}" if pd.notna(row['Mom_of_Mom']) else ""

        print(f"  [{quad_icon}] {row['Sector']:<20s}  "
              f"1W: {row['1W_RS']:+6.2f}%  "
              f"1M: {row['1M_RS']:+6.2f}%  "
              f"3M: {row['3M_RS']:+6.2f}%  "
              f"Composite: {row['Composite']:+6.2f}  "
              f"{row['Quadrant']:<11s}{mom}")

    print(f"\n{'='*80}")
    print("Quadrants:  [+] LEADING (trade)  [~] IMPROVING (watch)")
    print("            [-] WEAKENING (trim)  [x] LAGGING (avoid)")
    print(f"{'='*80}")

    # Show recommended sectors
    top = report[report['Quadrant'].isin(['LEADING', 'IMPROVING'])].head(args.top)
    if not top.empty:
        print(f"\nRecommended sectors for stock selection:")
        for _, row in top.iterrows():
            industries = SECTOR_INDUSTRY_MAP.get(row['Sector'], [])
            print(f"  {row['Sector']} ({row['Quadrant']}) -> Industries: {', '.join(industries)}")
    else:
        print("\nNo sectors in LEADING/IMPROVING quadrant. Consider reducing exposure.")


if __name__ == '__main__':
    main()
