#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="${1:-}"
if [[ ! "$VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "Usage: $0 <version>, for example $0 v1.2.3" >&2
  exit 2
fi
for command_name in docker npm zip tar shasum; do
  command -v "$command_name" >/dev/null 2>&1 || { echo "Missing required command: $command_name" >&2; exit 1; }
done
GO_BIN="${GO_BIN:-$(command -v go || true)}"
[[ -n "$GO_BIN" && -x "$GO_BIN" ]] || { echo "Set GO_BIN to a Go 1.25.5 executable." >&2; exit 1; }
docker buildx version >/dev/null 2>&1 || { echo "Docker Buildx is required." >&2; exit 1; }
BUILDER_NAME="${BUILDX_BUILDER:-dbgold-release}"
if ! docker buildx inspect "$BUILDER_NAME" >/dev/null 2>&1; then
  docker buildx create --name "$BUILDER_NAME" --driver docker-container >/dev/null
fi
docker buildx inspect --bootstrap "$BUILDER_NAME" >/dev/null
if [[ "${ALLOW_DIRTY:-0}" != "1" ]] && [[ -n "$(git -C "$ROOT_DIR" status --porcelain --untracked-files=no)" ]]; then
  echo "The Git worktree has tracked changes. Commit them first or set ALLOW_DIRTY=1 for a development build." >&2
  exit 1
fi

GIT_COMMIT="$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
WORK_DIR="$(mktemp -d)"
cleanup() { rm -rf "$WORK_DIR"; }
trap cleanup EXIT
FINAL_OUTPUT_DIR="$ROOT_DIR/release/$VERSION"
if [[ -e "$FINAL_OUTPUT_DIR" ]]; then
  echo "Release output already exists: $FINAL_OUTPUT_DIR" >&2
  exit 1
fi
OUTPUT_DIR="$WORK_DIR/output"
mkdir -p "$OUTPUT_DIR"

cd "$ROOT_DIR/frontend"
npm ci
npm audit --omit=dev --audit-level=high
npm run build
cd "$ROOT_DIR"
"$GO_BIN" vet ./...
"$GO_BIN" test ./...

LDFLAGS="-s -w -X dbgold/api/handler.Version=$VERSION -X dbgold/api/handler.GitCommit=$GIT_COMMIT -X dbgold/api/handler.BuildTime=$BUILD_TIME"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$GO_BIN" build -trimpath -ldflags "$LDFLAGS" -o "$WORK_DIR/dbgold.exe" .

for ARCH in amd64 arm64; do
  PACKAGE_DIR="$WORK_DIR/linux-$ARCH"
  mkdir -p "$PACKAGE_DIR"
  docker buildx build \
    --builder "$BUILDER_NAME" \
    --platform "linux/$ARCH" \
    --build-arg "VERSION=$VERSION" \
    --build-arg "GIT_COMMIT=$GIT_COMMIT" \
    --build-arg "BUILD_TIME=$BUILD_TIME" \
    --tag "dbgold:$VERSION" \
    --output "type=docker,dest=$PACKAGE_DIR/image.tar" \
    "$ROOT_DIR"
  cp "$ROOT_DIR/README.md" "$PACKAGE_DIR/README.md"
  cp "$ROOT_DIR/deploy/linux/compose.yaml" "$ROOT_DIR/deploy/linux/dbgold.env.example" "$ROOT_DIR/deploy/linux/"*.sh "$PACKAGE_DIR/"
  printf '%s\n' "$VERSION" > "$PACKAGE_DIR/VERSION"
  printf '%s\n' "$ARCH" > "$PACKAGE_DIR/ARCH"
  printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "os": "linux",\n  "architecture": "%s"\n}\n' \
    "$VERSION" "$GIT_COMMIT" "$BUILD_TIME" "$ARCH" > "$PACKAGE_DIR/release-manifest.json"
  chmod +x "$PACKAGE_DIR/"*.sh
  (cd "$PACKAGE_DIR" && for file in ARCH VERSION README.md compose.yaml dbgold.env.example image.tar release-manifest.json *.sh; do shasum -a 256 "$file"; done > manifest.sha256)
  tar -C "$PACKAGE_DIR" -czf "$OUTPUT_DIR/dbgold-$VERSION-linux-$ARCH.tar.gz" .
done

WINDOWS_DIR="$WORK_DIR/windows-amd64"
mkdir -p "$WINDOWS_DIR/web"
cp "$WORK_DIR/dbgold.exe" "$WINDOWS_DIR/dbgold.exe"
cp -R "$ROOT_DIR/frontend/dist/." "$WINDOWS_DIR/web/"
cp "$ROOT_DIR/deploy/windows/dbgold.env.example" "$ROOT_DIR/deploy/windows/"*.ps1 "$WINDOWS_DIR/"
cp "$ROOT_DIR/README.md" "$WINDOWS_DIR/README.md"
printf '%s\n' "$VERSION" > "$WINDOWS_DIR/VERSION"
printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "os": "windows",\n  "architecture": "amd64"\n}\n' \
  "$VERSION" "$GIT_COMMIT" "$BUILD_TIME" > "$WINDOWS_DIR/release-manifest.json"
(cd "$WINDOWS_DIR" && find . -type f ! -name manifest.sha256 -print | LC_ALL=C sort | while read -r file; do shasum -a 256 "${file#./}"; done > manifest.sha256)
(cd "$WINDOWS_DIR" && zip -qr "$OUTPUT_DIR/dbgold-$VERSION-windows-amd64.zip" .)

(cd "$OUTPUT_DIR" && shasum -a 256 dbgold-* > SHA256SUMS)
printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "artifacts": ["linux/amd64", "linux/arm64", "windows/amd64"]\n}\n' \
  "$VERSION" "$GIT_COMMIT" "$BUILD_TIME" > "$OUTPUT_DIR/release-manifest.json"
mkdir -p "$ROOT_DIR/release"
mv "$OUTPUT_DIR" "$FINAL_OUTPUT_DIR"
echo "Release artifacts are available in $FINAL_OUTPUT_DIR"
