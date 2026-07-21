#!/usr/bin/env bash
set -euo pipefail

DEPLOY_DIR="/opt/dbgold"
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
[[ -f "$DEPLOY_DIR/compose.yaml" ]] || { echo "Invalid deployment directory: $DEPLOY_DIR" >&2; exit 1; }
if tar -tzf "$BACKUP_FILE" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then
  echo "Backup contains an unsafe path and will not be restored." >&2
  exit 1
fi

cd "$DEPLOY_DIR"
if [[ -f config/dbgold.env ]]; then
  docker compose --env-file config/dbgold.env -f compose.yaml stop || true
fi
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
  echo "Restore extraction failed; original data was put back." >&2
  exit 1
fi
chown -R 65532:65532 "$DEPLOY_DIR/data" "$DEPLOY_DIR/uploads" "$DEPLOY_DIR/logs" 2>/dev/null || true
docker compose --env-file config/dbgold.env -f compose.yaml up -d
for _ in $(seq 1 30); do
  if docker compose --env-file config/dbgold.env -f compose.yaml exec -T dbgold /app/dbgold healthcheck >/dev/null 2>&1; then
    echo "Restore completed. Pre-restore data is retained at $SAFETY_DIR"
    exit 0
  fi
  sleep 1
done
docker compose --env-file config/dbgold.env -f compose.yaml stop || true
FAILED_DIR="$SAFETY_DIR/failed-readiness"
mkdir -p "$FAILED_DIR"
for item in data uploads config; do
  [[ -e "$DEPLOY_DIR/$item" ]] && mv "$DEPLOY_DIR/$item" "$FAILED_DIR/$item"
  [[ -e "$SAFETY_DIR/$item" ]] && mv "$SAFETY_DIR/$item" "$DEPLOY_DIR/$item"
done
chown -R 65532:65532 "$DEPLOY_DIR/data" "$DEPLOY_DIR/uploads" "$DEPLOY_DIR/logs" 2>/dev/null || true
docker compose --env-file config/dbgold.env -f compose.yaml up -d || true
echo "Restored version failed readiness; the pre-restore data was put back. Failed data is at $FAILED_DIR" >&2
exit 1
