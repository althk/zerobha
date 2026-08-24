#!/usr/bin/env bash
# ==============================================================================
# Zerobha VM Operations Script
# All-in-one management: Prerequisites, Docker lifecycle, and Cloud Backups
# ==============================================================================

set -eo pipefail

BASE_DIR="/opt/zerobha"
DATA_DIR="${BASE_DIR}/data"
LOGS_DIR="${BASE_DIR}/logs"
CONTAINER_NAME="zerobha"
IMAGE_NAME="zerobha:latest"
GDRIVE_REMOTE="${GDRIVE_REMOTE:-gdrive}" # change or pass GDRIVE_REMOTE=myremote

# Colors for terminal output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }

ensure_dirs() {
    sudo mkdir -p "${DATA_DIR}" "${LOGS_DIR}"
    sudo chown -R "$USER":"$USER" "${BASE_DIR}" 2>/dev/null || true
}

# ------------------------------------------------------------------------------
# 1. Setup Prerequisites on Host (Debian 12)
# ------------------------------------------------------------------------------
cmd_setup_prereqs() {
    log_info "Setting up prerequisites on Debian 12..."

    # 1. Set System Timezone to IST (UTC+0530)
    log_info "Configuring timezone to Asia/Kolkata (IST)..."
    sudo timedatectl set-timezone Asia/Kolkata
    log_success "Timezone set to $(timedatectl show --property=Timezone --value)"

    # 2. Update package list & install core utilities
    log_info "Installing core packages (sqlite3, rclone, curl, gzip, ca-certificates)..."
    sudo apt-get update -y
    sudo apt-get install -y sqlite3 rclone curl gzip ca-certificates apt-transport-https gnupg lsb-release

    # 3. Install Docker if not present
    if ! command -v docker &> /dev/null; then
        log_info "Docker not found. Installing official Docker..."
        curl -fsSL https://get.docker.com | sudo sh
        sudo usermod -aG docker "$USER" || true
        sudo systemctl enable --now docker
        log_success "Docker installed successfully."
        log_warn "Note: You may need to logout and log back in for docker group permissions."
    else
        log_info "Docker is already installed."
    fi

    # 4. Create required directories
    ensure_dirs
    log_success "Created directory structure at ${BASE_DIR}"

    # 5. Configure automated cron backup at 15:45 IST (Mon-Fri)
    local script_path
    script_path=$(realpath "$0")
    local cron_job="45 15 * * 1-5 ${script_path} backup >> ${LOGS_DIR}/backup.log 2>&1"
    
    if crontab -l 2>/dev/null | grep -F "${script_path} backup" > /dev/null; then
        log_info "Cron backup job already exists."
    else
        log_info "Adding 15:45 IST daily post-market backup to crontab..."
        (crontab -l 2>/dev/null || true; echo "${cron_job}") | crontab -
        log_success "Automated cron backup scheduled."
    fi

    echo ""
    log_success "============================================================"
    log_success "Prerequisites setup complete!"
    log_success "Next steps:"
    log_success "  1. Run '${script_path} rclone-setup' to link Google Drive"
    log_success "  2. Run '${script_path} docker-load zerobha.tar.gz'"
    log_success "  3. Run '${script_path} docker-run' to start trading"
    log_success "============================================================"
}

# ------------------------------------------------------------------------------
# 2. Rclone Google Drive Setup
# ------------------------------------------------------------------------------
cmd_rclone_setup() {
    log_info "Starting interactive rclone Google Drive configuration..."
    log_info "When prompted:"
    log_info "  - Name: 'gdrive'"
    log_info "  - Storage type: 'drive' (Google Drive)"
    log_info "  - Follow on-screen instructions to authorize your Google Account."
    echo ""
    rclone config
}

# ------------------------------------------------------------------------------
# 3. Docker Image Load
# ------------------------------------------------------------------------------
cmd_docker_load() {
    local tar_file="${1:-zerobha.tar.gz}"
    if [ ! -f "${tar_file}" ]; then
        # Check common fallback locations
        if [ -f "$HOME/${tar_file}" ]; then
            tar_file="$HOME/${tar_file}"
        elif [ -f "${BASE_DIR}/${tar_file}" ]; then
            tar_file="${BASE_DIR}/${tar_file}"
        else
            log_error "Docker archive '${tar_file}' not found."
            log_info "Usage: $0 docker-load [path_to_zerobha.tar.gz]"
            exit 1
        fi
    fi

    log_info "Loading Docker image from '${tar_file}'..."
    docker load < "${tar_file}"
    log_success "Docker image '${IMAGE_NAME}' loaded."
}

# ------------------------------------------------------------------------------
# 4. Docker Run / Start / Restart / Stop / Logs
# ------------------------------------------------------------------------------
cmd_docker_run() {
    ensure_dirs

    # Stop and remove existing container if running
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        log_info "Stopping and removing existing '${CONTAINER_NAME}' container..."
        docker stop "${CONTAINER_NAME}" &>/dev/null || true
        docker rm "${CONTAINER_NAME}" &>/dev/null || true
    fi

    log_info "Starting '${CONTAINER_NAME}' container..."
    docker run -d \
        --name "${CONTAINER_NAME}" \
        --restart unless-stopped \
        -v "${LOGS_DIR}:/app/logs" \
        -v "${DATA_DIR}:/app/data" \
        "${IMAGE_NAME}"

    log_success "Container '${CONTAINER_NAME}' started successfully."
    log_info "Run '$0 logs' to view live trading output."
}

cmd_docker_stop() {
    log_info "Stopping '${CONTAINER_NAME}' container..."
    docker stop "${CONTAINER_NAME}"
    log_success "Container stopped."
}

cmd_docker_restart() {
    log_info "Restarting '${CONTAINER_NAME}' container..."
    docker restart "${CONTAINER_NAME}"
    log_success "Container restarted."
}

cmd_docker_logs() {
    docker logs -f "${CONTAINER_NAME}"
}

cmd_status() {
    echo -e "${BLUE}=== System Time & Timezone ===${NC}"
    timedatectl | grep -E "Local time|Time zone" || date
    echo ""
    echo -e "${BLUE}=== Docker Container Status ===${NC}"
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        docker ps --filter "name=${CONTAINER_NAME}" --format "table {{.Names}}\t{{.Status}}\t{{.RunningFor}}\t{{.Image}}"
    else
        log_warn "Container '${CONTAINER_NAME}' is not created or running."
    fi
    echo ""
    echo -e "${BLUE}=== Data & Storage ===${NC}"
    ls -lh "${DATA_DIR}" 2>/dev/null || true
    echo ""
    echo -e "${BLUE}=== Crontab Backup Jobs ===${NC}"
    crontab -l 2>/dev/null | grep "backup" || log_warn "No backup cron job configured."
}

# ------------------------------------------------------------------------------
# 5. Cloud Backup to Google Drive
# ------------------------------------------------------------------------------
cmd_backup() {
    local date_str
    date_str=$(date +'%Y-%m-%d_%H%M%S')
    local date_day
    date_day=$(date +'%Y-%m-%d')
    local backup_tmp="/tmp/zerobha_backup_${date_str}"
    
    mkdir -p "${backup_tmp}"
    log_info "[$(date)] Starting backup process..."

    # 1. Safe SQLite snapshot (zero transaction corruption risk)
    local db_path="${DATA_DIR}/zerobha.db"
    if [ -f "${db_path}" ]; then
        log_info "Creating online SQLite database snapshot..."
        sqlite3 "${db_path}" ".backup '${backup_tmp}/zerobha_${date_day}.db'"
    elif [ -f "${BASE_DIR}/zerobha.db" ]; then
        sqlite3 "${BASE_DIR}/zerobha.db" ".backup '${backup_tmp}/zerobha_${date_day}.db'"
    else
        log_warn "No SQLite database found at ${db_path} yet (skipping DB snapshot)."
    fi

    # 2. Capture Docker & system logs
    log_info "Gathering logs..."
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        docker logs "${CONTAINER_NAME}" > "${backup_tmp}/docker_${CONTAINER_NAME}_${date_day}.log" 2>&1 || true
    fi
    if [ -d "${LOGS_DIR}" ]; then
        cp -r "${LOGS_DIR}/"* "${backup_tmp}/" 2>/dev/null || true
    fi

    # 3. Compress text logs
    for log_file in "${backup_tmp}"/*.log; do
        [ -f "$log_file" ] && gzip -f "$log_file"
    done

    # 4. Upload to Google Drive via rclone
    if command -v rclone &>/dev/null; then
        local destination="${GDRIVE_REMOTE}:zerobha_backups/${date_day}"
        log_info "Syncing backup to '${destination}'..."
        if rclone copy "${backup_tmp}" "${destination}"; then
            log_success "Backup uploaded to Google Drive: ${destination}"
        else
            log_error "rclone failed to upload to ${destination}. Check 'rclone config'."
        fi
    else
        log_error "rclone not installed. Run '$0 setup-prereqs' first."
    fi

    # 5. Clean up temporary directory
    rm -rf "${backup_tmp}"
    log_success "[$(date)] Backup process complete."
}

# ------------------------------------------------------------------------------
# Usage & Command Router
# ------------------------------------------------------------------------------
cmd_help() {
    cat <<EOF
Usage: $(basename "$0") <command> [arguments]

Core Commands:
  setup-prereqs        Install Docker, rclone, sqlite3, set IST timezone & schedule cron
  rclone-setup         Configure Google Drive access interactively
  docker-load [file]   Load Docker image from tar.gz (default: zerobha.tar.gz)
  docker-run           Start the trading container in the background (with data/logs mounts)
  stop                 Stop the trading container
  restart              Restart the trading container
  logs                 Stream live trading logs
  status               Check container status, system time, disk usage & backups
  backup               Execute an immediate safe snapshot & upload to Google Drive

Examples:
  ./$(basename "$0") setup-prereqs
  ./$(basename "$0") docker-load zerobha.tar.gz
  ./$(basename "$0") docker-run
  ./$(basename "$0") backup
EOF
}

case "${1:-help}" in
    setup-prereqs|setup)
        cmd_setup_prereqs
        ;;
    rclone-setup|rclone)
        cmd_rclone_setup
        ;;
    docker-load|load)
        shift
        cmd_docker_load "$@"
        ;;
    docker-run|run|start)
        cmd_docker_run
        ;;
    stop)
        cmd_docker_stop
        ;;
    restart)
        cmd_docker_restart
        ;;
    logs)
        cmd_docker_logs
        ;;
    status)
        cmd_status
        ;;
    backup)
        cmd_backup
        ;;
    help|--help|-h)
        cmd_help
        ;;
    *)
        log_error "Unknown command: $1"
        cmd_help
        exit 1
        ;;
esac
