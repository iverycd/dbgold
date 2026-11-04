# Sourced by release.sh on the build host.
check_binary_target() {
  local description
  description="$(LC_ALL=C file -b "$2")"
  case "$1:$description" in
    linux-amd64:ELF\ 64-bit*x86-64*|linux-arm64:ELF\ 64-bit*aarch64*|windows-amd64:PE32+*x86-64*) return 0 ;;
    *) printf 'Wrong binary for %s: %s (%s)\n' "$1" "$2" "$description" >&2; return 1 ;;
  esac
}

check_no_bundled_java() {
  local root="$1" found
  found="$(cd "$root" && find . \( -ipath './runtime' -o -iname java -o -iname java.exe \) -print -quit)" || return 1
  [[ -z "$found" ]] || { printf 'Unexpected bundled Java runtime in %s: %s\n' "$root" "$found" >&2; return 1; }
}

portable_manifest() {
  (
    cd "$1"
    local files=() f
    while IFS= read -r f; do files+=("${f#./}"); done < <(find . -type f ! -path ./manifest.sha256 | LC_ALL=C sort)
    shasum -a 256 "${files[@]}" > manifest.sha256
  )
}

build_portable_package() {
  local os="$1" arch="$2" name="dbgold-$VERSION-portable-$1-$2"
  local stage="$WORK_DIR/portable-$1-$2" bin=dbgold start=start.sh
  local pkg="$stage/dbgold"
  if [[ "$os" == windows ]]; then bin=dbgold.exe; start=start.cmd; fi
  mkdir -p "$pkg"/{web,lib,config,data,uploads,logs,tmp,backups}
  (
    cd "$ROOT_DIR"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" "$GO_BIN" build -trimpath -ldflags "$LDFLAGS" -o "$pkg/$bin" .
  )
  cp "$ROOT_DIR/deploy/portable/$start" "$pkg/$start"
  if [[ "$os" == linux ]]; then chmod 0755 "$pkg/$start" "$pkg/$bin"; fi
  cp -R "$ROOT_DIR/frontend/dist/." "$pkg/web/"
  cp "$ROOT_DIR/jdbcbridge/java/build/dbgold-oscar-bridge.jar" "$ROOT_DIR/third_party/oscar/oscarJDBC8.jar" "$pkg/lib/"
  cp "$ROOT_DIR/third_party/oscar/README.md" "$pkg/lib/OSCAR-JDBC-NOTICE.md"
  cp "$ROOT_DIR/third_party/oscar/LICENSE" "$pkg/lib/OSCAR-JDBC-LICENSE"
  printf '%s\n' "$VERSION" > "$pkg/VERSION"
  printf '{\n  "edition": "portable",\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "os": "%s",\n  "architecture": "%s"\n}\n' \
    "$VERSION" "$GIT_COMMIT" "$BUILD_TIME" "$os" "$arch" > "$pkg/release-manifest.json"
  portable_manifest "$pkg"
  if [[ "$os" == windows ]]; then
    (cd "$stage" && zip -qrX "$OUTPUT_DIR/$name.zip" dbgold)
  else
    COPYFILE_DISABLE=1 tar -czf "$OUTPUT_DIR/$name.tar.gz" -C "$stage" dbgold
  fi
}

validate_portable_archive() {
  local os="$1" arch="$2" target="$1-$2" name="dbgold-$VERSION-portable-$1-$2"
  local unpacked="$WORK_DIR/checked-$target" bin=dbgold start=start.sh
  mkdir -p "$unpacked"
  if [[ "$os" == windows ]]; then
    bin=dbgold.exe; start=start.cmd
    unzip -q "$OUTPUT_DIR/$name.zip" -d "$unpacked"
  else
    tar -xzf "$OUTPUT_DIR/$name.tar.gz" -C "$unpacked"
  fi
  local pkg="$unpacked/dbgold" path
  [[ "$(ls -A "$unpacked")" == dbgold && -d "$pkg" ]]
  check_no_bundled_java "$pkg"
  local expected
  expected="$(printf '%s\n' "$bin" "$start" VERSION release-manifest.json manifest.sha256 web lib config data uploads logs tmp backups | LC_ALL=C sort)"
  [[ "$(LC_ALL=C ls -1A "$pkg")" == "$expected" ]] || {
    printf 'Unexpected top-level files in %s\n' "$name" >&2
    return 1
  }
  if [[ -n "$(find "$pkg" ! -type f ! -type d -print -quit)" ]]; then
    printf 'Portable package contains an unexpected file type: %s\n' "$target" >&2
    return 1
  fi
  for path in config data uploads logs tmp backups; do
    [[ -d "$pkg/$path" && -z "$(ls -A "$pkg/$path")" ]]
  done
  for path in "$bin" "$start" web/index.html lib/{dbgold-oscar-bridge.jar,oscarJDBC8.jar,OSCAR-JDBC-NOTICE.md,OSCAR-JDBC-LICENSE} VERSION release-manifest.json manifest.sha256; do
    [[ -s "$pkg/$path" ]] || { printf 'Missing %s in %s\n' "$path" "$name" >&2; return 1; }
  done
  [[ "$(cat "$pkg/VERSION")" == "$VERSION" ]]
  [[ "$(shasum -a 256 "$pkg/lib/oscarJDBC8.jar" | awk '{print $1}')" == "$OSCAR_JDBC_SHA256" ]]
  check_binary_target "$target" "$pkg/$bin"
  if [[ "$os" == linux ]]; then [[ -x "$pkg/$start" && -x "$pkg/$bin" ]]; fi
  # Recompute from all archived files to detect both corruption and omissions.
  mv "$pkg/manifest.sha256" "$unpacked/archived.sha256"
  portable_manifest "$pkg"
  cmp "$unpacked/archived.sha256" "$pkg/manifest.sha256"
  rm -rf -- "$unpacked"
}

validate_portable_artifacts() {
  local target
  for target in linux-amd64 linux-arm64 windows-amd64; do
    validate_portable_archive "${target%-*}" "${target#*-}"
  done
  (cd "$OUTPUT_DIR" && shasum -a 256 -c SHA256SUMS)
}
