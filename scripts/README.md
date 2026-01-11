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

Identifies high beta stocks from a given universe for volatile trading opportunities.

*(Additional documentation to be added)*

---

### 3. sector_momentum_analyzer.py

Analyzes NSE sectoral indices for relative strength ranking against Nifty 50.

*(Additional documentation to be added)*

---

### 4. data_downloader.py

Utility script for downloading historical market data.

*(Additional documentation to be added)*

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
