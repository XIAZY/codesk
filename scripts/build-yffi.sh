#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

stage_link_library() {
	source_lib="$1"
	link_lib="$2"
	tmp_lib="${link_lib}.tmp.$$"

	trap 'rm -f "$tmp_lib"' EXIT INT TERM
	cp "$source_lib" "$tmp_lib"
	mv -f "$tmp_lib" "$link_lib"
	trap - EXIT INT TERM
}

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
	stage_link_library "$target_lib" "$link_lib"
else
	cargo build -p yffi --release --locked
	host_lib="$ROOT/third_party/y-crdt/target/release/deps/libyrs.a"
	link_lib="$ROOT/third_party/y-crdt/target/release/libyrs.a"
	if [ ! -f "$host_lib" ]; then
		printf 'build-yffi: missing host library: %s\n' "$host_lib" >&2
		exit 1
	fi
	stage_link_library "$host_lib" "$link_lib"
fi
