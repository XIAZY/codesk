#!/usr/bin/env sh
set -eu

runner_source_revision="${1:-}"
version="${2:-${VERSION:-dev}}"
out_dir="${3:-${OUT_DIR:-dist/windows-desktop-acceptance}}"
root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
go_toolchain='go1.26.5'
export GOTOOLCHAIN="$go_toolchain"
export GOENV=off
export GO111MODULE=on
export GOWORK=off
export GOAMD64=v1
export GOARM64=v8.0
export LC_ALL=C
export TZ=UTC
export SOURCE_DATE_EPOCH=0
unset GOFLAGS GOEXPERIMENT

fail() {
	printf 'build-windows-desktop-acceptance: %s\n' "$*" >&2
	exit 1
}

case "$runner_source_revision" in
	*[!0-9a-f]*|'') fail 'runner source revision must be the exact lowercase hexadecimal Git head' ;;
esac
[ "${#runner_source_revision}" -eq 40 ] || fail 'runner source revision must contain exactly 40 hexadecimal characters'
[ "${runner_source_revision#0000000000000000000000000000000000000000}" = "$runner_source_revision" ] || fail 'runner source revision must not be all zeroes'

case "$version" in
	''|.*|*[!A-Za-z0-9._-]*) fail 'runner version must be a filesystem-safe identifier' ;;
esac
[ "${#version}" -le 64 ] || fail 'runner version must not exceed 64 characters'

case "$out_dir" in
	''|/|.|..) fail 'output directory must name a new non-root directory' ;;
	*/|*//*|*/./*|*/../*|./*|../*|*/.|*/..) fail 'output directory must not contain empty, dot, or parent components' ;;
	/*) out_abs="$out_dir" ;;
	*) out_abs="$root_dir/$out_dir" ;;
esac
[ ! -e "$out_abs" ] && [ ! -L "$out_abs" ] || fail 'output directory must not already exist'

command -v go >/dev/null 2>&1 || fail 'go is required'
command -v git >/dev/null 2>&1 || fail 'git is required'
actual_go_version="$(go env GOVERSION 2>/dev/null || true)"
[ "$actual_go_version" = "$go_toolchain" ] || fail "$go_toolchain is required (got ${actual_go_version:-unknown})"
actual_revision="$(git -C "$root_dir" rev-parse --verify HEAD 2>/dev/null)" || fail 'source checkout has no exact Git head'
[ "$actual_revision" = "$runner_source_revision" ] || fail "runner source revision $runner_source_revision is not checked out (got ${actual_revision:-unknown})"
worktree_status="$(git -C "$root_dir" status --porcelain=v1 --untracked-files=all 2>/dev/null)" || fail 'source checkout status cannot be verified'
[ -z "$worktree_status" ] || fail 'source checkout is not release-clean'

for arch in amd64 arm64; do
	dependencies="$(CGO_ENABLED=0 GOOS=windows GOARCH="$arch" \
		go -C "$root_dir" list -mod=readonly -deps ./daemon/cmd/codesk-desktop-acceptance)" ||
		fail "dependency enumeration failed on windows/$arch"
	[ -n "$dependencies" ] || fail "dependency enumeration was empty on windows/$arch"
	for dependency in $dependencies; do
		case "$dependency" in
			notty/daemon/internal/desktop|notty/daemon/internal/syncer|notty/internal/ycrdt)
				fail "native acceptance runner has forbidden dependency $dependency on windows/$arch"
				;;
		esac
	done
done

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/codesk-windows-acceptance.XXXXXX")"
cleanup() {
	status=$?
	trap - EXIT INT TERM
	rm -rf "$tmp_dir"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

mkdir -p "$tmp_dir/publish"
for arch in amd64 arm64; do
	name="CodeskAcceptance_${version}_windows_${arch}.exe"
	printf 'build-windows-desktop-acceptance: building %s\n' "$name"
	(
		cd "$root_dir"
		CGO_ENABLED=0 GOOS=windows GOARCH="$arch" \
			go build -mod=readonly -buildvcs=false -trimpath \
			-ldflags "-buildid= -s -w -X main.builtRunnerSourceRevision=$runner_source_revision" \
			-o "$tmp_dir/publish/$name" ./daemon/cmd/codesk-desktop-acceptance
	)
	[ -s "$tmp_dir/publish/$name" ] || fail "runner build did not create $name"
done

mkdir -p "$(dirname -- "$out_abs")"
mv "$tmp_dir/publish" "$out_abs"
printf 'Built source-bound Windows desktop acceptance runners in %s\n' "$out_abs"
