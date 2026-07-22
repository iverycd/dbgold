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

[[ -f "$DEPLOY_DIR/compose.yaml" && -f "$DEPLOY_DIR/config/dbgold.env" ]] || { echo "Invalid deployment directory: $DEPLOY_DIR" >&2; exit 1; }
if [[ -x "$DEPLOY_DIR/bin/docker-compose" ]]; then
  COMPOSE_BIN="$DEPLOY_DIR/bin/docker-compose"
elif [[ -x "$SCRIPT_DIR/bin/docker-compose" ]]; then
  COMPOSE_BIN="$SCRIPT_DIR/bin/docker-compose"
else
  echo "Bundled Docker Compose is missing or not executable." >&2
  exit 1
fi
mkdir -p "$DEPLOY_DIR/backups"
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_FILE="$DEPLOY_DIR/backups/dbgold-$TIMESTAMP.tar.gz"
WAS_RUNNING=0

cd "$DEPLOY_DIR"
compose() {
  "$COMPOSE_BIN" --env-file config/dbgold.env -f compose.yaml "$@"
}
if (( ALREADY_STOPPED == 0 )); then
  if [[ -n "$(compose ps --status running -q)" ]]; then
    WAS_RUNNING=1
    compose stop
  fi
fi

restart_if_needed() {
  if (( WAS_RUNNING == 1 )); then
    compose up -d
  fi
}
trap restart_if_needed EXIT
tar -czf "$BACKUP_FILE" data uploads config
chmod 0600 "$BACKUP_FILE"
trap - EXIT
restart_if_needed
echo "$BACKUP_FILE"
