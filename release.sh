#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="${1:-}"
BUILDER_NAME="${BUILDX_BUILDER:-dbgold-release}"
GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}"
KEEP_WORK_DIR="${KEEP_WORK_DIR:-0}"
ALLOW_DIRTY="${ALLOW_DIRTY:-0}"
MIN_FREE_KB=$((4 * 1024 * 1024))
CURRENT_STAGE="initialization"
ERROR_REPORTED=0
WORK_DIR=""

log() {
  printf '[%s] %s\n' "$(date '+%H:%M:%S')" "$*"
}

on_error() {
  local line_no="$1"
  local failed_command="$2"
  local exit_code="$3"
  if [[ "$ERROR_REPORTED" == "0" ]]; then
    ERROR_REPORTED=1
    printf '\nRelease failed during stage "%s" (line %s, exit %s).\nCommand: %s\n' \
      "$CURRENT_STAGE" "$line_no" "$exit_code" "$failed_command" >&2
  fi
  return "$exit_code"
}

cleanup() {
  local exit_code=$?
  if [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
    if [[ "$exit_code" -ne 0 && "$KEEP_WORK_DIR" == "1" ]]; then
      printf 'Temporary build files were kept at: %s\n' "$WORK_DIR" >&2
    else
      rm -rf -- "$WORK_DIR"
    fi
  fi
}

trap 'on_error "$LINENO" "$BASH_COMMAND" "$?"' ERR
trap cleanup EXIT

run_stage() {
  local stage_name="$1"
  shift
  local started_at=$SECONDS
  CURRENT_STAGE="$stage_name"
  log "==> $stage_name"
  "$@"
  log "<== $stage_name completed in $((SECONDS - started_at))s"
}

require_command() {
  local command_name="$1"
  command -v "$command_name" >/dev/null 2>&1 || {
    printf 'Missing required command: %s\n' "$command_name" >&2
    return 1
  }
}

preflight() {
  if [[ ! "$VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
    printf 'Usage: %s <version>, for example %s v1.2.3\n' "$0" "$0" >&2
    return 2
  fi
  if [[ "$KEEP_WORK_DIR" != "0" && "$KEEP_WORK_DIR" != "1" ]]; then
    printf 'KEEP_WORK_DIR must be 0 or 1.\n' >&2
    return 1
  fi
  if [[ "$ALLOW_DIRTY" != "0" && "$ALLOW_DIRTY" != "1" ]]; then
    printf 'ALLOW_DIRTY must be 0 or 1.\n' >&2
    return 1
  fi

  local command_name
  for command_name in docker npm node zip unzip tar shasum git find sort awk sed file; do
    require_command "$command_name"
  done

  GO_BIN="${GO_BIN:-$(command -v go || true)}"
  [[ -n "$GO_BIN" && -x "$GO_BIN" ]] || {
    printf 'Set GO_BIN to a Go 1.25.5 executable.\n' >&2
    return 1
  }
  local go_version
  go_version="$("$GO_BIN" version | awk '{print $3}')"
  if [[ "$go_version" != "go1.25.5" ]]; then
    printf 'Go 1.25.5 is required, but %s reports %s.\n' "$GO_BIN" "$go_version" >&2
    return 1
  fi

  docker info >/dev/null 2>&1 || {
    printf 'Docker Engine is not running or is not accessible.\n' >&2
    return 1
  }
  docker buildx version >/dev/null 2>&1 || {
    printf 'Docker Buildx is required.\n' >&2
    return 1
  }

  FINAL_OUTPUT_DIR="$ROOT_DIR/release/$VERSION"
  if [[ -e "$FINAL_OUTPUT_DIR" ]]; then
    printf 'Release output already exists: %s\n' "$FINAL_OUTPUT_DIR" >&2
    return 1
  fi
  if [[ "$ALLOW_DIRTY" != "1" ]] && [[ -n "$(git -C "$ROOT_DIR" status --porcelain --untracked-files=no)" ]]; then
    printf 'The Git worktree has tracked changes. Commit them first or set ALLOW_DIRTY=1 for a development build.\n' >&2
    return 1
  fi

  local available_kb
  available_kb="$(df -Pk "$ROOT_DIR" | awk 'NR == 2 {print $4}')"
  if [[ ! "$available_kb" =~ ^[0-9]+$ || "$available_kb" -lt "$MIN_FREE_KB" ]]; then
    printf 'At least 4 GiB of free disk space is required under %s.\n' "$ROOT_DIR" >&2
    return 1
  fi

  if ! docker buildx inspect "$BUILDER_NAME" >/dev/null 2>&1; then
    log "Creating Buildx builder: $BUILDER_NAME"
    docker buildx create --name "$BUILDER_NAME" --driver docker-container >/dev/null
  fi
  docker buildx inspect --bootstrap "$BUILDER_NAME" >/dev/null

  log "Go: $go_version"
  log "Node: $(node --version); npm: $(npm --version)"
  log "Buildx builder: $BUILDER_NAME"
  log "GOPROXY: $GOPROXY"
}

prepare_workspace() {
  WORK_DIR="$(mktemp -d)"
  OUTPUT_DIR="$WORK_DIR/output"
  mkdir -p "$OUTPUT_DIR"

  GIT_COMMIT="$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  if [[ -n "$(git -C "$ROOT_DIR" status --porcelain --untracked-files=no)" ]]; then
    GIT_COMMIT="${GIT_COMMIT}-dirty"
  fi
  BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  LDFLAGS="-s -w -X dbgold/api/handler.Version=$VERSION -X dbgold/api/handler.GitCommit=$GIT_COMMIT -X dbgold/api/handler.BuildTime=$BUILD_TIME"
}

build_frontend() {
  cd "$ROOT_DIR/frontend"
  npm ci --audit=false
  npm audit --omit=dev --audit-level=high
  npm run build
}

validate_go() {
  cd "$ROOT_DIR"
  "$GO_BIN" vet ./...
  "$GO_BIN" test ./...
}

build_windows() {
  cd "$ROOT_DIR"
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$GO_BIN" build \
    -trimpath -ldflags "$LDFLAGS" -o "$WORK_DIR/dbgold.exe" .
}

build_linux_package() {
  local arch="$1"
  local package_name="dbgold-$VERSION-linux-$arch"
  local package_dir="$WORK_DIR/$package_name"
  mkdir -p "$package_dir"

  docker buildx build \
    --builder "$BUILDER_NAME" \
    --progress=plain \
    --platform "linux/$arch" \
    --build-arg "VERSION=$VERSION" \
    --build-arg "GIT_COMMIT=$GIT_COMMIT" \
    --build-arg "BUILD_TIME=$BUILD_TIME" \
    --build-arg "GOPROXY=$GOPROXY" \
    --tag "dbgold:$VERSION" \
    --output "type=docker,dest=$package_dir/image.tar" \
    "$ROOT_DIR"

  cp "$ROOT_DIR/README.md" "$package_dir/README.md"
  cp "$ROOT_DIR/deploy/linux/dbgold.env.example" "$ROOT_DIR/deploy/linux/"*.sh "$package_dir/"
  printf '%s\n' "$VERSION" > "$package_dir/VERSION"
  printf '%s\n' "$arch" > "$package_dir/ARCH"
  printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "os": "linux",\n  "architecture": "%s"\n}\n' \
    "$VERSION" "$GIT_COMMIT" "$BUILD_TIME" "$arch" > "$package_dir/release-manifest.json"
  chmod +x "$package_dir/"*.sh
  (
    cd "$package_dir"
    find . -type f ! -name manifest.sha256 -print | LC_ALL=C sort | while read -r file; do
      shasum -a 256 "${file#./}"
    done > manifest.sha256
  )
  tar -C "$WORK_DIR" -czf "$OUTPUT_DIR/$package_name.tar.gz" "$package_name"
}

package_windows() {
  local package_name="dbgold-$VERSION-windows-amd64"
  local package_dir="$WORK_DIR/$package_name"
  mkdir -p "$package_dir/web"
  cp "$WORK_DIR/dbgold.exe" "$package_dir/dbgold.exe"
  cp -R "$ROOT_DIR/frontend/dist/." "$package_dir/web/"
  cp "$ROOT_DIR/deploy/windows/dbgold.env.example" "$ROOT_DIR/deploy/windows/"*.ps1 "$package_dir/"
  cp "$ROOT_DIR/README.md" "$package_dir/README.md"
  printf '%s\n' "$VERSION" > "$package_dir/VERSION"
  printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "os": "windows",\n  "architecture": "amd64"\n}\n' \
    "$VERSION" "$GIT_COMMIT" "$BUILD_TIME" > "$package_dir/release-manifest.json"
  (
    cd "$package_dir"
    find . -type f ! -name manifest.sha256 -print | LC_ALL=C sort | while read -r file; do
      shasum -a 256 "${file#./}"
    done > manifest.sha256
  )
  (cd "$WORK_DIR" && zip -qr "$OUTPUT_DIR/$package_name.zip" "$package_name")
}

write_release_metadata() {
  (
    cd "$OUTPUT_DIR"
    shasum -a 256 dbgold-* > SHA256SUMS
  )
  printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "artifacts": ["linux/amd64", "linux/arm64", "windows/amd64"]\n}\n' \
    "$VERSION" "$GIT_COMMIT" "$BUILD_TIME" > "$OUTPUT_DIR/release-manifest.json"
}

validate_artifacts() {
  local arch package_name archive image_manifest config_path image_config file_output
  for arch in amd64 arm64; do
    package_name="dbgold-$VERSION-linux-$arch"
    archive="$OUTPUT_DIR/$package_name.tar.gz"
    (cd "$WORK_DIR/$package_name" && shasum -a 256 -c manifest.sha256)
    tar -tzf "$archive" >/dev/null
    tar -tzf "$archive" | awk -v expected="$package_name/image.tar" '$0 == expected { found = 1 } END { exit !found }'
    image_manifest="$(tar -xOzf "$archive" "$package_name/image.tar" | tar -xOf - manifest.json)"
    config_path="$(printf '%s\n' "$image_manifest" | sed -n 's/.*"Config":"\([^"]*\)".*/\1/p')"
    [[ -n "$config_path" ]]
    image_config="$(tar -xOzf "$archive" "$package_name/image.tar" | tar -xOf - "$config_path")"
    [[ "$image_config" == *"\"architecture\":\"$arch\""* ]]
  done

  package_name="dbgold-$VERSION-windows-amd64"
  archive="$OUTPUT_DIR/$package_name.zip"
  (cd "$WORK_DIR/$package_name" && shasum -a 256 -c manifest.sha256)
  unzip -tq "$archive" >/dev/null
  unzip -Z1 "$archive" | awk -v expected="$package_name/dbgold.exe" '$0 == expected { found = 1 } END { exit !found }'
  file_output="$(file "$WORK_DIR/$package_name/dbgold.exe")"
  [[ "$file_output" == *"PE32+"* && "$file_output" == *"x86-64"* ]]

  (cd "$OUTPUT_DIR" && shasum -a 256 -c SHA256SUMS)
  [[ -s "$OUTPUT_DIR/release-manifest.json" ]]
}

publish_artifacts() {
  mkdir -p "$ROOT_DIR/release"
  mv "$OUTPUT_DIR" "$FINAL_OUTPUT_DIR"
}

run_stage "Preflight checks" preflight
run_stage "Prepare temporary workspace" prepare_workspace
run_stage "Frontend install, security audit, and build" build_frontend
run_stage "Go vet and tests" validate_go
run_stage "Windows amd64 build" build_windows
run_stage "Linux amd64 image and package" build_linux_package amd64
run_stage "Linux arm64 image and package" build_linux_package arm64
run_stage "Windows amd64 package" package_windows
run_stage "Release metadata" write_release_metadata
run_stage "Artifact integrity checks" validate_artifacts
run_stage "Publish artifacts" publish_artifacts

CURRENT_STAGE="complete"
log "Release artifacts are available in $FINAL_OUTPUT_DIR"
