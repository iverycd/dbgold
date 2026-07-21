#!/usr/bin/env bash
set -euo pipefail

SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="/opt/dbgold"
CONFIRMED=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy-dir) DEPLOY_DIR="${2:-}"; shift 2 ;;
    --confirm-no-running-tasks) CONFIRMED=1; shift ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

(( CONFIRMED == 1 )) || { echo "Confirm that no migration task is running with --confirm-no-running-tasks." >&2; exit 1; }
[[ -f "$SOURCE_DIR/image.tar" && -f "$SOURCE_DIR/VERSION" ]] || { echo "Run upgrade.sh from an extracted release package." >&2; exit 1; }
[[ -f "$DEPLOY_DIR/config/dbgold.env" ]] || { echo "No existing deployment found at $DEPLOY_DIR" >&2; exit 1; }
(cd "$SOURCE_DIR" && sha256sum -c manifest.sha256)

cd "$DEPLOY_DIR"
OLD_VERSION="$(awk -F= '$1=="DBGOLD_VERSION" {print $2; exit}' config/dbgold.env)"
NEW_VERSION="$(tr -d '[:space:]' < "$SOURCE_DIR/VERSION")"
BACKUP_FILE=""

rollback_on_error() {
  STATUS=$?
  trap - ERR
  echo "Upgrade failed; restoring $OLD_VERSION." >&2
  if [[ -n "$BACKUP_FILE" && -f "$BACKUP_FILE" ]]; then
    "$SOURCE_DIR/restore.sh" --deploy-dir "$DEPLOY_DIR" --backup "$BACKUP_FILE" --yes || true
  else
    docker compose --env-file config/dbgold.env -f compose.yaml up -d || true
  fi
  exit "$STATUS"
}
trap rollback_on_error ERR

docker compose --env-file config/dbgold.env -f compose.yaml stop
BACKUP_FILE="$($SOURCE_DIR/backup.sh --deploy-dir "$DEPLOY_DIR" --already-stopped)"
docker load -i "$SOURCE_DIR/image.tar"
install -m 0644 "$SOURCE_DIR/compose.yaml" compose.yaml
for script in backup.sh restore.sh upgrade.sh set-port.sh; do install -m 0755 "$SOURCE_DIR/$script" "$script"; done
sed -i "s/^DBGOLD_VERSION=.*/DBGOLD_VERSION=$NEW_VERSION/" config/dbgold.env

if docker compose --env-file config/dbgold.env -f compose.yaml up -d; then
  for _ in $(seq 1 30); do
    if docker compose --env-file config/dbgold.env -f compose.yaml exec -T dbgold /app/dbgold healthcheck >/dev/null 2>&1; then
      trap - ERR
      echo "Upgrade completed: $OLD_VERSION -> $NEW_VERSION"
      echo "Cold backup: $BACKUP_FILE"
      exit 0
    fi
    sleep 1
  done
fi
false
