# Zerobha VM Deployment & Operations Guide

The trader runs **natively** on a Debian 12 VM under systemd — no Docker.
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

The service's working directory is `/opt/zerobha`, so the relative `[paths]`
and CSV names resolve there. This is the same `data/` + `logs/` layout the old
Docker volumes used, so an existing database carries straight over.

**Lifecycle.** `zerobha.timer` starts `zerobha.service` at 07:00 IST Mon–Fri
(and at boot if the VM was down then). The trader exits on its own on holidays,
outside 07:00–15:05, and at 15:30 after square-off; a clean exit stays down
until the next timer, a crash is restarted after 30 s.

**Ports.** The trader binds 9880 (Kite callback) and 9080 (dashboard) to
`127.0.0.1` only. The dashboard has no auth, so reach both through an SSH
`LocalForward`, never by opening them.

## One-time setup

1. Create `config.local.toml` from `config.toml` (credentials above the first
   `[section]` header; `db_path = "data/zerobha.db"`, `log_dir = "logs"`).
2. Register `http://localhost:9880/auth/kite/callback` as the redirect URL in
   the Kite developer console.
3. Prepare the VM — timezone, packages, dirs, backup cron, and removal of the
   old Docker container if one exists:

   ```bash
   ./zerobha.sh setup user@vm
   ./zerobha.sh rclone-setup user@vm   # new remote "gdrive", type "drive"
   ```

## Deploy

```bash
./zerobha.sh deploy user@vm
```

Builds `bin/trader-linux`, copies it with the config and CSVs, (re)installs the
systemd units and restarts the service. Config changes are shipped the same
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
./zerobha.sh logs myvm
```

Open the printed `https://kite.zerodha.com/connect/login?...` URL, log in, and
the callback reaches the VM through the forward. The dashboard is then at
`http://localhost:9080`.

## Operations

| Task | Command |
| --- | --- |
| Follow logs | `./zerobha.sh logs user@vm` |
| Service, timer, data and cron status | `./zerobha.sh status user@vm` |
| Start now / restart / stop | `./zerobha.sh start\|restart\|stop user@vm` |
| Backup now | `./zerobha.sh backup user@vm` |
| Rebuild & redeploy | `./zerobha.sh deploy user@vm` |

On the VM itself: `journalctl -u zerobha -f`, `systemctl status zerobha`,
`systemctl list-timers zerobha.timer`.

Backups run at 15:45 IST Mon–Fri: an online SQLite snapshot, the day's
journal and `logs/`, uploaded with rclone to `gdrive:zerobha_backups/<date>`.
