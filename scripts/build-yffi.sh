#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

cd "$ROOT/third_party/y-crdt"
if [ -n "${RUST_TARGET:-}" ]; then
	cargo build -p yffi --release --locked --target "$RUST_TARGET"
	target_lib="$ROOT/third_party/y-crdt/target/$RUST_TARGET/release/libyrs.a"
	link_lib="$ROOT/third_party/y-crdt/target/release/libyrs.a"
	if [ ! -f "$target_lib" ]; then
		printf 'build-yffi: missing target library: %s\n' "$target_lib" >&2
		exit 1
	fi
	mkdir -p "$(dirname "$link_lib")"
	cp "$target_lib" "$link_lib"
else
	cargo build -p yffi --release --locked
fi
