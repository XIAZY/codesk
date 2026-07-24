#!/usr/bin/env sh
set -eu

[ "$#" -eq 0 ] || { printf 'upload-r2: usage: upload-r2.sh\n' >&2; exit 1; }

root_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$root_dir/scripts/lib/deploy-env.sh"
. "$root_dir/scripts/lib/testtmp.sh"
load_notty_deploy_env "$root_dir"

target="${UPLOAD_TARGET:-}"
version=
static_dist_dir="${STATIC_DIST_DIR:-$root_dir/dist/static}"
daemon_dist_root="${DAEMON_DIST_ROOT:-$static_dist_dir/daemons}"
macos_gui_dist_dir="${MACOS_GUI_DIST_DIR:-$root_dir/dist/macos-desktop}"
windows_gui_msi_root="${WINDOWS_GUI_MSI_ROOT:-$root_dir/dist/windows-gui/msi}"
tmp_dir=

resolve_source_path() {
	case "$1" in
		/*) printf '%s' "$1" ;;
		[A-Za-z]:/*|[A-Za-z]:\\*)
			if command -v cygpath >/dev/null 2>&1; then
				cygpath -u "$1"
			else
				printf '%s' "$1"
			fi
			;;
		*) printf '%s/%s' "$root_dir" "$1" ;;
	esac
}

static_dist_dir="$(resolve_source_path "$static_dist_dir")"
daemon_dist_root="$(resolve_source_path "$daemon_dist_root")"
macos_gui_dist_dir="$(resolve_source_path "$macos_gui_dist_dir")"
windows_gui_msi_root="$(resolve_source_path "$windows_gui_msi_root")"

die() {
	printf 'upload-r2: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	[ -z "$tmp_dir" ] || rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

need() {
	eval "value=\${$1:-}"
	[ -n "$value" ] || die "$1 is required"
}

need_file() {
	[ -f "$1" ] && [ ! -L "$1" ] || die "missing real file: $1"
}

need_dir() {
	[ -d "$1" ] && [ ! -L "$1" ] || die "missing real directory: $1"
}

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		die 'sha256sum or shasum is required for Windows GUI uploads'
	fi
}

assert_exact_top_level_entries() {
	assert_entries_dir="$1"
	assert_entries_expected="$2"
	assert_entries_label="$3"
	assert_entries_actual="$tmp_dir/$assert_entries_label.actual"
	{
		for assert_entries_path in \
			"$assert_entries_dir"/* \
			"$assert_entries_dir"/.[!.]* \
			"$assert_entries_dir"/..?*
		do
			[ -e "$assert_entries_path" ] || [ -L "$assert_entries_path" ] || continue
			printf '%s\n' "${assert_entries_path##*/}"
		done
	} | LC_ALL=C sort >"$assert_entries_actual"
	cmp -s "$assert_entries_expected" "$assert_entries_actual" ||
		die "$assert_entries_label inventory mismatch"
}

daemon_archive_names() {
	for daemon_os in linux darwin windows; do
		for daemon_arch in amd64 arm64; do
			case "$daemon_os" in
				windows) daemon_ext=.zip ;;
				*) daemon_ext=.tar.gz ;;
			esac
			printf 'notty-daemon_%s_%s_%s%s\n' "$version" "$daemon_os" "$daemon_arch" "$daemon_ext"
		done
	done
}

stage_daemon_release() {
	daemon_source_dir="$daemon_dist_root/$version"
	daemon_staged_dir="$tmp_dir/release"
	daemon_staged_installers="$tmp_dir/installers"
	need_dir "$daemon_source_dir"
	{
		daemon_archive_names
		printf '%s\n' manifest.json SHA256SUMS
	} | LC_ALL=C sort >"$tmp_dir/daemon-release.expected-files"
	assert_exact_top_level_entries "$daemon_source_dir" "$tmp_dir/daemon-release.expected-files" daemon-release-source-files
	mkdir "$daemon_staged_dir" "$daemon_staged_installers" ||
		die 'could not create private daemon staging directories'
	while IFS= read -r daemon_name; do
		daemon_source_file="$daemon_source_dir/$daemon_name"
		need_file "$daemon_source_file"
		[ -s "$daemon_source_file" ] || die "empty daemon release file: $daemon_source_file"
		cp "$daemon_source_file" "$daemon_staged_dir/$daemon_name" ||
			die "could not stage daemon release file: $daemon_source_file"
	done <"$tmp_dir/daemon-release.expected-files"
	for daemon_installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
		if [ -f "$daemon_dist_root/$daemon_installer" ]; then
			daemon_installer_source="$daemon_dist_root/$daemon_installer"
		else
			daemon_installer_source="$root_dir/deploy/daemons/$daemon_installer"
		fi
		need_file "$daemon_installer_source"
		[ -s "$daemon_installer_source" ] || die "empty daemon installer: $daemon_installer_source"
		cp "$daemon_installer_source" "$daemon_staged_installers/$daemon_installer" ||
			die "could not stage daemon installer: $daemon_installer_source"
	done
	command -v go >/dev/null 2>&1 || die 'go is required for daemon release validation'
	go run "$root_dir/scripts/verify-daemon-release.go" "$daemon_staged_dir" "$version" ||
		die 'daemon release preflight failed'
}

stage_windows_gui_arch() {
	stage_arch="$1"
	stage_source_dir="$windows_gui_input_root/$stage_arch"
	stage_arch_dir="$windows_gui_staged_root/$stage_arch"
	stage_msi_name="Codesk_${version}_windows_${stage_arch}.msi"

	need_dir "$stage_source_dir"
	printf '%s\n' "$stage_msi_name" SHA256SUMS provenance.json | LC_ALL=C sort >"$tmp_dir/$stage_arch.source-expected-files"
	assert_exact_top_level_entries "$stage_source_dir" "$tmp_dir/$stage_arch.source-expected-files" "$stage_arch-source-files"
	mkdir "$stage_arch_dir" || die "could not create private Windows GUI staging directory: $stage_arch_dir"
	for stage_name in "$stage_msi_name" SHA256SUMS provenance.json; do
		stage_source_file="$stage_source_dir/$stage_name"
		need_file "$stage_source_file"
		[ -s "$stage_source_file" ] || die "empty Windows GUI release file: $stage_source_file"
		cp "$stage_source_file" "$stage_arch_dir/$stage_name" ||
			die "could not stage Windows GUI release file: $stage_source_file"
	done
}

preflight_windows_gui_arch() {
	preflight_arch="$1"
	preflight_arch_dir="$windows_gui_msi_root/$preflight_arch"
	preflight_msi_name="Codesk_${version}_windows_${preflight_arch}.msi"
	preflight_msi="$preflight_arch_dir/$preflight_msi_name"
	preflight_checksums="$preflight_arch_dir/SHA256SUMS"
	preflight_provenance="$preflight_arch_dir/provenance.json"

	need_dir "$preflight_arch_dir"
	printf '%s\n' "$preflight_msi_name" SHA256SUMS provenance.json | LC_ALL=C sort >"$tmp_dir/$preflight_arch.expected-files"
	assert_exact_top_level_entries "$preflight_arch_dir" "$tmp_dir/$preflight_arch.expected-files" "$preflight_arch-files"
	for preflight_file in "$preflight_msi" "$preflight_checksums" "$preflight_provenance"; do
		need_file "$preflight_file"
		[ -s "$preflight_file" ] || die "empty Windows GUI release file: $preflight_file"
	done

	preflight_normalized="$tmp_dir/$preflight_arch.checksums"
	awk '
		{
			line = $0
			sub(/\r$/, "", line)
			hash = substr(line, 1, 64)
			separator = substr(line, 65, 2)
			name = substr(line, 67)
			if (length(hash) != 64 || hash ~ /[^0-9a-f]/ || separator != "  " ||
				name == "" || name ~ /[[:space:]]/) {
				bad = 1
				exit
			}
			print hash "\t" name
		}
		END {
			if (bad || NR != 2) exit 1
		}
	' "$preflight_checksums" >"$preflight_normalized" || die "invalid $preflight_arch SHA256SUMS"
	printf '%s\n' "$preflight_msi_name" provenance.json >"$tmp_dir/$preflight_arch.expected-checksum-files"
	cut -f 2 "$preflight_normalized" >"$tmp_dir/$preflight_arch.actual-checksum-files"
	cmp -s "$tmp_dir/$preflight_arch.expected-checksum-files" "$tmp_dir/$preflight_arch.actual-checksum-files" ||
		die "$preflight_arch SHA256SUMS inventory mismatch"

	for preflight_name in "$preflight_msi_name" provenance.json; do
		preflight_expected_hash="$(awk -F '\t' -v name="$preflight_name" '$2 == name { print $1 }' "$preflight_normalized")"
		preflight_actual_hash="$(sha256_file "$preflight_arch_dir/$preflight_name")"
		[ "$preflight_expected_hash" = "$preflight_actual_hash" ] ||
			die "$preflight_arch checksum mismatch for $preflight_name"
	done
	printf '%s\n' "$(awk -F '\t' -v name="$preflight_msi_name" '$2 == name { print $1 }' "$preflight_normalized")" >"$tmp_dir/$preflight_arch.msi-sha256"
}

windows_gui_powershell() {
	if [ -n "${WINDOWS_GUI_POWERSHELL:-}" ]; then
		printf '%s' "$WINDOWS_GUI_POWERSHELL"
	elif command -v powershell.exe >/dev/null 2>&1; then
		printf '%s' powershell.exe
	elif command -v pwsh >/dev/null 2>&1; then
		printf '%s' pwsh
	else
		die 'PowerShell is required for Windows GUI provenance validation'
	fi
}

strip_prefix_slashes() {
	printf '%s' "$1" | sed 's:^/*::; s:/*$::'
}

join_key() {
	join_key_prefix="$(strip_prefix_slashes "$1")"
	join_key_path="$(printf '%s' "$2" | sed 's:^/*::')"
	if [ -n "$join_key_prefix" ]; then
		printf '%s/%s' "$join_key_prefix" "$join_key_path"
	else
		printf '%s' "$join_key_path"
	fi
}

s3_uri() {
	s3_uri_bucket="$1"
	s3_uri_prefix="$(strip_prefix_slashes "$2")"
	if [ -n "$s3_uri_prefix" ]; then
		printf 's3://%s/%s' "$s3_uri_bucket" "$s3_uri_prefix"
	else
		printf 's3://%s' "$s3_uri_bucket"
	fi
}

content_type_for() {
	case "$1" in
		*.html) printf 'text/html; charset=utf-8' ;;
		*.css) printf 'text/css; charset=utf-8' ;;
		*.js|*.mjs) printf 'application/javascript; charset=utf-8' ;;
		*.json) printf 'application/json; charset=utf-8' ;;
		*.svg) printf 'image/svg+xml' ;;
		*.png) printf 'image/png' ;;
		*.jpg|*.jpeg) printf 'image/jpeg' ;;
		*.webp) printf 'image/webp' ;;
		*.txt|*SHA256SUMS) printf 'text/plain; charset=utf-8' ;;
		*.sh) printf 'text/x-shellscript; charset=utf-8' ;;
		*.ps1) printf 'text/plain; charset=utf-8' ;;
		*.tar.gz) printf 'application/gzip' ;;
		*.zip) printf 'application/zip' ;;
		*.msi) printf 'application/x-msi' ;;
		*.dmg) printf 'application/x-apple-diskimage' ;;
		*) printf 'application/octet-stream' ;;
	esac
}

aws_s3() {
	aws --endpoint-url "$R2_ENDPOINT_URL" s3 "$@"
}

wrangler_cmd() {
	if command -v wrangler >/dev/null 2>&1; then
		wrangler "$@"
	else
		npx wrangler "$@"
	fi
}

wrangler_put() {
	wrangler_put_bucket="$1"
	wrangler_put_key="$2"
	wrangler_put_file="$3"
	wrangler_put_cache_control="$4"
	wrangler_cmd r2 object put "$wrangler_put_bucket/$wrangler_put_key" \
		--remote \
		--file "$wrangler_put_file" \
		--content-type "$(content_type_for "$wrangler_put_file")" \
		--cache-control "$wrangler_put_cache_control" \
		--force
}

download_optional_file() {
	download_bucket="$1"
	download_key="$(strip_prefix_slashes "$2")"
	download_path="$3"
	rm -f "$download_path" "$download_path.listing" "$download_path.stdout" "$download_path.stderr"
	if [ "$uploader" = aws ]; then
		if ! aws_s3 ls "$(s3_uri "$download_bucket" "$download_key")" >"$download_path.listing"; then
			rm -f "$download_path.listing"
			return 2
		fi
		if [ ! -s "$download_path.listing" ]; then
			rm -f "$download_path.listing"
			return 1
		fi
		rm -f "$download_path.listing"
		if ! aws_s3 cp "$(s3_uri "$download_bucket" "$download_key")" "$download_path"; then
			rm -f "$download_path"
			return 2
		fi
		return 0
	fi

	if wrangler_cmd r2 object get "$download_bucket/$download_key" \
		--remote --file "$download_path" >"$download_path.stdout" 2>"$download_path.stderr"; then
		rm -f "$download_path.stdout" "$download_path.stderr"
		return 0
	fi
	if grep -Fq 'The specified key does not exist.' "$download_path.stderr"; then
		rm -f "$download_path" "$download_path.stdout" "$download_path.stderr"
		return 1
	fi
	cat "$download_path.stdout" "$download_path.stderr" >&2
	rm -f "$download_path" "$download_path.stdout" "$download_path.stderr"
	return 2
}

publication_manifest_version() {
	publication_manifest_path="$1"
	awk '
		{
			line = $0
			version_tokens += gsub(/"version"/, "", line)
			if ($0 ~ /^  "version": "(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)",$/) {
				version_lines++
				value = $0
				sub(/^  "version": "/, "", value)
				sub(/",$/, "", value)
			}
		}
		END {
			if (version_tokens != 1 || version_lines != 1) exit 1
			split(value, parts, ".")
			if (parts[1] > 255 || parts[2] > 255 || parts[3] > 65535) exit 1
			print value
		}
	' "$publication_manifest_path"
}

compare_release_versions() {
	compare_candidate="$1"
	compare_remote="$2"
	awk -v candidate="$compare_candidate" -v remote="$compare_remote" 'BEGIN {
		split(candidate, candidate_parts, ".")
		split(remote, remote_parts, ".")
		for (i = 1; i <= 3; i++) {
			if (candidate_parts[i] > remote_parts[i]) { print 1; exit }
			if (candidate_parts[i] < remote_parts[i]) { print -1; exit }
		}
		print 0
	}'
}

# Public deploys are serialized by contract. Automating or parallelizing them
# requires atomic R2 conditional admission before this read/write state machine.
guard_versioned_release() {
	guard_label="$1"
	guard_bucket="$2"
	guard_ledger_key="$3"
	guard_latest_key="$4"
	guard_candidate_manifest="$5"
	guard_conflict_policy="$6"
	guard_remote_ledger="$tmp_dir/$guard_label.remote-version-manifest.json"
	guard_remote_latest="$tmp_dir/$guard_label.remote-latest-manifest.json"
	guard_ledger_state=missing
	guard_latest_state=missing
	case "$guard_conflict_policy" in
		deny|replace-current) ;;
		*) die "$guard_label conflict policy is invalid: $guard_conflict_policy" ;;
	esac
	guard_candidate_version="$(publication_manifest_version "$guard_candidate_manifest")" ||
		die "$guard_label candidate manifest version is invalid or ambiguous"
	[ "$guard_candidate_version" = "$version" ] ||
		die "$guard_label candidate manifest version $guard_candidate_version does not match $version"

	if download_optional_file "$guard_bucket" "$guard_ledger_key" "$guard_remote_ledger"; then
		if cmp -s "$guard_remote_ledger" "$guard_candidate_manifest"; then
			guard_ledger_state=identical
		else
			guard_ledger_state=conflict
		fi
	else
		guard_download_status="$?"
		[ "$guard_download_status" -eq 1 ] || die "$guard_label version ledger could not be read"
	fi
	if download_optional_file "$guard_bucket" "$guard_latest_key" "$guard_remote_latest"; then
		guard_latest_state=present
	else
		guard_download_status="$?"
		[ "$guard_download_status" -eq 1 ] || die "$guard_label remote latest manifest could not be read"
	fi

	if [ "$guard_latest_state" = present ]; then
		guard_latest_version="$(publication_manifest_version "$guard_remote_latest")" ||
			die "$guard_label remote latest manifest version is invalid or ambiguous"
		guard_version_order="$(compare_release_versions "$version" "$guard_latest_version")"
		case "$guard_version_order" in
			-1) guard_latest_state=newer ;;
			0) guard_latest_state=equal ;;
			1) guard_latest_state=older ;;
			*) die "$guard_label version comparison returned an unexpected result" ;;
		esac
	fi

	case "$guard_ledger_state:$guard_latest_state" in
		missing:missing|missing:older)
			publication_state=fresh-publish
			;;
		identical:missing|identical:older)
			publication_state=forward-completion
			;;
		identical:equal)
			cmp -s "$guard_remote_latest" "$guard_candidate_manifest" ||
				die "$guard_label latest names release $version with a different manifest"
			publication_state=already-current
			;;
		conflict:equal)
			[ "$guard_conflict_policy" = replace-current ] ||
				die "$guard_label release $version is already published with a different manifest"
			publication_state=replace-current
			;;
		missing:equal)
			die "$guard_label latest names release $version but its version ledger is missing"
			;;
		missing:newer|identical:newer)
			die "$guard_label release $version would move latest backward from $guard_latest_version"
			;;
		conflict:*)
			die "$guard_label release $version is already published with a different manifest"
			;;
		*) die "$guard_label publication state is invalid: $guard_ledger_state:$guard_latest_state" ;;
	esac

	if [ "$publication_state" = already-current ]; then
		printf '%s release %s is already published; no writes needed\n' "$guard_label" "$version"
		exit 0
	fi
}

upload_file() {
	upload_file_bucket="$1"
	upload_file_key="$2"
	upload_file_path="$3"
	upload_file_cache_control="$4"
	need_file "$upload_file_path"
	if [ "$uploader" = aws ]; then
		aws_s3 cp "$upload_file_path" "$(s3_uri "$upload_file_bucket" "$upload_file_key")" \
			--content-type "$(content_type_for "$upload_file_path")" \
			--cache-control "$upload_file_cache_control"
	else
		wrangler_put "$upload_file_bucket" "$(strip_prefix_slashes "$upload_file_key")" "$upload_file_path" "$upload_file_cache_control"
	fi
}

upload_committed_release_dir() {
	upload_release_src="$1"
	upload_release_bucket="$2"
	upload_release_prefix="$3"
	upload_release_cache_control="$4"
	upload_release_manifest="$upload_release_src/manifest.json"
	upload_release_unsorted="$tmp_dir/committed-release-files.unsorted"
	upload_release_sorted="$tmp_dir/committed-release-files.sorted"
	need_dir "$upload_release_src"
	need_file "$upload_release_manifest"

	find "$upload_release_src" -type f >"$upload_release_unsorted" ||
		die 'could not enumerate immutable release payloads'
	LC_ALL=C sort "$upload_release_unsorted" >"$upload_release_sorted" ||
		die 'could not sort immutable release payloads'
	while IFS= read -r upload_release_file; do
		upload_release_rel="${upload_release_file#"$upload_release_src"/}"
		[ "$upload_release_rel" = manifest.json ] ||
			upload_file "$upload_release_bucket" "$(join_key "$upload_release_prefix" "$upload_release_rel")" \
				"$upload_release_file" "$upload_release_cache_control"
	done <"$upload_release_sorted"
	upload_file "$upload_release_bucket" "$(join_key "$upload_release_prefix" manifest.json)" \
		"$upload_release_manifest" "$upload_release_cache_control"
}

upload_dir() {
	upload_dir_src="$1"
	upload_dir_bucket="$2"
	upload_dir_prefix="$3"
	upload_dir_cache_control="$4"
	upload_dir_delete_mode="$5"
	need_dir "$upload_dir_src"
	if [ "$uploader" = aws ]; then
		if [ "$upload_dir_delete_mode" = delete ]; then
			aws_s3 sync "$upload_dir_src/" "$(s3_uri "$upload_dir_bucket" "$upload_dir_prefix")/" --delete --cache-control "$upload_dir_cache_control"
		else
			aws_s3 sync "$upload_dir_src/" "$(s3_uri "$upload_dir_bucket" "$upload_dir_prefix")/" --cache-control "$upload_dir_cache_control"
		fi
	else
		find "$upload_dir_src" -type f | LC_ALL=C sort | while IFS= read -r upload_dir_file; do
			upload_dir_rel="${upload_dir_file#"$upload_dir_src"/}"
			upload_dir_key="$(join_key "$upload_dir_prefix" "$upload_dir_rel")"
			printf '  %s\n' "$upload_dir_key"
			wrangler_put "$upload_dir_bucket" "$upload_dir_key" "$upload_dir_file" "$upload_dir_cache_control"
		done
	fi
}

case "$target" in
	frontend|daemon|macos-gui|windows-gui) ;;
	*) die 'UPLOAD_TARGET must be frontend, daemon, macos-gui, or windows-gui' ;;
esac

case "$target" in
	frontend)
		need R2_HOMEPAGE_BUCKET
		need R2_APP_BUCKET
		;;
	daemon)
		version="$("$root_dir/scripts/read-daemon-version.sh")"
		need R2_DAEMONS_BUCKET
		;;
	macos-gui|windows-gui)
		version="$("$root_dir/scripts/read-daemon-version.sh")"
		need R2_DESKTOP_BUCKET
		;;
esac

if command -v aws >/dev/null 2>&1 && [ -n "${R2_ENDPOINT_URL:-}" ]; then
	uploader=aws
else
	if [ -z "${CLOUDFLARE_API_TOKEN:-}" ] && [ -n "${NOTTY_CLOUDFLARE_TOKEN:-}" ]; then
		CLOUDFLARE_API_TOKEN="$NOTTY_CLOUDFLARE_TOKEN"
		export CLOUDFLARE_API_TOKEN
	fi
	[ -n "${CLOUDFLARE_API_TOKEN:-}" ] || die 'aws with R2_ENDPOINT_URL or CLOUDFLARE_API_TOKEN/NOTTY_CLOUDFLARE_TOKEN is required'
	[ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ] || die 'CLOUDFLARE_ACCOUNT_ID is required for Wrangler R2 uploads'
	command -v wrangler >/dev/null 2>&1 || command -v npx >/dev/null 2>&1 || die 'wrangler or npx is required for Cloudflare API-token uploads'
	uploader=wrangler
fi

release_cache_control="${RELEASE_CACHE_CONTROL:-public, max-age=31536000, immutable}"
daemon_release_cache_control="${DAEMON_RELEASE_CACHE_CONTROL:-public, max-age=60}"
latest_cache_control="${LATEST_CACHE_CONTROL:-public, max-age=60}"

if [ "$target" = frontend ]; then
	homepage_prefix="${R2_HOMEPAGE_PREFIX:-}"
	app_prefix="${R2_APP_PREFIX:-}"
	need_file "$static_dist_dir/homepage/index.html"
	need_file "$static_dist_dir/app/index.html"

	printf 'Uploading homepage to %s\n' "$(s3_uri "$R2_HOMEPAGE_BUCKET" "$homepage_prefix")"
	upload_dir "$static_dist_dir/homepage" "$R2_HOMEPAGE_BUCKET" "$homepage_prefix" 'public, max-age=300' delete
	if [ -z "$homepage_prefix" ] && [ "$uploader" = wrangler ]; then
		upload_file "$R2_HOMEPAGE_BUCKET" '' "$static_dist_dir/homepage/index.html" 'public, max-age=300'
	fi

	printf 'Uploading app to %s\n' "$(s3_uri "$R2_APP_BUCKET" "$app_prefix")"
	upload_dir "$static_dist_dir/app" "$R2_APP_BUCKET" "$app_prefix" "$release_cache_control" delete
	upload_file "$R2_APP_BUCKET" "$(join_key "$app_prefix" index.html)" "$static_dist_dir/app/index.html" 'public, max-age=60'
	if [ -z "$app_prefix" ] && [ "$uploader" = wrangler ]; then
		upload_file "$R2_APP_BUCKET" '' "$static_dist_dir/app/index.html" 'public, max-age=60'
	fi
fi

if [ "$target" = daemon ]; then
	tmp_dir="$(notty_test_mktemp notty-daemon-upload)"
	stage_daemon_release
	daemon_prefix="$(strip_prefix_slashes "${R2_DAEMONS_PREFIX:-daemons}")"
	guard_versioned_release daemon "$R2_DAEMONS_BUCKET" \
		"$daemon_prefix/$version/manifest.json" "$daemon_prefix/latest/manifest.json" \
		"$daemon_staged_dir/manifest.json" replace-current

	if [ "$publication_state" = fresh-publish ] || [ "$publication_state" = replace-current ]; then
		if [ "$publication_state" = replace-current ]; then
			printf 'Replacing daemon release %s in %s\n' "$version" "$(s3_uri "$R2_DAEMONS_BUCKET" "$daemon_prefix/$version")"
		else
			printf 'Uploading complete daemon release %s to %s\n' "$version" "$(s3_uri "$R2_DAEMONS_BUCKET" "$daemon_prefix/$version")"
		fi
		upload_committed_release_dir "$daemon_staged_dir" "$R2_DAEMONS_BUCKET" \
			"$daemon_prefix/$version" "$daemon_release_cache_control"
	fi

	for installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
		upload_file "$R2_DAEMONS_BUCKET" "$(join_key "$daemon_prefix" "$installer")" "$daemon_staged_installers/$installer" 'public, max-age=300'
	done
	upload_file "$R2_DAEMONS_BUCKET" "$daemon_prefix/latest/manifest.json" "$daemon_staged_dir/manifest.json" "$latest_cache_control"
fi

if [ "$target" = macos-gui ]; then
	tmp_dir="$(notty_test_mktemp notty-macos-gui-upload)"
	macos_gui_prefix="$(join_key "${R2_DESKTOP_PREFIX:-desktop}" macos)"
	version_dir="$macos_gui_dist_dir/$version"
	dmg="$version_dir/Codesk_${version}_macos_universal.dmg"
	need_file "$dmg"
	need_file "$version_dir/manifest.json"
	need_file "$version_dir/SHA256SUMS"
	guard_versioned_release macos-gui "$R2_DESKTOP_BUCKET" \
		"$macos_gui_prefix/$version/manifest.json" "$macos_gui_prefix/latest/manifest.json" \
		"$version_dir/manifest.json" deny

	if [ "$publication_state" = fresh-publish ]; then
		printf 'Uploading macOS GUI release %s to %s\n' "$version" "$(s3_uri "$R2_DESKTOP_BUCKET" "$macos_gui_prefix/$version")"
		upload_committed_release_dir "$version_dir" "$R2_DESKTOP_BUCKET" \
			"$macos_gui_prefix/$version" "$release_cache_control"
	fi
	upload_file "$R2_DESKTOP_BUCKET" "$macos_gui_prefix/latest/SHA256SUMS" "$version_dir/SHA256SUMS" "$latest_cache_control"
	upload_file "$R2_DESKTOP_BUCKET" "$macos_gui_prefix/latest/manifest.json" "$version_dir/manifest.json" "$latest_cache_control"
fi

if [ "$target" = windows-gui ]; then
	windows_gui_prefix="$(join_key "${R2_DESKTOP_PREFIX:-desktop}" windows)"
	tmp_dir="$(notty_test_mktemp notty-windows-gui-upload)"
	windows_gui_input_root="$windows_gui_msi_root"
	windows_gui_staged_root="$tmp_dir/msi"
	need_dir "$windows_gui_input_root"
	printf '%s\n' amd64 arm64 >"$tmp_dir/windows-architectures.expected"
	assert_exact_top_level_entries "$windows_gui_input_root" "$tmp_dir/windows-architectures.expected" windows-source-architectures
	mkdir "$windows_gui_staged_root" || die "could not create private Windows GUI staging root: $windows_gui_staged_root"
	for arch in amd64 arm64; do
		stage_windows_gui_arch "$arch"
	done
	windows_gui_msi_root="$windows_gui_staged_root"
	assert_exact_top_level_entries "$windows_gui_staged_root" "$tmp_dir/windows-architectures.expected" windows-staged-architectures
	for arch in amd64 arm64; do
		preflight_windows_gui_arch "$arch"
	done

	command -v git >/dev/null 2>&1 || die 'git is required for Windows GUI provenance validation'
	windows_gui_source_head="$(git -C "$root_dir" rev-parse --verify HEAD 2>/dev/null)" ||
		die 'could not resolve the Windows GUI upload source HEAD'
	windows_gui_source_base="$(git -C "$root_dir" rev-parse --verify 'HEAD^1' 2>/dev/null)" ||
		die 'could not resolve the Windows GUI upload source parent'
	for windows_gui_source_commit in "$windows_gui_source_head" "$windows_gui_source_base"; do
		printf '%s\n' "$windows_gui_source_commit" | grep -Eq '^[0-9a-f]{40}$' ||
			die 'Windows GUI upload source commits must be full lowercase object IDs'
	done
	windows_gui_ps="$(windows_gui_powershell)"
	windows_gui_ps_script="$root_dir/scripts/verify-windows-gui-upload-provenance.ps1"
	windows_gui_ps_msi_root="$windows_gui_msi_root"
	if command -v cygpath >/dev/null 2>&1; then
		windows_gui_ps_script="$(cygpath -w "$windows_gui_ps_script")"
		windows_gui_ps_msi_root="$(cygpath -w "$windows_gui_ps_msi_root")"
	fi
	MSYS2_ARG_CONV_EXCL='*' "$windows_gui_ps" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass \
		-File "$windows_gui_ps_script" \
		-MsiRoot "$windows_gui_ps_msi_root" \
		-Version "$version" \
		-SourceHead "$windows_gui_source_head" \
		-SourceBase "$windows_gui_source_base" \
		-Repository "${WINDOWS_GUI_REPOSITORY:-XIAZY/notty}" ||
		die 'Windows GUI provenance preflight failed'

	manifest="$tmp_dir/manifest.json"
	printf '{\n  "version": "%s",\n  "artifacts": [\n' "$version" >"$manifest"
	first=1
	for arch in amd64 arm64; do
		msi_name="Codesk_${version}_windows_${arch}.msi"
		sum="$(sed -n '1p' "$tmp_dir/$arch.msi-sha256")"
		if [ "$first" -eq 0 ]; then printf ',\n' >>"$manifest"; fi
		first=0
		printf '    {"os": "windows", "arch": "%s", "file": "%s/%s", "sha256": "%s"}' \
			"$arch" "$arch" "$msi_name" "$sum" >>"$manifest"
	done
	printf '\n  ]\n}\n' >>"$manifest"
	guard_versioned_release windows-gui "$R2_DESKTOP_BUCKET" \
		"$windows_gui_prefix/$version/manifest.json" "$windows_gui_prefix/latest/manifest.json" \
		"$manifest" deny

	if [ "$publication_state" = fresh-publish ]; then
		for arch in amd64 arm64; do
			arch_dir="$windows_gui_staged_root/$arch"
			msi_name="Codesk_${version}_windows_${arch}.msi"
			upload_file "$R2_DESKTOP_BUCKET" "$windows_gui_prefix/$version/$arch/$msi_name" "$arch_dir/$msi_name" "$release_cache_control"
			upload_file "$R2_DESKTOP_BUCKET" "$windows_gui_prefix/$version/$arch/SHA256SUMS" "$arch_dir/SHA256SUMS" "$release_cache_control"
			upload_file "$R2_DESKTOP_BUCKET" "$windows_gui_prefix/$version/$arch/provenance.json" "$arch_dir/provenance.json" "$release_cache_control"
		done
		upload_file "$R2_DESKTOP_BUCKET" "$windows_gui_prefix/$version/manifest.json" "$manifest" "$release_cache_control"
	fi
	upload_file "$R2_DESKTOP_BUCKET" "$windows_gui_prefix/latest/manifest.json" "$manifest" "$latest_cache_control"
	printf 'Uploaded Windows GUI release %s to %s\n' "$version" "$(s3_uri "$R2_DESKTOP_BUCKET" "$windows_gui_prefix/$version")"
fi

if [ "$target" = frontend ]; then
	printf 'Uploaded frontend assets\n'
else
	printf 'Uploaded %s assets for %s\n' "$target" "$version"
fi
