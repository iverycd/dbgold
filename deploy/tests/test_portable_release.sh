#!/usr/bin/env bash
# Test the three archive formats without downloading runtimes or redistributing vendor files.
set -Eeuo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
source "$repo/deploy/portable/package.sh"
scratch="$(mktemp -d)"
trap 'rm -rf -- "$scratch"' EXIT
ROOT_DIR="$scratch/fixture repo"
WORK_DIR="$scratch/build"
OUTPUT_DIR="$scratch/out"
VERSION=v1.0.0
GIT_COMMIT=test
BUILD_TIME=2026-11-04T00:00:00Z
LDFLAGS="-s -w"
GO_BIN="${GO_BIN:-go}"
mkdir -p "$ROOT_DIR"/{deploy/portable,frontend/dist,jdbcbridge/java/build,third_party/oscar} "$WORK_DIR" "$OUTPUT_DIR"
printf 'module fixture\n\ngo 1.21\n' > "$ROOT_DIR/go.mod"
printf 'package main\nfunc main() {}\n' > "$ROOT_DIR/main.go"
printf 'start test\n' | tee "$ROOT_DIR/deploy/portable/start.sh" > "$ROOT_DIR/deploy/portable/start.cmd"
printf '<html></html>\n' > "$ROOT_DIR/frontend/dist/index.html"
printf 'bridge fixture' > "$ROOT_DIR/jdbcbridge/java/build/dbgold-oscar-bridge.jar"
for f in README.md LICENSE oscarJDBC8.jar; do printf 'test fixture' > "$ROOT_DIR/third_party/oscar/$f"; done
OSCAR_JDBC_SHA256="$(shasum -a 256 "$ROOT_DIR/third_party/oscar/oscarJDBC8.jar" | awk '{print $1}')"

# Never accept an unverified Java archive, even if the URL resolves successfully.
(curl() { printf corrupt > "${6}"; }; prepare_runtime linux amd64) && {
  echo "Corrupt runtime was accepted" >&2; exit 1
}

if [[ -n "${TEST_TEMURIN_CACHE:-}" ]]; then
  # Exercise real vendor archive extraction and checksum validation using a local download.
  curl() { cp "$TEST_TEMURIN_CACHE/${7##*/}" "$6"; }
else
  # Reuse the cross-compiled fixture executable as a stand-in for the Java launcher.
  prepare_runtime() {
    local target="$1-$2" suffix=""
    if [[ "$1" == windows ]]; then suffix=.exe; fi
    mkdir -p "$WORK_DIR/runtime-$target/bin"
    cp "$WORK_DIR/portable-$target/dbgold/dbgold$suffix" "$WORK_DIR/runtime-$target/bin/java$suffix"
  }
fi

for target in linux-amd64 linux-arm64 windows-amd64; do
  build_portable_package "${target%-*}" "${target#*-}"
  validate_portable_archive "${target%-*}" "${target#*-}"
done
if check_binary_target linux-arm64 "$WORK_DIR/portable-linux-amd64/dbgold/runtime/bin/java"; then
  echo "Wrong Java architecture was accepted" >&2; exit 1
fi

# Simulate a damaged download of the application.
printf '\nmodified\n' >> "$WORK_DIR/portable-linux-amd64/dbgold/web/index.html"
COPYFILE_DISABLE=1 tar -czf "$OUTPUT_DIR/dbgold-$VERSION-portable-linux-amd64.tar.gz" \
  -C "$WORK_DIR/portable-linux-amd64" dbgold
# Invoke in a separate shell so the negative test uses the release entrypoint's errexit semantics.
export WORK_DIR OUTPUT_DIR VERSION OSCAR_JDBC_SHA256
if bash -ec 'source "$1"; validate_portable_archive linux amd64' bash "$repo/deploy/portable/package.sh"; then
  echo "Corrupt archive was accepted" >&2; exit 1
fi
echo "Portable release package checks passed"
