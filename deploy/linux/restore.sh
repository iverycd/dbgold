#!/usr/bin/env bash
set -euo pipefail

DEPLOY_DIR="/opt/dbgold"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_FILE=""
CONFIRMED=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy-dir) DEPLOY_DIR="${2:-}"; shift 2 ;;
    --backup) BACKUP_FILE="${2:-}"; shift 2 ;;
    --yes) CONFIRMED=1; shift ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -n "$BACKUP_FILE" && -f "$BACKUP_FILE" ]] || { echo "A valid --backup file is required." >&2; exit 1; }
(( CONFIRMED == 1 )) || { echo "Restore replaces current data. Re-run with --yes." >&2; exit 1; }
[[ -f "$DEPLOY_DIR/config/dbgold.env" ]] || { echo "Invalid deployment directory: $DEPLOY_DIR" >&2; exit 1; }
[[ -f "$SCRIPT_DIR/docker-runtime.sh" ]] || { echo "Docker runtime helper is missing." >&2; exit 1; }
source "$SCRIPT_DIR/docker-runtime.sh"
docker_container_assert_replaceable
if tar -tzf "$BACKUP_FILE" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then
  echo "Backup contains an unsafe path and will not be restored." >&2
  exit 1
fi

docker_container_stop || true
SAFETY_DIR="$DEPLOY_DIR/backups/pre-restore-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$SAFETY_DIR"
for item in data uploads config; do
  if [[ -e "$DEPLOY_DIR/$item" ]]; then
    mv "$DEPLOY_DIR/$item" "$SAFETY_DIR/$item"
  fi
done

if ! tar -xzf "$BACKUP_FILE" -C "$DEPLOY_DIR"; then
  FAILED_DIR="$SAFETY_DIR/failed-extraction"
  mkdir -p "$FAILED_DIR"
  for item in data uploads config; do
    [[ -e "$DEPLOY_DIR/$item" ]] && mv "$DEPLOY_DIR/$item" "$FAILED_DIR/$item"
    [[ -e "$SAFETY_DIR/$item" ]] && mv "$SAFETY_DIR/$item" "$DEPLOY_DIR/$item"
  done
  docker_container_recreate >/dev/null 2>&1 || true
  echo "Restore extraction failed; original data was put back." >&2
  exit 1
fi
chown -R 65532:65532 "$DEPLOY_DIR/data" "$DEPLOY_DIR/uploads" "$DEPLOY_DIR/logs" 2>/dev/null || true
if docker_container_recreate && docker_container_wait_ready 30; then
  echo "Restore completed. Pre-restore data is retained at $SAFETY_DIR"
  exit 0
fi
docker_container_logs 100 >&2 || true
docker_container_remove || true
FAILED_DIR="$SAFETY_DIR/failed-readiness"
mkdir -p "$FAILED_DIR"
for item in data uploads config; do
  [[ -e "$DEPLOY_DIR/$item" ]] && mv "$DEPLOY_DIR/$item" "$FAILED_DIR/$item"
  [[ -e "$SAFETY_DIR/$item" ]] && mv "$SAFETY_DIR/$item" "$DEPLOY_DIR/$item"
done
chown -R 65532:65532 "$DEPLOY_DIR/data" "$DEPLOY_DIR/uploads" "$DEPLOY_DIR/logs" 2>/dev/null || true
docker_container_recreate >/dev/null 2>&1 || true
echo "Restored version failed readiness; the pre-restore data was put back. Failed data is at $FAILED_DIR" >&2
exit 1
