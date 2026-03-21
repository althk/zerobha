#!/usr/bin/env python3
"""
Build Watchlist Pipeline.

Chains sector momentum analysis with stock selection to produce a filtered
watchlist CSV for the ORB trading strategy.

Pipeline:
  1. Run sector momentum analyzer -> identify top sectors (LEADING/IMPROVING)
  2. Map sectors to Nifty 500 industries
  3. Filter stocks to those industries, calculate beta + RS
  4. Rank and output final watchlist CSV

Usage:
  python scripts/build_watchlist.py
  python scripts/build_watchlist.py --top-sectors 3 --min-beta 1.2 --limit 30
  python scripts/build_watchlist.py --output my_watchlist.csv
"""
import argparse
import os
import sys
import subprocess
from datetime import datetime

import pandas as pd

# Add scripts dir to path for imports
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sector_momentum_analyzer import (
    calculate_sector_momentum, NSE_SECTORS, SECTOR_INDUSTRY_MAP
)


def main():
    parser = argparse.ArgumentParser(
        description='Build trading watchlist from sector momentum + high beta analysis')
    parser.add_argument('--symbols', type=str, default='ind_nifty500list.csv',
                        help='Nifty 500 CSV with Symbol and Industry columns')
    parser.add_argument('--output', type=str, default='high_beta_stocks.csv',
                        help='Final watchlist output CSV')
    parser.add_argument('--top-sectors', type=int, default=4,
                        help='Number of top sectors to pick (default: 4)')
    parser.add_argument('--min-beta', type=float, default=1.0,
                        help='Minimum beta threshold (default: 1.0)')
    parser.add_argument('--limit', type=int, default=50,
                        help='Max stocks in final watchlist (default: 50)')
    parser.add_argument('--workers', type=int, default=os.cpu_count(),
                        help='Parallel workers for stock data download')
    parser.add_argument('--sector-output', type=str, default='top_sectors.csv',
                        help='Sector analysis output CSV')
    parser.add_argument('--include-all-if-no-leaders', action='store_true',
                        help='Fall back to all sectors if no LEADING/IMPROVING found')
    args = parser.parse_args()

    print(f"{'='*70}")
    print(f"BUILD WATCHLIST PIPELINE")
    print(f"{'='*70}")

    # Step 1: Sector momentum analysis
    print(f"\n[Step 1/3] Running sector momentum analysis...")
    sector_report = calculate_sector_momentum(NSE_SECTORS)

    if sector_report.empty:
        print("ERROR: Sector analysis failed. Check internet connection.")
        sys.exit(1)

    sector_report.to_csv(args.sector_output, index=False)
    print(f"  Saved sector report to {args.sector_output}")

    # Step 2: Pick top sectors and map to industries
    print(f"\n[Step 2/3] Selecting top sectors...")

    # Prefer LEADING and IMPROVING quadrants
    actionable = sector_report[
        sector_report['Quadrant'].isin(['LEADING', 'IMPROVING'])
    ].head(args.top_sectors)

    if actionable.empty:
        if args.include_all_if_no_leaders:
            print("  No LEADING/IMPROVING sectors found. Using top by composite score.")
            actionable = sector_report.head(args.top_sectors)
        else:
            print("  No LEADING/IMPROVING sectors found.")
            print("  Use --include-all-if-no-leaders to fall back to top sectors by score.")
            # Still produce a watchlist from top composite sectors
            actionable = sector_report.head(args.top_sectors)

    selected_sectors = actionable['Sector'].tolist()
    print(f"  Selected sectors: {selected_sectors}")

    # Map to Nifty 500 industry names
    industries = set()
    for sector in selected_sectors:
        mapped = SECTOR_INDUSTRY_MAP.get(sector, [])
        industries.update(mapped)

    if not industries:
        print("  WARNING: No industry mapping found for selected sectors.")
        print("  Falling back to all industries.")
        industries = None

    if industries:
        print(f"  Mapped industries: {sorted(industries)}")

    # Step 3: Run find_high_beta with sector filter
    print(f"\n[Step 3/3] Finding high-beta stocks in selected sectors...")

    script_dir = os.path.dirname(os.path.abspath(__file__))
    find_beta_script = os.path.join(script_dir, 'find_high_beta.py')

    cmd = [
        sys.executable, find_beta_script,
        '--symbols', args.symbols,
        '--output', args.output,
        '--workers', str(args.workers),
        '--min-beta', str(args.min_beta),
        '--limit', str(args.limit),
    ]

    if industries:
        cmd.extend(['--sectors', ','.join(sorted(industries))])

    result = subprocess.run(cmd, capture_output=False)

    if result.returncode != 0:
        print("ERROR: Stock analysis failed.")
        sys.exit(1)

    # Summary
    print(f"\n{'='*70}")
    print(f"PIPELINE COMPLETE")
    print(f"{'='*70}")

    if os.path.exists(args.output):
        df = pd.read_csv(args.output)
        print(f"  Watchlist: {args.output} ({len(df)} stocks)")
        print(f"  Sectors:   {args.sector_output}")

        if not df.empty:
            print(f"\n  Top 10 stocks:")
            for _, row in df.head(10).iterrows():
                industry = row.get('industry', '')
                rs = row.get('rs_1m')
                rs_str = f"  RS: {rs:+.2f}%" if pd.notna(rs) else ""
                print(f"    {row['symbol']:<20s} beta={row['beta']:.2f}  "
                      f"{industry}{rs_str}")

    # Display sector context
    print(f"\n  Sector context:")
    for _, row in actionable.iterrows():
        print(f"    {row['Sector']:<20s} {row['Quadrant']:<11s} "
              f"Composite={row['Composite']:+.2f}")


if __name__ == '__main__':
    main()
