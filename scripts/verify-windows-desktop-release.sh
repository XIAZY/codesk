#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$root_dir"
exec go run ./daemon/cmd/codesk-desktop-release verify "$@"
