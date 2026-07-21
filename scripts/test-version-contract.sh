#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
. "$repo_dir/scripts/lib/testtmp.sh"
tmp_dir="$(notty_test_mktemp notty-version-contract)"

cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

fail() {
	printf 'version-contract FAIL: %s\n' "$*" >&2
	exit 1
}

pass() {
	printf 'version-contract PASS: %s\n' "$*"
}

setup_fake_repo() {
	rm -rf "$tmp_dir/fake"
	mkdir -p "$tmp_dir/fake/scripts/lib" "$tmp_dir/fake/deploy/daemons"
	cp "$repo_dir/scripts/build-daemon-release.sh" "$tmp_dir/fake/scripts/"
	cp "$repo_dir/scripts/build-macos-desktop-release.sh" "$tmp_dir/fake/scripts/"
	cp "$repo_dir/scripts/lib/testtmp.sh" "$tmp_dir/fake/scripts/lib/"
	touch "$tmp_dir/fake/deploy/daemons/install.sh"
	touch "$tmp_dir/fake/deploy/daemons/uninstall.sh"
	touch "$tmp_dir/fake/deploy/daemons/install.ps1"
	touch "$tmp_dir/fake/deploy/daemons/uninstall.ps1"
	touch "$tmp_dir/fake/deploy/daemons/run-windows.ps1"
	cp "$repo_dir/Makefile" "$tmp_dir/fake/"
	printf 'module notty\ngo 1.26\n' > "$tmp_dir/fake/go.mod"
}

expect_fail() {
	label="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		fail "$label: expected failure but command succeeded"
	fi
	pass "$label"
}

# ── Makefile: missing VERSION ──
setup_fake_repo
expect_fail "Makefile rejects missing VERSION" \
	make -C "$tmp_dir/fake" -n build-daemon

# ── Makefile: empty VERSION ──
setup_fake_repo
printf '' > "$tmp_dir/fake/VERSION"
expect_fail "Makefile rejects empty VERSION" \
	make -C "$tmp_dir/fake" -n build-daemon

# ── Makefile: VERSION cannot be overridden by env ──
setup_fake_repo
printf '1.2.3\n' > "$tmp_dir/fake/VERSION"
resolved="$(VERSION=injected make -C "$tmp_dir/fake" -n -p 2>/dev/null \
	| grep '^override VERSION := ' | head -1 | sed 's/^override VERSION := //')" || true
if [ "$resolved" = "injected" ]; then
	fail "Makefile VERSION was overridden by environment variable"
fi
pass "Makefile VERSION ignores environment override"

# ── Makefile: VERSION cannot be overridden by CLI ──
resolved="$(make -C "$tmp_dir/fake" -n -p VERSION=injected 2>/dev/null \
	| grep '^override VERSION := ' | head -1 | sed 's/^override VERSION := //')" || true
if [ "$resolved" = "injected" ]; then
	fail "Makefile VERSION was overridden by command-line"
fi
pass "Makefile VERSION ignores command-line override"

# ── Makefile: FILE_VERSION cannot be overridden by CLI ──
resolved="$(make -C "$tmp_dir/fake" -n -p FILE_VERSION=injected 2>/dev/null \
	| grep '^override FILE_VERSION := ' | head -1 | sed 's/^override FILE_VERSION := //')" || true
if [ "$resolved" = "injected" ]; then
	fail "Makefile FILE_VERSION was overridden by command-line"
fi
pass "Makefile FILE_VERSION ignores command-line override"

# ── build-daemon-release.sh: missing VERSION ──
setup_fake_repo
expect_fail "build-daemon-release.sh rejects missing VERSION" \
	sh "$tmp_dir/fake/scripts/build-daemon-release.sh" "$tmp_dir/fake-dist"

# ── build-daemon-release.sh: empty VERSION ──
setup_fake_repo
printf '' > "$tmp_dir/fake/VERSION"
expect_fail "build-daemon-release.sh rejects empty VERSION" \
	sh "$tmp_dir/fake/scripts/build-daemon-release.sh" "$tmp_dir/fake-dist"

# ── build-daemon-release.sh: positional arg is dist_dir, not version ──
setup_fake_repo
printf '1.2.3\n' > "$tmp_dir/fake/VERSION"
dry_output="$(sh -x "$tmp_dir/fake/scripts/build-daemon-release.sh" "$tmp_dir/fake-dist" 2>&1 || true)"
if printf '%s' "$dry_output" | grep -q 'version=.*fake-dist'; then
	fail "build-daemon-release.sh still treats \$1 as version"
fi
pass "build-daemon-release.sh \$1 is dist_dir, not version"

# ── build-macos-desktop-release.sh: missing VERSION ──
setup_fake_repo
expect_fail "build-macos-desktop-release.sh rejects missing VERSION" \
	sh "$tmp_dir/fake/scripts/build-macos-desktop-release.sh" "$tmp_dir/fake-dist"

# ── build-macos-desktop-release.sh: empty VERSION ──
setup_fake_repo
printf '' > "$tmp_dir/fake/VERSION"
expect_fail "build-macos-desktop-release.sh rejects empty VERSION" \
	sh "$tmp_dir/fake/scripts/build-macos-desktop-release.sh" "$tmp_dir/fake-dist"

# ── build-macos-desktop-release.sh: positional arg is dist_dir, not version ──
setup_fake_repo
printf '1.2.3\n' > "$tmp_dir/fake/VERSION"
dry_output="$(sh -x "$tmp_dir/fake/scripts/build-macos-desktop-release.sh" "$tmp_dir/fake-dist" 2>&1 || true)"
if printf '%s' "$dry_output" | grep -q 'version=.*fake-dist'; then
	fail "build-macos-desktop-release.sh still treats \$1 as version"
fi
pass "build-macos-desktop-release.sh \$1 is dist_dir, not version"

# ── make.ps1: missing VERSION (requires pwsh) ──
if command -v pwsh >/dev/null 2>&1; then
	setup_fake_repo
	cp "$repo_dir/make.ps1" "$tmp_dir/fake/"
	expect_fail "make.ps1 rejects missing VERSION" \
		pwsh -NoLogo -NoProfile -NonInteractive -File "$tmp_dir/fake/make.ps1" windows-gui-build

	setup_fake_repo
	cp "$repo_dir/make.ps1" "$tmp_dir/fake/"
	printf '' > "$tmp_dir/fake/VERSION"
	expect_fail "make.ps1 rejects empty VERSION" \
		pwsh -NoLogo -NoProfile -NonInteractive -File "$tmp_dir/fake/make.ps1" windows-gui-build

	setup_fake_repo
	cp "$repo_dir/make.ps1" "$tmp_dir/fake/"
	printf '1.2.3\n' > "$tmp_dir/fake/VERSION"
	expect_fail "make.ps1 rejects GUI_VERSION setting" \
		pwsh -NoLogo -NoProfile -NonInteractive -File "$tmp_dir/fake/make.ps1" windows-gui-build "GUI_VERSION=injected"

	pass "make.ps1 PowerShell contract verified"
else
	printf 'version-contract SKIP: pwsh not available for make.ps1 tests\n'
fi

# ── Static assertion: no fail-open fallback to "dev" in release scripts ──
for script in build-daemon-release.sh build-macos-desktop-release.sh; do
	path="$repo_dir/scripts/$script"
	if grep -Eq 'VERSION:-dev|VERSION:-\}' "$path"; then
		fail "$script contains fail-open dev fallback"
	fi
done
pass "release scripts contain no fail-open dev fallback"

# ── Static assertion: Makefile uses override for FILE_VERSION and VERSION ──
if ! grep -q '^override FILE_VERSION :=' "$repo_dir/Makefile"; then
	fail "Makefile FILE_VERSION is not override-protected"
fi
override_count="$(grep -c '^override VERSION :=' "$repo_dir/Makefile")"
if [ "$override_count" -lt 2 ]; then
	fail "Makefile VERSION is not override-protected in both branches"
fi
pass "Makefile FILE_VERSION and VERSION are override-protected"

# ── Static assertion: GUI_VERSION is not defined in Makefile ──
if grep -q '^GUI_VERSION' "$repo_dir/Makefile"; then
	fail "Makefile still defines GUI_VERSION"
fi
pass "Makefile does not define GUI_VERSION"

# ── Static assertion: Makefile daemon-release does not pass version arg ──
daemon_release_line="$(grep 'scripts/build-daemon-release.sh' "$repo_dir/Makefile" || true)"
if printf '%s' "$daemon_release_line" | grep -q '"$(VERSION)".*"$(DIST_DIR)"'; then
	fail "Makefile daemon-release still passes VERSION as first positional arg"
fi
pass "Makefile daemon-release calls script without version arg"

# ── Static assertion: Makefile macos-gui does not pass version arg ──
macos_gui_line="$(grep 'scripts/build-macos-desktop-release.sh' "$repo_dir/Makefile" || true)"
if printf '%s' "$macos_gui_line" | grep -q '"$(GUI_VERSION)"'; then
	fail "Makefile macos-gui still passes GUI_VERSION as first positional arg"
fi
pass "Makefile macos-gui calls script without version arg"

# ── Static assertion: Makefile windows-gui calls do not pass GUI_VERSION ──
windows_gui_lines="$(grep 'make.ps1 windows-gui' "$repo_dir/Makefile" || true)"
if printf '%s' "$windows_gui_lines" | grep -q 'GUI_VERSION='; then
	fail "Makefile windows-gui calls still pass GUI_VERSION"
fi
pass "Makefile windows-gui calls do not pass GUI_VERSION"

# ── Static assertion: build-static.sh does not pass version arg to daemon release ──
if grep -q 'build-daemon-release.sh.*"\$version"' "$repo_dir/scripts/build-static.sh"; then
	fail "build-static.sh still passes version arg to build-daemon-release.sh"
fi
pass "build-static.sh calls daemon release without version arg"

printf '\nversion-contract: all tests passed\n'
