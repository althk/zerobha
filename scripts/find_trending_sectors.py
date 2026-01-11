#!/usr/bin/env python3
"""
Find Trending Sectors Script.

Analyzes Nifty 500 stocks to identify top trending sectors based on relative strength
vs a benchmark index (Nifty 50 by default). Calculates both absolute and relative returns
across different time periods (1 week, 1 month, 3 months) and generates buy/sell signals
based on momentum patterns.
"""
import argparse
import concurrent.futures
import logging
import os
import sys
from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import List, Optional, Tuple

import pandas as pd
import yfinance as yf

# Configure logging with file:linenumber
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(filename)s:%(lineno)d - %(message)s',
    handlers=[logging.StreamHandler(sys.stdout)]
)
logger = logging.getLogger(__name__)

# Weight configuration for different time periods
WEIGHT_1W = 0.5    # 50% weight for 1 week returns
WEIGHT_1M = 0.3    # 30% weight for 1 month returns
WEIGHT_3M = 0.2    # 20% weight for 3 month returns


@dataclass
class StockReturn:
    """Represents calculated returns for a single stock."""
    symbol: str
    industry: str
    return_1w: Optional[float]
    return_1m: Optional[float]
    return_3m: Optional[float]
    weighted_return: Optional[float]
    # Relative strength vs benchmark
    rs_1w: Optional[float]
    rs_1m: Optional[float]
    rs_3m: Optional[float]
    weighted_rs: Optional[float]


def calculate_returns(prices: pd.Series) -> Tuple[Optional[float], Optional[float], Optional[float]]:
    """
    Calculate 1-week, 1-month, and 3-month returns from price series.

    Args:
        prices: Series of closing prices indexed by date.

    Returns:
        Tuple of (1w_return, 1m_return, 3m_return) as percentages.
    """
    if prices.empty or len(prices) < 5:
        return None, None, None

    prices = prices.sort_index()
    current_price = prices.iloc[-1]

    # Calculate trading days for each period
    # Approximate trading days: 1 week ~5 days, 1 month ~22 days, 3 months ~66 days
    return_1w = None
    return_1m = None
    return_3m = None

    if len(prices) >= 5:
        price_1w_ago = prices.iloc[-min(5, len(prices))]
        if price_1w_ago > 0:
            return_1w = ((current_price - price_1w_ago) / price_1w_ago) * 100

    if len(prices) >= 22:
        price_1m_ago = prices.iloc[-min(22, len(prices))]
        if price_1m_ago > 0:
            return_1m = ((current_price - price_1m_ago) / price_1m_ago) * 100

    if len(prices) >= 66:
        price_3m_ago = prices.iloc[-min(66, len(prices))]
        if price_3m_ago > 0:
            return_3m = ((current_price - price_3m_ago) / price_3m_ago) * 100

    return return_1w, return_1m, return_3m


def calculate_weighted_return(
    return_1w: Optional[float],
    return_1m: Optional[float],
    return_3m: Optional[float]
) -> Optional[float]:
    """
    Calculate weighted average return from individual period returns.

    Uses configurable weights for each period. If some returns are missing,
    redistributes weights proportionally.

    Args:
        return_1w: 1-week return percentage.
        return_1m: 1-month return percentage.
        return_3m: 3-month return percentage.

    Returns:
        Weighted average return or None if insufficient data.
    """
    returns = []
    weights = []

    if return_1w is not None:
        returns.append(return_1w)
        weights.append(WEIGHT_1W)

    if return_1m is not None:
        returns.append(return_1m)
        weights.append(WEIGHT_1M)

    if return_3m is not None:
        returns.append(return_3m)
        weights.append(WEIGHT_3M)

    if not returns:
        return None

    # Normalize weights to sum to 1
    total_weight = sum(weights)
    normalized_weights = [w / total_weight for w in weights]

    weighted_return = sum(r * w for r, w in zip(returns, normalized_weights))
    return weighted_return


def fetch_benchmark_returns(
    benchmark_symbol: str, start_str: str, end_str: str
) -> Tuple[Optional[float], Optional[float], Optional[float]]:
    """
    Fetch benchmark data and calculate returns for all time periods.

    Args:
        benchmark_symbol: Ticker symbol for benchmark (e.g., ^NSEI for Nifty 50).
        start_str: Start date string.
        end_str: End date string.

    Returns:
        Tuple of (1w_return, 1m_return, 3m_return) for the benchmark.
    """
    try:
        logger.info(f"Fetching benchmark data for {benchmark_symbol}...")
        data = yf.download(
            benchmark_symbol, start=start_str, end=end_str, progress=False, auto_adjust=True
        )

        if data.empty:
            logger.warning(f"No data for benchmark {benchmark_symbol}")
            return None, None, None

        close_prices = data["Close"]
        if hasattr(close_prices, "iloc") and len(close_prices.shape) > 1:
            close_prices = close_prices.iloc[:, 0]

        bench_1w, bench_1m, bench_3m = calculate_returns(close_prices)
        logger.info(
            f"Benchmark returns: 1W={bench_1w:.2f}%, 1M={bench_1m:.2f}%, 3M={bench_3m:.2f}%"
            if bench_1w and bench_1m and bench_3m
            else "Benchmark returns: Some periods unavailable"
        )
        return bench_1w, bench_1m, bench_3m

    except Exception as e:
        logger.error(f"Error fetching benchmark data: {e}")
        return None, None, None


def process_stock(args: Tuple) -> Optional[StockReturn]:
    """
    Process a single stock: download data and calculate returns.

    Args:
        args: Tuple of (symbol, industry, start_date_str, end_date_str, bench_1w, bench_1m, bench_3m).

    Returns:
        StockReturn object or None on failure.
    """
    symbol, industry, start_str, end_str, bench_1w, bench_1m, bench_3m = args

    try:
        ticker_symbol = symbol if symbol.endswith(".NS") else f"{symbol}.NS"

        data = yf.download(
            ticker_symbol, start=start_str, end=end_str, progress=False, auto_adjust=True
        )

        if data.empty:
            logger.debug(f"No data for {symbol}")
            return None

        # Handle multi-column case
        close_prices = data["Close"]
        if hasattr(close_prices, "iloc") and len(close_prices.shape) > 1:
            close_prices = close_prices.iloc[:, 0]

        return_1w, return_1m, return_3m = calculate_returns(close_prices)
        weighted_return = calculate_weighted_return(
            return_1w, return_1m, return_3m)

        # Calculate relative strength vs benchmark
        rs_1w = (
            return_1w - bench_1w if return_1w is not None and bench_1w is not None else None
        )
        rs_1m = (
            return_1m - bench_1m if return_1m is not None and bench_1m is not None else None
        )
        rs_3m = (
            return_3m - bench_3m if return_3m is not None and bench_3m is not None else None
        )
        weighted_rs = calculate_weighted_return(rs_1w, rs_1m, rs_3m)

        return StockReturn(
            symbol=symbol,
            industry=industry,
            return_1w=return_1w,
            return_1m=return_1m,
            return_3m=return_3m,
            weighted_return=weighted_return,
            rs_1w=rs_1w,
            rs_1m=rs_1m,
            rs_3m=rs_3m,
            weighted_rs=weighted_rs,
        )

    except Exception as e:
        logger.debug(f"Error processing {symbol}: {e}")
        return None


def aggregate_sector_returns(stock_returns: List[StockReturn]) -> pd.DataFrame:
    """
    Aggregate stock returns by sector (industry).

    Calculates average returns for each sector and ranks them.

    Args:
        stock_returns: List of StockReturn objects.

    Returns:
        DataFrame with sector-level aggregated returns.
    """
    records = []
    for sr in stock_returns:
        if sr.weighted_return is not None:
            records.append(
                {
                    "symbol": sr.symbol,
                    "industry": sr.industry,
                    "return_1w": sr.return_1w,
                    "return_1m": sr.return_1m,
                    "return_3m": sr.return_3m,
                    "weighted_return": sr.weighted_return,
                    "rs_1w": sr.rs_1w,
                    "rs_1m": sr.rs_1m,
                    "rs_3m": sr.rs_3m,
                    "weighted_rs": sr.weighted_rs,
                }
            )

    if not records:
        return pd.DataFrame()

    df = pd.DataFrame(records)

    # Aggregate by industry
    sector_stats = df.groupby("industry").agg(
        {
            "weighted_return": ["mean", "median", "std", "count"],
            "return_1w": "mean",
            "return_1m": "mean",
            "return_3m": "mean",
            "rs_1w": "mean",
            "rs_1m": "mean",
            "rs_3m": "mean",
            "weighted_rs": "mean",
        }
    ).round(2)

    # Flatten column names
    sector_stats.columns = [
        "avg_weighted_return",
        "median_weighted_return",
        "std_weighted_return",
        "stock_count",
        "avg_return_1w",
        "avg_return_1m",
        "avg_return_3m",
        "avg_rs_1w",
        "avg_rs_1m",
        "avg_rs_3m",
        "avg_weighted_rs",
    ]

    sector_stats = sector_stats.reset_index()
    # Sort by weighted RS (relative strength) instead of absolute return
    sector_stats = sector_stats.sort_values("avg_weighted_rs", ascending=False)

    return sector_stats


def generate_signal(
    rs_1w: Optional[float], rs_1m: Optional[float], rs_3m: Optional[float], weighted_rs: Optional[float]
) -> Tuple[str, str]:
    """
    Generate buy/sell signal based on relative strength patterns.

    Args:
        rs_1w: 1-week relative strength.
        rs_1m: 1-month relative strength.
        rs_3m: 3-month relative strength.
        weighted_rs: Weighted relative strength.

    Returns:
        Tuple of (signal_emoji, signal_description).
    """
    # Handle missing data
    if weighted_rs is None:
        return "⚪", "NEUTRAL (Insufficient data)"

    # Check if momentum is improving (short-term > mid-term)
    is_improving = rs_1w is not None and rs_1m is not None and rs_1w > rs_1m

    # STRONG BUY: All RS positive AND improving momentum
    if all(
        rs is not None and rs > 0 for rs in [rs_1w, rs_1m, rs_3m]
    ) and is_improving:
        return "🟢", "STRONG BUY (Outperforming with accelerating momentum)"

    # BUY: Weighted RS positive OR strong recent momentum
    if weighted_rs > 0 or (rs_1w is not None and rs_1w > 0):
        return "🟡", "BUY (Outperforming the benchmark)"

    # NEUTRAL: Weighted RS near zero
    if -1.0 <= weighted_rs <= 1.0:
        return "⚪", "NEUTRAL (Moving in-line with benchmark)"

    # SELL: Negative RS AND deteriorating momentum
    if weighted_rs < 0 and not is_improving:
        return "🟠", "SELL (Underperforming)"

    # STRONG SELL: All RS negative AND worsening
    if all(
        rs is not None and rs < 0 for rs in [rs_1w, rs_1m, rs_3m]
    ) and not is_improving:
        return "🔴", "STRONG SELL (Underperforming with weakening momentum)"

    # Default to SELL if weighted RS is negative
    return "🟠", "SELL (Underperforming)"


def main():
    """Main entry point for the script."""
    parser = argparse.ArgumentParser(
        description="Find top trending sectors from Nifty 500 stocks"
    )
    parser.add_argument(
        "--symbols",
        type=str,
        default="ind_nifty500list.csv",
        help="Path to CSV file with symbols (columns: 'Symbol', 'Industry')"
    )
    parser.add_argument(
        "--output",
        type=str,
        default="trending_sectors.csv",
        help="Output CSV file path"
    )
    parser.add_argument(
        "--top",
        type=int,
        default=5,
        help="Number of top sectors to display (default: 5)"
    )
    parser.add_argument(
        "--workers",
        type=int,
        default=os.cpu_count(),
        help="Number of parallel workers for downloading data",
    )
    parser.add_argument(
        "--benchmark",
        type=str,
        default="^NSEI",
        help="Benchmark symbol for relative strength calculation (default: ^NSEI - Nifty 50)",
    )
    parser.add_argument("-v", "--verbose", action="store_true",
                        help="Enable verbose logging")
    args = parser.parse_args()

    if args.verbose:
        logger.setLevel(logging.DEBUG)

    # Date range: fetch ~110 days to ensure we have at least 66 trading days for 3-month calculation
    # (accounting for weekends, holidays, and market closures)
    end_date = datetime.now()
    start_date = end_date - timedelta(days=110)

    logger.info(
        f"Analyzing sectors using data from {start_date.strftime('%Y-%m-%d')} "
        f"to {end_date.strftime('%Y-%m-%d')}"
    )
    logger.info(
        f"Weights: 1W={WEIGHT_1W*100:.0f}%, 1M={WEIGHT_1M*100:.0f}%, 3M={WEIGHT_3M*100:.0f}%"
    )

    # Load symbols from CSV
    if not os.path.exists(args.symbols):
        logger.error(f"Symbols file not found: {args.symbols}")
        sys.exit(1)

    try:
        df_symbols = pd.read_csv(args.symbols)
        required_cols = {'Symbol', 'Industry'}
        if not required_cols.issubset(df_symbols.columns):
            logger.error(f"CSV must contain columns: {required_cols}")
            sys.exit(1)

        symbols_with_industry = list(
            zip(df_symbols['Symbol'].tolist(), df_symbols['Industry'].tolist())
        )
    except Exception as e:
        logger.error(f"Error reading symbols file: {e}")
        sys.exit(1)

    logger.info(f"Processing {len(symbols_with_industry)} stocks...")
    logger.info(f"Benchmark: {args.benchmark}")

    start_str = start_date.strftime("%Y-%m-%d")
    end_str = (end_date + timedelta(days=1)).strftime("%Y-%m-%d")

    # Fetch benchmark returns first
    bench_1w, bench_1m, bench_3m = fetch_benchmark_returns(
        args.benchmark, start_str, end_str
    )

    if bench_1w is None and bench_1m is None and bench_3m is None:
        logger.warning(
            "Failed to fetch benchmark data. Relative strength calculations will be unavailable."
        )

    # Prepare tasks for parallel processing (include benchmark returns)
    tasks = [
        (symbol, industry, start_str, end_str, bench_1w, bench_1m, bench_3m)
        for symbol, industry in symbols_with_industry
    ]

    stock_returns = []
    with concurrent.futures.ProcessPoolExecutor(max_workers=args.workers) as executor:
        for result in executor.map(process_stock, tasks):
            if result:
                stock_returns.append(result)

    logger.info(f"Successfully processed {len(stock_returns)} stocks")

    # Aggregate by sector
    sector_stats = aggregate_sector_returns(stock_returns)

    if sector_stats.empty:
        logger.error(
            "No sector data computed. Check if data was downloaded correctly.")
        sys.exit(1)

    # Save full results
    sector_stats.to_csv(args.output, index=False)
    logger.info(f"Full sector analysis saved to {args.output}")

    # Display top sectors
    print(f"\n{'='*80}")
    print(f"TOP {args.top} TRENDING SECTORS (vs {args.benchmark})")
    print(f"{'='*80}")
    print(
        f"Ranked by Relative Strength "
        f"(Weights: 1W={WEIGHT_1W*100:.0f}%, 1M={WEIGHT_1M*100:.0f}%, 3M={WEIGHT_3M*100:.0f}%)"
    )
    print(f"{'='*80}\n")

    def format_return(value):
        """Format return percentage, handling NaN values."""
        if pd.isna(value):
            return "N/A"
        return f"{value:+.2f}%"

    def format_rs(value):
        """Format relative strength with direction indicator."""
        if pd.isna(value):
            return "N/A"
        indicator = "▲" if value > 0 else "▼" if value < 0 else "―"
        return f"{value:+.2f}% {indicator}"

    top_sectors = sector_stats.head(args.top)
    for i, (_, row) in enumerate(top_sectors.iterrows(), 1):
        # Generate signal
        signal_emoji, signal_desc = generate_signal(
            row["avg_rs_1w"], row["avg_rs_1m"], row["avg_rs_3m"], row["avg_weighted_rs"]
        )

        print(f"  {i}. {row['industry']}")
        print(f"     Absolute Returns → 1W: {format_return(row['avg_return_1w'])}  |  "
              f"1M: {format_return(row['avg_return_1m'])}  |  "
              f"3M: {format_return(row['avg_return_3m'])}")
        print(f"     Relative Strength → 1W: {format_rs(row['avg_rs_1w'])} | "
              f"1M: {format_rs(row['avg_rs_1m'])} | "
              f"3M: {format_rs(row['avg_rs_3m'])}")
        print(f"     Weighted Absolute: {format_return(row['avg_weighted_return'])}  |  "
              f"Weighted RS: {format_rs(row['avg_weighted_rs'])}")
        print(f"     Signal: {signal_emoji} {signal_desc}")
        print(f"     Stocks in sector: {int(row['stock_count'])}")
        print()

    print(f"{'='*80}\n")

    # Show worst performing for reference
    print(f"BOTTOM {args.top} SECTORS (for reference)")
    print(f"{'-'*80}")
    bottom_sectors = sector_stats.tail(args.top).iloc[::-1]
    for idx, row in bottom_sectors.iterrows():
        signal_emoji, _ = generate_signal(
            row["avg_rs_1w"], row["avg_rs_1m"], row["avg_rs_3m"], row["avg_weighted_rs"]
        )
        print(
            f"  {signal_emoji} {row['industry']}: "
            f"Weighted RS = {format_rs(row['avg_weighted_rs'])}"
        )
    print()


if __name__ == "__main__":
    main()
