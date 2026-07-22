#!/usr/bin/env bash
set -euo pipefail

LINUX_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
cleanup() { rm -rf "$TEST_DIR"; }
trap cleanup EXIT

mkdir -p "$TEST_DIR/bin" "$TEST_DIR/deploy/config" "$TEST_DIR/deploy/data" "$TEST_DIR/deploy/uploads" "$TEST_DIR/deploy/logs"
cat > "$TEST_DIR/bin/docker" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "run" ]]; then
  printf '%s\n' "$@" > "$DOCKER_TEST_LOG"
  exit 0
fi
if [[ "$1" == "container" && "$2" == "inspect" ]]; then
  [[ "${FAKE_DOCKER_EXISTS:-0}" == "1" ]]
  exit
fi
if [[ "$1" == "inspect" && "$2" == "--format" ]]; then
  case "$3" in
    *com.dbgold.managed*) printf '%s\n' "${FAKE_MANAGED_LABEL:-}" ;;
    *com.docker.compose.service*) printf '%s\n' "${FAKE_COMPOSE_SERVICE:-}" ;;
    *Config.Image*) printf '%s\n' "${FAKE_IMAGE:-}" ;;
    *State.Running*) printf '%s\n' "${FAKE_RUNNING:-false}" ;;
  esac
  exit 0
fi
exit 0
EOF
chmod +x "$TEST_DIR/bin/docker"
cat > "$TEST_DIR/deploy/config/dbgold.env" <<'EOF'
DBGOLD_VERSION=v1.2.3
PORT=18089
EOF

export PATH="$TEST_DIR/bin:$PATH"
export DOCKER_TEST_LOG="$TEST_DIR/docker.log"
DEPLOY_DIR="$TEST_DIR/deploy"
source "$LINUX_DIR/docker-runtime.sh"
docker_container_create

grep -Fx -- '--network' "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- 'host' "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- '--read-only' "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- '--cap-drop' "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- 'ALL' "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- '--stop-timeout' "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- '20' "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- "$DEPLOY_DIR/data:/app/data:Z" "$DOCKER_TEST_LOG" >/dev/null
grep -Fx -- 'dbgold:v1.2.3' "$DOCKER_TEST_LOG" >/dev/null
if grep -Eq '^-p$|^--publish$' "$DOCKER_TEST_LOG"; then
  echo "docker run unexpectedly contains a published-port option" >&2
  exit 1
fi

export FAKE_DOCKER_EXISTS=1
export FAKE_IMAGE=nginx:latest
if docker_container_assert_replaceable 2>/dev/null; then
  echo "an unrelated container named dbgold was accepted" >&2
  exit 1
fi
export FAKE_IMAGE=dbgold:v1.0.0
docker_container_assert_replaceable

echo "docker-runtime tests passed"
