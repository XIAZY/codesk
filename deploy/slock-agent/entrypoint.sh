#!/usr/bin/env bash
set -Eeuo pipefail

home_dir="${HOME:-/root}"
codex_home="${CODEX_HOME:-${home_dir}/.codex}"
slock_home="${SLOCK_HOME:-${home_dir}/.slock}"
npm_cache="${NPM_CONFIG_CACHE:-${slock_home}/npm-cache}"
codex_auth_source="${CODEX_AUTH_SOURCE:-/run/host-codex/auth.json}"
codex_auth_dest="${CODEX_AUTH_DEST:-${codex_home}/auth.json}"

mkdir -p "${codex_home}" "${slock_home}" "${npm_cache}" "$(dirname "${codex_auth_dest}")"
chmod 700 "${codex_home}" "${slock_home}" 2>/dev/null || true

if [ -f "${codex_auth_source}" ]; then
	tmp_auth="$(mktemp "${codex_auth_dest}.tmp.XXXXXX")"
	cp "${codex_auth_source}" "${tmp_auth}"
	chmod 600 "${tmp_auth}" 2>/dev/null || true
	mv "${tmp_auth}" "${codex_auth_dest}"
elif [ ! -f "${codex_auth_dest}" ]; then
	printf 'slock-agent: no Codex auth file found. Mount ~/.codex/auth.json at %s or pre-populate %s.\n' "${codex_auth_source}" "${codex_auth_dest}" >&2
fi

if [ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]; then
	printf 'slock-agent: GitHub CLI token is available from environment.\n' >&2
else
	printf 'slock-agent: GH_TOKEN is not set, so GitHub CLI commands that require auth will fail.\n' >&2
fi

if [ ! -S /var/run/docker.sock ]; then
	printf 'slock-agent: host Docker socket is not mounted at /var/run/docker.sock, so docker commands will not reach the host daemon.\n' >&2
fi

prune_stale_slock_locks() {
	[ "${SLOCK_PRUNE_STALE_LOCKS:-1}" = "1" ] || return 0
	[ -d "${slock_home}/machines" ] || return 0

	current_hostname="$(hostname 2>/dev/null || true)"
	find "${slock_home}/machines" -mindepth 2 -maxdepth 2 -type d -name daemon.lock 2>/dev/null | while IFS= read -r lock_dir; do
		owner_file="${lock_dir}/owner.json"
		[ -f "${owner_file}" ] || continue

		lock_pid="$(jq -r '.pid // empty' "${owner_file}" 2>/dev/null || true)"
		lock_hostname="$(jq -r '.hostname // empty' "${owner_file}" 2>/dev/null || true)"

		if [ -n "${current_hostname}" ] && [ -n "${lock_hostname}" ] && [ "${lock_hostname}" != "${current_hostname}" ]; then
			printf 'slock-agent: removing stale Slock daemon lock from previous container host=%s.\n' "${lock_hostname}" >&2
			rm -rf "${lock_dir}"
			continue
		fi

		if [ -n "${lock_pid}" ] && ! kill -0 "${lock_pid}" 2>/dev/null; then
			printf 'slock-agent: removing stale Slock daemon lock for dead pid=%s.\n' "${lock_pid}" >&2
			rm -rf "${lock_dir}"
		fi
	done
}

if [ "$#" -eq 0 ]; then
	set -- slock-daemon
fi

if [ "${1}" != "slock-daemon" ]; then
	exec "$@"
fi
shift

: "${SLOCK_API_KEY:?SLOCK_API_KEY is required. Pass it with -e SLOCK_API_KEY=... or an env file.}"
prune_stale_slock_locks

exec npx -y "@slock-ai/daemon@${SLOCK_DAEMON_VERSION:-latest}" \
	--server-url "${SLOCK_SERVER_URL:-https://api.slock.ai}" \
	--api-key "${SLOCK_API_KEY}" \
	"$@"
