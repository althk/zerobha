# Zerobha Algorithmic Trading Bot

Zerobha is a high-performance algorithmic trading bot designed for Indian Equity markets (NSE). It emphasizes emotional detachment from trading decisions.

It features a modular architecture, robust risk management, and a real-time web dashboard.

## ✨ Features

- **ORB (Opening Range Breakout)**: Capitalizes on early morning volatility with configurable RSI/ADX thresholds, ATR-based range validation, and advanced filters: candle body strength, recent volume surges, gap analysis, and VWAP distance limits. Includes structural SL logic for optimized risk management.
- **Stock Selection Pipeline**: Automated sector-to-stock pipeline that identifies top-momentum sectors (RRG-style analysis), filters stocks by industry, and ranks by beta + relative strength.
- **Pre-market Filter**: Screens stocks by price range, liquidity (ADTV), volatility (ATR%), and beta before market open.
- **Real-time Web Dashboard**: Monitor funds, positions, and orders live at `http://localhost:8080`.
- **Risk Management**:
  - Daily Max Loss protection.
  - Max trades per day limit.
  - Auto-square off at 3:13 PM IST.
  - No new trades after 3:05 PM IST.
- **Data Pipeline**:
  - Real-time tick processing via Zerodha Kite Ticker.
  - Custom candle aggregation engine.
- **Backtesting**: Robust backtesting engine to validate strategies on historical data.

## 🚀 Getting Started

### Prerequisites

- [Go 1.24+](https://go.dev/dl/)
- Zerodha Kite Connect Account (API Key & Secret)
- Git

### Installation

1. Clone the repository:

    ```bash
    git clone https://github.com/althk/zerobha.git
    cd zerobha
    ```

2. Install dependencies:

    ```bash
    go mod tidy
    ```

### Configuration

The bot uses `config.toml` for configuration. Copy the example config or create a new one:

```toml
# config.toml

# Strategy (ORB is the only strategy)
strategy = "orb"

# Symbol list (CSV format)
csv_file = "high_beta_stocks.csv"

# Trade limit per day
limit = 50

# Candle timeframe
timeframe = "5m"

# Credentials
api_key = "your_zerodha_api_key"
api_secret = "your_zerodha_api_secret"
```

## 🖥️ Usage

### Live Trading

To start the trading bot in live mode:

```bash
go run cmd/trader/main.go -config my_config.toml
```

Once running, access the dashboard at **[http://localhost:8080](http://localhost:8080)**.

### Backtesting

```bash
go run ./cmd/backtest -csv high_beta_stocks.csv -timeframe 5m -cost-bps 3
```

Yahoo (`scripts/data_downloader.py`) only serves 60 days of intraday history. For a
sample large enough to be significant, pull candles from Kite instead — it needs an
interactive login and the historical-data add-on:

```bash
go run ./cmd/histdl -csv high_beta_stocks.csv -interval 5minute -days 400
go run ./cmd/backtest -csv high_beta_stocks.csv -timeframe 5minute -cost-bps 3
```

### Deployment

`./zerobha.sh` builds the Docker image and drives a remote Debian VM over SSH
from your own machine — no separate `ssh` session or manual `scp` of the
script needed. See [`DEPLOYMENT_GUIDE.md`](DEPLOYMENT_GUIDE.md) for the full
walkthrough (VM setup, Google Drive backups, Kite login via an SSH
`LocalForward`); the short version:

```bash
./zerobha.sh setup user@vm    # one-time: Docker, rclone, backup cron
./zerobha.sh deploy user@vm   # build, ship and start the container
```

### Stock Selection Pipeline

Build a sector-aware watchlist before market open:

```bash
# Full pipeline: sector momentum -> filter by industry -> rank by beta + RS
python scripts/build_watchlist.py

# Customize: top 3 sectors, beta >= 1.2, max 30 stocks
python scripts/build_watchlist.py --top-sectors 3 --min-beta 1.2 --limit 30

# Pre-market filter (price, liquidity, volatility, beta)
python scripts/premarket_filter.py
python scripts/premarket_filter.py --input ind_nifty500list.csv --output filtered_watchlist.csv
```

## 🛡️ Risk Management Rules

- **Trade Cutoff**: No new trades are taken after **15:05 IST**.
- **Auto Square-off**: All MIS positions and active GTT orders are auto-squared off at **15:13 IST**.
- **Max Loss**: Configurable per-day loss limit (default ₹2000).

## 📂 Project Structure

- `cmd/trader`: Entry point for live trading.
- `cmd/backtest`: Entry point for backtesting.
- `cmd/histdl`: Downloads Kite historical candles into `test/data/` for backtesting.
- `internal/core`: Core engine, candle builder, and interfaces.
- `internal/config`: Configuration, including the `[orb]` tuning section in `config.toml`.
- `internal/web`: Web dashboard server.
- `pkg/strategy`: The ORB trading strategy.
- `pkg/indicators`: Technical indicators (RSI, ADX, ATR, SMA, EMA, VWAP).
- `pkg/broker`: Broker adapters (Zerodha, Sim).
- `scripts/`: Python utilities for stock selection and data management.
- `web/`: Frontend assets (HTML/CSS/JS).

## ⚠️ Disclaimer

This software is for educational purposes only. Algorithmic trading involves significant risk. The authors are not responsible for any financial losses incurred while using this bot. Use at your own risk.
