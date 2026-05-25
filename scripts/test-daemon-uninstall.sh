#!/usr/bin/env sh
set -eu

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
workspace_id="ws:test/value"
daemon_name="$(printf '%s' "$workspace_id" | sed 's/[^A-Za-z0-9_.-]/-/g')"
second_workspace_id="ws:second/value"
second_daemon_name="$(printf '%s' "$second_workspace_id" | sed 's/[^A-Za-z0-9_.-]/-/g')"
tmp_home="$(mktemp -d "${TMPDIR:-/tmp}/notty-uninstall-test.XXXXXX")"
fake_bin="$tmp_home/fake-bin"
install_dir="$tmp_home/.notty/bin"
data_dir="$tmp_home/.notty"

cleanup() {
	rm -rf "$tmp_home"
}
trap cleanup EXIT INT TERM

fail() {
	printf 'uninstall test failed: %s\n' "$*" >&2
	exit 1
}

assert_missing() {
	[ ! -e "$1" ] || fail "expected missing: $1"
}

assert_exists() {
	[ -e "$1" ] || fail "expected existing: $1"
}

assert_fails() {
	if "$@" >/dev/null 2>&1; then
		fail "expected command to fail: $*"
	fi
}

mkdir -p "$fake_bin" "$install_dir" "$tmp_home/Library/LaunchAgents" "$tmp_home/.config/systemd/user"

cat > "$fake_bin/launchctl" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
cat > "$fake_bin/systemctl" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
chmod +x "$fake_bin/launchctl" "$fake_bin/systemctl"

HOME="$tmp_home" PATH="$fake_bin:$PATH" assert_fails sh "$root_dir/deploy/daemons/uninstall.sh" \
	--install-dir "$install_dir" \
	--data-dir "$data_dir"

HOME="$tmp_home" PATH="$fake_bin:$PATH" assert_fails sh "$root_dir/deploy/daemons/uninstall.sh" \
	--workspace-id "$workspace_id" \
	--install-dir "$install_dir" \
	--data-dir "$data_dir"

mkdir -p "$data_dir/daemons/$daemon_name" "$data_dir/runtime/$daemon_name"
mkdir -p "$data_dir/daemons/$second_daemon_name" "$data_dir/runtime/$second_daemon_name"
mkdir -p "$tmp_home/Notty/workspaces/$daemon_name" "$tmp_home/Notty/agents/$daemon_name"
mkdir -p "$tmp_home/Notty/workspaces/$second_daemon_name" "$tmp_home/Notty/agents/$second_daemon_name"
mkdir -p "$tmp_home/Library/LaunchAgents" "$tmp_home/.config/systemd/user"
touch "$data_dir/daemons/$daemon_name/run.sh" "$data_dir/daemons/$second_daemon_name/run.sh"
touch "$tmp_home/Library/LaunchAgents/com.notty.daemon.$daemon_name.plist"
touch "$tmp_home/Library/LaunchAgents/com.notty.daemon.$second_daemon_name.plist"
touch "$tmp_home/.config/systemd/user/notty-daemon-$daemon_name.service"
touch "$tmp_home/.config/systemd/user/notty-daemon-$second_daemon_name.service"

HOME="$tmp_home" PATH="$fake_bin:$PATH" sh "$root_dir/deploy/daemons/uninstall.sh" \
	--all \
	--install-dir "$install_dir" \
	--data-dir "$data_dir" >/dev/null

assert_missing "$data_dir/daemons"
assert_missing "$data_dir/runtime"
assert_missing "$tmp_home/Notty/workspaces"
assert_missing "$tmp_home/Notty/agents"
assert_missing "$tmp_home/Library/LaunchAgents/com.notty.daemon.$daemon_name.plist"
assert_missing "$tmp_home/Library/LaunchAgents/com.notty.daemon.$second_daemon_name.plist"
assert_missing "$tmp_home/.config/systemd/user/notty-daemon-$daemon_name.service"
assert_missing "$tmp_home/.config/systemd/user/notty-daemon-$second_daemon_name.service"
assert_missing "$install_dir/notty-daemon"
assert_missing "$install_dir/notty-agent-tool"

mkdir -p "$install_dir" "$data_dir/daemons/$daemon_name" "$tmp_home/Notty/workspaces/$daemon_name"
touch "$install_dir/notty-daemon" "$install_dir/notty-agent-tool" "$data_dir/daemons/$daemon_name/run.sh"

HOME="$tmp_home" PATH="$fake_bin:$PATH" sh "$root_dir/deploy/daemons/uninstall.sh" \
	--all \
	--keep-binaries \
	--install-dir "$install_dir" \
	--data-dir "$data_dir" >/dev/null

assert_missing "$data_dir/daemons"
assert_missing "$tmp_home/Notty/workspaces"
assert_exists "$install_dir/notty-daemon"
assert_exists "$install_dir/notty-agent-tool"

printf 'daemon uninstall script test passed\n'
