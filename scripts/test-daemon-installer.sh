#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
installer="$repo_dir/deploy/daemons/install.sh"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/notty-installer-test.XXXXXX")"

cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

fail() {
	printf 'installer test failed: %s\n' "$*" >&2
	exit 1
}

assert_file() {
	[ -f "$1" ] || fail "expected file $1"
}

assert_executable() {
	[ -x "$1" ] || fail "expected executable $1"
}

assert_missing() {
	[ ! -e "$1" ] || fail "expected $1 to be absent"
}

checksum_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1"
	else
		fail "sha256sum or shasum is required"
	fi
}

case "$(uname -s)" in
	Darwin) os="darwin" ;;
	Linux) os="linux" ;;
	*) fail "unsupported test operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
	x86_64|amd64) arch="amd64" ;;
	arm64|aarch64) arch="arm64" ;;
	*) fail "unsupported test architecture: $(uname -m)" ;;
esac

version="test"
static_root="$tmp_dir/static"
release_dir="$static_root/$version"

create_package() {
	package="notty-daemon_${version}_${1}_${2}"
	package_dir="$release_dir/$package"
	mkdir -p "$package_dir/bin" "$static_root/latest"
	cat > "$package_dir/bin/notty-daemon" <<'EOF'
#!/usr/bin/env sh
echo fake notty daemon
EOF
	cat > "$package_dir/bin/notty-agent-tool" <<'EOF'
#!/usr/bin/env sh
echo fake notty agent tool
EOF
	chmod +x "$package_dir/bin/notty-daemon" "$package_dir/bin/notty-agent-tool"
	tar -czf "$release_dir/$package.tar.gz" -C "$release_dir" "$package"
}

create_package "$os" "$arch"
if [ "$os/$arch" != "linux/amd64" ]; then
	create_package linux amd64
fi
(
	cd "$release_dir"
	for archive in notty-daemon_"$version"_*.tar.gz; do
		checksum_file "$archive"
	done > SHA256SUMS
)
printf '{"version":"%s"}\n' "$version" > "$static_root/latest/manifest.json"
package="notty-daemon_${version}_${os}_${arch}"

ok_codex="$tmp_dir/codex-ok"
cat > "$ok_codex" <<'EOF'
#!/usr/bin/env sh
case "${1:-}" in
	--version)
		echo "codex fake"
		exit 0
		;;
	app-server)
		if [ "${2:-}" = "--help" ]; then
			echo "codex app-server fake help"
			exit 0
		fi
		;;
esac
exit 0
EOF
chmod +x "$ok_codex"

test_bin="$tmp_dir/test-bin"
mkdir -p "$test_bin"
cat > "$test_bin/curl" <<'EOF'
#!/usr/bin/env sh
set -eu
url=""
dest=""
while [ "$#" -gt 0 ]; do
	case "$1" in
		-o)
			dest="$2"
			shift 2
			;;
		-*)
			shift
			;;
		*)
			url="$1"
			shift
			;;
	esac
done
[ -n "$url" ] || exit 2
[ -n "$dest" ] || exit 2
case "$url" in
	file://*)
		cp "${url#file://}" "$dest"
		;;
	*)
		exit 3
		;;
esac
EOF
chmod +x "$test_bin/curl"

bad_codex="$tmp_dir/codex-bad"
cat > "$bad_codex" <<'EOF'
#!/usr/bin/env sh
exit 42
EOF
chmod +x "$bad_codex"

ok_claude="$tmp_dir/claude-ok"
cat > "$ok_claude" <<'EOF'
#!/usr/bin/env sh
case "${1:-}" in
	--version)
		echo "9.9.9 (Claude Code)"
		exit 0
		;;
esac
exit 2
EOF
chmod +x "$ok_claude"

bad_claude="$tmp_dir/claude-bad"
cat > "$bad_claude" <<'EOF'
#!/usr/bin/env sh
exit 42
EOF
chmod +x "$bad_claude"

common_args="
--backend-url http://127.0.0.1:8080
--workspace-id ws-test
--daemon-token nottyd_test
--static-base file://$static_root
--install-dir $tmp_dir/install
--data-dir $tmp_dir/data
--no-service
"

PATH="$test_bin:$PATH" NOTTY_CODEX_COMMAND="$tmp_dir/missing-codex" NOTTY_CLAUDE_COMMAND="$tmp_dir/missing-claude" HOME="$tmp_dir/home-missing" sh "$installer" $common_args > "$tmp_dir/missing.out" 2> "$tmp_dir/missing.err"
grep -q "Codex runtime unavailable" "$tmp_dir/missing.err" || fail "missing codex warning did not explain runtime availability"
grep -q "Codex agents will be unavailable" "$tmp_dir/missing.err" || fail "missing codex warning did not name the degraded mode"
grep -q "Claude Code runtime unavailable" "$tmp_dir/missing.err" || fail "missing claude warning did not explain runtime availability"
assert_executable "$tmp_dir/install/notty-daemon"
assert_file "$tmp_dir/data/daemons/ws-test/daemon.env"
grep -q "NOTTY_CODEX_COMMAND='$tmp_dir/missing-codex'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "missing codex install did not preserve configured command"
grep -q "NOTTY_CLAUDE_COMMAND='$tmp_dir/missing-claude'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "missing claude install did not preserve configured command"

PATH="$test_bin:$PATH" NOTTY_CODEX_COMMAND="$bad_codex" NOTTY_CLAUDE_COMMAND="$bad_claude" HOME="$tmp_dir/home-bad" sh "$installer" $common_args > "$tmp_dir/bad.out" 2> "$tmp_dir/bad.err"
grep -q "'$bad_codex --version' did not run successfully" "$tmp_dir/bad.err" || fail "bad codex warning did not explain failed smoke test"
grep -q "'$bad_claude --version' did not run successfully" "$tmp_dir/bad.err" || fail "bad claude warning did not explain failed smoke test"
assert_executable "$tmp_dir/install/notty-daemon"
assert_file "$tmp_dir/data/daemons/ws-test/daemon.env"
grep -q "NOTTY_CODEX_COMMAND='$bad_codex'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "bad codex install did not preserve configured command"

old_codex="$tmp_dir/codex-old"
cat > "$old_codex" <<'EOF'
#!/usr/bin/env sh
case "${1:-}" in
	--version)
		echo "codex old"
		exit 0
		;;
esac
exit 2
EOF
chmod +x "$old_codex"

PATH="$test_bin:$PATH" NOTTY_CODEX_COMMAND="$old_codex" HOME="$tmp_dir/home-old" sh "$installer" $common_args > "$tmp_dir/old.out" 2> "$tmp_dir/old.err"
grep -q "does not support 'app-server'" "$tmp_dir/old.err" || fail "old codex warning did not explain app-server availability"
assert_executable "$tmp_dir/install/notty-daemon"
assert_file "$tmp_dir/data/daemons/ws-test/daemon.env"
grep -q "NOTTY_CODEX_COMMAND='$old_codex'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "old codex install did not preserve configured command"

PATH="$test_bin:$PATH" NOTTY_CODEX_COMMAND="$ok_codex" NOTTY_CLAUDE_COMMAND="$ok_claude" HOME="$tmp_dir/home-ok" sh "$installer" $common_args > "$tmp_dir/ok.out" 2> "$tmp_dir/ok.err"
assert_executable "$tmp_dir/install/notty-daemon"
assert_executable "$tmp_dir/install/notty-agent-tool"
assert_file "$tmp_dir/data/daemons/ws-test/daemon.env"
grep -q "NOTTY_CODEX_COMMAND='$ok_codex'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not preserve configured Codex command"
grep -q "NOTTY_CLAUDE_COMMAND='$ok_claude'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not preserve configured Claude Code command"
if grep -q "Claude Code runtime unavailable" "$tmp_dir/ok.err"; then fail "healthy claude should not warn about runtime availability"; fi
grep -q "NOTTY_DAEMON_VERSION='$version'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not preserve daemon version"
grep -q "NOTTY_DATA_DIR='$tmp_dir/data'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not preserve daemon data dir"
if grep -q "^export PATH=" "$tmp_dir/data/daemons/ws-test/daemon.env"; then
	fail "env file must not persist a PATH snapshot"
fi
grep -q "NOTTY_TOOL_DIR_CODEX='" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not persist resolved codex tool dir"
grep -q "NOTTY_TOOL_DIR_CLAUDE='$tmp_dir'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not persist resolved claude tool dir"
grep -q "NOTTY_DAEMON_VERSION NOTTY_DATA_DIR" "$tmp_dir/data/daemons/ws-test/run.sh" || fail "run script did not export daemon version"
grep -q "NOTTY_DATA_DIR NOTTY_WORKSPACE_DIR" "$tmp_dir/data/daemons/ws-test/run.sh" || fail "run script did not export daemon data dir"
grep -q "NOTTY_CODEX_COMMAND NOTTY_CLAUDE_COMMAND NOTTY_TOOL_DIR_CODEX NOTTY_TOOL_DIR_CLAUDE" "$tmp_dir/data/daemons/ws-test/run.sh" || fail "run script did not export runtime commands and tool dirs"
grep -q "notty_derive_daemon_path" "$tmp_dir/data/daemons/ws-test/run.sh" || fail "run script does not re-derive PATH at start"
grep -q "export PATH='$tmp_dir/install':\"\$PATH\"" "$tmp_dir/data/daemons/ws-test/run.sh" || fail "run script did not prepend install directory to PATH"

fake_path="$tmp_dir/fake-path"
mkdir -p "$fake_path"
cp "$ok_codex" "$fake_path/codex"
chmod +x "$fake_path/codex"

PATH="$fake_path:$test_bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$tmp_dir/home-path" \
	NOTTY_INSTALL_DIR="$tmp_dir/install-path" \
	NOTTY_DATA_DIR="$tmp_dir/data-path" \
	sh "$installer" \
	--backend-url http://127.0.0.1:8080 \
	--workspace-id ws-path \
	--daemon-token nottyd_test \
	--static-base "file://$static_root" \
	--no-service > "$tmp_dir/path.out" 2> "$tmp_dir/path.err"
assert_executable "$tmp_dir/install-path/notty-daemon"
assert_file "$tmp_dir/data-path/daemons/ws-path/daemon.env"
grep -q "NOTTY_CODEX_COMMAND='codex'" "$tmp_dir/data-path/daemons/ws-path/daemon.env" || fail "env file did not preserve bare Codex command"
grep -q "NOTTY_DATA_DIR='$tmp_dir/data-path'" "$tmp_dir/data-path/daemons/ws-path/daemon.env" || fail "env file did not preserve configured data dir"
grep -q "NOTTY_TOOL_DIR_CODEX='$fake_path'" "$tmp_dir/data-path/daemons/ws-path/daemon.env" || fail "env file did not persist resolved codex tool dir from install shell PATH"

fallback_home="$tmp_dir/home-fallback"
mkdir -p "$fallback_home/.local/bin"
cp "$ok_codex" "$fallback_home/.local/bin/codex"
chmod +x "$fallback_home/.local/bin/codex"

PATH="$test_bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$fallback_home" \
	NOTTY_INSTALL_DIR="$tmp_dir/install-fallback" \
	NOTTY_DATA_DIR="$tmp_dir/data-fallback" \
	sh "$installer" \
	--backend-url http://127.0.0.1:8080 \
	--workspace-id ws-fallback \
	--daemon-token nottyd_test \
	--static-base "file://$static_root" \
	--no-service > "$tmp_dir/fallback.out" 2> "$tmp_dir/fallback.err"
assert_executable "$tmp_dir/install-fallback/notty-daemon"
assert_file "$tmp_dir/data-fallback/daemons/ws-fallback/daemon.env"
grep -q "NOTTY_CODEX_COMMAND='codex'" "$tmp_dir/data-fallback/daemons/ws-fallback/daemon.env" || fail "fallback install did not preserve bare Codex command"
grep -q "NOTTY_TOOL_DIR_CODEX='$fallback_home/.local/bin'" "$tmp_dir/data-fallback/daemons/ws-fallback/daemon.env" || fail "fallback install did not persist common Codex path as the tool dir"

linux_bin="$tmp_dir/linux-bin"
mkdir -p "$linux_bin"
cat > "$linux_bin/uname" <<'EOF'
#!/usr/bin/env sh
case "${1:-}" in
	-s)
		echo Linux
		;;
	-m)
		echo x86_64
		;;
	*)
		/usr/bin/uname "$@"
		;;
esac
EOF
cat > "$linux_bin/systemctl" <<'EOF'
#!/usr/bin/env sh
exit 1
EOF
cat > "$linux_bin/nohup" <<EOF
#!/usr/bin/env sh
printf 'called\n' > "$tmp_dir/nohup-called"
exit 42
EOF
cp "$ok_codex" "$linux_bin/codex"
chmod +x "$linux_bin/uname" "$linux_bin/systemctl" "$linux_bin/nohup" "$linux_bin/codex"

PATH="$linux_bin:$test_bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$tmp_dir/home-linux" \
	NOTTY_INSTALL_DIR="$tmp_dir/install-linux" \
	NOTTY_DATA_DIR="$tmp_dir/data-linux" \
	sh "$installer" \
	--backend-url http://127.0.0.1:8080 \
	--workspace-id ws-linux \
	--daemon-token nottyd_test \
	--static-base "file://$static_root" > "$tmp_dir/linux.out" 2> "$tmp_dir/linux.err"
assert_executable "$tmp_dir/install-linux/notty-daemon"
assert_file "$tmp_dir/data-linux/daemons/ws-linux/daemon.env"
assert_file "$tmp_dir/data-linux/daemons/ws-linux/run.sh"
assert_missing "$tmp_dir/nohup-called"
assert_missing "$tmp_dir/home-linux/.config/systemd/user/notty-daemon-ws-linux.service"
grep -q "systemd user service start failed; daemon was not started" "$tmp_dir/linux.err" || fail "linux systemd failure did not explain daemon was not started"
grep -q "Start the daemon in the foreground with:" "$tmp_dir/linux.out" || fail "linux systemd failure did not print foreground start instructions"
grep -q "$tmp_dir/data-linux/daemons/ws-linux/run.sh" "$tmp_dir/linux.out" || fail "linux foreground instructions did not include run script"

http_bin="$tmp_dir/http-bin"
mkdir -p "$http_bin"
cat > "$http_bin/curl" <<EOF
#!/usr/bin/env sh
set -eu
url=""
dest=""
while [ "\$#" -gt 0 ]; do
	case "\$1" in
		-o)
			dest="\$2"
			shift 2
			;;
		http://static.test/*)
			url="\$1"
			shift
			;;
		*)
			shift
			;;
	esac
done
[ -n "\$url" ] || exit 2
[ -n "\$dest" ] || exit 2
printf '%s\n' "\$url" >> "$tmp_dir/curl.urls"
path="\${url#http://static.test/}"
path="\${path%%\\?*}"
cp "$static_root/\$path" "\$dest"
EOF
chmod +x "$http_bin/curl"

PATH="$http_bin:/usr/bin:/bin:/usr/sbin:/sbin" \
	NOTTY_CODEX_COMMAND="$ok_codex" \
	HOME="$tmp_dir/home-http" \
	NOTTY_INSTALL_DIR="$tmp_dir/install-http" \
	NOTTY_DATA_DIR="$tmp_dir/data-http" \
	sh "$installer" \
	--backend-url http://127.0.0.1:8080 \
	--workspace-id ws-http \
	--daemon-token nottyd_test \
	--static-base http://static.test \
	--no-service > "$tmp_dir/http.out" 2> "$tmp_dir/http.err"
assert_executable "$tmp_dir/install-http/notty-daemon"
grep -q 'latest/manifest.json?notty_cache_bust=' "$tmp_dir/curl.urls" || fail "http manifest download did not bypass cache"
grep -q 'SHA256SUMS?notty_cache_bust=' "$tmp_dir/curl.urls" || fail "http checksum download did not bypass cache"
grep -q "$package.tar.gz?notty_cache_bust=" "$tmp_dir/curl.urls" || fail "http artifact download did not bypass cache"

# PATH policy: the interactive PATH is detection-only; only the resolved tool
# dir persists, and run.sh re-derives PATH at every daemon start so a tool
# that moves after install heals on restart.
junk_path="$tmp_dir/junk-bin"
heal_home="$tmp_dir/home-heal"
heal_codex_dir="$tmp_dir/heal-initial-bin"
mkdir -p "$junk_path" "$heal_codex_dir" "$heal_home"
cp "$ok_codex" "$heal_codex_dir/codex"
chmod +x "$heal_codex_dir/codex"

PATH="$heal_codex_dir:$junk_path:$test_bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$heal_home" \
	NOTTY_INSTALL_DIR="$tmp_dir/install-heal" \
	NOTTY_DATA_DIR="$tmp_dir/data-heal" \
	sh "$installer" \
	--backend-url http://127.0.0.1:8080 \
	--workspace-id ws-heal \
	--daemon-token nottyd_test \
	--static-base "file://$static_root" \
	--no-service > "$tmp_dir/heal.out" 2> "$tmp_dir/heal.err"

heal_env="$tmp_dir/data-heal/daemons/ws-heal/daemon.env"
heal_run="$tmp_dir/data-heal/daemons/ws-heal/run.sh"
assert_file "$heal_env"
assert_file "$heal_run"
grep -q "NOTTY_TOOL_DIR_CODEX='$heal_codex_dir'" "$heal_env" || fail "heal install did not persist resolved codex tool dir"
if grep -q "$junk_path" "$heal_env"; then
	fail "daemon.env leaked unrelated interactive PATH entries"
fi
if grep -q "$junk_path" "$heal_run"; then
	fail "run script leaked unrelated interactive PATH entries"
fi
grep -q "Daemon service PATH: $tmp_dir/install-heal:$heal_codex_dir:" "$tmp_dir/heal.out" || fail "install did not print the derived daemon service PATH"

cat > "$tmp_dir/install-heal/notty-daemon" <<'EOF'
#!/usr/bin/env sh
printf 'probe codex=%s\n' "$(command -v codex 2>/dev/null || echo missing)"
EOF
chmod +x "$tmp_dir/install-heal/notty-daemon"

# Drift after install: codex's install-time dir disappears entirely and codex
# reappears in ~/.local/bin, which did not exist at install time.
rm -rf "$heal_codex_dir"
mkdir -p "$heal_home/.local/bin"
cp "$ok_codex" "$heal_home/.local/bin/codex"
chmod +x "$heal_home/.local/bin/codex"

PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$heal_home" sh "$heal_run" > "$tmp_dir/heal-run.out" 2>&1
grep -q "notty-daemon start: PATH=" "$tmp_dir/heal-run.out" || fail "daemon start did not log its effective PATH"
grep -q "notty-daemon start: codex=codex resolved=$heal_home/.local/bin/codex" "$tmp_dir/heal-run.out" || fail "daemon start did not log the resolved codex location"
grep -q "probe codex=$heal_home/.local/bin/codex" "$tmp_dir/heal-run.out" || fail "daemon start did not re-derive PATH to find moved codex"
if grep -q "$heal_codex_dir" "$tmp_dir/heal-run.out"; then
	fail "daemon start kept a dead tool dir on PATH"
fi

printf 'daemon installer tests passed\n'
