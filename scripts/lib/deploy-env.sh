load_notty_env_file() {
	env_file="$1"
	[ -f "$env_file" ] || return 0

	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
			''|\#*) continue ;;
		esac

		key="${line%%=*}"
		value="${line#*=}"
		case "$key" in
			''|*[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_]*)
				printf 'invalid env key in %s: %s\n' "$env_file" "$key" >&2
				return 1
				;;
		esac

		eval "is_set=\${$key+x}"
		if [ -z "$is_set" ]; then
			export "$key=$value"
		fi
	done < "$env_file"
}

load_notty_deploy_env() {
	root_dir="$1"
	load_notty_env_file "${NOTTY_DEPLOY_ENV_FILE:-$root_dir/deploy/env/prod.deploy.env}"
}
