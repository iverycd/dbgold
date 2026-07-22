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
[[ -f "$DEPLOY_DIR/docker-runtime.sh" ]] || { echo "Docker runtime helper is missing." >&2; exit 1; }
source "$DEPLOY_DIR/docker-runtime.sh"
docker_container_assert_replaceable
docker_load_runtime_config
CURRENT_PORT="$DBGOLD_PORT"
if [[ "$CURRENT_PORT" == "$PORT_VALUE" ]]; then
  echo "dbgold is already configured for port $PORT_VALUE"
  exit 0
fi
if docker_port_is_listening "$PORT_VALUE"; then
  echo "Port $PORT_VALUE is already in use." >&2
  exit 1
fi
sed -i "s/^PORT=.*/PORT=$PORT_VALUE/" "$CONFIG_FILE"

if docker_container_recreate && docker_container_wait_ready 30; then
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
  echo "dbgold is ready on 0.0.0.0:$PORT_VALUE using Docker host networking"
  exit 0
fi
docker_container_logs 100 >&2 || true
sed -i "s/^PORT=.*/PORT=$CURRENT_PORT/" "$CONFIG_FILE"
docker_container_recreate >/dev/null 2>&1 || true
echo "dbgold failed its readiness check after changing the port." >&2
exit 1
