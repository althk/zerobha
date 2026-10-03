# Zerobha VM Deployment & Operations Guide

The trader runs **natively** on a Debian 12 VM in a tmux session called
`zerobha` — no Docker, no systemd.
Everything is driven from `./zerobha.sh` on your own machine over SSH. It is a
bash script, so on Windows run it from **Git Bash or WSL**; it needs `go`,
`ssh` and `scp` on `PATH`.

## Layout on the VM

```text
/opt/zerobha/
  trader                     the Linux binary (cross-compiled locally)
  config.local.toml          the config, shipped on every deploy
  zerodha-mis-margins.csv    MIS leverage map, opened by relative path
  indices.csv                emacross/donchian watchlist
  ind_nifty500list.csv       the Upstox gate's isin_csv
  data/zerobha.db            [paths] db_path = "data/zerobha.db"
  logs/                      [paths] log_dir = "logs"
  backup.sh                  installed by setup
```

The trader runs from `/opt/zerobha`, so the relative `[paths]`
and CSV names resolve there. This is the same `data/` + `logs/` layout the old
Docker volumes used, so an existing database carries straight over.

**Lifecycle.** Nothing starts it automatically: run `./zerobha.sh start` each
trading morning. It runs in a detached tmux session `zerobha`, and exits on its
own on holidays, outside 07:00–15:05, and at 15:30 after square-off; the session
closes with it. A crash is not restarted — check `status` and `start` again.

**Ports.** The trader binds 9880 (Kite callback) and 9080 (dashboard) to
`127.0.0.1` only. The dashboard has no auth, so reach both through an SSH
`LocalForward`, never by opening them.

## One-time setup

1. Create `config.local.toml` from `config.toml` (credentials above the first
   `[section]` header; `db_path = "data/zerobha.db"`, `log_dir = "logs"`).
2. Register `http://localhost:9880/auth/kite/callback` as the redirect URL in
   the Kite developer console.
3. Prepare the VM — timezone, packages (incl. tmux), dirs, backup cron, and
   removal of the old Docker container / systemd units if they exist:

   ```bash
   ./zerobha.sh setup user@vm
   ./zerobha.sh rclone-setup user@vm   # new remote "gdrive", type "drive"
   ```

## Deploy

```bash
./zerobha.sh deploy user@vm
```

Builds `bin/trader-linux`, copies it with the config and CSVs, and restarts the
trader in its tmux session. Config changes are shipped the same
way. A restart during market hours needs a fresh Kite login.

If the Windows C: drive is full, the build fails writing Go's temp files; point
them elsewhere with `GOTMPDIR='D:\tmp\gobuild' ./zerobha.sh deploy user@vm`.

## Morning Kite login

Add the forwards to `~/.ssh/config` once:

```text
Host myvm
  HostName YOUR_VM_IP
  User youruser
  LocalForward 9880 localhost:9880
  LocalForward 9080 localhost:9080
```

`zerobha.sh` clears forwards on its own connections, so keep a plain
`ssh myvm` open to carry them, and in a second terminal:

```bash
./zerobha.sh start myvm
./zerobha.sh logs myvm
```

Open the printed `https://kite.zerodha.com/connect/login?...` URL, log in, and
the callback reaches the VM through the forward. The dashboard is then at
`http://localhost:9080`.

## Operations

| Task | Command |
| --- | --- |
| Follow today's log file | `./zerobha.sh logs user@vm` |
| Attach to the tmux session (detach: Ctrl-b d) | `./zerobha.sh attach user@vm` |
| Trader session, data and cron status | `./zerobha.sh status user@vm` |
| Start now / restart / stop | `./zerobha.sh start\|restart\|stop user@vm` |
| Backup now | `./zerobha.sh backup user@vm` |
| Rebuild & redeploy | `./zerobha.sh deploy user@vm` |

On the VM itself: `tmux attach -t zerobha` (detach with **Ctrl-b d** — Ctrl-c
stops the trader), `tail -F /opt/zerobha/logs/zerobha_$(date +%F).log`.

Backups run at 15:45 IST Mon–Fri: an online SQLite snapshot and
`logs/`, uploaded with rclone to `gdrive:zerobha_backups/<date>`.
