#!/bin/sh
set -eu
root=$(dirname -- "$(readlink -f -- "$0")")
export TMPDIR="$root/tmp" SQLITE_TMPDIR="$root/tmp"
exec "$root/dbgold" portable "$@"
