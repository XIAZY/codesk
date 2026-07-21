#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'verify-windows-gui-ci: usage: verify-windows-gui-ci.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
repository="${WINDOWS_GUI_REPOSITORY:-XIAZY/notty}"
run_id="${WINDOWS_GUI_RUN_ID:-}"
out="${WINDOWS_GUI_CI_DIR:-$root_dir/dist/windows-gui/ci}"

die() {
	printf 'verify-windows-gui-ci: %s\n' "$*" >&2
	exit 1
}

command -v gh >/dev/null 2>&1 || die 'the GitHub CLI (gh) is required'
head="$(git -C "$root_dir" rev-parse --verify HEAD)"
case "$head" in
	????????????????????????????????????????) ;;
	*) die 'could not resolve a full source checkout HEAD' ;;
esac

if [ -z "$run_id" ]; then
	run_id="$(gh run list -R "$repository" --workflow ci.yml --commit "$head" --status success --limit 1 --json databaseId --jq '.[0].databaseId')"
fi
case "$run_id" in
	''|*[!0-9]*) die 'no successful exact-HEAD CI run was found; set WINDOWS_GUI_RUN_ID explicitly' ;;
esac
metadata="$(gh run view "$run_id" -R "$repository" --json headSha,conclusion --jq '.headSha + " " + .conclusion')"
[ "$metadata" = "$head success" ] || die "CI run $run_id is not a successful exact-HEAD run: $metadata (want $head success)"

case "$out" in
	''|/|.) die "unsafe WINDOWS_GUI_CI_DIR: $out" ;;
esac
release_version="$("$root_dir/scripts/read-version.sh")"
rm -rf "$out/$run_id"
mkdir -p "$out/$run_id/amd64" "$out/$run_id/arm64"

for arch in amd64 arm64; do
	dir="$out/$run_id/$arch"
	gh run download "$run_id" -R "$repository" -n "windows-desktop-msi-$arch" -D "$dir"
	expected="$(printf '%s\n' "Codesk_${release_version}_windows_${arch}.msi" SHA256SUMS provenance.json | LC_ALL=C sort)"
	actual="$(LC_ALL=C ls -1A "$dir")"
	[ "$actual" = "$expected" ] || die "unexpected $arch artifact inventory:\n$actual"
	for path in "$dir"/* "$dir"/.[!.]* "$dir"/..?*; do
		[ -e "$path" ] || continue
		[ -f "$path" ] && [ ! -L "$path" ] || die "artifact entry is not a real file: $path"
	done

	normalized="$out/$run_id/.SHA256SUMS-$arch"
	awk '{ sub(/\r$/, ""); print }' "$dir/SHA256SUMS" >"$normalized"
	expected_checksums="$(printf '%s\n' "Codesk_${release_version}_windows_${arch}.msi" provenance.json)"
	actual_checksums="$(awk 'length($1) == 64 && $1 ~ /^[0-9a-f]+$/ && NF == 2 { print $2; next } { exit 1 }' "$normalized")" || die "invalid $arch SHA256SUMS format"
	[ "$actual_checksums" = "$expected_checksums" ] || die "unexpected $arch SHA256SUMS inventory:\n$actual_checksums"
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$dir" && sha256sum -c -) <"$normalized"
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$dir" && shasum -a 256 -c -) <"$normalized"
	else
		die 'sha256sum or shasum is required'
	fi
	rm -f "$normalized"
done

printf 'Verified Windows GUI CI artifacts for %s from run %s in %s\n' "$head" "$run_id" "$out/$run_id"
