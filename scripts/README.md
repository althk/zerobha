# Scripts Directory

This directory contains utility scripts for market analysis and stock screening.

## Available Scripts

### 1. find_trending_sectors.py

Analyzes Nifty 500 stocks to identify top trending sectors based on **relative strength** analysis against a benchmark index (Nifty 50 by default).

#### Features

- **Relative Strength Analysis**: Compares sector performance against benchmark to identify true market leaders
- **Multi-Timeframe Analysis**: Calculates returns for 1 week, 1 month, and 3 months
- **Smart Weighting**: Weighted scoring (1W: 50%, 1M: 30%, 3M: 20%) prioritizing recent momentum
- **Buy/Sell Signals**: Automated signal generation based on relative strength momentum patterns
- **Parallel Processing**: Concurrent data downloads for fast execution
- **Configurable Benchmark**: Compare against any index (Nifty 50, Bank Nifty, etc.)

#### Usage

**Basic Usage:**
```bash
python3 scripts/find_trending_sectors.py
```

**With Custom Options:**
```bash
python3 scripts/find_trending_sectors.py \
  --symbols ind_nifty500list.csv \
  --output trending_sectors.csv \
  --top 10 \
  --benchmark ^NSEI \
  --workers 8 \
  --verbose
```

#### Command-Line Arguments

| Argument | Type | Default | Description |
|----------|------|---------|-------------|
| `--symbols` | string | `ind_nifty500list.csv` | Path to CSV file with symbols (columns: 'Symbol', 'Industry') |
| `--output` | string | `trending_sectors.csv` | Output CSV file path |
| `--top` | int | `5` | Number of top sectors to display |
| `--workers` | int | CPU count | Number of parallel workers for downloading data |
| `--benchmark` | string | `^NSEI` | Benchmark symbol for relative strength calculation |
| `-v`, `--verbose` | flag | - | Enable verbose logging |

#### Understanding the Output

The script displays comprehensive sector analysis with multiple metrics:

**Sample Output:**
```
================================================================================
TOP 5 TRENDING SECTORS (vs ^NSEI)
================================================================================
Ranked by Relative Strength (Weights: 1W=50%, 1M=30%, 3M=20%)
================================================================================

  1. Information Technology
     Absolute Returns → 1W: +5.20%  |  1M: +12.30%  |  3M: +18.50%
     Relative Strength → 1W: +2.10% ▲ | 1M: +4.80% ▲ | 3M: +6.20% ▲
     Weighted Absolute: +9.80%  |  Weighted RS: +3.50% ▲
     Signal: 🟢 STRONG BUY (Outperforming with accelerating momentum)
     Stocks in sector: 45
```

**Metrics Explained:**

- **Absolute Returns**: Raw percentage gains/losses for the sector
- **Relative Strength (RS)**: Sector return minus benchmark return
  - `▲` = Outperforming benchmark
  - `▼` = Underperforming benchmark
  - `―` = Moving in-line with benchmark
- **Weighted Metrics**: Time-weighted averages emphasizing recent performance

**Signal Classification:**

| Signal | Emoji | Criteria | Interpretation |
|--------|-------|----------|----------------|
| **STRONG BUY** | 🟢 | All RS positive + improving momentum (RS_1W > RS_1M) | Best opportunity - outperforming with acceleration |
| **BUY** | 🟡 | Weighted RS > 0 OR RS_1W > 0 | Good opportunity - outperforming benchmark |
| **NEUTRAL** | ⚪ | Weighted RS between -1% and +1% | Moving in-line with market |
| **SELL** | 🟠 | Weighted RS < 0 AND deteriorating | Underperforming - avoid |
| **STRONG SELL** | 🔴 | All RS negative + worsening | Worst case - strong underperformance |

#### How Relative Strength Works

**Formula:**
```
Relative Strength (RS) = Sector Return - Benchmark Return
```

**Examples:**
- Sector: +10%, Nifty 50: +6% → RS = **+4%** (outperforming ▲)
- Sector: +3%, Nifty 50: +6% → RS = **-3%** (underperforming ▼)

**Why It Matters:**

Relative strength reveals which sectors are truly leading the market:
- In a bull market where everything is up, RS shows which sectors are rising *faster*
- In a bear market, RS identifies sectors that are falling *slower* (defensive plays)
- Sectors with strong positive RS are momentum plays for sector rotation strategies

#### Input File Format

The script expects a CSV file with at minimum these columns:

```csv
Symbol,Industry
TCS,Information Technology
RELIANCE,Oil & Gas
HDFCBANK,Finance
INFY,Information Technology
...
```

#### Output File

The script saves a CSV file with aggregated sector statistics:

```csv
industry,avg_weighted_return,median_weighted_return,std_weighted_return,stock_count,avg_return_1w,avg_return_1m,avg_return_3m,avg_rs_1w,avg_rs_1m,avg_rs_3m,avg_weighted_rs
Information Technology,9.80,9.50,2.30,45,5.20,12.30,18.50,2.10,4.80,6.20,3.50
...
```

#### Technical Details

- **Data Source**: Yahoo Finance (yfinance library)
- **Lookback Period**: 110 calendar days (~75-77 trading days) to ensure sufficient data for 3-month calculations
- **Processing**: Parallel execution using ProcessPoolExecutor
- **Benchmark**: Downloads once at startup for efficiency
- **Error Handling**: Gracefully handles missing data and download failures

#### Dependencies

```bash
pip install pandas yfinance
```

---

### 2. find_high_beta.py

Identifies high beta stocks from a given universe, with optional sector filtering and per-stock relative strength scoring.

#### Usage

```bash
# All Nifty 500 stocks
python3 scripts/find_high_beta.py

# Filter to specific industries + minimum beta
python3 scripts/find_high_beta.py --sectors "Healthcare,Metals & Mining" --min-beta 1.2 --limit 30

# Custom date range
python3 scripts/find_high_beta.py --start-date 2025-01-01 --end-date 2025-03-01
```

#### Command-Line Arguments

| Argument | Type | Default | Description |
|----------|------|---------|-------------|
| `--symbols` | string | `ind_nifty500list.csv` | CSV with 'Symbol' column (and optional 'Industry') |
| `--output` | string | `high_beta_stocks.csv` | Output CSV path |
| `--start-date` | string | 60 days ago | Start date (YYYY-MM-DD) |
| `--end-date` | string | today | End date (YYYY-MM-DD) |
| `--workers` | int | CPU count | Parallel download workers |
| `--sectors` | string | all | Comma-separated industry filter (e.g. `"Healthcare,Realty"`) |
| `--min-beta` | float | none | Minimum beta threshold |
| `--limit` | int | none | Max stocks to output |

#### Output Columns

`symbol, industry, beta, last_price, rs_1m`

- `rs_1m`: 1-month relative strength vs Nifty 50 (%). Positive = outperforming.

---

### 3. sector_momentum_analyzer.py

RRG-style (Relative Rotation Graph) analysis of NSE sectoral indices vs Nifty 50. Classifies sectors into four quadrants and detects momentum-of-momentum.

#### Features

- **1W/1M/3M windows** with weights 30%/40%/30% — tuned for short-term momentum trading
- **RRG quadrant classification**: LEADING, WEAKENING, IMPROVING, LAGGING
- **Momentum-of-momentum (MoM)**: Detects whether the composite score itself is accelerating or decelerating by comparing current vs 1-week-ago composite
- **Sector-to-industry mapping**: Maps each NSE index to Nifty 500 industry names for downstream stock filtering

#### Usage

```bash
python3 scripts/sector_momentum_analyzer.py
python3 scripts/sector_momentum_analyzer.py --output top_sectors.csv --top 5
```

#### Command-Line Arguments

| Argument | Type | Default | Description |
|----------|------|---------|-------------|
| `--output` | string | `top_sectors.csv` | Output CSV path |
| `--benchmark` | string | `^NSEI` | Benchmark ticker |
| `--top` | int | `4` | Number of top sectors to highlight |

#### Output Columns

`Sector, Ticker, 1W_RS, 1M_RS, 3M_RS, Composite, Mom_of_Mom, Quadrant`

#### Quadrant Interpretation

| Quadrant | RS Level | Momentum | Action |
|----------|----------|----------|--------|
| **LEADING** | Positive | Rising | Trade — strongest sector |
| **WEAKENING** | Positive | Falling | Trim — losing steam |
| **IMPROVING** | Negative | Rising | Watch — potential turnaround |
| **LAGGING** | Negative | Falling | Avoid |

---

### 4. build_watchlist.py

End-to-end pipeline that chains sector momentum analysis with stock selection. This is the recommended way to generate the trading watchlist.

#### Pipeline Flow

```
Sector Momentum Analyzer
    → Top LEADING/IMPROVING sectors
        → Map to Nifty 500 industry names
            → find_high_beta.py (filtered to those industries)
                → high_beta_stocks.csv (ranked watchlist)
```

#### Usage

```bash
# Default: top 4 sectors, beta >= 1.0, max 50 stocks
python3 scripts/build_watchlist.py

# Customize
python3 scripts/build_watchlist.py --top-sectors 3 --min-beta 1.2 --limit 30

# Use all top sectors even if none are LEADING/IMPROVING
python3 scripts/build_watchlist.py --include-all-if-no-leaders
```

#### Command-Line Arguments

| Argument | Type | Default | Description |
|----------|------|---------|-------------|
| `--symbols` | string | `ind_nifty500list.csv` | Nifty 500 CSV |
| `--output` | string | `high_beta_stocks.csv` | Final watchlist CSV |
| `--top-sectors` | int | `4` | Number of top sectors |
| `--min-beta` | float | `1.0` | Minimum beta |
| `--limit` | int | `50` | Max stocks in watchlist |
| `--workers` | int | CPU count | Parallel workers |
| `--sector-output` | string | `top_sectors.csv` | Sector report CSV |
| `--include-all-if-no-leaders` | flag | - | Fall back to top sectors by score if no LEADING/IMPROVING |

---

### 5. premarket_filter.py

Pre-market stock screener that filters the input universe down to stocks suitable for intraday ORB trading. Run before market open (~8:30 AM IST).

#### Filters Applied

1. Price between ₹100 and ₹5,000
2. ADTV (avg daily traded value) > ₹50 Cr over last 20 days
3. ATR(14) as % of closing price > 1.5%
4. Beta > 1.2

#### Usage

```bash
python3 scripts/premarket_filter.py
python3 scripts/premarket_filter.py --input ind_nifty500list.csv --output filtered_watchlist.csv
```

#### Output Columns

`symbol, beta, last_price, atr_pct, adtv_cr` — sorted by `atr_pct` descending.

---

### 6. data_downloader.py

Downloads historical OHLCV data from Yahoo Finance into `test/data/` for backtesting.

```bash
python3 scripts/data_downloader.py
```

---

## Notes

- All scripts use logging with file:line number format for easy debugging
- Enable verbose mode (`-v`) for detailed execution logs
- Scripts are designed to be run from the project root directory

## Contributing

When adding new scripts to this directory:
1. Include comprehensive docstrings
2. Add command-line argument parsing with `--help` support
3. Use logging instead of print statements
4. Update this README with script documentation
5. Follow the project's code style (run `ruff format` before committing)
