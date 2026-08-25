#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BUILD_DIR="$SCRIPT_DIR/build"
CLASS_DIR="$BUILD_DIR/classes"

rm -rf "$CLASS_DIR"
mkdir -p "$CLASS_DIR"
javac --release 8 -encoding UTF-8 -d "$CLASS_DIR" \
  "$SCRIPT_DIR/src/com/dbgold/oscar/BridgeMain.java"
jar --create --file "$BUILD_DIR/dbgold-oscar-bridge.jar" \
  --main-class com.dbgold.oscar.BridgeMain -C "$CLASS_DIR" .

