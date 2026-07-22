#!/usr/bin/env bash
set -euo pipefail

DEPLOY_DIR="/opt/dbgold"
PORT_VALUE=""
ALLOW_CIDR=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT_VALUE="${2:-}"; shift 2 ;;
    --deploy-dir) DEPLOY_DIR="${2:-}"; shift 2 ;;
    --allow-cidr) ALLOW_CIDR="${2:-}"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
if [[ ! "$PORT_VALUE" =~ ^[0-9]+$ ]] || (( PORT_VALUE < 1024 || PORT_VALUE > 65535 )); then
  echo "--port must be an integer between 1024 and 65535." >&2
  exit 1
fi
if [[ -n "$ALLOW_CIDR" && ! "$ALLOW_CIDR" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/([0-9]|[12][0-9]|3[0-2])$ ]]; then
  echo "--allow-cidr must be an IPv4 CIDR." >&2
  exit 1
fi
CONFIG_FILE="$DEPLOY_DIR/config/dbgold.env"
[[ -f "$CONFIG_FILE" ]] || { echo "Configuration not found: $CONFIG_FILE" >&2; exit 1; }
COMPOSE_BIN="$DEPLOY_DIR/bin/docker-compose"
[[ -x "$COMPOSE_BIN" ]] || { echo "Bundled Docker Compose is missing or not executable: $COMPOSE_BIN" >&2; exit 1; }
CURRENT_PORT="$(awk -F= '$1=="PORT" {print $2; exit}' "$CONFIG_FILE")"
if [[ "$CURRENT_PORT" == "$PORT_VALUE" ]]; then
  echo "dbgold is already configured for port $PORT_VALUE"
  exit 0
fi
if command -v ss >/dev/null 2>&1 && ss -ltnH | awk '{print $4}' | grep -Eq "[:.]${PORT_VALUE}$"; then
  echo "Port $PORT_VALUE is already in use." >&2
  exit 1
fi
sed -i "s/^PORT=.*/PORT=$PORT_VALUE/" "$CONFIG_FILE"

cd "$DEPLOY_DIR"
compose() {
  "$COMPOSE_BIN" --env-file config/dbgold.env -f compose.yaml "$@"
}
compose up -d --force-recreate
for _ in $(seq 1 30); do
  if compose exec -T dbgold /app/dbgold healthcheck >/dev/null 2>&1; then
    if [[ -n "$ALLOW_CIDR" ]]; then
      if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
        firewall-cmd --permanent --remove-rich-rule="rule family=ipv4 source address=$ALLOW_CIDR port port=$CURRENT_PORT protocol=tcp accept" >/dev/null 2>&1 || true
        firewall-cmd --permanent --add-rich-rule="rule family=ipv4 source address=$ALLOW_CIDR port port=$PORT_VALUE protocol=tcp accept"
        firewall-cmd --reload
      elif command -v ufw >/dev/null 2>&1 && ufw status | grep -q '^Status: active'; then
        ufw --force delete allow from "$ALLOW_CIDR" to any port "$CURRENT_PORT" proto tcp >/dev/null 2>&1 || true
        ufw allow from "$ALLOW_CIDR" to any port "$PORT_VALUE" proto tcp
      fi
    fi
    echo "dbgold is ready on 0.0.0.0:$PORT_VALUE"
    exit 0
  fi
  sleep 1
done
sed -i "s/^PORT=.*/PORT=$CURRENT_PORT/" "$CONFIG_FILE"
compose up -d --force-recreate >/dev/null 2>&1 || true
echo "dbgold failed its readiness check after changing the port." >&2
exit 1
