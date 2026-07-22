#!/usr/bin/env bash
set -euo pipefail

DEPLOY_DIR="/opt/dbgold"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ALREADY_STOPPED=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy-dir) DEPLOY_DIR="${2:-}"; shift 2 ;;
    --already-stopped) ALREADY_STOPPED=1; shift ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -f "$DEPLOY_DIR/config/dbgold.env" ]] || { echo "Invalid deployment directory: $DEPLOY_DIR" >&2; exit 1; }
[[ -f "$SCRIPT_DIR/docker-runtime.sh" ]] || { echo "Docker runtime helper is missing." >&2; exit 1; }
source "$SCRIPT_DIR/docker-runtime.sh"
docker_container_assert_replaceable
mkdir -p "$DEPLOY_DIR/backups"
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_FILE="$DEPLOY_DIR/backups/dbgold-$TIMESTAMP.tar.gz"
WAS_RUNNING=0

if (( ALREADY_STOPPED == 0 )); then
  if docker_container_exists && docker_container_is_running; then
    WAS_RUNNING=1
    docker_container_stop
  fi
fi

restart_if_needed() {
  if (( WAS_RUNNING == 1 )); then
    docker_container_start
  fi
}
trap restart_if_needed EXIT
cd "$DEPLOY_DIR"
tar -czf "$BACKUP_FILE" data uploads config
chmod 0600 "$BACKUP_FILE"
trap - EXIT
restart_if_needed
echo "$BACKUP_FILE"
