#!/usr/bin/env bash
# Verify all four native archives without downloading runtimes or redistributing vendor files.
set -Eeuo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
source "$repo/release.sh"
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
cp -R "$repo/deploy/windows" "$ROOT_DIR/deploy/"
printf 'Release test\n' > "$ROOT_DIR/README.md"
printf 'module fixture\n\ngo 1.21\n' > "$ROOT_DIR/go.mod"
printf 'package main\nfunc main() {}\n' > "$ROOT_DIR/main.go"
printf 'start test\n' | tee "$ROOT_DIR/deploy/portable/start.sh" > "$ROOT_DIR/deploy/portable/start.cmd"
printf '<html></html>\n' > "$ROOT_DIR/frontend/dist/index.html"
printf 'bridge fixture' > "$ROOT_DIR/jdbcbridge/java/build/dbgold-oscar-bridge.jar"
for f in README.md LICENSE oscarJDBC8.jar; do printf 'test fixture' > "$ROOT_DIR/third_party/oscar/$f"; done
OSCAR_JDBC_SHA256="$(shasum -a 256 "$ROOT_DIR/third_party/oscar/oscarJDBC8.jar" | awk '{print $1}')"

curl() { echo 'Unexpected release download' >&2; return 1; }

for target in linux-amd64 linux-arm64 windows-amd64; do
  build_portable_package "${target%-*}" "${target#*-}"
  validate_portable_archive "${target%-*}" "${target#*-}"
done
if check_binary_target linux-arm64 "$WORK_DIR/portable-linux-amd64/dbgold/dbgold"; then
  echo "Wrong application architecture was accepted" >&2; exit 1
fi
build_windows
package_windows
validate_windows_archive

# Reject an accidental return of the bundled JVM in the Windows service ZIP.
name="dbgold-$VERSION-windows-amd64"
mkdir -p "$WORK_DIR/$name/runtime/bin"
printf 'unexpected runtime' > "$WORK_DIR/$name/runtime/bin/java.exe"
(cd "$WORK_DIR" && zip -qr "$OUTPUT_DIR/$name.zip" "$name/runtime")
if validate_windows_archive; then
  echo "Windows archive containing a runtime was accepted" >&2; exit 1
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
echo "Portable and Windows service release package checks passed"
