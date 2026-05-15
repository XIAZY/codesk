#!/usr/bin/env sh
set -eu

install_dir="${NOTTY_INSTALL_DIR:-$HOME/.notty/bin}"
data_dir="${NOTTY_DATA_DIR:-$HOME/.notty}"
workspace_id=""
keep_binaries=0
uninstall_all=0

usage() {
	cat <<'EOF'
Usage: uninstall.sh --workspace-id <id> [options]
       uninstall.sh --all [options]

Options:
  --all                 Uninstall every local Notty daemon and remove all local Notty-managed data.
  --install-dir <path>  Binary install directory. Defaults to ~/.notty/bin.
  --data-dir <path>     Notty data directory. Defaults to ~/.notty.
  --keep-binaries       Keep notty-daemon and notty-agent-tool even if no daemon configs remain.
  -h, --help            Show this help.
EOF
}

die() {
	printf 'notty uninstall: %s\n' "$*" >&2
	exit 1
}

need_value() {
	[ "$#" -ge 2 ] || die "$1 requires a value"
	case "$2" in
		-*) die "$1 requires a value" ;;
	esac
}

safe_name() {
	printf '%s' "$1" | sed 's/[^A-Za-z0-9_.-]/-/g'
}

remove_empty_dir() {
	rmdir "$1" >/dev/null 2>&1 || true
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--all)
			uninstall_all=1
			shift
			;;
		--workspace-id)
			need_value "$1" "${2:-}"
			workspace_id="$2"
			shift 2
			;;
		--install-dir)
			need_value "$1" "${2:-}"
			install_dir="$2"
			shift 2
			;;
		--data-dir)
			need_value "$1" "${2:-}"
			data_dir="$2"
			shift 2
			;;
		--keep-binaries)
			keep_binaries=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			die "unknown argument: $1"
			;;
	esac
done

[ "$uninstall_all" -eq 0 ] || [ -z "$workspace_id" ] || die "--all cannot be combined with --workspace-id"
[ "$uninstall_all" -eq 1 ] || [ -n "$workspace_id" ] || die "--workspace-id is required unless --all is used"

stop_launchd() {
	daemon_name="$1"
	label="com.notty.daemon.$daemon_name"
	plist="$HOME/Library/LaunchAgents/$label.plist"
	if command -v launchctl >/dev/null 2>&1; then
		launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || true
		launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1 || true
	fi
	rm -f "$plist"
	remove_empty_dir "$HOME/Library/LaunchAgents"
}

stop_systemd_user() {
	daemon_name="$1"
	service_name="notty-daemon-$daemon_name.service"
	service_file="$HOME/.config/systemd/user/$service_name"
	if command -v systemctl >/dev/null 2>&1; then
		systemctl --user disable --now "$service_name" >/dev/null 2>&1 || true
		systemctl --user daemon-reload >/dev/null 2>&1 || true
	fi
	rm -f "$service_file"
	remove_empty_dir "$HOME/.config/systemd/user"
	remove_empty_dir "$HOME/.config/systemd"
	remove_empty_dir "$HOME/.config"
}

stop_background_process() {
	run_script="$1"
	[ -n "$run_script" ] || return 0
	[ -f "$run_script" ] || return 0
	if command -v pgrep >/dev/null 2>&1; then
		for pid in $(pgrep -f "$run_script" 2>/dev/null || true); do
			[ "$pid" = "$$" ] && continue
			kill "$pid" >/dev/null 2>&1 || true
		done
	fi
}

remove_binaries() {
	rm -f "$install_dir/notty-daemon" "$install_dir/notty-agent-tool"
	remove_empty_dir "$install_dir"
}

remove_binaries_if_unused() {
	[ "$keep_binaries" -eq 0 ] || return 0
	if [ -d "$data_dir/daemons" ] && find "$data_dir/daemons" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | grep -q .; then
		return 0
	fi
	remove_binaries
}

uninstall_one() {
	daemon_name="$1"
	daemon_dir="$data_dir/daemons/$daemon_name"
	runtime_dir="$data_dir/runtime/$daemon_name"
	workspace_dir="$HOME/Notty/workspaces/$daemon_name"
	agent_workspace_root="$HOME/Notty/agents/$daemon_name"
	run_script="$daemon_dir/run.sh"

	stop_launchd "$daemon_name"
	stop_systemd_user "$daemon_name"
	stop_background_process "$run_script"

	rm -rf "$daemon_dir" "$runtime_dir" "$workspace_dir" "$agent_workspace_root"
}

if [ "$uninstall_all" -eq 1 ]; then
	if [ -d "$data_dir/daemons" ]; then
		for daemon_dir in "$data_dir"/daemons/*; do
			[ -d "$daemon_dir" ] || continue
			uninstall_one "$(basename "$daemon_dir")"
		done
	fi

	rm -rf "$data_dir/daemons" "$data_dir/runtime" "$HOME/Notty/workspaces" "$HOME/Notty/agents"
	[ "$keep_binaries" -eq 1 ] || remove_binaries
	remove_empty_dir "$data_dir"
	remove_empty_dir "$HOME/Notty"
	printf 'Notty daemon uninstall complete for all workspaces.\n'
	exit 0
fi

daemon_name="$(safe_name "$workspace_id")"
uninstall_one "$daemon_name"
remove_empty_dir "$data_dir/daemons"
remove_empty_dir "$data_dir/runtime"
remove_binaries_if_unused
remove_empty_dir "$data_dir"
remove_empty_dir "$HOME/Notty/workspaces"
remove_empty_dir "$HOME/Notty/agents"
remove_empty_dir "$HOME/Notty"

printf 'Notty daemon uninstall complete for workspace %s.\n' "$workspace_id"
