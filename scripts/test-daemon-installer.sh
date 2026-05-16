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
package="notty-daemon_${version}_${os}_${arch}"
static_root="$tmp_dir/static"
release_dir="$static_root/$version"
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
(cd "$release_dir" && shasum -a 256 "$package.tar.gz" > SHA256SUMS)
printf '{"version":"%s"}\n' "$version" > "$static_root/latest/manifest.json"

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

bad_codex="$tmp_dir/codex-bad"
cat > "$bad_codex" <<'EOF'
#!/usr/bin/env sh
exit 42
EOF
chmod +x "$bad_codex"

common_args="
--backend-url http://127.0.0.1:8080
--workspace-id ws-test
--daemon-token nottyd_test
--static-base file://$static_root
--install-dir $tmp_dir/install
--data-dir $tmp_dir/data
--no-service
"

if NOTTY_CODEX_COMMAND="$tmp_dir/missing-codex" HOME="$tmp_dir/home-missing" sh "$installer" $common_args > "$tmp_dir/missing.out" 2> "$tmp_dir/missing.err"; then
	fail "installer succeeded without codex"
fi
grep -q "Codex CLI is required" "$tmp_dir/missing.err" || fail "missing codex error did not explain requirement"
assert_missing "$tmp_dir/data"

if NOTTY_CODEX_COMMAND="$bad_codex" HOME="$tmp_dir/home-bad" sh "$installer" $common_args > "$tmp_dir/bad.out" 2> "$tmp_dir/bad.err"; then
	fail "installer succeeded with failing codex"
fi
grep -q "did not run successfully" "$tmp_dir/bad.err" || fail "bad codex error did not explain failed smoke test"
assert_missing "$tmp_dir/data"

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

if NOTTY_CODEX_COMMAND="$old_codex" HOME="$tmp_dir/home-old" sh "$installer" $common_args > "$tmp_dir/old.out" 2> "$tmp_dir/old.err"; then
	fail "installer succeeded with codex missing app-server"
fi
grep -q "does not support 'app-server'" "$tmp_dir/old.err" || fail "old codex error did not explain app-server requirement"
assert_missing "$tmp_dir/data"

NOTTY_CODEX_COMMAND="$ok_codex" HOME="$tmp_dir/home-ok" sh "$installer" $common_args > "$tmp_dir/ok.out" 2> "$tmp_dir/ok.err"
assert_executable "$tmp_dir/install/notty-daemon"
assert_executable "$tmp_dir/install/notty-agent-tool"
assert_file "$tmp_dir/data/daemons/ws-test/daemon.env"
grep -q "NOTTY_CODEX_COMMAND='$ok_codex'" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not preserve configured Codex command"
grep -q "^export PATH='" "$tmp_dir/data/daemons/ws-test/daemon.env" || fail "env file did not persist daemon PATH"
grep -q "NOTTY_CODEX_COMMAND PATH" "$tmp_dir/data/daemons/ws-test/run.sh" || fail "run script did not export daemon PATH"
grep -q "export PATH='$tmp_dir/install':\"\$PATH\"" "$tmp_dir/data/daemons/ws-test/run.sh" || fail "run script did not prepend install directory to PATH"

fake_path="$tmp_dir/fake-path"
mkdir -p "$fake_path"
cp "$ok_codex" "$fake_path/codex"
chmod +x "$fake_path/codex"

PATH="$fake_path:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$tmp_dir/home-path" \
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
grep -q "$fake_path" "$tmp_dir/data-path/daemons/ws-path/daemon.env" || fail "env file did not persist install shell PATH"

fallback_home="$tmp_dir/home-fallback"
mkdir -p "$fallback_home/.local/bin"
cp "$ok_codex" "$fallback_home/.local/bin/codex"
chmod +x "$fallback_home/.local/bin/codex"

PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$fallback_home" \
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
grep -q "$fallback_home/.local/bin" "$tmp_dir/data-fallback/daemons/ws-fallback/daemon.env" || fail "fallback install did not persist common Codex path"

printf 'daemon installer tests passed\n'
