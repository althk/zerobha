# Zerobha VM Deployment & Operations Guide

A step-by-step guide to building the Docker container on your local machine, deploying it to a Debian VM, completing Kite authentication, and syncing data/logs to Google Drive.

---

## Architecture Summary

- **Local Machine (Windows)**: Builds the self-contained Docker image (`zerobha:latest`).
- **VM (Debian)**: Runs the container in the background, maps ports `9880` (Kite Auth) and `9080` (Dashboard), and persists `zerobha.db` + `logs/`.
- **Cloud Backup (Google Drive)**: `rclone` creates a safe SQLite database snapshot and compresses logs daily at 15:45 IST.

---

## Step 1: Build & Export Container (Local Windows PC)

Open PowerShell in the project directory:

```powershell
# 1. Build the Docker image (bakes in binary, timezone, and configuration)
docker build -t zerobha:latest .

# 2. Export and compress the image
docker save zerobha:latest | gzip > zerobha.tar.gz

# 3. Copy the image archive and the operations script to your VM
scp zerobha.tar.gz zerobha.sh user@YOUR_VM_IP:~/
```

---

## Step 2: One-Time VM Setup (Debian)

SSH into your VM:

```bash
ssh user@YOUR_VM_IP
```

Make `zerobha.sh` executable and run initial system setup:

```bash
chmod +x zerobha.sh

# Installs Docker, rclone, sqlite3, sets timezone to Asia/Kolkata (IST),
# creates /opt/zerobha directories, and configures the 15:45 IST daily backup cron.
./zerobha.sh setup-prereqs
```

---

## Step 3: Link Google Drive for Backups (One-Time)

Run the interactive rclone setup wizard:

```bash
./zerobha.sh rclone-setup
```

1. Type `n` (New remote).
2. Name: `gdrive`
3. Storage type: `drive` (Google Drive)
4. Follow the on-screen link to authorize your Google Account.

---

## Step 4: Load and Start the Container

```bash
# 1. Load the Docker image
./zerobha.sh docker-load ~/zerobha.tar.gz

# 2. Start the trading daemon in the background
./zerobha.sh docker-run
```

---

## Step 5: Morning Kite Login (from your Local PC)

Because cloud VMs are headless (no web browser), use **SSH Port Forwarding** to log in through your local Windows browser.

### 1. Open an SSH Tunnel from Windows

In PowerShell on your Windows PC:

```powershell
ssh -L 9880:localhost:9880 -L 9080:localhost:9080 user@YOUR_VM_IP
```

### 2. View Login URL on the VM

```bash
./zerobha.sh logs
```

You will see output like:

```text
Open the following url in your browser:
https://kite.zerodha.com/connect/login?api_key=...
```

### 3. Authenticate

1. Copy the login URL and open it in Chrome/Edge on your Windows PC.
2. Log in with your Zerodha credentials and 2FA.
3. Zerodha will redirect to `http://localhost:9880/auth/kite/callback`.
4. The SSH tunnel routes this callback directly into the VM container, and the terminal will log:

   ```text
   request token: xxxxxxxxxxxxxxxx
   login successful!
   Connected to Zerodha WebSocket. Subscribing...
   ```

5. You can now open `http://localhost:9080` in your Windows browser to view the live dashboard.

---

## Step 6: Operations & Backup Management

All common operations are handled by `./zerobha.sh`:

| Task | Command |
| --- | --- |
| **View Live Trading Logs** | `./zerobha.sh logs` |
| **Check Container & Backup Status** | `./zerobha.sh status` |
| **Restart Strategy** | `./zerobha.sh restart` |
| **Stop Strategy** | `./zerobha.sh stop` |
| **Trigger Immediate Backup** | `./zerobha.sh backup` |

### Automatic Backup Schedule

The setup script automatically adds a cron job to snapshot the database and upload logs:

- **Frequency**: Every Monday through Friday at **15:45 IST** (after 15:30 market squareoff).
- **Destination**: `Google Drive: /zerobha_backups/YYYY-MM-DD/`
- **Integrity**: Uses SQLite's online `.backup` API to prevent database corruption during writes.

---

## Paper Trading

Run the whole stack against real market data with simulated fills before
committing real capital. Quotes, candles and the tick feed are live; orders never
reach the exchange.

Enable it either way:

```toml
# config.local.toml — above the first [section] header
paper_trading = true
paper_capital = 1000000.0   # virtual capital; 0 means "absent" and takes the default
```

```bash
# or per-run, without touching the config
./trader -config config.local.toml -paper
```

For the container, add the flag to the compose/`docker run` command, or set
`paper_trading = true` in the config that gets baked into the image. Note the
Dockerfile's `CMD` passes only `-config`: the strategy is selected by the
config's `strategy` key, and an undefined flag makes the trader exit on start-up.

**What is simulated and what is not.** Positions get the same protective stop and
target they would live — the paper broker holds them itself and fills them from
the price feed, so exits fire on the strategy's real rules rather than running to
the 15:13 square-off. Margin is blocked using the same leverage map the sizer
uses. What paper cannot show you is slippage, queue position, partial fills, or
whether the order would have been accepted at all: fills happen at the observed
price.

**The dashboard shows an amber `PAPER TRADING` badge** at `:9080`, and all
performance figures are scoped to the mode you are running — paper and live
trades share `zerobha.db` but are never blended into the same win rate, profit
factor or equity curve.

**State survives a restart.** The simulated book is persisted per trading date,
so restarting the container mid-session resumes with the day's positions and
balance intact rather than coming back flat with full virtual capital.
