#!/usr/bin/env bash
# Build/ship/run zerobha on a remote Debian 12 VM over SSH, from your local
# machine. Modelled on chartinkbot's deploy.sh — nothing here runs commands
# directly on the VM by hand any more; every command is invoked locally and
# does its remote work over ssh.
#
# Usage: ./zerobha.sh <command> [user@host]
#   build         Build the docker image locally
#   save          Build (if needed) and save+gzip the image to a local tar.gz
#   copy          scp the saved image to the remote VM
#   run           Load the image on the remote VM and (re)start the container
#   deploy        save + copy + run, in order
#   restart       Restart the container on the remote VM without reloading the image
#   stop          Stop the container on the remote VM
#   logs          Tail the container's logs on the remote VM (Ctrl-C to detach)
#   status        Show system time, container status, data dir and backup cron
#   setup         One-time VM prep: timezone, packages, Docker, dirs, backup
#                 script + cron (alias: setup-prereqs)
#   rclone-setup  Interactive `rclone config` on the remote VM, to link Google Drive
#   backup        Run the installed backup script on the remote VM immediately
#
# Every command except build and save takes the target host as its second
# argument, e.g. `./zerobha.sh run user@vm`. It falls back to $REMOTE_HOST when
# the argument is omitted, so exporting it once per shell saves retyping it.
#
# Examples:
#   ./zerobha.sh deploy user@vm
#   REMOTE_HOST=user@vm ./zerobha.sh logs
#
# Configure via environment variables (all have defaults, override as needed):
#   IMAGE_NAME     Local image name/tag                       (default: zerobha:latest)
#   TAR_FILE       Local path for the saved image              (default: ./zerobha.tar.gz)
#   REMOTE_DIR     Base dir on the remote VM (data/logs/backup) (default: /opt/zerobha)
#   CONTAINER_NAME Name of the running container                (default: zerobha)
#   SSH_PORT       SSH port                                     (default: 22)
#   SSH_OPTS       Extra ssh/scp options (space-separated)
#                  (default: -o ClearAllForwardings=yes)
#   CONTAINER_TZ   Container timezone (log timestamps)          (default: Asia/Kolkata)
#   GDRIVE_REMOTE  rclone remote name for backups                (default: gdrive)
#
# REMOTE_DIR must be an ABSOLUTE path: every remote command quotes it, so a
# leading ~ reaches the VM literally instead of expanding to $HOME.
#
# Unlike chartinkbot, zerobha's config is BAKED INTO THE IMAGE — the Dockerfile
# does `COPY config.local.toml`, so there is no config file to ship separately
# and no bind mount for it (`copy` only ships the image tarball). Changing the
# config means editing config.local.toml, then `./zerobha.sh deploy` again.
#
# The container is NOT run with --network host (unlike chartinkbot): it
# publishes -p 9880 (Kite auth callback) and -p 9080 (dashboard) explicitly,
# and bind-mounts $REMOTE_DIR/data and $REMOTE_DIR/logs at /app/data and
# /app/logs. Both ports bind 0.0.0.0 and the dashboard has no auth — see
# DEPLOYMENT_GUIDE.md before exposing a cloud VM's ports to the internet.
#
# The backup schedule has to keep running on the VM with nothing scp'd there
# but the image, so `setup` writes a small self-contained backup.sh directly
# on the VM (piped over ssh, not templated through a second layer of heredoc
# escaping) and installs its cron entry idempotently. `backup` just runs it.

set -euo pipefail

COMMAND="${1:-}"
IMAGE_NAME="${IMAGE_NAME:-zerobha:latest}"
TAR_FILE="${TAR_FILE:-./zerobha.tar.gz}"
# The target host is the second argument; $REMOTE_HOST remains a fallback so
# an exported value still works when the argument is omitted.
REMOTE_HOST="${2:-${REMOTE_HOST:-}}"
REMOTE_DIR="${REMOTE_DIR:-/opt/zerobha}"
CONTAINER_NAME="${CONTAINER_NAME:-zerobha}"
SSH_PORT="${SSH_PORT:-22}"
# Renamed from TZ, which is a standard exported env var — an operator with TZ
# already set in their shell would otherwise silently ship a different
# container timezone than the documented Asia/Kolkata, and IST is load-bearing
# for this bot's market-hours logic.
CONTAINER_TZ="${CONTAINER_TZ:-Asia/Kolkata}"
GDRIVE_REMOTE="${GDRIVE_REMOTE:-gdrive}"
# Defaults to clearing all port forwardings: this script never needs one of
# its own, and a ~/.ssh/config with permanent LocalForward entries (e.g. for
# the Kite callback on 9880 and the dashboard on 9080) would otherwise try to
# rebind those local ports on every ssh/scp this script makes, emitting
# "Address already in use" / "Could not request local forwarding" once an
# interactive session already holds them. This only affects the script's own
# connections — a plain `ssh myvm` still gets its forwards untouched. Only
# build the array when SSH_OPTS is non-empty; `IFS=' ' read -r -a SSH_OPTS <<<
# ""` would otherwise yield a 1-element array containing an empty string and
# pass a stray empty argument to ssh/scp.
SSH_OPTS_RAW="${SSH_OPTS-"-o ClearAllForwardings=yes"}"
if [[ -n "$SSH_OPTS_RAW" ]]; then
  IFS=' ' read -r -a SSH_OPTS <<< "$SSH_OPTS_RAW"
else
  SSH_OPTS=()
fi

# Colors for local terminal output only — nothing below runs on the VM.
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }

require_remote_host() {
  if [[ -z "$REMOTE_HOST" ]]; then
    log_error "no target host given (expected './zerobha.sh $COMMAND user@host')"
    exit 1
  fi
}

cmd_build() {
  log_info "Building image $IMAGE_NAME"
  docker build -t "$IMAGE_NAME" .
}

cmd_save() {
  cmd_build
  log_info "Saving image to $TAR_FILE"
  docker save "$IMAGE_NAME" | gzip > "$TAR_FILE"
}

cmd_copy() {
  require_remote_host
  if [[ ! -f "$TAR_FILE" ]]; then
    log_error "$TAR_FILE not found, run './zerobha.sh save' first"
    exit 1
  fi
  log_info "Creating $REMOTE_DIR on $REMOTE_HOST"
  # /opt requires root, so this needs sudo; chown hands the dir back to the
  # login user so later steps (copy, run) don't need sudo themselves. -t
  # allocates a pty so sudo can prompt for a password on a VM without
  # passwordless sudo — without it there's no tty for sudo to read from and
  # the command just hangs or fails.
  ssh -t "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" \
    "sudo mkdir -p '$REMOTE_DIR' && sudo chown \"\$USER\":\"\$USER\" '$REMOTE_DIR'"
  log_info "Copying $TAR_FILE to $REMOTE_HOST:$REMOTE_DIR/"
  scp "${SSH_OPTS[@]}" -P "$SSH_PORT" "$TAR_FILE" "$REMOTE_HOST:$REMOTE_DIR/"
}

cmd_run() {
  require_remote_host
  local tar_basename
  tar_basename="$(basename "$TAR_FILE")"
  log_info "Loading image and (re)starting container on $REMOTE_HOST"
  # shellcheck disable=SC2087
  ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" bash -s <<EOF
set -euo pipefail
REMOTE_DIR="$REMOTE_DIR"
cd "\$REMOTE_DIR"
mkdir -p data logs
case "$tar_basename" in
  *.gz|*.tgz) gunzip -c "$tar_basename" | docker load ;;
  *)          docker load -i "$tar_basename" ;;
esac
docker stop "$CONTAINER_NAME" 2>/dev/null || true
docker rm "$CONTAINER_NAME" 2>/dev/null || true
docker run -d \
  --name "$CONTAINER_NAME" \
  --restart unless-stopped \
  -e TZ="$CONTAINER_TZ" \
  -p 9880:9880 \
  -p 9080:9080 \
  -v "\$REMOTE_DIR/data:/app/data" \
  -v "\$REMOTE_DIR/logs:/app/logs" \
  "$IMAGE_NAME"
EOF
  # Strip any "user@" prefix for display — $REMOTE_HOST is the ssh target
  # (user@host), and interpolating it as-is into a URL produces
  # http://user@vm:9080, which is not a usable link.
  local display_host="${REMOTE_HOST##*@}"
  log_success "Container started."
  log_info "  Dashboard:          http://$display_host:9080 (reach it through an SSH LocalForward in your ~/.ssh/config)"
  # This is the URL registered with Kite and reached via the ssh forward from
  # localhost, not the VM's own address — printing the VM host here would be
  # actively misleading.
  log_info "  Kite auth callback: http://localhost:9880/auth/kite/callback"
  log_info "Use './zerobha.sh logs $REMOTE_HOST' to follow output."
}

cmd_deploy() {
  # Check the host before the (slow) build — cmd_copy would catch a missing
  # host too, but only after cmd_save has already built and gzipped the image.
  require_remote_host
  cmd_save
  cmd_copy
  cmd_run
}

cmd_restart() {
  require_remote_host
  log_info "Restarting container on $REMOTE_HOST (no image reload)"
  ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "docker restart $CONTAINER_NAME"
}

cmd_stop() {
  require_remote_host
  log_info "Stopping container on $REMOTE_HOST"
  ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "docker stop $CONTAINER_NAME"
}

cmd_logs() {
  require_remote_host
  # -t so Ctrl-C detaches the local terminal cleanly instead of hanging the
  # remote `docker logs -f` in the background.
  ssh -t "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "docker logs -f $CONTAINER_NAME"
}

cmd_status() {
  require_remote_host
  # shellcheck disable=SC2087
  ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" bash -s <<EOF
set -euo pipefail
CONTAINER_NAME="$CONTAINER_NAME"
DATA_DIR="$REMOTE_DIR/data"
echo "=== System Time & Timezone ==="
timedatectl | grep -E "Local time|Time zone" || date
echo ""
echo "=== Docker Container Status ==="
if docker ps -a --format '{{.Names}}' | grep -q "^\${CONTAINER_NAME}\$"; then
  docker ps --filter "name=\${CONTAINER_NAME}" --format "table {{.Names}}\t{{.Status}}\t{{.RunningFor}}\t{{.Image}}"
else
  echo "Container '\${CONTAINER_NAME}' is not created or running."
fi
echo ""
echo "=== Data & Storage ==="
ls -lh "\$DATA_DIR" 2>/dev/null || true
echo ""
echo "=== Crontab Backup Jobs ==="
crontab -l 2>/dev/null | grep backup || echo "No backup cron job configured."
EOF
}

# Generates the self-contained backup script that gets installed on the VM.
# Written locally and piped over ssh (never scp'd as a second file) so the
# only thing that ever needs to reach the VM by hand is the image tarball.
#
# The heredoc delimiter is deliberately UNQUOTED so REMOTE_DIR, CONTAINER_NAME
# and GDRIVE_REMOTE (this shell's values, known now) get baked in as literal
# assignments at generation time; every other "$" is escaped with a backslash
# so it survives into the file untouched, to be evaluated on the VM each time
# cron runs it.
generate_backup_script() {
  cat <<BACKUP_EOF
#!/usr/bin/env bash
# Installed by 'zerobha.sh setup'. Snapshots the database and logs and
# uploads them to Google Drive via rclone. Regenerate by re-running
# './zerobha.sh setup $REMOTE_HOST' rather than hand-editing this file.
set -euo pipefail

BASE_DIR="$REMOTE_DIR"
DATA_DIR="\${BASE_DIR}/data"
LOGS_DIR="\${BASE_DIR}/logs"
CONTAINER_NAME="$CONTAINER_NAME"
GDRIVE_REMOTE="$GDRIVE_REMOTE"

date_day=\$(date +'%Y-%m-%d')
backup_tmp="/tmp/zerobha_backup_\$(date +'%Y-%m-%d_%H%M%S')"
mkdir -p "\$backup_tmp"
echo "[\$(date)] Starting backup..."

# Online SQLite snapshot — safe to run against a database being written to.
db_path="\${DATA_DIR}/zerobha.db"
if [ -f "\$db_path" ]; then
  sqlite3 "\$db_path" ".backup '\${backup_tmp}/zerobha_\${date_day}.db'"
else
  echo "warning: no database at \$db_path yet, skipping DB snapshot"
fi

if docker ps -a --format '{{.Names}}' | grep -q "^\${CONTAINER_NAME}\$"; then
  docker logs "\$CONTAINER_NAME" > "\${backup_tmp}/docker_\${CONTAINER_NAME}_\${date_day}.log" 2>&1 || true
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
  log_info "Installing prerequisites on $REMOTE_HOST (Debian 12)..."
  # -t allocates a pty so sudo (run repeatedly below) can prompt for a
  # password on a VM without passwordless sudo. The heredoc still supplies
  # the script over this same stdin; -t only affects the tty sudo reads from.
  # shellcheck disable=SC2087
  ssh -t "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" bash -s <<EOF
set -euo pipefail
echo "==> Setting timezone to Asia/Kolkata"
sudo timedatectl set-timezone Asia/Kolkata
echo "==> Installing packages (sqlite3, rclone, curl, gzip, ca-certificates)"
sudo apt-get update -y
sudo apt-get install -y sqlite3 rclone curl gzip ca-certificates apt-transport-https gnupg lsb-release
if ! command -v docker >/dev/null 2>&1; then
  echo "==> Installing Docker"
  curl -fsSL https://get.docker.com | sudo sh
  sudo usermod -aG docker "\$USER" || true
  sudo systemctl enable --now docker
  echo "Note: log out and back in for the docker group membership to take effect."
else
  echo "==> Docker already installed"
fi
echo "==> Creating $REMOTE_DIR/{data,logs}"
sudo mkdir -p "$REMOTE_DIR/data" "$REMOTE_DIR/logs"
sudo chown -R "\$USER":"\$USER" "$REMOTE_DIR"
EOF

  log_info "Installing backup script at $REMOTE_HOST:$REMOTE_DIR/backup.sh"
  generate_backup_script | ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" \
    "cat > '$REMOTE_DIR/backup.sh' && chmod +x '$REMOTE_DIR/backup.sh'"

  log_info "Scheduling the 15:45 IST Mon-Fri backup cron job"
  # shellcheck disable=SC2087
  ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" bash -s <<EOF
set -euo pipefail
CRON_JOB="45 15 * * 1-5 $REMOTE_DIR/backup.sh >> $REMOTE_DIR/logs/backup.log 2>&1"
existing_crontab="\$(crontab -l 2>/dev/null || true)"
# Also strip any old-style entry from a previous version of this script
# (invoking "zerobha.sh backup" directly instead of the installed
# backup.sh) so it doesn't survive alongside the new one and fire twice.
if echo "\$existing_crontab" | grep -F "zerobha.sh backup" > /dev/null 2>&1; then
  echo "==> Removing old-style 'zerobha.sh backup' cron entry"
  # grep -v exits 1 if it filters out every input line; || true keeps this assignment from aborting under set -e
  existing_crontab="\$(echo "\$existing_crontab" | grep -v -F "zerobha.sh backup" || true)"
fi
if echo "\$existing_crontab" | grep -F "$REMOTE_DIR/backup.sh" > /dev/null 2>&1; then
  echo "==> Cron backup job already present"
  echo "\$existing_crontab" | crontab -
else
  (echo "\$existing_crontab"; echo "\$CRON_JOB") | crontab -
  echo "==> Backup cron job installed"
fi
EOF

  log_success "Setup complete."
  log_success "Next: './zerobha.sh rclone-setup $REMOTE_HOST', then './zerobha.sh deploy $REMOTE_HOST'."
}

cmd_rclone_setup() {
  require_remote_host
  log_info "Starting interactive rclone Google Drive configuration on $REMOTE_HOST..."
  log_info "When prompted:"
  log_info "  - New remote, name: 'gdrive'"
  log_info "  - Storage type: 'drive' (Google Drive)"
  log_info "  - Follow the on-screen link to authorize your Google Account."
  echo ""
  ssh -t "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "rclone config"
}

cmd_backup() {
  require_remote_host
  log_info "Running backup script on $REMOTE_HOST"
  ssh "${SSH_OPTS[@]}" -p "$SSH_PORT" "$REMOTE_HOST" "$REMOTE_DIR/backup.sh"
}

usage() {
  cat <<EOF
Usage: $(basename "$0") <command> [user@host]

Commands:
  build         Build the docker image locally
  save          Build (if needed) and save+gzip the image to \$TAR_FILE
  copy          scp the saved image to the remote VM
  run           Load the image on the remote VM and (re)start the container
  deploy        save + copy + run, in order
  restart       Restart the container without reloading the image
  stop          Stop the container
  logs          Tail the container's logs (Ctrl-C to detach)
  status        Show system time, container status, data dir and backup cron
  setup         One-time VM prep: timezone, packages, Docker, dirs, backup
                script + cron (alias: setup-prereqs)
  rclone-setup  Interactive 'rclone config' on the VM, to link Google Drive
  backup        Run the installed backup script on the VM immediately

Every command except build and save takes the target host as its second
argument, e.g. './zerobha.sh run user@vm'. Falls back to \$REMOTE_HOST when
omitted, so exporting it once per shell saves retyping it.

Examples:
  ./zerobha.sh deploy user@vm
  REMOTE_HOST=user@vm ./zerobha.sh logs

Environment variables (all optional, defaults shown):
  IMAGE_NAME=zerobha:latest
  TAR_FILE=./zerobha.tar.gz
  REMOTE_DIR=/opt/zerobha
  CONTAINER_NAME=zerobha
  SSH_PORT=22
  SSH_OPTS="-o ClearAllForwardings=yes"
  CONTAINER_TZ=Asia/Kolkata
  GDRIVE_REMOTE=gdrive
EOF
}

case "$COMMAND" in
  build)                cmd_build ;;
  save)                 cmd_save ;;
  copy)                 cmd_copy ;;
  run)                  cmd_run ;;
  deploy)               cmd_deploy ;;
  restart)              cmd_restart ;;
  stop)                 cmd_stop ;;
  logs)                 cmd_logs ;;
  status)               cmd_status ;;
  setup|setup-prereqs)  cmd_setup ;;
  rclone-setup)         cmd_rclone_setup ;;
  backup)               cmd_backup ;;
  help|--help|-h)       usage; exit 0 ;;
  "")
    usage
    exit 1
    ;;
  *)
    log_error "Unknown command: $COMMAND"
    usage
    exit 1
    ;;
esac
