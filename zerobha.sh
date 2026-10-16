#!/usr/bin/env bash
# Build/ship/run the zerobha trader on a Debian 12 VM over SSH, from your
# machine (Git Bash is fine). The trader runs in tmux session "zerobha".
#
# Usage: ./zerobha.sh <command> [user@host]     (host falls back to $REMOTE_HOST)
#   setup         One-time VM prep: timezone, packages, dirs, backup cron
#   rclone-setup  Interactive `rclone config` on the VM (remote "gdrive", type "drive")
#   build         Cross-compile bin/trader-linux
#   copy          Copy the binary, config and CSVs to the VM
#   deploy        build + copy + restart
#   start|run     Start the trader (it exits on its own outside 07:00-15:05 IST)
#   stop|restart  Stop (Ctrl-C into tmux) / restart the trader
#   logs          Follow today's log (Kite login URL appears here)
#   attach        Attach to tmux (detach with Ctrl-b d, NOT Ctrl-c)
#   status        VM time, trader session, data dir, backup cron
#   config        Print the effective config (strategies, capital, limits);
#                 on the VM with a host, else locally from config.local.toml
#   backup        Run the backup (DB + logs to Google Drive) now
#
# On the VM, $REMOTE_DIR (relative to home, or absolute) holds trader,
# config.local.toml, the CSVs, data/ and logs/. The trader binds 9880 (Kite
# callback) and 9080 (dashboard) to 127.0.0.1; reach them through an SSH
# LocalForward.
#
# Optional env: REMOTE_DIR=opt/zerobha  SSH_PORT=22  GDRIVE_REMOTE=gdrive
#   SSH_OPTS="-o ClearAllForwardings=yes" (default: skips the forwards in
#   ~/.ssh/config, which an open session already holds)

set -euo pipefail

COMMAND="${1:-}"
REMOTE_HOST="${2:-${REMOTE_HOST:-}}"
REMOTE_DIR="${REMOTE_DIR:-opt/zerobha}"
SSH_PORT="${SSH_PORT:-22}"
GDRIVE_REMOTE="${GDRIVE_REMOTE:-gdrive}"
SESSION="zerobha"
BINARY="bin/trader-linux"
# Files the trader opens by relative path: the config, the MIS leverage map,
# the strategy watchlist and the Upstox gate's isin_csv.
SHIP_FILES=(config.local.toml zerodha-mis-margins.csv indices.csv sensex.csv ind_nifty500list.csv watchlist.csv)

SSH_OPTS_RAW="${SSH_OPTS-"-o ClearAllForwardings=yes"}"
SSH_OPTS=()
[[ -n "$SSH_OPTS_RAW" ]] && read -r -a SSH_OPTS <<< "$SSH_OPTS_RAW"

log() { echo -e "\033[0;34m[INFO]\033[0m $*"; }
ok()  { echo -e "\033[0;32m[OK]\033[0m $*"; }
die() { echo -e "\033[0;31m[ERROR]\033[0m $*" >&2; exit 1; }

need_host() { [[ -n "$REMOTE_HOST" ]] || die "no host: ./zerobha.sh $COMMAND user@host"; }
rssh()      { ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "$@"; }
rssh_tty()  { ssh -t "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "$@"; }
rcp()       { scp "${SSH_OPTS[@]}" -P "$SSH_PORT" "$@"; }

cmd_build() {
  log "Building $BINARY (linux/amd64)"
  mkdir -p bin
  # modernc.org/sqlite is pure Go, so no Linux C toolchain is needed.
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$BINARY" ./cmd/trader
}

cmd_copy() {
  need_host
  [[ -f "$BINARY" ]] || die "$BINARY not found, run './zerobha.sh build' first"
  for f in "${SHIP_FILES[@]}"; do [[ -f "$f" ]] || die "$f not found locally"; done
  rssh "mkdir -p '$REMOTE_DIR/data' '$REMOTE_DIR/logs'"
  # Upload under a temporary name: overwriting a running binary fails.
  rcp "$BINARY" "$REMOTE_HOST:$REMOTE_DIR/trader.new"
  rcp "${SHIP_FILES[@]}" "$REMOTE_HOST:$REMOTE_DIR/"
  rssh "cd '$REMOTE_DIR' && chmod +x trader.new && mv -f trader.new trader && chmod 600 config.local.toml"
}

cmd_start() {
  need_host
  rssh bash -s <<EOF
set -euo pipefail
tmux has-session -t $SESSION 2>/dev/null && { echo "already running; use restart or stop" >&2; exit 1; }
cd '$REMOTE_DIR'
tmux new-session -d -s $SESSION -c "\$PWD" "TZ=Asia/Kolkata ./trader -config config.local.toml"
EOF
  ok "Started in tmux '$SESSION' (exits on its own outside 07:00-15:05 IST)."
  log "Logs / Kite login URL: ./zerobha.sh logs $REMOTE_HOST"
}

cmd_stop() {
  need_host
  rssh bash -s <<EOF
tmux has-session -t $SESSION 2>/dev/null || { echo "not running"; exit 0; }
tmux send-keys -t $SESSION C-c
for _ in \$(seq 1 20); do tmux has-session -t $SESSION 2>/dev/null || { echo stopped; exit 0; }; sleep 1; done
echo "did not exit in 20s, killing"; tmux kill-session -t $SESSION
EOF
}

cmd_status() {
  need_host
  rssh bash -s <<EOF
timedatectl | grep -E "Local time|Time zone" || date
tmux has-session -t $SESSION 2>/dev/null && echo "trader: running" || echo "trader: NOT running"
ls -lh "$REMOTE_DIR" "$REMOTE_DIR/data" 2>/dev/null || true
crontab -l 2>/dev/null | grep backup || echo "no backup cron job"
EOF
}

# The VM's backup script: online SQLite snapshot + gzipped logs, uploaded with
# rclone. Unquoted heredoc: REMOTE_DIR and GDRIVE_REMOTE are baked in now.
backup_script() {
  cat <<BACKUP_EOF
#!/usr/bin/env bash
# Installed by 'zerobha.sh setup'; regenerate with setup, don't edit.
set -euo pipefail
cd "\$HOME" && cd "$REMOTE_DIR"   # relative to home, or absolute
day=\$(date +%F)
tmp=\$(mktemp -d)
if [ -f data/zerobha.db ]; then
  sqlite3 data/zerobha.db ".backup '\$tmp/zerobha_\$day.db'"
fi
for f in logs/*.log; do
  if [ -f "\$f" ]; then gzip -c "\$f" > "\$tmp/\$(basename "\$f").gz"; fi
done
if rclone copy "\$tmp" "$GDRIVE_REMOTE:zerobha_backups/\$day"; then
  echo "[\$(date)] backup uploaded"
else
  echo "[\$(date)] rclone upload failed" >&2
fi
rm -rf "\$tmp"
BACKUP_EOF
}

cmd_setup() {
  need_host
  rssh bash -s <<EOF
set -euo pipefail
sudo timedatectl set-timezone Asia/Kolkata
sudo apt-get update -y
sudo apt-get install -y sqlite3 rclone ca-certificates tzdata gzip tmux
mkdir -p "$REMOTE_DIR/data" "$REMOTE_DIR/logs" 2>/dev/null || { sudo mkdir -p "$REMOTE_DIR/data" "$REMOTE_DIR/logs"; sudo chown -R "\$USER": "$REMOTE_DIR"; }
EOF
  backup_script | rssh "cat > '$REMOTE_DIR/backup.sh' && chmod +x '$REMOTE_DIR/backup.sh'"
  # 15:45 IST Mon-Fri, added once. Cron starts in $HOME, as ssh does.
  rssh "crontab -l 2>/dev/null | grep -qF '$REMOTE_DIR/backup.sh' || (crontab -l 2>/dev/null; echo '45 15 * * 1-5 $REMOTE_DIR/backup.sh >> $REMOTE_DIR/logs/backup.log 2>&1') | crontab -"
  ok "Setup done. Next: ./zerobha.sh rclone-setup $REMOTE_HOST (once), then ./zerobha.sh deploy $REMOTE_HOST"
}

usage() { sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; }

case "$COMMAND" in
  build)          cmd_build ;;
  copy)           cmd_copy ;;
  deploy)         need_host; cmd_build; cmd_copy; cmd_stop; cmd_start ;;
  start|run)      cmd_start ;;
  stop)           cmd_stop ;;
  restart)        cmd_stop; cmd_start ;;
  logs)           need_host; rssh_tty "tail -n 100 -F \"$REMOTE_DIR/logs/zerobha_\$(date +%F).log\"" ;;
  attach)         need_host; rssh_tty "tmux attach -t $SESSION" ;;
  status)         cmd_status ;;
  config)
    if [[ -n "$REMOTE_HOST" ]]; then rssh "cd '$REMOTE_DIR' && ./trader -config config.local.toml -show-config"
    else go run ./cmd/trader -config config.local.toml -show-config; fi ;;
  setup)          cmd_setup ;;
  rclone-setup)   need_host; rssh_tty "rclone config" ;;
  backup)         need_host; rssh "$REMOTE_DIR/backup.sh" ;;
  help|-h|--help) usage ;;
  *)              usage; exit 1 ;;
esac
