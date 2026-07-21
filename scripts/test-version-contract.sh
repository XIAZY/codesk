#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/notty-version-contract.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
expect_fail() {
	expect_name="$1"
	shift
	if "$@" >"$tmp_dir/output" 2>&1; then
		fail "$expect_name"
	fi
	pass "$expect_name"
}

new_fixture() {
	new_fixture_dir="$1"
	rm -rf "$new_fixture_dir"
	mkdir -p "$new_fixture_dir/scripts/lib"
	cp "$repo_dir/Makefile" "$new_fixture_dir/Makefile"
	cp "$repo_dir/scripts/read-version.sh" "$new_fixture_dir/scripts/read-version.sh"
	cp "$repo_dir/scripts/read-version.ps1" "$new_fixture_dir/scripts/read-version.ps1"
	cp "$repo_dir/scripts/lib/deploy-env.sh" "$new_fixture_dir/scripts/lib/deploy-env.sh"
	cp "$repo_dir/scripts/lib/testtmp.sh" "$new_fixture_dir/scripts/lib/testtmp.sh"
	for script in \
		build-daemon-release.sh build-daemon-platform.sh build-macos-desktop-release.sh \
		build-windows-desktop-payloads.sh build-backend-image.sh build-frontend.sh \
		push-backend-image.sh upload-r2.sh deploy-backend.sh deploy-daemon.sh \
		deploy-frontend.sh deploy-macos-gui.sh
	do
		cp "$repo_dir/scripts/$script" "$new_fixture_dir/scripts/$script"
	done
}

fixture="$tmp_dir/repo"
new_fixture "$fixture"
mkdir -p "$tmp_dir/invalid"

for version in 0.0.0 1.2.3 255.255.65535; do
	printf '%s\n' "$version" >"$fixture/VERSION"
	actual="$("$fixture/scripts/read-version.sh")"
	[ "$actual" = "$version" ] || fail "POSIX reader changed valid $version"
	pass "POSIX reader accepts $version"
done

check_invalid() {
	invalid_name="$1"
	shift
	"$@" >"$fixture/VERSION"
	cp "$fixture/VERSION" "$tmp_dir/invalid/$invalid_name"
	expect_fail "POSIX reader rejects $invalid_name" "$fixture/scripts/read-version.sh"
}

rm -f "$fixture/VERSION"
expect_fail 'POSIX reader rejects missing VERSION' "$fixture/scripts/read-version.sh"
check_invalid empty printf ''
check_invalid leading-zero printf '01.2.3\n'
check_invalid prefix printf 'v1.2.3\n'
check_invalid suffix printf '1.2.3-rc1\n'
check_invalid leading-whitespace printf ' 1.2.3\n'
check_invalid trailing-whitespace printf '1.2.3 \n'
check_invalid CRLF printf '1.2.3\r\n'
check_invalid NUL printf '1.2.3\000\n'
check_invalid non-ASCII printf '1.2.\200\n'
check_invalid multiline printf '1.2.3\n2.3.4\n'
check_invalid missing-LF printf '1.2.3'
check_invalid major-overflow printf '256.0.0\n'
check_invalid minor-overflow printf '0.256.0\n'
check_invalid build-overflow printf '0.0.65536\n'

printf '1.2.3\n' >"$fixture/VERSION"
[ "$(VERSION=injected "$fixture/scripts/read-version.sh")" = 1.2.3 ] ||
	fail 'environment VERSION overrode the repository file'
pass 'environment VERSION cannot override the repository file'

for version in 0.0.0 1.2.3 255.255.65535; do
	printf '%s\n' "$version" >"$fixture/VERSION"
	resolved="$(make -s -C "$fixture" -pn 2>/dev/null | sed -n 's/^REPOSITORY_VERSION := //p' | head -1)"
	[ "$resolved" = "$version" ] || fail "Make did not resolve $version from the shared reader"
	pass "Make resolves $version from the shared reader"
done
printf '1.2.3\n' >"$fixture/VERSION"
resolved="$(make -s -C "$fixture" -pn VERSION=9.9.9 FILE_VERSION=8.8.8 2>/dev/null | sed -n 's/^REPOSITORY_VERSION := //p' | head -1)"
[ "$resolved" = 1.2.3 ] || fail 'Make caller version variables overrode the repository file'
pass 'Make caller version variables cannot override the repository file'
for invalid in '01.2.3' '256.0.0'; do
	printf '%s\n' "$invalid" >"$fixture/VERSION"
	expect_fail "Make rejects $invalid" make -s -C "$fixture" -pn
done

rm -f "$fixture/VERSION"
expect_fail 'Make rejects missing VERSION' make -s -C "$fixture" -pn
for invalid_file in "$tmp_dir/invalid"/*; do
	name="$(basename -- "$invalid_file")"
	cp "$invalid_file" "$fixture/VERSION"
	expect_fail "Make rejects invalid $name file" make -s -C "$fixture" -pn
	for script in build-daemon-release.sh build-macos-desktop-release.sh build-windows-desktop-payloads.sh; do
		expect_fail "$script rejects invalid $name file before build work" sh "$fixture/scripts/$script"
	done
done

rm -f "$fixture/VERSION"
for script in build-daemon-release.sh build-macos-desktop-release.sh build-windows-desktop-payloads.sh; do
	expect_fail "$script rejects missing VERSION before build work" sh "$fixture/scripts/$script"
done

for script in build-daemon-release.sh build-macos-desktop-release.sh; do
	expect_fail "$script rejects excess positional arguments" sh "$fixture/scripts/$script" one two
	grep -q 'usage:' "$tmp_dir/output" || fail "$script excess-arity failure did not report usage"
done
expect_fail 'upload-r2.sh rejects every positional argument' sh "$fixture/scripts/upload-r2.sh" 1.2.3
grep -q 'usage:' "$tmp_dir/output" || fail 'upload-r2.sh excess-arity failure did not report usage'
expect_fail 'build-frontend.sh rejects every positional argument' sh "$fixture/scripts/build-frontend.sh" 1.2.3
grep -q 'usage:' "$tmp_dir/output" || fail 'build-frontend.sh excess-arity failure did not report usage'

if command -v pwsh >/dev/null 2>&1; then
	for version in 0.0.0 1.2.3 255.255.65535; do
		printf '%s\n' "$version" >"$fixture/VERSION"
		actual="$(pwsh -NoLogo -NoProfile -NonInteractive -File "$fixture/scripts/read-version.ps1")"
		[ "$actual" = "$version" ] || fail "PowerShell reader changed valid $version"
		pass "PowerShell reader accepts $version"
	done
	rm -f "$fixture/VERSION"
	expect_fail 'PowerShell reader rejects missing VERSION' pwsh -NoLogo -NoProfile -NonInteractive -File "$fixture/scripts/read-version.ps1"
	for invalid_file in "$tmp_dir/invalid"/*; do
		name="$(basename -- "$invalid_file")"
		cp "$invalid_file" "$fixture/VERSION"
		expect_fail "PowerShell reader rejects $name" pwsh -NoLogo -NoProfile -NonInteractive -File "$fixture/scripts/read-version.ps1"
	done
else
	printf '%s\n' 'SKIP: PowerShell runtime assertions (pwsh unavailable)'
fi

printf '%s\n' 'All version contract tests passed.'
