#!/usr/bin/env bash
# Build/ship/run zerobha NATIVELY (no Docker) on a remote Debian 12 VM over
# SSH, from your local machine. Every command runs locally and does its remote
# work over ssh.
#
# Usage: ./zerobha.sh <command> [user@host]
#   build         Cross-compile the Linux trader binary locally (bin/trader-linux)
#   copy          scp the binary, config and CSVs to the VM
#   deploy        build + copy + restart the trader
#   start|run     Start the trader in tmux session "zerobha" (it exits on its
#                 own outside 07:00-15:05)
#   restart       Restart the trader (a fresh Kite login is needed afterwards)
#   stop          Stop the trader (Ctrl-C into the tmux session)
#   logs          Follow today's log file (Ctrl-C stops following only)
#   attach        Attach to the tmux session (detach with Ctrl-b d)
#   status        Show system time, trader session, data dir, backup cron
#   setup         One-time VM prep: timezone, packages (incl. tmux), dirs,
#                 backup script + cron, removal of old Docker/systemd setups
#   rclone-setup  Interactive `rclone config` on the VM, to link Google Drive
#   backup        Run the installed backup script on the VM immediately
#
# The host falls back to $REMOTE_HOST when the argument is omitted.
#
# Layout on the VM ($REMOTE_DIR, default /opt/zerobha):
#   trader              the binary
#   config.local.toml   the config (the [paths] knobs resolve against this dir:
#                       db_path = "data/zerobha.db", log_dir = "logs")
#   *.csv               files the trader opens by relative path
#   data/  logs/        database and logs - same layout the Docker volumes used,
#                       so an existing zerobha.db carries straight over
#
# The trader runs in a detached tmux session called "zerobha", started by hand
# with 'start'. It exits on its own outside hours and at 15:30, closing the
# session. It binds 9880 (Kite callback) and 9080 (dashboard) to 127.0.0.1
# only; reach them through an SSH LocalForward.
#
# Environment variables (all optional):
#   REMOTE_DIR=${HOME}/opt/zerobha   SSH_PORT=22   GDRIVE_REMOTE=gdrive
#   SSH_OPTS="-o ClearAllForwardings=yes"
#   (ClearAllForwardings stops a LocalForward in ~/.ssh/config from trying to
#   rebind 9880/9080 locally on every connection this script makes.)

set -euo pipefail

COMMAND="${1:-}"
REMOTE_HOST="${2:-${REMOTE_HOST:-}}"
REMOTE_DIR="${REMOTE_DIR:-opt/zerobha}"
SSH_PORT="${SSH_PORT:-22}"
GDRIVE_REMOTE="${GDRIVE_REMOTE:-gdrive}"
SESSION="zerobha"
BINARY="bin/trader-linux"
# Files the trader opens by path relative to its working directory:
# the config, the MIS leverage map (internal/core/engine.go), the strategy
# watchlist (emacross/donchian csv_file) and the Upstox gate's isin_csv.
SHIP_FILES=(config.local.toml zerodha-mis-margins.csv indices.csv ind_nifty500list.csv)

SSH_OPTS_RAW="${SSH_OPTS-"-o ClearAllForwardings=yes"}"
if [[ -n "$SSH_OPTS_RAW" ]]; then
  IFS=' ' read -r -a SSH_OPTS <<< "$SSH_OPTS_RAW"
else
  SSH_OPTS=()
fi

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }

require_remote_host() {
  if [[ -z "$REMOTE_HOST" ]]; then
    log_error "no target host given (expected './zerobha.sh $COMMAND user@host')"
    exit 1
  fi
}

# rssh runs a command on the VM; rssh_tty allocates a pty so sudo can prompt.
rssh()     { ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "$@"; }
rssh_tty() { ssh -t "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "$@"; }

cmd_build() {
  log_info "Building $BINARY (linux/amd64)"
  mkdir -p bin
  # modernc.org/sqlite is pure Go, so CGO stays off and no Linux toolchain is needed.
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$BINARY" ./cmd/trader
}

cmd_copy() {
  require_remote_host
  [[ -f "$BINARY" ]] || { log_error "$BINARY not found, run './zerobha.sh build' first"; exit 1; }
  for f in "${SHIP_FILES[@]}"; do
    [[ -f "$f" ]] || { log_error "$f not found locally"; exit 1; }
  done
  log_info "Preparing $REMOTE_DIR on $REMOTE_HOST"
  rssh_tty "sudo mkdir -p '$REMOTE_DIR/data' '$REMOTE_DIR/logs' && sudo chown -R \"\$USER\":\"\$USER\" '$REMOTE_DIR'"
  log_info "Copying binary, config and CSVs"
  # The binary goes up under a temporary name and is renamed into place:
  # overwriting a running executable fails with "Text file busy".
  scp "${SSH_OPTS[@]}" -P "$SSH_PORT" "$BINARY" "$REMOTE_HOST:$REMOTE_DIR/trader.new"
  scp "${SSH_OPTS[@]}" -P "$SSH_PORT" "${SHIP_FILES[@]}" "$REMOTE_HOST:$REMOTE_DIR/"
  rssh "chmod +x '$REMOTE_DIR/trader.new' && mv -f '$REMOTE_DIR/trader.new' '$REMOTE_DIR/trader' && chmod 600 '$REMOTE_DIR/config.local.toml'"
}

cmd_deploy() {
  require_remote_host
  cmd_build
  cmd_copy
  cmd_restart
}

# The trader runs in a detached tmux session; when it exits (outside hours,
# holiday, 15:30 shutdown, crash) the session closes with it. Its output also
# goes to logs/zerobha_<date>.log, which is what 'logs' follows.
cmd_start() {
  require_remote_host
  # shellcheck disable=SC2087
  rssh bash -s <<EOF
set -euo pipefail
if tmux has-session -t $SESSION 2>/dev/null; then
  echo "tmux session '$SESSION' is already running; use restart or stop" >&2
  exit 1
fi
cd '$REMOTE_DIR'
tmux new-session -d -s $SESSION -c "\$PWD" "TZ=Asia/Kolkata ./trader -config config.local.toml"
EOF
  print_access
}

cmd_stop() {
  require_remote_host
  # shellcheck disable=SC2087
  rssh bash -s <<EOF
if ! tmux has-session -t $SESSION 2>/dev/null; then
  echo "tmux session '$SESSION' is not running"
  exit 0
fi
# Ctrl-C lets the trader shut down cleanly; kill the session if it hangs.
tmux send-keys -t $SESSION C-c
for _ in \$(seq 1 20); do
  tmux has-session -t $SESSION 2>/dev/null || { echo "stopped"; exit 0; }
  sleep 1
done
echo "trader did not exit within 20s, killing the session"
tmux kill-session -t $SESSION
EOF
}

cmd_restart() {
  require_remote_host
  log_info "Restarting the trader in tmux session '$SESSION' on $REMOTE_HOST"
  cmd_stop
  cmd_start
}

print_access() {
  log_success "Started in tmux session '$SESSION'. Outside 07:00-15:05 IST it exits immediately, which is expected."
  log_info "  Logs (Kite login URL appears here): ./zerobha.sh logs $REMOTE_HOST"
  log_info "  Or on the VM: tmux attach -t $SESSION   (detach with Ctrl-b d, NOT Ctrl-c)"
  log_info "  Kite callback / dashboard: http://localhost:9880 / your LocalForward to 9080"
}

cmd_logs() {
  require_remote_host
  rssh_tty "tail -n 100 -F \"$REMOTE_DIR/logs/zerobha_\$(date +%F).log\""
}

cmd_attach() {
  require_remote_host
  rssh_tty "tmux attach -t $SESSION"
}

cmd_status() {
  require_remote_host
  # shellcheck disable=SC2087
  rssh bash -s <<EOF
echo "=== System Time & Timezone ==="
timedatectl | grep -E "Local time|Time zone" || date
echo ""
echo "=== Trader ==="
if tmux has-session -t $SESSION 2>/dev/null; then
  echo "tmux session '$SESSION' is running"
  pgrep -af '$REMOTE_DIR/trader|\./trader -config' || true
else
  echo "tmux session '$SESSION' is not running"
fi
echo ""
echo "=== Data ==="
ls -lh "$REMOTE_DIR" "$REMOTE_DIR/data" 2>/dev/null || true
echo ""
echo "=== Backup cron ==="
crontab -l 2>/dev/null | grep backup || echo "No backup cron job configured."
EOF
}

# The backup script installed on the VM. Unquoted heredoc: REMOTE_DIR
# and GDRIVE_REMOTE are baked in now; escaped "$" survive into the file.
generate_backup_script() {
  cat <<BACKUP_EOF
#!/usr/bin/env bash
# Installed by 'zerobha.sh setup'. Snapshots the database and logs and
# uploads them to Google Drive via rclone. Regenerate with setup, don't edit.
set -euo pipefail

BASE_DIR="$REMOTE_DIR"
DATA_DIR="\${BASE_DIR}/data"
LOGS_DIR="\${BASE_DIR}/logs"
GDRIVE_REMOTE="$GDRIVE_REMOTE"

date_day=\$(date +'%Y-%m-%d')
backup_tmp="/tmp/zerobha_backup_\$(date +'%Y-%m-%d_%H%M%S')"
mkdir -p "\$backup_tmp"
echo "[\$(date)] Starting backup..."

# Online SQLite snapshot - safe against a database being written to.
db_path="\${DATA_DIR}/zerobha.db"
if [ -f "\$db_path" ]; then
  sqlite3 "\$db_path" ".backup '\${backup_tmp}/zerobha_\${date_day}.db'"
else
  echo "warning: no database at \$db_path yet, skipping DB snapshot"
fi

if [ -d "\$LOGS_DIR" ]; then
  cp -r "\${LOGS_DIR}/"* "\$backup_tmp/" 2>/dev/null || true
fi

for log_file in "\$backup_tmp"/*.log; do
  [ -f "\$log_file" ] && gzip -f "\$log_file"
done

if command -v rclone >/dev/null 2>&1; then
  destination="\${GDRIVE_REMOTE}:zerobha_backups/\${date_day}"
  if rclone copy "\$backup_tmp" "\$destination"; then
    echo "Backup uploaded to \$destination"
  else
    echo "error: rclone failed to upload to \$destination" >&2
  fi
else
  echo "error: rclone not installed, run './zerobha.sh setup' first" >&2
fi

rm -rf "\$backup_tmp"
echo "[\$(date)] Backup complete."
BACKUP_EOF
}

cmd_setup() {
  require_remote_host
  log_info "Preparing $REMOTE_HOST (Debian 12)"
  # shellcheck disable=SC2087
  rssh_tty bash -s <<EOF
set -euo pipefail
echo "==> Setting timezone to Asia/Kolkata"
sudo timedatectl set-timezone Asia/Kolkata
echo "==> Installing packages"
sudo apt-get update -y
sudo apt-get install -y sqlite3 rclone ca-certificates tzdata gzip tmux
if [ -f /etc/systemd/system/$SESSION.service ]; then
  echo "==> Removing the old zerobha systemd service and timer"
  sudo systemctl disable --now $SESSION.timer $SESSION.service 2>/dev/null || true
  sudo rm -f /etc/systemd/system/$SESSION.service /etc/systemd/system/$SESSION.timer
  sudo systemctl daemon-reload
fi
if command -v docker >/dev/null 2>&1 && sudo docker ps -a --format '{{.Names}}' | grep -q '^zerobha\$'; then
  echo "==> Removing the old zerobha Docker container (data/ and logs/ are kept)"
  sudo docker rm -f zerobha
fi
echo "==> Creating $REMOTE_DIR/{data,logs}"
sudo mkdir -p "$REMOTE_DIR/data" "$REMOTE_DIR/logs"
sudo chown -R "\$USER":"\$USER" "$REMOTE_DIR"
# Leftover image tarball from the Docker deployment.
rm -f "$REMOTE_DIR/zerobha.tar.gz" "$REMOTE_DIR/zerobha.tar"
EOF

  log_info "Installing backup script at $REMOTE_DIR/backup.sh"
  generate_backup_script | rssh "cat > '$REMOTE_DIR/backup.sh' && chmod +x '$REMOTE_DIR/backup.sh'"

  log_info "Scheduling the 15:45 IST Mon-Fri backup cron job"
  # shellcheck disable=SC2087
  rssh bash -s <<EOF
set -euo pipefail
CRON_JOB="45 15 * * 1-5 $REMOTE_DIR/backup.sh >> $REMOTE_DIR/logs/backup.log 2>&1"
existing="\$(crontab -l 2>/dev/null || true)"
if echo "\$existing" | grep -qF "$REMOTE_DIR/backup.sh"; then
  echo "==> Cron backup job already present"
else
  (echo "\$existing"; echo "\$CRON_JOB") | crontab -
  echo "==> Backup cron job installed"
fi
EOF
  log_success "Setup complete. Next: './zerobha.sh deploy $REMOTE_HOST'."
}

cmd_rclone_setup() {
  require_remote_host
  log_info "Interactive rclone config: new remote 'gdrive', storage type 'drive'."
  rssh_tty "rclone config"
}

cmd_backup() {
  require_remote_host
  rssh "$REMOTE_DIR/backup.sh"
}

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; }

case "$COMMAND" in
  build)                cmd_build ;;
  copy)                 cmd_copy ;;
  deploy)               cmd_deploy ;;
  start|run)            cmd_start ;;
  restart)              cmd_restart ;;
  stop)                 cmd_stop ;;
  logs)                 cmd_logs ;;
  attach)               cmd_attach ;;
  status)               cmd_status ;;
  setup|setup-prereqs)  cmd_setup ;;
  rclone-setup)         cmd_rclone_setup ;;
  backup)               cmd_backup ;;
  help|--help|-h)       usage ;;
  "")                   usage; exit 1 ;;
  *)                    log_error "Unknown command: $COMMAND"; usage; exit 1 ;;
esac
