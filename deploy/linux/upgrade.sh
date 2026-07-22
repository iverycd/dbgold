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
[[ -f "$SOURCE_DIR/docker-runtime.sh" ]] || { echo "Docker runtime helper is missing." >&2; exit 1; }
source "$SOURCE_DIR/docker-runtime.sh"
docker_container_assert_replaceable

OLD_VERSION="$(awk -F= '$1=="DBGOLD_VERSION" {print $2; exit}' "$DEPLOY_DIR/config/dbgold.env")"
NEW_VERSION="$(tr -d '[:space:]' < "$SOURCE_DIR/VERSION")"
BACKUP_FILE=""

rollback_on_error() {
  STATUS=$?
  trap - ERR
  echo "Upgrade failed; restoring $OLD_VERSION." >&2
  if [[ -n "$BACKUP_FILE" && -f "$BACKUP_FILE" ]]; then
    "$SOURCE_DIR/restore.sh" --deploy-dir "$DEPLOY_DIR" --backup "$BACKUP_FILE" --yes || true
  else
    docker_container_start || true
  fi
  exit "$STATUS"
}
trap rollback_on_error ERR

docker_container_stop
BACKUP_FILE="$("$SOURCE_DIR/backup.sh" --deploy-dir "$DEPLOY_DIR" --already-stopped)"
docker load -i "$SOURCE_DIR/image.tar"
for script in backup.sh restore.sh upgrade.sh set-port.sh docker-runtime.sh; do install -m 0755 "$SOURCE_DIR/$script" "$DEPLOY_DIR/$script"; done
sed -i "s/^DBGOLD_VERSION=.*/DBGOLD_VERSION=$NEW_VERSION/" "$DEPLOY_DIR/config/dbgold.env"

if docker_container_recreate && docker_container_wait_ready 30; then
  docker_cleanup_legacy_compose
  trap - ERR
  echo "Upgrade completed: $OLD_VERSION -> $NEW_VERSION"
  echo "Cold backup: $BACKUP_FILE"
  exit 0
fi
docker_container_logs 100 >&2 || true
false
