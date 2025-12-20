# Zerobha Algorithmic Trading Bot

Zerobha is a high-performance algorithmic trading bot designed for Indian Equity markets (NSE). It emphasizes emotional detachment from trading decisions.

It features a modular architecture with pluggable strategies, robust risk management, and a real-time web dashboard.

## ✨ Features

-   **Multi-Strategy Support**:
    -   **ORB (Opening Range Breakout)**: Capitalizes on early morning volatility.
    -   **Donchian Breakout**: Trend following system based on price channels.
    -   **EMA Pullback**: Trend following strategy on pullbacks.
    -   **EMA Trend + RSI**: Combined momentum and trend strategy.
    -   **ADX + RSI + VWAP**: Complex multi-indicator strategy.
-   **Real-time Web Dashboard**: Monitor funds, positions, and orders live at `http://localhost:8080`.
-   **Risk Management**:
    -   Daily Max Loss protection.
    -   Max trades per day limit.
    -   Auto-square off at 3:13 PM IST.
    -   No new trades after 3:05 PM IST.
-   **Data Pipeline**:
    -   Real-time tick processing via Zerodha Kite Ticker.
    -   Custom candle aggregation engine.
-   **Backtesting**: Robust backtesting engine to valid strategies on historical data.

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

# Strategy: orb, donchian, ema_pullback, adx_rsi_vwap
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

To backtest a strategy (e.g., Donchian):

```bash
go run cmd/backtest/main.go
```
*(Ensure you have historical data in the required format before backtesting)*

## 🛡️ Risk Management Rules

-   **Trade Cutoff**: No new trades are taken after **15:05 IST**.
-   **Auto Square-off**: All MIS positions and active GTT orders are auto-squared off at **15:13 IST**.
-   **Max Loss**: Configurable per-day loss limit (default ₹2000).

## 📂 Project Structure

-   `cmd/trader`: Entry point for live trading.
-   `cmd/backtest`: Entry point for backtesting.
-   `internal/core`: Core engine, candle builder, and interfaces.
-   `internal/web`: Web dashboard server.
-   `pkg/strategy`: Trading strategies implementation.
-   `pkg/broker`: Broker adapters (Zerodha, Sim).
-   `web/`: Frontend assets (HTML/CSS/JS).

## ⚠️ Disclaimer

This software is for educational purposes only. Algorithmic trading involves significant risk. The authors are not responsible for any financial losses incurred while using this bot. Use at your own risk.
