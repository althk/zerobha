#!/usr/bin/env python3
"""
Sector Momentum Analyzer.

Calculates Relative Rotation Graph (RRG) style analysis for NSE sectoral indices
vs Nifty 50 benchmark. Outputs ranked sectors with quadrant classification.

Weightage: 1W (30%), 1M (40%), 3M (30%)
"""

import argparse
import pandas as pd
import json
import time
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, time as dt_time

# Sector index to Nifty 500 industry mapping.
# Used by build_watchlist.py to filter stocks belonging to top sectors.
# NOTE: names must match the Industry column of the Nifty 500 CSV exactly.
SECTOR_INDUSTRY_MAP = {
    "Nifty Bank": ["Financial Services"],
    "Nifty IT": ["Information Technology"],
    "Nifty FMCG": ["Fast Moving Consumer Goods"],
    "Nifty Pharma": ["Healthcare"],
    "Nifty Auto": ["Automobile and Auto Components"],
    "Nifty Metal": ["Metals & Mining"],
    "Nifty Realty": ["Realty"],
    "Nifty Media": ["Media Entertainment & Publication"],
    "Nifty Energy": ["Oil Gas & Consumable Fuels", "Power"],
    "Nifty Infra": ["Construction", "Capital Goods"],
    "Nifty PSU Bank": ["Financial Services"],
    "Nifty Private Bank": ["Financial Services"],
    "Nifty PSE": ["Power", "Oil Gas & Consumable Fuels", "Capital Goods"],
    "Nifty Consumption": [
        "Consumer Durables",
        "Fast Moving Consumer Goods",
        "Consumer Services",
        "Automobile and Auto Components",
    ],
    "Nifty Service Sector": ["Consumer Services"],
    "Nifty Financial Services": ["Financial Services"],
    "Nifty Commodities": [
        "Chemicals",
        "Metals & Mining",
        "Construction Materials",
        "Oil Gas & Consumable Fuels",
        "Power",
    ],
}

# Default NSE sectoral indices (Upstox instrument keys). Yahoo stopped serving
# history for most of these in 2026-10 (one bar per ticker), so the data now
# comes from Upstox, which serves index candles anonymously.
NSE_SECTORS = {
    "Nifty Bank": "NSE_INDEX|Nifty Bank",
    "Nifty IT": "NSE_INDEX|Nifty IT",
    "Nifty FMCG": "NSE_INDEX|Nifty FMCG",
    "Nifty Pharma": "NSE_INDEX|Nifty Pharma",
    "Nifty Auto": "NSE_INDEX|Nifty Auto",
    "Nifty Consumption": "NSE_INDEX|Nifty Consumption",
    "Nifty PSE": "NSE_INDEX|Nifty PSE",
    "Nifty Service Sector": "NSE_INDEX|Nifty Serv Sector",
    "Nifty Metal": "NSE_INDEX|Nifty Metal",
    "Nifty Realty": "NSE_INDEX|Nifty Realty",
    "Nifty Media": "NSE_INDEX|Nifty Media",
    "Nifty Energy": "NSE_INDEX|Nifty Energy",
    "Nifty Infra": "NSE_INDEX|Nifty Infra",
    "Nifty PSU Bank": "NSE_INDEX|Nifty PSU Bank",
    "Nifty Private Bank": "NSE_INDEX|Nifty Pvt Bank",
    "Nifty Financial Services": "NSE_INDEX|Nifty Fin Service",
}

BENCHMARK = "NSE_INDEX|Nifty 50"

# Upstox v3 daily candles; newest bar first, [ts, o, h, l, c, vol, oi]
UPSTOX_CANDLE_URL = (
    "https://api.upstox.com/v3/historical-candle/{key}/days/1/{to}/{frm}"
)
# Cloudflare refuses Python's default User-Agent (error 1010), and a burst of
# parallel requests earns a ~15-minute ban (1015), so fetch one at a time.
UPSTOX_HEADERS = {"Accept": "application/json", "User-Agent": "zerobha-sectors/1.0"}
UPSTOX_PACE_SEC = 0.3

# Weights for composite score
WEIGHT_1W = 0.30
WEIGHT_1M = 0.40
WEIGHT_3M = 0.30

# Trading days per period
WINDOWS = {
    "1W": 5,
    "1M": 21,
    "3M": 63,
}

# Shift (in trading days) for the previous-week snapshot used in
# momentum-of-momentum
PREV_SHIFT = 5

# NSE regular session close (local time); bars before this are partial
NSE_CLOSE = dt_time(15, 30)


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
        return "LEADING"
    elif rs_ratio_positive and not rs_momentum_positive:
        return "WEAKENING"
    elif not rs_ratio_positive and rs_momentum_positive:
        return "IMPROVING"
    else:
        return "LAGGING"


def window_perf(series, days, offset=0):
    """
    Return over `days` trading days, ending `offset` rows back from the
    latest bar. Returns None if the series is too short or values are NaN.
    """
    if len(series) < days + offset + 1:
        return None
    current = series.iloc[-(offset + 1)]
    past = series.iloc[-(days + offset + 1)]
    if pd.isna(current) or pd.isna(past) or past == 0:
        return None
    return (current / past) - 1


def fetch_daily_closes(keys, start_date, end_date):
    """
    Daily closes for Upstox instrument keys over [start_date, end_date], one
    column per key, indexed by date. A key that fails is left out (and
    reported) rather than aborting the whole run.
    """
    closes = {}
    for key in keys:
        url = UPSTOX_CANDLE_URL.format(
            key=urllib.parse.quote(key), to=end_date, frm=start_date
        )
        try:
            req = urllib.request.Request(url, headers=UPSTOX_HEADERS)
            with urllib.request.urlopen(req, timeout=30) as resp:
                candles = json.load(resp)["data"]["candles"]
        except Exception as e:
            print(f"  Fetch failed for {key}: {e}")
            continue
        finally:
            time.sleep(UPSTOX_PACE_SEC)
        if candles:
            closes[key] = pd.Series(
                {pd.Timestamp(c[0][:10]): float(c[4]) for c in candles}
            )
    if not closes:
        return pd.DataFrame()
    return pd.DataFrame(closes).sort_index()


def calculate_sector_momentum(sectors, benchmark=BENCHMARK, as_of_date=None):
    """
    Calculates Composite Relative Strength Score for NSE sectors.

    Uses 1W/1M/3M windows with RRG quadrant classification and
    momentum-of-momentum detection.
    """
    as_of_date = as_of_date or datetime.now()
    weights = {"1W": WEIGHT_1W, "1M": WEIGHT_1M, "3M": WEIGHT_3M}

    # Need enough history for 3M + previous-week shift + holiday buffer
    max_lookback = WINDOWS["3M"] + PREV_SHIFT + 20
    start_date = (as_of_date - timedelta(days=int(max_lookback * 1.7))).strftime(
        "%Y-%m-%d"
    )

    tickers = [benchmark] + list(sectors.values())
    print(f"Fetching data for {len(tickers)} symbols from {start_date}...")

    data = fetch_daily_closes(tickers, start_date, as_of_date.strftime("%Y-%m-%d"))
    if data.empty:
        print("Error fetching data: no candles returned")
        return pd.DataFrame()

    # Forward-fill interior gaps (holidays, missed sessions). Do NOT bfill:
    # back-filling copies future prices into the past and fabricates flat
    # returns for indices with a short listing history.
    data = data.ffill()

    # Drop today's bar if the session is still open: it is partial and would
    # contaminate the short windows (assumes this machine runs on IST).
    if len(data) > 0:
        now = datetime.now()
        if data.index[-1].date() == now.date() and now.time() < NSE_CLOSE:
            data = data.iloc[:-1]
            print("  Dropped today's partial bar (market still open)")

    min_rows = WINDOWS["3M"] + 1

    if benchmark not in data.columns or data[benchmark].dropna().empty:
        print(f"Error: no data for benchmark {benchmark}")
        return pd.DataFrame()

    bench = data[benchmark].dropna()
    if len(bench) < min_rows:
        print(
            f"Error: insufficient benchmark history "
            f"({len(bench)} rows, need {min_rows})"
        )
        return pd.DataFrame()

    # Benchmark performance for each window
    bench_perf = {}
    bench_perf_prev = {}  # Previous period (for momentum-of-momentum)
    for label, days in WINDOWS.items():
        perf = window_perf(bench, days)
        if perf is not None:
            bench_perf[label] = perf

        prev = window_perf(bench, days, offset=PREV_SHIFT)
        if prev is not None:
            bench_perf_prev[label] = prev

    results = []

    for sector_name, ticker in sectors.items():
        if ticker not in data.columns:
            print(f"  Skipping {sector_name} ({ticker}): no data")
            continue

        # dropna trims the leading NaNs of short-history tickers; interior
        # gaps were already forward-filled above
        prices = data[ticker].dropna()
        if len(prices) < min_rows:
            print(
                f"  Skipping {sector_name} ({ticker}): insufficient history "
                f"({len(prices)} rows, need {min_rows})"
            )
            continue

        # Current RS for each window
        rs_scores = {}
        rs_scores_prev = {}  # RS from 1 week ago (for momentum-of-momentum)
        valid = True

        for label, days in WINDOWS.items():
            perf = window_perf(prices, days)
            # Require the benchmark window too: defaulting it to 0 would
            # silently turn relative strength into absolute performance
            if perf is None or label not in bench_perf:
                valid = False
                break
            rs_scores[label] = perf - bench_perf[label]

            # Previous period RS (1 week ago snapshot)
            prev_perf = window_perf(prices, days, offset=PREV_SHIFT)
            if prev_perf is not None and label in bench_perf_prev:
                rs_scores_prev[label] = prev_perf - bench_perf_prev[label]

        if not valid:
            print(f"  Skipping {sector_name} ({ticker}): incomplete window data")
            continue

        # Composite score
        composite = sum(weights[k] * rs_scores[k] for k in WINDOWS) * 100

        # Previous composite (for momentum-of-momentum)
        prev_composite = None
        if len(rs_scores_prev) == len(WINDOWS):
            prev_composite = sum(weights[k] * rs_scores_prev[k] for k in WINDOWS) * 100

        # Momentum-of-momentum: is the composite score itself rising?
        mom_of_mom = None
        if prev_composite is not None:
            mom_of_mom = composite - prev_composite

        # Momentum axis: prefer momentum-of-momentum (composite vs. a week
        # ago — an apples-to-apples comparison). Fall back to per-day
        # normalized short-vs-medium RS when prior history is unavailable;
        # raw 1W vs 1M values are not comparable since longer windows have
        # naturally larger magnitudes.
        if mom_of_mom is not None:
            rs_momentum_positive = mom_of_mom > 0
        else:
            rs_momentum_positive = (
                rs_scores["1W"] / WINDOWS["1W"] > rs_scores["1M"] / WINDOWS["1M"]
            )

        # RRG quadrant
        quadrant = classify_quadrant(composite > 0, rs_momentum_positive)

        results.append(
            {
                "Sector": sector_name,
                "Ticker": ticker,
                "1W_RS": round(rs_scores["1W"] * 100, 2),
                "1M_RS": round(rs_scores["1M"] * 100, 2),
                "3M_RS": round(rs_scores["3M"] * 100, 2),
                "Composite": round(composite, 2),
                "Mom_of_Mom": round(mom_of_mom, 2) if mom_of_mom is not None else None,
                "Quadrant": quadrant,
            }
        )

    df = pd.DataFrame(results)
    if not df.empty:
        df = df.sort_values("Composite", ascending=False).reset_index(drop=True)
    return df


def main():
    parser = argparse.ArgumentParser(
        description="NSE Sector Momentum Analyzer (RRG-style)"
    )
    parser.add_argument(
        "--output",
        type=str,
        default="top_sectors.csv",
        help="Output CSV path (default: top_sectors.csv)",
    )
    parser.add_argument(
        "--benchmark",
        type=str,
        default=BENCHMARK,
        help=f"Benchmark Upstox instrument key (default: {BENCHMARK})",
    )
    parser.add_argument(
        "--top",
        type=int,
        default=4,
        help="Number of top sectors to highlight (default: 4)",
    )
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
    print(
        f"Weights: 1W={WEIGHT_1W*100:.0f}% | 1M={WEIGHT_1M*100:.0f}% | 3M={WEIGHT_3M*100:.0f}%"
    )
    print(f"{'='*80}\n")

    for _, row in report.iterrows():
        quad_icon = {
            "LEADING": "+",
            "IMPROVING": "~",
            "WEAKENING": "-",
            "LAGGING": "x",
        }.get(row["Quadrant"], "?")

        mom = f"  MoM: {row['Mom_of_Mom']:+.2f}" if pd.notna(row["Mom_of_Mom"]) else ""

        print(
            f"  [{quad_icon}] {row['Sector']:<20s}  "
            f"1W: {row['1W_RS']:+6.2f}%  "
            f"1M: {row['1M_RS']:+6.2f}%  "
            f"3M: {row['3M_RS']:+6.2f}%  "
            f"Composite: {row['Composite']:+6.2f}  "
            f"{row['Quadrant']:<11s}{mom}"
        )

    print(f"\n{'='*80}")
    print("Quadrants:  [+] LEADING (trade)  [~] IMPROVING (watch)")
    print("            [-] WEAKENING (trim)  [x] LAGGING (avoid)")
    print(f"{'='*80}")

    # Show recommended sectors
    top = report[report["Quadrant"].isin(["LEADING", "IMPROVING"])].head(args.top)
    if not top.empty:
        print(f"\nRecommended sectors for stock selection:")
        for _, row in top.iterrows():
            industries = SECTOR_INDUSTRY_MAP.get(row["Sector"], [])
            print(
                f"  {row['Sector']} ({row['Quadrant']}) -> Industries: {', '.join(industries)}"
            )
    else:
        print("\nNo sectors in LEADING/IMPROVING quadrant. Consider reducing exposure.")


if __name__ == "__main__":
    main()
