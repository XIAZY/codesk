#!/usr/bin/env sh
set -eu

static_base="${NOTTY_DAEMON_STATIC_BASE:-}"
version="latest"
install_dir="${NOTTY_INSTALL_DIR:-$HOME/.notty/bin}"
data_dir="${NOTTY_DATA_DIR:-$HOME/.notty}"
backend_url=""
workspace_id=""
daemon_token=""
no_service=0

usage() {
	cat <<'EOF'
Usage: install.sh --backend-url <url> --workspace-id <id> --daemon-token <token> [options]

Options:
  --static-base <url>   Static daemon artifact base URL.
  --version <version>   Release version. Defaults to latest.
  --install-dir <path>  Binary install directory. Defaults to ~/.notty/bin.
  --data-dir <path>     Notty data directory. Defaults to ~/.notty.
  --no-service          Install files but do not start a service.
  -h, --help            Show this help.

Environment:
  NOTTY_CODEX_COMMAND   Optional Codex executable to use. Defaults to codex.
  NOTTY_CLAUDE_COMMAND  Optional Claude Code executable to use. Defaults to claude.
EOF
}

die() {
	printf 'notty install: %s\n' "$*" >&2
	exit 1
}

warn() {
	printf 'notty install warning: %s\n' "$*" >&2
}

need_value() {
	[ "$#" -ge 2 ] || die "$1 requires a value"
	case "$2" in
		-*) die "$1 requires a value" ;;
	esac
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--backend-url)
			need_value "$1" "${2:-}"
			backend_url="$2"
			shift 2
			;;
		--workspace-id)
			need_value "$1" "${2:-}"
			workspace_id="$2"
			shift 2
			;;
		--daemon-token)
			need_value "$1" "${2:-}"
			daemon_token="$2"
			shift 2
			;;
		--static-base)
			need_value "$1" "${2:-}"
			static_base="$2"
			shift 2
			;;
		--version)
			need_value "$1" "${2:-}"
			version="$2"
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
		--no-service)
			no_service=1
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

[ -n "$backend_url" ] || die "--backend-url is required"
[ -n "$workspace_id" ] || die "--workspace-id is required"
[ -n "$daemon_token" ] || die "--daemon-token is required"
[ -n "$static_base" ] || die "--static-base is required"

static_base="$(printf '%s' "$static_base" | sed 's:/*$::')"
backend_url="$(printf '%s' "$backend_url" | sed 's:/*$::')"

case "$(uname -s)" in
	Darwin) os="darwin" ;;
	Linux) os="linux" ;;
	*) die "unsupported operating system: $(uname -s)" ;;
esac
artifact_base="$static_base"

case "$(uname -m)" in
	x86_64|amd64) arch="amd64" ;;
	arm64|aarch64) arch="arm64" ;;
	*) die "unsupported architecture: $(uname -m)" ;;
esac

download_to() {
	url="$1"
	dest="$2"
	case "$url" in
		http://*|https://*)
			case "$url" in
				*\?*) url="$url&notty_cache_bust=$(date +%s).$$" ;;
				*) url="$url?notty_cache_bust=$(date +%s).$$" ;;
			esac
			;;
	esac
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" -o "$dest"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$dest" "$url"
	else
		die "curl or wget is required"
	fi
}

checksum_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

shell_quote() {
	printf "'"
	printf '%s' "$1" | sed "s/'/'\\\\''/g"
	printf "'"
}

safe_name() {
	printf '%s' "$1" | sed 's/[^A-Za-z0-9_.-]/-/g'
}

codex_command="${NOTTY_CODEX_COMMAND:-codex}"
claude_command="${NOTTY_CLAUDE_COMMAND:-claude}"

# PATH policy: the installer's shell PATH is used to *detect* tools only.
# What persists for the daemon is the resolved tool directory plus a fixed
# list of standard directories; the effective PATH is derived from those at
# install time and re-derived at every daemon start (see run.sh below), so
# a tool that moves after install heals on the next restart. The interactive
# PATH is never snapshotted into the service environment.
#
# Kept as a single-quoted string, not a $(cat <<heredoc): macOS /bin/sh is
# bash 3.2, whose command-substitution parser mis-tracks the `)` in case
# patterns inside a heredoc and mangles the captured text.
notty_path_helpers='
notty_path_has_dir() {
	case ":$PATH:" in
		*:"$1":*) return 0 ;;
		*) return 1 ;;
	esac
}

notty_append_path_dir() {
	[ -n "${1:-}" ] || return 0
	[ -d "$1" ] || return 0
	if notty_path_has_dir "$1"; then
		return 0
	fi
	if [ -n "${PATH:-}" ]; then
		PATH="$PATH:$1"
	else
		PATH="$1"
	fi
}

notty_append_known_tool_dirs() {
	notty_append_path_dir "${HOME:-}/.local/bin"
	notty_append_path_dir "${HOME:-}/.npm-global/bin"
	notty_append_path_dir "/opt/homebrew/bin"
	notty_append_path_dir "/usr/local/bin"
	notty_append_path_dir "/usr/bin"
	notty_append_path_dir "/bin"
	notty_append_path_dir "/usr/sbin"
	notty_append_path_dir "/sbin"
	npm_prefix="$(npm config get prefix 2>/dev/null || true)"
	if [ -n "$npm_prefix" ]; then
		notty_append_path_dir "$npm_prefix/bin"
	fi
}

notty_derive_daemon_path() {
	PATH=""
	notty_append_path_dir "${NOTTY_TOOL_DIR_CODEX:-}"
	notty_append_path_dir "${NOTTY_TOOL_DIR_CLAUDE:-}"
	notty_append_known_tool_dirs
	export PATH
}
'
eval "$notty_path_helpers"

detection_path="$(
	notty_append_known_tool_dirs
	printf '%s' "$PATH"
)"

codex_tool_dir=""
case "$codex_command" in
	*/*)
		if [ -x "$codex_command" ]; then
			codex_tool_dir="$(dirname -- "$codex_command")"
		fi
		;;
	*)
		codex_resolved="$(PATH="$detection_path" command -v -- "$codex_command" 2>/dev/null || true)"
		if [ -n "$codex_resolved" ]; then
			codex_tool_dir="$(dirname -- "$codex_resolved")"
		fi
		;;
esac

claude_tool_dir=""
case "$claude_command" in
	*/*)
		if [ -x "$claude_command" ]; then
			claude_tool_dir="$(dirname -- "$claude_command")"
		fi
		;;
	*)
		claude_resolved="$(PATH="$detection_path" command -v -- "$claude_command" 2>/dev/null || true)"
		if [ -n "$claude_resolved" ]; then
			claude_tool_dir="$(dirname -- "$claude_resolved")"
		fi
		;;
esac

degraded_mode="Codex agents will be unavailable until Codex is configured; other runtimes such as Claude Code are unaffected."

check_codex() {
	case "$codex_command" in
		*/*)
			if [ ! -x "$codex_command" ]; then
				warn "Codex runtime unavailable: $codex_command is not executable. $degraded_mode Install Codex or set NOTTY_CODEX_COMMAND to the Codex executable path."
				return 0
			fi
			;;
		*)
			if ! PATH="$detection_path" command -v "$codex_command" >/dev/null 2>&1; then
				warn "Codex runtime unavailable: '$codex_command' was not found on PATH. $degraded_mode Install Codex or set NOTTY_CODEX_COMMAND to the Codex executable path."
				return 0
			fi
			;;
	esac

	if ! PATH="$detection_path" "$codex_command" --version >/dev/null 2>&1; then
		warn "Codex runtime unavailable: '$codex_command --version' did not run successfully. $degraded_mode Fix Codex to enable Codex agents."
		return 0
	fi
	if ! PATH="$detection_path" "$codex_command" app-server --help >/dev/null 2>&1; then
		warn "Codex runtime unavailable: '$codex_command' does not support 'app-server'. $degraded_mode Upgrade Codex to enable Codex agents."
		return 0
	fi
}

check_codex

check_claude() {
	case "$claude_command" in
		*/*)
			if [ ! -x "$claude_command" ]; then
				warn "Claude Code runtime unavailable: $claude_command is not executable. Claude Code agents are unavailable until it is fixed. Install Claude Code or set NOTTY_CLAUDE_COMMAND to the Claude Code executable path."
				return 0
			fi
			;;
		*)
			if ! PATH="$detection_path" command -v "$claude_command" >/dev/null 2>&1; then
				warn "Claude Code runtime unavailable: '$claude_command' was not found on PATH. Claude Code agents are unavailable until it is installed. Install Claude Code or set NOTTY_CLAUDE_COMMAND to the Claude Code executable path."
				return 0
			fi
			;;
	esac

	if ! PATH="$detection_path" "$claude_command" --version >/dev/null 2>&1; then
		warn "Claude Code runtime unavailable: '$claude_command --version' did not run successfully. Fix Claude Code to enable Claude Code agents."
		return 0
	fi
}

check_claude

daemon_service_path="$(
	NOTTY_TOOL_DIR_CODEX="$codex_tool_dir"
	NOTTY_TOOL_DIR_CLAUDE="$claude_tool_dir"
	notty_derive_daemon_path
	printf '%s' "$install_dir:$PATH"
)"
printf 'Daemon service PATH: %s\n' "$daemon_service_path"
if [ -n "$codex_tool_dir" ]; then
	printf 'Codex resolved in: %s\n' "$codex_tool_dir"
fi
if [ -n "$claude_tool_dir" ]; then
	printf 'Claude Code resolved in: %s\n' "$claude_tool_dir"
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/notty-install.XXXXXX")"
cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

if [ "$version" = "latest" ]; then
	manifest="$tmp_dir/manifest.json"
	download_to "$artifact_base/latest/manifest.json" "$manifest"
	version="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -n 1)"
	[ -n "$version" ] || die "could not determine latest daemon version"
fi

artifact="notty-daemon_${version}_${os}_${arch}.tar.gz"
version_base="$artifact_base/$version"
archive="$tmp_dir/$artifact"
sums="$tmp_dir/SHA256SUMS"

printf 'Installing Notty daemon %s for %s/%s\n' "$version" "$os" "$arch"
download_to "$version_base/SHA256SUMS" "$sums"
expected="$(awk -v file="$artifact" '$2 == file {print $1}' "$sums" | head -n 1)"
[ -n "$expected" ] || die "release $version does not contain $artifact"

download_to "$version_base/$artifact" "$archive"
actual="$(checksum_file "$archive")"
[ "$actual" = "$expected" ] || die "checksum mismatch for $artifact"

tar -xzf "$archive" -C "$tmp_dir"
package_dir="$tmp_dir/notty-daemon_${version}_${os}_${arch}"
[ -x "$package_dir/bin/notty-daemon" ] || die "archive is missing bin/notty-daemon"
[ -x "$package_dir/bin/notty-agent-tool" ] || die "archive is missing bin/notty-agent-tool"

daemon_name="$(safe_name "$workspace_id")"
daemon_dir="$data_dir/daemons/$daemon_name"
workspace_dir="$HOME/Notty/workspaces/$daemon_name"
agent_workspace_root="$HOME/Notty/agents/$daemon_name"
log_file="$daemon_dir/daemon.log"
env_file="$daemon_dir/daemon.env"
run_script="$daemon_dir/run.sh"

mkdir -p "$install_dir" "$daemon_dir" "$workspace_dir" "$agent_workspace_root"
cp "$package_dir/bin/notty-daemon" "$install_dir/.notty-daemon.$$"
cp "$package_dir/bin/notty-agent-tool" "$install_dir/.notty-agent-tool.$$"
chmod +x "$install_dir/.notty-daemon.$$" "$install_dir/.notty-agent-tool.$$"
mv "$install_dir/.notty-daemon.$$" "$install_dir/notty-daemon"
mv "$install_dir/.notty-agent-tool.$$" "$install_dir/notty-agent-tool"

{
	printf 'export NOTTY_BACKEND_URL=%s\n' "$(shell_quote "$backend_url")"
	printf 'export NOTTY_WORKSPACE_ID=%s\n' "$(shell_quote "$workspace_id")"
	printf 'export NOTTY_DAEMON_TOKEN=%s\n' "$(shell_quote "$daemon_token")"
	printf 'export NOTTY_DATA_DIR=%s\n' "$(shell_quote "$data_dir")"
	printf 'export NOTTY_WORKSPACE_DIR=%s\n' "$(shell_quote "$workspace_dir")"
	printf 'export NOTTY_AGENT_WORKSPACE_ROOT=%s\n' "$(shell_quote "$agent_workspace_root")"
	printf 'export NOTTY_CODEX_COMMAND=%s\n' "$(shell_quote "$codex_command")"
	printf 'export NOTTY_CLAUDE_COMMAND=%s\n' "$(shell_quote "$claude_command")"
	printf 'export NOTTY_TOOL_DIR_CODEX=%s\n' "$(shell_quote "$codex_tool_dir")"
	printf 'export NOTTY_TOOL_DIR_CLAUDE=%s\n' "$(shell_quote "$claude_tool_dir")"
} > "$env_file"
chmod 600 "$env_file"

# run.sh re-derives PATH on every start instead of trusting an install-time
# snapshot, so tools that move after install are found again on restart.
# Agent sessions inherit this PATH, so it is also the deliberately bounded
# tool surface for agents — do not widen it back to the interactive PATH.
{
	printf '#!/usr/bin/env sh\n'
	printf 'set -eu\n'
	printf '. %s\n' "$(shell_quote "$env_file")"
	printf 'export NOTTY_BACKEND_URL NOTTY_WORKSPACE_ID NOTTY_DAEMON_TOKEN NOTTY_DATA_DIR NOTTY_WORKSPACE_DIR NOTTY_AGENT_WORKSPACE_ROOT NOTTY_CODEX_COMMAND NOTTY_CLAUDE_COMMAND NOTTY_TOOL_DIR_CODEX NOTTY_TOOL_DIR_CLAUDE\n'
	printf '%s\n' "$notty_path_helpers"
	printf 'notty_derive_daemon_path\n'
	printf 'export PATH=%s:"$PATH"\n' "$(shell_quote "$install_dir")"
	printf 'echo "notty-daemon start: PATH=$PATH"\n'
	printf 'echo "notty-daemon start: codex=$NOTTY_CODEX_COMMAND resolved=$(command -v -- "$NOTTY_CODEX_COMMAND" 2>/dev/null || echo not-found)"\n'
	printf 'echo "notty-daemon start: claude=$NOTTY_CLAUDE_COMMAND resolved=$(command -v -- "$NOTTY_CLAUDE_COMMAND" 2>/dev/null || echo not-found)"\n'
	printf 'exec %s\n' "$(shell_quote "$install_dir/notty-daemon")"
} > "$run_script"
chmod +x "$run_script"

start_background() {
	(
		nohup "$run_script" >> "$log_file" 2>&1 &
	)
	printf 'Started daemon in background. Log: %s\n' "$log_file"
}

print_foreground_start() {
	printf 'Installed daemon binaries and config.\n'
	printf 'Start the daemon in the foreground with:\n'
	printf '  %s\n' "$run_script"
	printf 'Use that command as the foreground process when running under Docker or another supervisor.\n'
}

install_launchd() {
	label="com.notty.daemon.$daemon_name"
	plist="$HOME/Library/LaunchAgents/$label.plist"
	mkdir -p "$HOME/Library/LaunchAgents"
	cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key>
  <array><string>$run_script</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$log_file</string>
  <key>StandardErrorPath</key><string>$log_file</string>
</dict>
</plist>
EOF
	launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || true
	if launchctl bootstrap "gui/$(id -u)" "$plist" >/dev/null 2>&1; then
		launchctl kickstart -k "gui/$(id -u)/$label" >/dev/null 2>&1 || true
		printf 'Installed LaunchAgent %s. Log: %s\n' "$label" "$log_file"
	else
		warn "LaunchAgent start failed; falling back to background process"
		start_background
	fi
}

install_systemd_user() {
	service_name="notty-daemon-$daemon_name.service"
	service_dir="$HOME/.config/systemd/user"
	service_file="$service_dir/$service_name"
	mkdir -p "$service_dir"
	cat > "$service_file" <<EOF
[Unit]
Description=Notty daemon for $workspace_id

[Service]
ExecStart=$run_script
Restart=always
RestartSec=5
StandardOutput=append:$log_file
StandardError=append:$log_file

[Install]
WantedBy=default.target
EOF
	if systemctl --user daemon-reload >/dev/null 2>&1 && systemctl --user enable --now "$service_name" >/dev/null 2>&1; then
		printf 'Installed systemd user service %s. Log: %s\n' "$service_name" "$log_file"
	else
		systemctl --user disable --now "$service_name" >/dev/null 2>&1 || true
		rm -f "$service_file"
		systemctl --user daemon-reload >/dev/null 2>&1 || true
		warn "systemd user service start failed; daemon was not started"
		print_foreground_start
	fi
}

if [ "$no_service" -eq 1 ]; then
	print_foreground_start
else
	case "$os" in
		darwin)
			if command -v launchctl >/dev/null 2>&1; then
				install_launchd
			else
				start_background
			fi
			;;
		linux)
			if command -v systemctl >/dev/null 2>&1; then
				install_systemd_user
			else
				warn "systemd is unavailable; daemon was not started"
				print_foreground_start
			fi
			;;
	esac
fi

printf 'Notty daemon install complete.\n'
