#!/usr/bin/env bash
set -euo pipefail

SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="/opt/dbgold"
PORT_VALUE="18089"
ALLOW_CIDR=""

usage() {
  echo "Usage: $0 [--port 18089] [--deploy-dir /opt/dbgold] [--allow-cidr 192.168.1.0/24]"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT_VALUE="${2:-}"; shift 2 ;;
    --deploy-dir) DEPLOY_DIR="${2:-}"; shift 2 ;;
    --allow-cidr) ALLOW_CIDR="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "Run this installer as root." >&2
  exit 1
fi
if [[ ! "$PORT_VALUE" =~ ^[0-9]+$ ]] || (( PORT_VALUE < 1024 || PORT_VALUE > 65535 )); then
  echo "Port must be an integer between 1024 and 65535." >&2
  exit 1
fi
if [[ -n "$ALLOW_CIDR" && ! "$ALLOW_CIDR" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/([0-9]|[12][0-9]|3[0-2])$ ]]; then
  echo "--allow-cidr must be an IPv4 CIDR, for example 192.168.1.0/24." >&2
  exit 1
fi
for command_name in docker sha256sum tar; do
  command -v "$command_name" >/dev/null 2>&1 || { echo "Missing required command: $command_name" >&2; exit 1; }
done

case "$(uname -m)" in
  x86_64|amd64) DETECTED_ARCH="amd64" ;;
  aarch64|arm64) DETECTED_ARCH="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
PACKAGE_ARCH="$(tr -d '[:space:]' < "$SOURCE_DIR/ARCH")"
if [[ "$DETECTED_ARCH" != "$PACKAGE_ARCH" ]]; then
  echo "Package architecture is $PACKAGE_ARCH, but this server is $DETECTED_ARCH." >&2
  exit 1
fi
SOURCE_COMPOSE="$SOURCE_DIR/bin/docker-compose"
if [[ ! -f "$SOURCE_COMPOSE" ]]; then
  echo "Bundled Docker Compose is missing: $SOURCE_COMPOSE" >&2
  exit 1
fi
if [[ ! -x "$SOURCE_COMPOSE" ]]; then
  echo "Bundled Docker Compose is not executable: $SOURCE_COMPOSE" >&2
  exit 1
fi
(cd "$SOURCE_DIR" && sha256sum -c manifest.sha256)
if ! COMPOSE_VERSION_OUTPUT="$("$SOURCE_COMPOSE" version 2>&1)"; then
  echo "Bundled Docker Compose cannot run on this server. Verify that the release package matches $DETECTED_ARCH." >&2
  echo "$COMPOSE_VERSION_OUTPUT" >&2
  exit 1
fi
if ! DOCKER_INFO_OUTPUT="$(docker info 2>&1)"; then
  if grep -qiE 'permission denied|access denied' <<<"$DOCKER_INFO_OUTPUT"; then
    echo "Docker is installed, but the current user cannot access the Docker daemon." >&2
  else
    echo "Docker is installed, but the Docker daemon is not reachable. Start Docker and run the installer again." >&2
  fi
  echo "$DOCKER_INFO_OUTPUT" >&2
  exit 1
fi

if command -v ss >/dev/null 2>&1 && ss -ltnH | awk '{print $4}' | grep -Eq "[:.]${PORT_VALUE}$"; then
  echo "Port $PORT_VALUE is already in use. Choose another port with --port." >&2
  exit 1
fi

IMAGE_BYTES="$(stat -c %s "$SOURCE_DIR/image.tar")"
AVAILABLE_KB="$(df -Pk "$(dirname "$DEPLOY_DIR")" | awk 'NR==2 {print $4}')"
REQUIRED_KB="$((IMAGE_BYTES / 1024 + 1048576))"
if (( AVAILABLE_KB < REQUIRED_KB )); then
  echo "Insufficient disk space: at least $REQUIRED_KB KiB is required." >&2
  exit 1
fi

VERSION="$(tr -d '[:space:]' < "$SOURCE_DIR/VERSION")"
mkdir -p "$DEPLOY_DIR" "$DEPLOY_DIR/bin" "$DEPLOY_DIR/data" "$DEPLOY_DIR/uploads" "$DEPLOY_DIR/logs" "$DEPLOY_DIR/config" "$DEPLOY_DIR/backups"
install -m 0644 "$SOURCE_DIR/compose.yaml" "$DEPLOY_DIR/compose.yaml"
install -m 0755 "$SOURCE_COMPOSE" "$DEPLOY_DIR/bin/docker-compose"
for script in backup.sh restore.sh upgrade.sh set-port.sh; do
  install -m 0755 "$SOURCE_DIR/$script" "$DEPLOY_DIR/$script"
done

random_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$1"
  else
    od -An -N "$1" -tx1 /dev/urandom | tr -d ' \n'
  fi
}

CONFIG_FILE="$DEPLOY_DIR/config/dbgold.env"
if [[ ! -f "$CONFIG_FILE" ]]; then
  JWT_SECRET="$(random_hex 32)"
  ADMIN_PASS="Db!$(random_hex 10)"
  sed \
    -e "s/__SET_BY_INSTALLER__/$VERSION/" \
    -e "s/^JWT_SECRET=.*/JWT_SECRET=$JWT_SECRET/" \
    "$SOURCE_DIR/dbgold.env.example" > "$CONFIG_FILE"
  sed -i "s/^ADMIN_PASS=.*/ADMIN_PASS=$ADMIN_PASS/; s/^PORT=.*/PORT=$PORT_VALUE/" "$CONFIG_FILE"
  chmod 0600 "$CONFIG_FILE"
  echo "Initial administrator: admin"
  echo "Initial administrator password: $ADMIN_PASS"
  echo "Store this password securely; it will not be printed again."
else
  CURRENT_PORT="$(awk -F= '$1=="PORT" {print $2; exit}' "$CONFIG_FILE")"
  if [[ -n "$CURRENT_PORT" && "$CURRENT_PORT" != "$PORT_VALUE" ]]; then
    echo "Keeping existing configured port $CURRENT_PORT (the --port value is only used on first install)."
  fi
  sed -i "s/^DBGOLD_VERSION=.*/DBGOLD_VERSION=$VERSION/" "$CONFIG_FILE"
fi

chown -R 65532:65532 "$DEPLOY_DIR/data" "$DEPLOY_DIR/uploads" "$DEPLOY_DIR/logs"
docker load -i "$SOURCE_DIR/image.tar"

if [[ -n "$ALLOW_CIDR" ]]; then
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd --permanent --add-rich-rule="rule family=ipv4 source address=$ALLOW_CIDR port port=$PORT_VALUE protocol=tcp accept"
    firewall-cmd --reload
  elif command -v ufw >/dev/null 2>&1 && ufw status | grep -q '^Status: active'; then
    ufw allow from "$ALLOW_CIDR" to any port "$PORT_VALUE" proto tcp
  else
    echo "No active firewalld/UFW detected; verify the host firewall allows $ALLOW_CIDR to TCP $PORT_VALUE."
  fi
else
  echo "No firewall rule was added. Use --allow-cidr on first install or configure TCP $PORT_VALUE manually."
fi

cd "$DEPLOY_DIR"
COMPOSE_BIN="$DEPLOY_DIR/bin/docker-compose"
compose() {
  "$COMPOSE_BIN" --env-file config/dbgold.env -f compose.yaml "$@"
}
compose up -d
for _ in $(seq 1 30); do
  if compose exec -T dbgold /app/dbgold healthcheck >/dev/null 2>&1; then
    ACTIVE_PORT="$(awk -F= '$1=="PORT" {print $2; exit}' config/dbgold.env)"
    echo "dbgold $VERSION is ready on 0.0.0.0:$ACTIVE_PORT"
    exit 0
  fi
  sleep 1
done
compose logs --tail=100 dbgold >&2 || true
echo "dbgold failed its readiness check." >&2
exit 1
