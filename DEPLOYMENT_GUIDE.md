# Zerobha VM Deployment & Operations Guide

A step-by-step guide to building the Docker container on your local machine, deploying it to a Debian VM, completing Kite authentication, and syncing data/logs to Google Drive.

---

## Architecture Summary

- **Local Machine (Windows)**: Builds the self-contained Docker image (`zerobha:latest`).
- **VM (Debian)**: Runs the container in the background, maps ports `9880` (Kite Auth) and `9080` (Dashboard), and persists two volumes — `/app/data` (holding `zerobha.db`) and `/app/logs` (the daily log and the order journal CSV).
  Both are plain relative paths under the image's `/app` working directory, set by `[paths]` in the baked config: `db_path = "data/zerobha.db"` and `log_dir = "logs"`. Keep them on separate volumes — the backup reads the logs volume, so a `log_dir` that resolves inside the data volume silently uploads nothing.
- **Cloud Backup (Google Drive)**: `rclone` creates a safe SQLite database snapshot and compresses logs daily at 15:45 IST.

---

## Step 1: Build & Export Container (Local Windows PC)

**Prerequisite:** the Dockerfile does `COPY config.local.toml`, and that file is
gitignored — a fresh clone does not have it and the build will fail. Create it
from `config.toml` first and fill in `api_key` / `api_secret` (both must stay
above the first `[section]` header, or TOML silently assigns them to the wrong
table). It is baked into the image, so it also decides the strategy, the paper
mode flag, and the `[paths]` below.

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

**The container only stays up during market hours.** The trader exits
immediately if today is a holiday or the time is outside 08:55–15:30 IST, and
the `unless-stopped` restart policy restarts it on a clean exit — so outside the
window `status` shows it restarting and `logs` repeats:

```text
Outside trading hours (08:55 - 15:30 IST), not starting trader
```

That is expected, not a failed deployment. It settles once the window opens.

---

## Step 5: Morning Kite Login (from your Local PC)

Because cloud VMs are headless (no web browser), use **SSH Port Forwarding** to log in through your local Windows browser.

**Prerequisite (one-time):** register `http://localhost:9880/auth/kite/callback`
as the redirect URL of your app in the Kite developer console. Zerodha will only
redirect there if it matches exactly.

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

> **Use `zerobha.sh` or `docker-compose.yml`, not both.** The repo carries a
> compose file that names the container `zerobha-trader` and bind-mounts the
> config, while `zerobha.sh` names it `zerobha` and relies on the baked config.
> Every script command matches on `^zerobha$`, so if you start via compose,
> `logs`, `stop`, `restart` and `backup` will all silently fail to find it.

> **Both ports bind `0.0.0.0`.** The dashboard on `9080` has no authentication,
> so on a cloud VM it is world-readable unless you firewall it. The SSH tunnel
> in Step 5 is for reaching the callback from your browser, not a substitute for
> closing the ports — restrict both in your VM's firewall or security group.

### Automatic Backup Schedule

The setup script automatically adds a cron job to snapshot the database and upload logs:

- **Frequency**: Every Monday through Friday at **15:45 IST** (after 15:30 market squareoff).
- **Destination**: `gdrive:zerobha_backups/YYYY-MM-DD` (rclone paths are remote-relative — no leading slash). Override the remote name with `GDRIVE_REMOTE=myremote`.
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

For the container, set `paper_trading = true` in `config.local.toml` before
building — `./zerobha.sh docker-run` builds its `docker run` invocation
internally and accepts no extra arguments, so the baked config is the only way
in. (You can also add `"-paper"` to the Dockerfile's `CMD`.) Note that `CMD`
passes only `-config`: the strategy comes from the config's `strategy` key, and
an undefined flag makes `flag.Parse` exit before the trader starts.

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
