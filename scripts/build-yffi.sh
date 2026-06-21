#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

cd "$ROOT/third_party/y-crdt"
if [ -n "${RUST_TARGET:-}" ]; then
	cargo build -p yffi --release --locked --target "$RUST_TARGET"
else
	cargo build -p yffi --release --locked
fi
