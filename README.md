# Zerobha Algorithmic Trading Bot

Zerobha is a high-performance algorithmic trading bot designed for Indian Equity markets (NSE). It emphasizes emotional detachment from trading decisions.

It features a modular architecture with pluggable strategies, robust risk management, and a real-time web dashboard.

## ✨ Features

-   **Multi-Strategy Support**:
    -   **ORB (Opening Range Breakout)**: Capitalizes on early morning volatility with configurable RSI/ADX thresholds, ATR-based range validation, volume filters, and concurrent position limits.
    -   **CPR + VWAP**: Intraday mean-reversion strategy using Central Pivot Range levels, VWAP confluence, EMA9 proximity, and ADX trend filters.
    -   **Donchian Breakout**: Trend following system based on price channels.
    -   **EMA Pullback**: Trend following strategy on pullbacks.
    -   **EMA Trend + RSI**: Combined momentum and trend strategy.
    -   **ADX + RSI + VWAP**: Complex multi-indicator strategy.
-   **Stock Selection Pipeline**: Automated sector-to-stock pipeline that identifies top-momentum sectors (RRG-style analysis), filters stocks by industry, and ranks by beta + relative strength.
-   **Pre-market Filter**: Screens stocks by price range, liquidity (ADTV), volatility (ATR%), and beta before market open.
-   **Real-time Web Dashboard**: Monitor funds, positions, and orders live at `http://localhost:8080`.
-   **Risk Management**:
    -   Daily Max Loss protection.
    -   Max trades per day limit.
    -   Auto-square off at 3:13 PM IST.
    -   No new trades after 3:05 PM IST.
-   **Data Pipeline**:
    -   Real-time tick processing via Zerodha Kite Ticker.
    -   Custom candle aggregation engine.
-   **Backtesting**: Robust backtesting engine to validate strategies on historical data.

## 🚀 Getting Started

### Prerequisites

-   [Go 1.24+](https://go.dev/dl/)
-   Zerodha Kite Connect Account (API Key & Secret)
-   Git

### Installation

1.  Clone the repository:
    ```bash
    git clone https://github.com/althk/zerobha.git
    cd zerobha
    ```

2.  Install dependencies:
    ```bash
    go mod tidy
    ```

### Configuration

The bot uses `config.toml` for configuration. Copy the example config or create a new one:

```toml
# config.toml

# Strategy: orb, cpr_vwap, donchian, ema_pullback, adx_rsi_vwap
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
go run cmd/backtest/main.go -strategy orb -csv high_beta_stocks.csv -start 2024-01-01 -end 2024-12-31
go run cmd/backtest/main.go -strategy cpr_vwap -csv cpr_vwap_stocks.csv -start 2024-01-01 -end 2024-12-31
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

-   **Trade Cutoff**: No new trades are taken after **15:05 IST**.
-   **Auto Square-off**: All MIS positions and active GTT orders are auto-squared off at **15:13 IST**.
-   **Max Loss**: Configurable per-day loss limit (default ₹2000).

## 📂 Project Structure

-   `cmd/trader`: Entry point for live trading.
-   `cmd/backtest`: Entry point for backtesting.
-   `internal/core`: Core engine, candle builder, and interfaces.
-   `internal/config`: Configuration with per-strategy tuning (`[orb]`, `[cprvwap]` sections in `config.toml`).
-   `internal/web`: Web dashboard server.
-   `pkg/strategy`: Trading strategies (ORB, CPR+VWAP, Donchian, etc.).
-   `pkg/indicators`: Technical indicators (RSI, ADX, ATR, SMA, VWAP, Donchian, CPR).
-   `pkg/broker`: Broker adapters (Zerodha, Sim).
-   `scripts/`: Python utilities for stock selection and data management.
-   `web/`: Frontend assets (HTML/CSS/JS).

## ⚠️ Disclaimer

This software is for educational purposes only. Algorithmic trading involves significant risk. The authors are not responsible for any financial losses incurred while using this bot. Use at your own risk.
