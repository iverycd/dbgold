#!/usr/bin/env bash
# Verify the portable archives and Windows service ZIP using fixture files.
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
PORTABLE=1
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

for target in "${PORTABLE_TARGETS[@]}"; do
  build_portable_package "${target%-*}" "${target#*-}"
done
write_release_metadata
validate_portable_artifacts
node - "$OUTPUT_DIR" "$WORK_DIR" <<'JS'
const { deepStrictEqual, strictEqual } = require('node:assert');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const [outputDir, workDir] = process.argv.slice(2);
const readManifest = (dir) => JSON.parse(readFileSync(join(dir, 'release-manifest.json'), 'utf8'));
const release = readManifest(outputDir);
const expectedTargets = ['linux/amd64', 'linux/arm64', 'windows/amd64', 'darwin/amd64', 'darwin/arm64'];
strictEqual(release.edition, 'portable');
deepStrictEqual(release.artifacts, expectedTargets);
for (const target of expectedTargets) {
  const [os, arch] = target.split('/');
  const manifest = readManifest(join(workDir, `portable-${os}-${arch}`, 'dbgold'));
  deepStrictEqual([manifest.edition, manifest.os, manifest.architecture], ['portable', os, arch]);
}
JS

if check_binary_target linux-arm64 "$WORK_DIR/portable-linux-amd64/dbgold/dbgold" ||
   check_binary_target darwin-amd64 "$WORK_DIR/portable-darwin-arm64/dbgold/dbgold" ||
   check_binary_target darwin-arm64 "$WORK_DIR/portable-darwin-amd64/dbgold/dbgold"; then
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

# Invoke archive negative tests in fresh shells with the release entrypoint's errexit semantics.
export WORK_DIR OUTPUT_DIR VERSION GIT_COMMIT BUILD_TIME OSCAR_JDBC_SHA256
chmod 0644 "$WORK_DIR/portable-darwin-arm64/dbgold/start.sh"
COPYFILE_DISABLE=1 tar -czf "$OUTPUT_DIR/dbgold-$VERSION-portable-darwin-arm64.tar.gz" \
  -C "$WORK_DIR/portable-darwin-arm64" dbgold
if bash -ec 'source "$1"; validate_portable_archive darwin arm64' bash "$repo/deploy/portable/package.sh"; then
  echo "Mac archive with a non-executable launcher was accepted" >&2; exit 1
fi

# Simulate a damaged download of the application.
printf '\nmodified\n' >> "$WORK_DIR/portable-linux-amd64/dbgold/web/index.html"
COPYFILE_DISABLE=1 tar -czf "$OUTPUT_DIR/dbgold-$VERSION-portable-linux-amd64.tar.gz" \
  -C "$WORK_DIR/portable-linux-amd64" dbgold
if bash -ec 'source "$1"; validate_portable_archive linux amd64' bash "$repo/deploy/portable/package.sh"; then
  echo "Corrupt archive was accepted" >&2; exit 1
fi
echo "Portable and Windows service release package checks passed"
