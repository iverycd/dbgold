#!/bin/sh
set -eu
script=$0
while [ -L "$script" ]; do
  dir=$(CDPATH= cd -- "$(dirname -- "$script")" && pwd -P)
  target=$(readlink -- "$script")
  case "$target" in
    /*) script=$target ;;
    *) script=$dir/$target ;;
  esac
done
root=$(CDPATH= cd -- "$(dirname -- "$script")" && pwd -P)
export TMPDIR="$root/tmp" SQLITE_TMPDIR="$root/tmp"
exec "$root/dbgold" portable "$@"
