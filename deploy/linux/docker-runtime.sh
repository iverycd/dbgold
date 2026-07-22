#!/usr/bin/env bash

DBGOLD_CONTAINER_NAME="dbgold"

docker_config_value() {
  local key="$1"
  local config_file="$DEPLOY_DIR/config/dbgold.env"
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$config_file"
}

docker_load_runtime_config() {
  local config_file="$DEPLOY_DIR/config/dbgold.env"
  [[ -f "$config_file" ]] || { echo "Configuration not found: $config_file" >&2; return 1; }
  DBGOLD_PORT="$(docker_config_value PORT)"
  DBGOLD_VERSION="$(docker_config_value DBGOLD_VERSION)"
  if [[ ! "$DBGOLD_PORT" =~ ^[0-9]+$ ]] || (( DBGOLD_PORT < 1024 || DBGOLD_PORT > 65535 )); then
    echo "Invalid PORT in $config_file: $DBGOLD_PORT" >&2
    return 1
  fi
  if [[ -z "$DBGOLD_VERSION" || ! "$DBGOLD_VERSION" =~ ^[0-9A-Za-z_.-]+$ ]]; then
    echo "Invalid DBGOLD_VERSION in $config_file: $DBGOLD_VERSION" >&2
    return 1
  fi
  DBGOLD_IMAGE="dbgold:$DBGOLD_VERSION"
}

docker_container_exists() {
  docker container inspect "$DBGOLD_CONTAINER_NAME" >/dev/null 2>&1
}

docker_container_is_running() {
  [[ "$(docker inspect --format '{{.State.Running}}' "$DBGOLD_CONTAINER_NAME" 2>/dev/null)" == "true" ]]
}

docker_container_assert_replaceable() {
  docker_container_exists || return 0
  local managed_label compose_service image
  managed_label="$(docker inspect --format '{{index .Config.Labels "com.dbgold.managed"}}' "$DBGOLD_CONTAINER_NAME" 2>/dev/null || true)"
  compose_service="$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$DBGOLD_CONTAINER_NAME" 2>/dev/null || true)"
  image="$(docker inspect --format '{{.Config.Image}}' "$DBGOLD_CONTAINER_NAME" 2>/dev/null || true)"
  if [[ "$managed_label" == "true" || "$compose_service" == "dbgold" || "$image" == dbgold:* ]]; then
    return 0
  fi
  echo "A container named $DBGOLD_CONTAINER_NAME already exists but is not managed by dbgold; it will not be modified." >&2
  return 1
}

docker_container_stop() {
  if docker_container_exists && docker_container_is_running; then
    docker stop --time 20 "$DBGOLD_CONTAINER_NAME" >/dev/null
  fi
}

docker_container_start() {
  if docker_container_exists && ! docker_container_is_running; then
    docker start "$DBGOLD_CONTAINER_NAME" >/dev/null
  fi
}

docker_container_remove() {
  docker_container_exists || return 0
  docker_container_assert_replaceable
  docker_container_stop
  docker rm "$DBGOLD_CONTAINER_NAME" >/dev/null
}

docker_container_create() {
  docker_load_runtime_config
  docker run --detach \
    --name "$DBGOLD_CONTAINER_NAME" \
    --label "com.dbgold.managed=true" \
    --label "com.dbgold.deploy-dir=$DEPLOY_DIR" \
    --restart unless-stopped \
    --network host \
    --env-file "$DEPLOY_DIR/config/dbgold.env" \
    --volume "$DEPLOY_DIR/data:/app/data:Z" \
    --volume "$DEPLOY_DIR/uploads:/app/uploads:Z" \
    --volume "$DEPLOY_DIR/logs:/app/logs:Z" \
    --read-only \
    --tmpfs /tmp:size=128m,mode=1777 \
    --stop-timeout 20 \
    --security-opt no-new-privileges \
    --cap-drop ALL \
    "$DBGOLD_IMAGE"
}

docker_container_recreate() {
  docker_container_assert_replaceable
  docker_container_remove
  docker_container_create
}

docker_container_healthcheck() {
  docker exec "$DBGOLD_CONTAINER_NAME" /app/dbgold healthcheck >/dev/null 2>&1
}

docker_container_wait_ready() {
  local attempts="${1:-30}"
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if docker_container_healthcheck; then
      return 0
    fi
    sleep 1
  done
  return 1
}

docker_container_logs() {
  docker logs --tail "${1:-100}" "$DBGOLD_CONTAINER_NAME"
}

docker_port_is_listening() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -ltnH | awk '{print $4}' | grep -Eq "[:.]${port}$"
    return
  fi
  if command -v netstat >/dev/null 2>&1; then
    netstat -ltn 2>/dev/null | awk 'NR > 2 {print $4}' | grep -Eq "[:.]${port}$"
    return
  fi
  echo "Neither ss nor netstat is available; TCP port $port could not be checked before startup." >&2
  return 1
}

docker_cleanup_legacy_compose() {
  rm -f "$DEPLOY_DIR/compose.yaml" "$DEPLOY_DIR/bin/docker-compose"
  rmdir "$DEPLOY_DIR/bin" 2>/dev/null || true
}
