# Zerobha VM Deployment & Operations Guide

A step-by-step guide to building the Docker container and deploying it to a
Debian VM, completing Kite authentication, and syncing data/logs to Google
Drive — all driven from `./zerobha.sh` on your own machine over SSH. Nothing
gets scp'd to the VM except the image itself.

---

## Architecture Summary

- **Local machine**: builds the self-contained Docker image (`zerobha:latest`)
  and runs every `./zerobha.sh` command shown below. The script drives the VM
  over SSH; you never open a separate `ssh` session or scp anything by hand.
  It is a bash script, so on Windows run it from **Git Bash or WSL**, not from
  PowerShell — it needs `ssh`, `scp` and `docker` on `PATH` there.
- **VM (Debian)**: runs the container in the background, maps ports `9880`
  (Kite Auth) and `9080` (Dashboard), and persists two volumes —
  `/opt/zerobha/data` (holding `zerobha.db`) and `/opt/zerobha/logs` (the daily
  log and the order journal CSV), bind-mounted at `/app/data` and `/app/logs`.
  Both are plain relative paths under the image's `/app` working directory, set
  by `[paths]` in the baked config: `db_path = "data/zerobha.db"` and
  `log_dir = "logs"`. Keep them on separate volumes — the backup reads the logs
  volume, so a `log_dir` that resolves inside the data volume silently uploads
  nothing.
- **Cloud Backup (Google Drive)**: a small `backup.sh` installed on the VM by
  `./zerobha.sh setup` creates a safe SQLite database snapshot and uploads
  compressed logs daily at 15:45 IST via `rclone`.

Every command takes the target host as its second argument, e.g.
`./zerobha.sh deploy user@vm`. Export `REMOTE_HOST=user@vm` once per shell if
you'd rather not repeat it.

---

## Step 1: Prerequisite — `config.local.toml`

The Dockerfile does `COPY config.local.toml`, and that file is gitignored — a
fresh clone does not have it and the build will fail. Create it from
`config.toml` first and fill in `api_key` / `api_secret` (both must stay above
the first `[section]` header, or TOML silently assigns them to the wrong
table). It is baked into the image, so it also decides the strategy, the paper
mode flag, and the `[paths]` above. **Config is not shipped separately** —
unlike some other bots here, there is no bind-mounted config file on the VM;
changing it means editing `config.local.toml` locally and running
`./zerobha.sh deploy` again.

---

## Step 2: One-Time VM Setup

From your project directory:

```bash
chmod +x zerobha.sh

# Installs Docker, rclone, sqlite3, sets timezone to Asia/Kolkata (IST),
# creates /opt/zerobha/{data,logs}, and installs the backup script plus its
# 15:45 IST daily cron job — all on the VM, driven from here.
./zerobha.sh setup user@YOUR_VM_IP
```

(`setup-prereqs` also works, as an alias.)

---

## Step 3: Link Google Drive for Backups (One-Time)

```bash
./zerobha.sh rclone-setup user@YOUR_VM_IP
```

This opens an interactive `rclone config` session on the VM over SSH:

1. Type `n` (New remote).
2. Name: `gdrive`
3. Storage type: `drive` (Google Drive)
4. Follow the on-screen link to authorize your Google Account.

---

## Step 4: Build, Ship and Start the Container

```bash
./zerobha.sh deploy user@YOUR_VM_IP
```

This runs `save` (build the image, then `docker save | gzip` it locally),
`copy` (scp the tarball to `/opt/zerobha` on the VM) and `run` (load the image
and `docker run` the container) in one shot. Run them individually if you want
to inspect a step — `./zerobha.sh save`, then `./zerobha.sh copy user@vm`, then
`./zerobha.sh run user@vm`.

**The container only stays up during market hours.** The trader exits
immediately if today is a holiday or the time is outside 08:55–15:30 IST, and
the `unless-stopped` restart policy restarts it on a clean exit — so outside the
window `./zerobha.sh status user@vm` shows it restarting and
`./zerobha.sh logs user@vm` repeats:

```text
Outside trading hours (08:55 - 15:30 IST), not starting trader
```

That is expected, not a failed deployment. It settles once the window opens.

---

## Step 5: Morning Kite Login

Because cloud VMs are headless (no web browser), the forwards for the Kite
callback (9880) and the dashboard (9080) come from your own `~/.ssh/config`,
not from `zerobha.sh` — add a `LocalForward` entry for the host once and every
ordinary `ssh` session to it carries the forwards automatically.

```text
Host myvm
  HostName YOUR_VM_IP
  User youruser
  LocalForward 9880 localhost:9880
  LocalForward 9080 localhost:9080
```

With that in place, `ssh myvm` on its own carries the forwards. `zerobha.sh`
does not: it passes `-o ClearAllForwardings=yes` on its own connections (see
the `SSH_OPTS` entry in the table below), specifically so that a permanent
`LocalForward` in `~/.ssh/config` does not try to rebind those local ports on
every `./zerobha.sh` command once something else already holds them. So for
this step you want **two sessions open at once**: a plain `ssh myvm` shell
that holds the forwards, and `./zerobha.sh logs myvm` in another terminal to
watch for the login URL.

**Prerequisite (one-time):** register `http://localhost:9880/auth/kite/callback`
as the redirect URL of your app in the Kite developer console. Zerodha will only
redirect there if it matches exactly.

### 1. Open a forwarding session, then view the login URL

```bash
ssh myvm          # leave this open — it is what carries the forwards
```

In a second terminal:

```bash
./zerobha.sh logs myvm
```

You will see output like:

```text
Open the following url in your browser:
https://kite.zerodha.com/connect/login?api_key=...
```

### 2. Authenticate

1. Copy the login URL and open it in Chrome/Edge on your machine.
2. Log in with your Zerodha credentials and 2FA.
3. Zerodha will redirect to `http://localhost:9880/auth/kite/callback`.
4. The `LocalForward` in your `~/.ssh/config` routes this callback directly
   into the VM container as soon as the `ssh myvm` session from Step 1 is
   open — `./zerobha.sh logs` does not carry it, since the script clears
   forwardings on its own connections — and the terminal running `logs` will
   show:

   ```text
   request token: xxxxxxxxxxxxxxxx
   login successful!
   Connected to Zerodha WebSocket. Subscribing...
   ```

5. You can now open `http://localhost:9080` in your browser to view the live
   dashboard, through the same `ssh myvm` forward.

---

## Step 6: Operations & Backup Management

All common operations are handled by `./zerobha.sh <command> user@vm` from your
local machine:

| Task | Command |
| --- | --- |
| **View Live Trading Logs** | `./zerobha.sh logs user@vm` |
| **Check Container & Backup Status** | `./zerobha.sh status user@vm` |
| **Restart Strategy** | `./zerobha.sh restart user@vm` |
| **Stop Strategy** | `./zerobha.sh stop user@vm` |
| **Trigger Immediate Backup** | `./zerobha.sh backup user@vm` |
| **Rebuild & Redeploy** | `./zerobha.sh deploy user@vm` |

`REMOTE_HOST=user@vm` can be exported once per shell to drop the second
argument from every command above.

> **Use `zerobha.sh` or `docker-compose.yml`, not both.** The repo carries a
> compose file that names the container `zerobha-trader` and bind-mounts the
> config, while `zerobha.sh run` names it `zerobha` and relies on the baked
> config. Every `zerobha.sh` command matches on `^zerobha$`, so if you start via
> compose, `logs`, `stop`, `restart` and `backup` will all silently fail to find
> it.

> **Both ports bind `0.0.0.0`.** The dashboard on `9080` has no authentication,
> so on a cloud VM it is world-readable unless you firewall it. The
> `LocalForward` entries in `~/.ssh/config` are for reaching the callback and
> dashboard from your browser, not a substitute for closing the ports —
> restrict both in your VM's firewall or security group.

### Automatic Backup Schedule

`./zerobha.sh setup` installs `backup.sh` on the VM and schedules it via cron
to snapshot the database and upload logs:

- **Frequency**: Every Monday through Friday at **15:45 IST** (after 15:30 market squareoff).
- **Destination**: `gdrive:zerobha_backups/YYYY-MM-DD` (rclone paths are remote-relative — no leading slash). Override the remote name with `GDRIVE_REMOTE=myremote ./zerobha.sh setup user@vm`.
- **Integrity**: Uses SQLite's online `.backup` API to prevent database corruption during writes.
- The cron job runs `backup.sh` directly on the VM — it does not depend on
  `zerobha.sh` still being present there. `./zerobha.sh backup user@vm` runs the
  same script on demand, from your local machine.

---

## Environment Variable Overrides

All of `zerobha.sh`'s knobs have defaults; override any of them as environment
variables ahead of the command:

| Variable | Default | Meaning |
| --- | --- | --- |
| `IMAGE_NAME` | `zerobha:latest` | Local image name/tag |
| `TAR_FILE` | `./zerobha.tar.gz` | Local path for the saved image |
| `REMOTE_DIR` | `/opt/zerobha` | Base dir on the VM (data/logs/backup script) |
| `CONTAINER_NAME` | `zerobha` | Name of the running container |
| `SSH_PORT` | `22` | SSH port |
| `SSH_OPTS` | `-o ClearAllForwardings=yes` | Extra ssh/scp options (space-separated). Clears forwardings on the script's own connections so a permanent `LocalForward` in `~/.ssh/config` doesn't try to rebind an already-held port on every command |
| `CONTAINER_TZ` | `Asia/Kolkata` | Container timezone (log timestamps) — named to avoid colliding with the standard `TZ` env var |
| `GDRIVE_REMOTE` | `gdrive` | rclone remote name for backups |

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
building — `./zerobha.sh run` builds its `docker run` invocation internally and
accepts no extra arguments, so the baked config is the only way in. (You can
also add `"-paper"` to the Dockerfile's `CMD`.) Note that `CMD` passes only
`-config`: the strategy comes from the config's `strategy` key, and an
undefined flag makes `flag.Parse` exit before the trader starts.

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
