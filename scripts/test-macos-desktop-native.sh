#!/bin/sh
set -eu

usage() {
	cat >&2 <<'USAGE'
usage: scripts/test-macos-desktop-native.sh <prepare|resume> \
  <previous-release-dir> <previous-version> \
  <candidate-release-dir> <candidate-version> <evidence-dir>

The test is intentionally split across a real macOS logout/login cycle.
Run prepare, log out and back in without manually launching Codesk, then run
resume with the same arguments. A dedicated macOS test account is required.

Required environment:
  CODESK_MACOS_ACCEPT_DESTRUCTIVE=1
  CODESK_MACOS_CONNECT_DRIVER=/absolute/executable   (prepare)
  CODESK_MACOS_SYNC_DRIVER=/absolute/executable      (both phases)
  CODESK_MACOS_PROVIDER_DRIVER=/absolute/executable  (resume)

Optional construction-only environment:
  CODESK_MACOS_ACCEPT_UNSIGNED_FUNCTIONAL=1
    Exercise native functional rows with unsigned releases. Evidence from this
    mode is explicitly non-trusted and cannot establish publishability.
USAGE
	exit 2
}

[ "$#" -eq 6 ] || usage
phase="$1"
previous_release="$2"
previous_version="$3"
candidate_release="$4"
candidate_version="$5"
evidence_dir="$6"

case "$phase" in
	prepare|resume) ;;
	*) usage ;;
esac

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
. "$root_dir/scripts/lib/testtmp.sh"

fail() {
	printf 'test-macos-desktop-native: %s\n' "$*" >&2
	exit 1
}

[ "${CODESK_MACOS_ACCEPT_DESTRUCTIVE:-}" = 1 ] ||
	fail 'set CODESK_MACOS_ACCEPT_DESTRUCTIVE=1 after selecting a dedicated test account'
[ "$(uname -s)" = Darwin ] || fail 'native macOS execution is required'
[ "$(id -u)" -ne 0 ] || fail 'run as the GUI test user, never as root'

newline='
'
reject_newline() {
	case "$2" in
		*"$newline"*) fail "$1 contains a newline" ;;
	esac
}

reject_newline 'previous release path' "$previous_release"
reject_newline 'candidate release path' "$candidate_release"
reject_newline 'evidence path' "$evidence_dir"

case "$previous_release" in /*) ;; *) previous_release="$(pwd)/$previous_release" ;; esac
case "$candidate_release" in /*) ;; *) candidate_release="$(pwd)/$candidate_release" ;; esac
case "$evidence_dir" in /*) ;; *) evidence_dir="$(pwd)/$evidence_dir" ;; esac
[ -d "$previous_release" ] || fail "previous release does not exist: $previous_release"
[ -d "$candidate_release" ] || fail "candidate release does not exist: $candidate_release"
previous_release="$(CDPATH= cd -- "$previous_release" && pwd -P)"
candidate_release="$(CDPATH= cd -- "$candidate_release" && pwd -P)"
[ "$previous_release" != "$candidate_release" ] || fail 'previous and candidate release directories must differ'
[ "$previous_version" != "$candidate_version" ] || fail 'previous and candidate versions must differ'

evidence_parent="$(dirname -- "$evidence_dir")"
evidence_base="$(basename -- "$evidence_dir")"
[ -d "$evidence_parent" ] || fail 'the evidence parent directory must already exist'
case "$evidence_base" in ''|.|..) fail 'invalid evidence directory name' ;; esac
evidence_parent="$(CDPATH= cd -- "$evidence_parent" && pwd -P)"
evidence_dir="$evidence_parent/$evidence_base"
case "$evidence_dir/" in
	"$root_dir/"*) fail 'evidence directory must be outside the source checkout' ;;
esac

data_dir="$HOME/Library/Application Support/Codesk"
logs_dir="$HOME/Library/Logs/Codesk"
cache_dir="$HOME/Library/Caches/Codesk"
legacy_dir="$HOME/.notty"
installed_app='/Applications/Codesk.app'
app_executable="$installed_app/Contents/MacOS/Codesk"
helper_executable="$installed_app/Contents/Helpers/notty-agent-tool"
keychain_service='com.getcodesk.desktop'
keychain_account='codesk:daemon-token'

case "$evidence_dir/" in
	"$data_dir/"*|"$logs_dir/"*|"$cache_dir/"*)
		fail 'evidence directory must be outside Codesk data, log, and cache roots'
		;;
esac

for command in awk codesign date ditto env git go grep hdiutil id lipo lsof open osascript plutil ps security shasum sort spctl stat sw_vers sysctl tar xcrun; do
	command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done

os_major="$(sw_vers -productVersion | awk -F. '{print $1}')"
case "$os_major" in ''|*[!0-9]*) fail 'could not determine the macOS major version' ;; esac
[ "$os_major" -ge 13 ] || fail 'macOS 13 or later is required'

host_arch="$(uname -m)"
case "$host_arch" in
	arm64|x86_64) ;;
	*) fail "unsupported native architecture $host_arch" ;;
esac
translated="$(sysctl -in sysctl.proc_translated 2>/dev/null || printf '0')"
[ "$translated" != 1 ] || fail 'the acceptance shell is translated by Rosetta; use a native shell'
has_arm64="$(sysctl -n hw.optional.arm64 2>/dev/null || printf '0')"
if [ "$has_arm64" = 1 ]; then
	[ "$host_arch" = arm64 ] || fail 'Apple Silicon acceptance must run as arm64'
else
	[ "$host_arch" = x86_64 ] || fail 'Intel acceptance must run as x86_64'
fi

timeout_seconds="${CODESK_MACOS_ACCEPT_TIMEOUT:-180}"
case "$timeout_seconds" in ''|*[!0-9]*) fail 'CODESK_MACOS_ACCEPT_TIMEOUT must be an integer' ;; esac
[ "$timeout_seconds" -ge 30 ] || fail 'CODESK_MACOS_ACCEPT_TIMEOUT must be at least 30 seconds'

case "${CODESK_MACOS_ACCEPT_UNSIGNED_FUNCTIONAL:-}" in
	"")
		evidence_scope='native-acceptance'
		artifact_trust='verified'
		publishable='requires-intel-arm64-review'
		;;
	1)
		evidence_scope='native-functional-only'
		artifact_trust='NOT_ESTABLISHED'
		publishable='false'
		;;
	*) fail 'CODESK_MACOS_ACCEPT_UNSIGNED_FUNCTIONAL must be unset or exactly 1' ;;
esac

require_driver() {
	name="$1"
	value="$2"
	[ -n "$value" ] || fail "$name is required for phase $phase"
	reject_newline "$name" "$value"
	case "$value" in /*) ;; *) fail "$name must be an absolute path" ;; esac
	[ -f "$value" ] && [ ! -L "$value" ] && [ -x "$value" ] ||
		fail "$name must be a real executable file"
}

connect_driver="${CODESK_MACOS_CONNECT_DRIVER:-}"
sync_driver="${CODESK_MACOS_SYNC_DRIVER:-}"
provider_driver="${CODESK_MACOS_PROVIDER_DRIVER:-}"
[ "$phase" != prepare ] || require_driver CODESK_MACOS_CONNECT_DRIVER "$connect_driver"
require_driver CODESK_MACOS_SYNC_DRIVER "$sync_driver"
[ "$phase" != resume ] || require_driver CODESK_MACOS_PROVIDER_DRIVER "$provider_driver"

umask 077
source_revision="$(git -C "$root_dir" rev-parse --verify HEAD)"
if [ "$phase" = prepare ]; then
	[ ! -e "$evidence_dir" ] || fail "prepare requires a new evidence directory: $evidence_dir"
	mkdir -p "$evidence_dir"
	chmod 0700 "$evidence_dir"
else
	[ -d "$evidence_dir" ] && [ ! -L "$evidence_dir" ] || fail 'resume requires the prepare evidence directory'
	evidence_dir="$(CDPATH= cd -- "$evidence_dir" && pwd -P)"
fi
transcript="$evidence_dir/transcript.log"
[ "$phase" != prepare ] || : >"$transcript"

record() {
	printf '%s\n' "$*" | tee -a "$transcript" >&2
}

run_public() {
	record "RUN $*"
	"$@" >>"$transcript" 2>&1
}

write_value() {
	name="$1"
	value="$2"
	reject_newline "$name" "$value"
	printf '%s\n' "$value" >"$evidence_dir/$name"
}

read_value() {
	name="$1"
	[ -f "$evidence_dir/$name" ] && [ ! -L "$evidence_dir/$name" ] || fail "missing evidence value $name"
	value="$(cat "$evidence_dir/$name")"
	reject_newline "$name" "$value"
	printf '%s' "$value"
}

hash_file() {
	shasum -a 256 "$1" | awk '{print $1}'
}

resolve_driver_path() {
	driver="$1"
	directory="$(CDPATH= cd -- "$(dirname -- "$driver")" && pwd -P)"
	resolved="$directory/$(basename -- "$driver")"
	[ -f "$resolved" ] && [ ! -L "$resolved" ] && [ -x "$resolved" ] || fail "resolved driver is not a real executable: $resolved"
	printf '%s' "$resolved"
}

connect_driver_hash=''
provider_driver_hash=''
if [ -n "$connect_driver" ]; then
	connect_driver="$(resolve_driver_path "$connect_driver")"
	connect_driver_hash="$(hash_file "$connect_driver")"
fi
sync_driver="$(resolve_driver_path "$sync_driver")"
sync_driver_hash="$(hash_file "$sync_driver")"
if [ -n "$provider_driver" ]; then
	provider_driver="$(resolve_driver_path "$provider_driver")"
	provider_driver_hash="$(hash_file "$provider_driver")"
fi

expected_driver_hash() {
	driver="$1"
	case "$driver" in
		"$connect_driver") [ -n "$connect_driver_hash" ] && printf '%s' "$connect_driver_hash" || fail 'connect driver identity is unavailable' ;;
		"$sync_driver") printf '%s' "$sync_driver_hash" ;;
		"$provider_driver") [ -n "$provider_driver_hash" ] && printf '%s' "$provider_driver_hash" || fail 'provider driver identity is unavailable' ;;
		*) fail "driver path is not bound to this run: $driver" ;;
	esac
}

path_fingerprint() {
	path="$1"
	if [ ! -e "$path" ] && [ ! -L "$path" ]; then
		printf 'ABSENT'
		return
	fi
	[ ! -L "$path" ] || fail "refusing to fingerprint symlink $path"
	parent="$(dirname -- "$path")"
	base="$(basename -- "$path")"
	(
		cd "$parent"
		tar -cf - "$base"
	) 2>/dev/null | shasum -a 256 | awk '{print $1}'
}

manifest_value() {
	plutil -extract "$2" raw -o - "$1/manifest.json"
}

tmp_dir="$(notty_test_mktemp codesk-macos-native)"
native_release_tool="$tmp_dir/codesk-macos-release"
mount_dir=''
mounted=0
replacement_stage=''
replacement_backup=''
replacement_active=0

remove_guarded_app_path() {
	path="$1"
	case "$path" in
		/Applications/Codesk.app|/Applications/.Codesk.app.acceptance.*|/Applications/.Codesk.app.backup.*) ;;
		*) fail "refusing to remove unexpected application path $path" ;;
	esac
	rm -rf "$path"
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	if [ "$mounted" -eq 1 ]; then
		hdiutil detach "$mount_dir" -quiet >/dev/null 2>&1 || status=1
	fi
	if [ "$replacement_active" -eq 1 ]; then
		remove_guarded_app_path "$installed_app"
		if [ -d "$replacement_backup" ]; then
			mv "$replacement_backup" "$installed_app" || status=1
		fi
	fi
	if [ -n "$replacement_stage" ] && [ -e "$replacement_stage" ]; then
		remove_guarded_app_path "$replacement_stage"
	fi
	rm -rf "$tmp_dir"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

record 'RUN build native macOS release verifier for installed-tree checks'
(
	cd "$root_dir"
	env GOTOOLCHAIN=go1.26.5 GOENV=off GO111MODULE=on GOWORK=off GOFLAGS= GOEXPERIMENT= \
		go build -buildvcs=false -trimpath -ldflags '-buildid=' \
		-o "$native_release_tool" ./daemon/cmd/codesk-macos-release
) >>"$transcript" 2>&1 || fail 'could not build the native macOS release verifier'

verify_release() {
	release="$1"
	version="$2"
	[ "$(manifest_value "$release" version)" = "$version" ] || fail "manifest version mismatch for $release"
	manifest_signed="$(manifest_value "$release" signed_and_notarized)"
	if [ "$evidence_scope" = native-functional-only ]; then
		[ "$manifest_signed" = false ] || fail "unsigned functional mode requires an unsigned manifest: $release"
		run_public env ALLOW_UNSIGNED_MACOS_DESKTOP=1 "$root_dir/scripts/verify-macos-desktop-release.sh" "$release" "$version"
		record "SKIP TRUST unsigned functional release has no signing, notarization, stapling, or Gatekeeper evidence: $release"
	else
		[ "$manifest_signed" = true ] || fail "release is not signed and notarized: $release"
		run_public env ALLOW_UNSIGNED_MACOS_DESKTOP= "$root_dir/scripts/verify-macos-desktop-release.sh" "$release" "$version"
		record "PASS TRUST signed, notarized, stapled, and Gatekeeper-assessed release: $release"
	fi
}

verify_release "$previous_release" "$previous_version"
verify_release "$candidate_release" "$candidate_version"

candidate_revision="$(manifest_value "$candidate_release" source_revision)"
[ "$candidate_revision" = "$source_revision" ] ||
	fail "candidate source $candidate_revision does not match checkout $source_revision"

previous_manifest_hash="$(hash_file "$previous_release/manifest.json")"
candidate_manifest_hash="$(hash_file "$candidate_release/manifest.json")"
previous_dmg_hash="$(manifest_value "$previous_release" disk_image.sha256)"
candidate_dmg_hash="$(manifest_value "$candidate_release" disk_image.sha256)"
previous_dmg_size="$(manifest_value "$previous_release" disk_image.size)"
candidate_dmg_size="$(manifest_value "$candidate_release" disk_image.size)"
previous_app_tree_hash="$(manifest_value "$previous_release" application.tree_sha256)"
candidate_app_tree_hash="$(manifest_value "$candidate_release" application.tree_sha256)"
candidate_snapshot_dir="$evidence_dir/candidate-install-snapshot"
candidate_snapshot_dmg="$candidate_snapshot_dir/Codesk_${candidate_version}_macos_universal.dmg"

if [ "$phase" = prepare ]; then
	mkdir "$candidate_snapshot_dir"
	chmod 0700 "$candidate_snapshot_dir"
	candidate_source_dmg="$candidate_release/Codesk_${candidate_version}_macos_universal.dmg"
	[ -f "$candidate_source_dmg" ] && [ ! -L "$candidate_source_dmg" ] || fail 'candidate DMG must be a real file before snapshotting'
	[ "$(hash_file "$candidate_source_dmg")" = "$candidate_dmg_hash" ] || fail 'candidate DMG changed before snapshotting'
	[ "$(stat -f '%z' "$candidate_source_dmg")" = "$candidate_dmg_size" ] || fail 'candidate DMG size changed before snapshotting'
	ditto "$candidate_source_dmg" "$candidate_snapshot_dmg"
	[ -f "$candidate_snapshot_dmg" ] && [ ! -L "$candidate_snapshot_dmg" ] || fail 'candidate DMG snapshot is not a real file'
	[ "$(hash_file "$candidate_snapshot_dmg")" = "$candidate_dmg_hash" ] || fail 'candidate DMG snapshot hash mismatch'
	[ "$(stat -f '%z' "$candidate_snapshot_dmg")" = "$candidate_dmg_size" ] || fail 'candidate DMG snapshot size mismatch'
	[ "$(hash_file "$candidate_source_dmg")" = "$candidate_dmg_hash" ] || fail 'candidate DMG changed while snapshotting'
	[ "$(stat -f '%z' "$candidate_source_dmg")" = "$candidate_dmg_size" ] || fail 'candidate DMG size changed while snapshotting'
	chmod 0400 "$candidate_snapshot_dmg"
	chmod 0500 "$candidate_snapshot_dir"
	record "PASS private read-only candidate DMG snapshot sha256=$candidate_dmg_hash size=$candidate_dmg_size"

	write_value schema 4
	write_value evidence-scope "$evidence_scope"
	write_value uid "$(id -u)"
	write_value host-arch "$host_arch"
	write_value previous-release "$previous_release"
	write_value previous-version "$previous_version"
	write_value previous-manifest-sha256 "$previous_manifest_hash"
	write_value previous-dmg-sha256 "$previous_dmg_hash"
	write_value previous-dmg-size "$previous_dmg_size"
	write_value previous-app-tree-sha256 "$previous_app_tree_hash"
	write_value candidate-release "$candidate_release"
	write_value candidate-version "$candidate_version"
	write_value candidate-source-revision "$candidate_revision"
	write_value candidate-manifest-sha256 "$candidate_manifest_hash"
	write_value candidate-dmg-sha256 "$candidate_dmg_hash"
	write_value candidate-dmg-size "$candidate_dmg_size"
	write_value candidate-app-tree-sha256 "$candidate_app_tree_hash"
	write_value connect-driver-path "$connect_driver"
	write_value connect-driver-sha256 "$connect_driver_hash"
	write_value sync-driver-path "$sync_driver"
	write_value sync-driver-sha256 "$sync_driver_hash"
	write_value legacy-before "$(path_fingerprint "$legacy_dir")"
	write_value stage preparing
	record "Codesk macOS native acceptance PREPARE"
	record "source_revision=$candidate_revision"
	record "host_arch=$host_arch"
	record "macOS=$(sw_vers -productVersion)"
	record "hardware_model=$(sysctl -n hw.model 2>/dev/null || printf unknown)"
	record "evidence_scope=$evidence_scope artifact_trust=$artifact_trust publishable=$publishable"
	record "driver connect path=$connect_driver sha256=$connect_driver_hash"
	record "driver sync path=$sync_driver sha256=$sync_driver_hash"
	if [ "$evidence_scope" = native-functional-only ]; then
		record 'WARNING: unsigned native functional evidence cannot establish trust, publishability, signing, notarization, stapling, or Gatekeeper acceptance.'
	fi
else
	[ "$(read_value schema)" = 4 ] || fail 'unsupported evidence schema'
	[ "$(read_value evidence-scope)" = "$evidence_scope" ] || fail 'resume evidence scope differs from prepare'
	[ "$(read_value uid)" = "$(id -u)" ] || fail 'resume must run as the prepare user'
	[ "$(read_value host-arch)" = "$host_arch" ] || fail 'resume architecture differs from prepare'
	[ "$(read_value previous-release)" = "$previous_release" ] || fail 'previous release path differs from prepare'
	[ "$(read_value previous-version)" = "$previous_version" ] || fail 'previous version differs from prepare'
	[ "$(read_value previous-manifest-sha256)" = "$previous_manifest_hash" ] || fail 'previous manifest changed after prepare'
	[ "$(read_value previous-dmg-sha256)" = "$previous_dmg_hash" ] || fail 'previous DMG changed after prepare'
	[ "$(read_value previous-dmg-size)" = "$previous_dmg_size" ] || fail 'previous DMG size changed after prepare'
	[ "$(read_value previous-app-tree-sha256)" = "$previous_app_tree_hash" ] || fail 'previous app tree changed after prepare'
	[ "$(read_value candidate-release)" = "$candidate_release" ] || fail 'candidate release path differs from prepare'
	[ "$(read_value candidate-version)" = "$candidate_version" ] || fail 'candidate version differs from prepare'
	[ "$(read_value candidate-source-revision)" = "$candidate_revision" ] || fail 'candidate source differs from prepare'
	[ "$(read_value candidate-manifest-sha256)" = "$candidate_manifest_hash" ] || fail 'candidate manifest changed after prepare'
	[ "$(read_value candidate-dmg-sha256)" = "$candidate_dmg_hash" ] || fail 'candidate DMG changed after prepare'
	[ "$(read_value candidate-dmg-size)" = "$candidate_dmg_size" ] || fail 'candidate DMG size changed after prepare'
	[ "$(read_value candidate-app-tree-sha256)" = "$candidate_app_tree_hash" ] || fail 'candidate app tree changed after prepare'
	[ -d "$candidate_snapshot_dir" ] && [ ! -L "$candidate_snapshot_dir" ] || fail 'candidate install snapshot directory is invalid'
	[ "$(stat -f '%Lp' "$candidate_snapshot_dir")" = 500 ] || fail 'candidate install snapshot directory is writable'
	[ -f "$candidate_snapshot_dmg" ] && [ ! -L "$candidate_snapshot_dmg" ] || fail 'candidate install snapshot DMG is invalid'
	[ "$(stat -f '%Lp' "$candidate_snapshot_dmg")" = 400 ] || fail 'candidate install snapshot DMG is writable'
	[ "$(hash_file "$candidate_snapshot_dmg")" = "$candidate_dmg_hash" ] || fail 'candidate install snapshot DMG changed after prepare'
	[ "$(stat -f '%z' "$candidate_snapshot_dmg")" = "$candidate_dmg_size" ] || fail 'candidate install snapshot DMG size changed after prepare'
	[ "$(read_value sync-driver-path)" = "$sync_driver" ] || fail 'sync driver path changed after prepare'
	[ "$(read_value sync-driver-sha256)" = "$sync_driver_hash" ] || fail 'sync driver bytes changed after prepare'
	[ "$(read_value stage)" = awaiting-login ] || fail 'prepare did not reach the login-cycle boundary'
	write_value provider-driver-path "$provider_driver"
	write_value provider-driver-sha256 "$provider_driver_hash"
	record "Codesk macOS native acceptance RESUME"
	record "evidence_scope=$evidence_scope artifact_trust=$artifact_trust publishable=$publishable"
	record "driver sync path=$sync_driver sha256=$sync_driver_hash (revalidated after logout/login)"
	record "driver provider path=$provider_driver sha256=$provider_driver_hash"
fi

verify_installed_app() {
	version="$1"
	expected_tree_hash="$2"
	[ -d "$installed_app" ] && [ ! -L "$installed_app" ] || fail 'Codesk.app is not installed as a real directory'
	if [ "$version" = dev ]; then
		installed_tree_hash="$("$native_release_tool" verify-app --print-tree-hash --app "$installed_app" --version "$version" --development)" ||
			fail 'installed application failed structural verification'
	else
		installed_tree_hash="$("$native_release_tool" verify-app --print-tree-hash --app "$installed_app" --version "$version")" ||
			fail 'installed application failed structural verification'
	fi
	[ "$installed_tree_hash" = "$expected_tree_hash" ] || fail 'installed application tree does not match the persisted manifest tree'
	if [ "$evidence_scope" = native-functional-only ]; then
		record 'SKIP TRUST installed unsigned app has no codesign, stapler, or Gatekeeper evidence'
	else
		run_public codesign --verify --strict --verbose=4 "$helper_executable"
		run_public codesign --verify --strict --verbose=4 "$installed_app"
		run_public xcrun stapler validate "$installed_app"
		run_public spctl --assess --type execute --verbose=4 "$installed_app"
	fi
	lipo "$app_executable" -verify_arch x86_64 arm64
	lipo "$helper_executable" -verify_arch x86_64 arm64
	[ "$(plutil -extract CFBundleIdentifier raw -o - "$installed_app/Contents/Info.plist")" = com.getcodesk.desktop ] || fail 'installed bundle identifier mismatch'
	[ "$(plutil -extract CFBundleShortVersionString raw -o - "$installed_app/Contents/Info.plist")" = "$version" ] || fail 'installed version mismatch'
	[ "$(plutil -extract LSMinimumSystemVersion raw -o - "$installed_app/Contents/Info.plist")" = 13.0 ] || fail 'minimum macOS version mismatch'
	[ "$(plutil -extract LSUIElement raw -o - "$installed_app/Contents/Info.plist")" = true ] || fail 'LSUIElement is not enabled'
	[ "$(plutil -extract LSMultipleInstancesProhibited raw -o - "$installed_app/Contents/Info.plist")" = true ] || fail 'LSMultipleInstancesProhibited is not enabled'
}

assert_dmg_identity() {
	dmg_path="$1"
	expected_dmg_hash="$2"
	expected_dmg_size="$3"
	[ -f "$dmg_path" ] && [ ! -L "$dmg_path" ] || fail "installation DMG is not a real file: $dmg_path"
	[ "$(hash_file "$dmg_path")" = "$expected_dmg_hash" ] || fail "installation DMG hash mismatch: $dmg_path"
	[ "$(stat -f '%z' "$dmg_path")" = "$expected_dmg_size" ] || fail "installation DMG size mismatch: $dmg_path"
}

verified_app_tree_hash() {
	app_path="$1"
	version="$2"
	if [ "$version" = dev ]; then
		"$native_release_tool" verify-app --print-tree-hash --app "$app_path" --version "$version" --development
	else
		"$native_release_tool" verify-app --print-tree-hash --app "$app_path" --version "$version"
	fi
}

install_from_dmg() {
	dmg="$1"
	version="$2"
	label="$3"
	expected_dmg_hash="$4"
	expected_dmg_size="$5"
	expected_tree_hash="$6"
	assert_dmg_identity "$dmg" "$expected_dmg_hash" "$expected_dmg_size"
	mount_dir="$tmp_dir/mount-$label"
	mkdir -p "$mount_dir"
	hdiutil attach -quiet -readonly -nobrowse -mountpoint "$mount_dir" "$dmg"
	mounted=1
	"$native_release_tool" verify-volume --mount "$mount_dir" || fail 'mounted DMG inventory is invalid'
	mounted_tree_hash="$(verified_app_tree_hash "$mount_dir/Codesk.app" "$version")" || fail 'mounted DMG app failed structural verification'
	[ "$mounted_tree_hash" = "$expected_tree_hash" ] || fail 'mounted DMG app tree does not match the persisted manifest tree'
	assert_dmg_identity "$dmg" "$expected_dmg_hash" "$expected_dmg_size"

	replacement_stage="/Applications/.Codesk.app.acceptance.$$"
	replacement_backup="/Applications/.Codesk.app.backup.$$"
	[ ! -e "$replacement_stage" ] && [ ! -e "$replacement_backup" ] || fail 'stale acceptance replacement path exists'
	ditto "$mount_dir/Codesk.app" "$replacement_stage"
	if [ -e "$installed_app" ] || [ -L "$installed_app" ]; then
		[ -d "$installed_app" ] && [ ! -L "$installed_app" ] || fail 'existing Codesk.app is not a real directory'
		mv "$installed_app" "$replacement_backup"
	fi
	replacement_active=1
	mv "$replacement_stage" "$installed_app"
	replacement_stage=''
	verify_installed_app "$version" "$expected_tree_hash"
	assert_dmg_identity "$dmg" "$expected_dmg_hash" "$expected_dmg_size"
	hdiutil detach "$mount_dir" -quiet
	mounted=0
	if [ "$replacement_active" -eq 1 ]; then
		remove_guarded_app_path "$replacement_backup"
		replacement_active=0
	fi
	replacement_backup=''
	record "PASS installed $version from exact DMG sha256=$expected_dmg_hash size=$expected_dmg_size tree_sha256=$expected_tree_hash ($label) artifact_trust=$artifact_trust"
}

codesk_main_pids() {
	ps -axo pid=,command= | while read -r pid command; do
		case "$command" in
			"$app_executable"|"$app_executable -psn_"*) printf '%s\n' "$pid" ;;
		esac
	done
}

codesk_all_pids() {
	ps -axo pid=,command= | while read -r pid command; do
		case "$command" in
			"$app_executable"|"$app_executable "*) printf '%s\n' "$pid" ;;
		esac
	done
}

watchdog_pid_for_app() {
	owner_pid="$1"
	watchdog_pids="$(ps -axo pid=,ppid=,command= | while read -r pid ppid command; do
		case "$command" in
			"$app_executable --codesk-process-watchdog $owner_pid $owner_pid 3")
				[ "$ppid" = "$owner_pid" ] || fail "Codesk watchdog $pid has parent $ppid, want $owner_pid"
				printf '%s\n' "$pid"
				;;
		esac
	done)"
	[ "$(printf '%s\n' "$watchdog_pids" | pid_count)" -eq 1 ] || fail "expected exactly one watchdog for Codesk pid=$owner_pid"
	printf '%s' "$watchdog_pids"
}

pid_count() {
	awk 'NF { count++ } END { print count + 0 }'
}

wait_for_single_app() {
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		pids="$(codesk_main_pids)"
		if [ "$(printf '%s\n' "$pids" | pid_count)" -eq 1 ]; then
			printf '%s' "$pids"
			return
		fi
		count=$((count + 1))
		sleep 1
	done
	fail 'timed out waiting for exactly one Codesk application process'
}

wait_for_no_codesk() {
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		[ -z "$(codesk_all_pids)" ] && return
		count=$((count + 1))
		sleep 1
	done
	fail 'Codesk application or watchdog process remained after exit'
}

process_group() {
	ps -p "$1" -o pgid= | awk '{print $1}'
}

wait_for_empty_group() {
	group="$1"
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		if ! ps -axo pgid= | awk -v group="$group" '$1 == group { found=1 } END { exit found ? 0 : 1 }'; then
			return
		fi
		count=$((count + 1))
		sleep 1
	done
	fail "process group $group survived its Codesk owner"
}

assert_native_process() {
	pid="$1"
	group="$(process_group "$pid")"
	[ "$group" = "$pid" ] || fail "Codesk PID $pid is not its process-group leader (group $group)"
	process_arch="$(ps -p "$pid" -o arch= | awk '{$1=$1; print}')"
	[ "$process_arch" = "$host_arch" ] || fail "Codesk process architecture $process_arch does not match native host $host_arch"
	record "PASS native Codesk process pid=$pid pgid=$group arch=$process_arch"
}

assert_no_dock_or_windows() {
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		if state="$(osascript <<'APPLESCRIPT'
tell application "System Events"
    if not (exists process "Codesk") then error "Codesk accessibility process not found"
    tell process "Codesk"
        return (background only as string) & ":" & (count of windows as string)
    end tell
end tell
APPLESCRIPT
)" 2>>"$transcript"; then
			[ "$state" = true:0 ] || fail "Codesk is not a windowless background UI element (state $state)"
			record 'PASS LSUIElement process has no Dock presence or application windows'
			return
		fi
		count=$((count + 1))
		sleep 1
	done
	fail 'System Events could not inspect Codesk; grant Accessibility access to the acceptance terminal'
}

menu_control() {
	operation="$1"
	title="$2"
	output="$(osascript - "$operation" "$title" <<'APPLESCRIPT'
on run arguments
    set requestedOperation to item 1 of arguments
    set requestedTitle to item 2 of arguments
    tell application "System Events"
        if not (exists process "Codesk") then error "Codesk process not found"
        tell process "Codesk"
            repeat with barReference in menu bars
                repeat with statusReference in menu bar items of barReference
                    set statusDescription to ""
                    try
                        set statusDescription to description of statusReference as text
                    end try
                    if statusDescription starts with "Codesk" then
                        click statusReference
                        delay 0.25
                        if not (exists menu 1 of statusReference) then error "Codesk status menu did not open"
                        if not (exists menu item requestedTitle of menu 1 of statusReference) then
                            key code 53
                            error "Codesk menu item not found: " & requestedTitle
                        end if
                        set requestedItem to menu item requestedTitle of menu 1 of statusReference
                        if requestedOperation is "click" then
                            click requestedItem
                            return "clicked"
                        end if
                        if requestedOperation is "exists" then
                            key code 53
                            return "exists"
                        end if
                        if requestedOperation is "mark" then
                            set markCharacter to missing value
                            try
                                set markCharacter to value of attribute "AXMenuItemMarkChar" of requestedItem
                            end try
                            key code 53
                            if markCharacter is missing value or markCharacter is "" then return "unmarked"
                            return "marked"
                        end if
                        key code 53
                        error "unknown menu operation"
                    end if
                end repeat
            end repeat
        end tell
    end tell
    error "Codesk status item was not found"
end run
APPLESCRIPT
)" 2>>"$transcript" || return 1
	printf '%s' "$output"
}

menu_click() {
	output="$(menu_control click "$1")" ||
		fail "could not click Codesk menu item $1; grant Accessibility access to the acceptance terminal"
	[ "$output" = clicked ] || fail "menu click failed: $1"
}

assert_menu_item() {
	title="$1"
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		output="$(menu_control exists "$title")" || output=''
		if [ "$output" = exists ]; then
			return
		fi
		count=$((count + 1))
		sleep 1
	done
	fail "menu item is absent or inaccessible: $title"
}

menu_mark_state() {
	output="$(menu_control mark "$1")" || return 1
	case "$output" in
		marked|unmarked) printf '%s' "$output" ;;
		*) return 1 ;;
	esac
}

wait_login_enabled() {
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		state="$(menu_mark_state 'Launch at login')" || state=unknown
		if [ "$state" = marked ]; then
			record 'PASS SMAppService launch-at-login enabled and menu state verified'
			return
		fi
		if [ "$state" = unmarked ]; then
			menu_click 'Launch at login'
		fi
		count=$((count + 5))
		sleep 5
	done
	fail 'launch at login did not become enabled; approve Codesk in System Settings > General > Login Items'
}

wait_login_disabled() {
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		state="$(menu_mark_state 'Launch at login')" || state=unknown
		if [ "$state" = unmarked ]; then
			record 'PASS SMAppService launch-at-login disabled and menu state verified'
			return
		fi
		count=$((count + 1))
		sleep 1
	done
	fail 'launch at login remained enabled'
}

launch_app() {
	launch_log="$tmp_dir/launch-app.log"
	record "RUN bounded Codesk launch timeout=${timeout_seconds}s path=$installed_app"
	run_bounded_to_log 'Codesk LaunchServices launch' "$launch_log" open -n "$installed_app"
	[ "$bounded_status" -eq 0 ] || fail "Codesk LaunchServices launch exited $bounded_status; inspect $launch_log"
	pid="$(wait_for_single_app)"
	assert_native_process "$pid"
	assert_no_dock_or_windows
	printf '%s' "$pid"
}

assert_private_root() {
	path="$1"
	[ -d "$path" ] && [ ! -L "$path" ] || fail "missing real private directory $path"
	mode="$(stat -f '%Lp' "$path")"
	[ "$mode" = 700 ] || fail "private directory $path has mode $mode, want 700"
}

assert_private_log() {
	log_file="$logs_dir/codesk-desktop.log"
	[ -f "$log_file" ] && [ ! -L "$log_file" ] || fail 'codesk-desktop.log is not a real file'
	[ "$(stat -f '%Lp' "$log_file")" = 600 ] || fail 'codesk-desktop.log mode is not 600'
}

latest_online_service_generation() {
	log_file="$logs_dir/codesk-desktop.log"
	[ -f "$log_file" ] && [ ! -L "$log_file" ] || fail 'cannot inspect service generation without a private desktop log'
	awk '
		{
			is_service = 0
			is_online = 0
			generation = ""
			for (index = 1; index <= NF; index++) {
				if ($index == "service") is_service = 1
				if ($index == "state=online") is_online = 1
				if ($index ~ /^generation=[0-9]+$/) {
					split($index, value, "=")
					generation = value[2]
				}
			}
			if (is_service && is_online && generation != "") latest = generation
		}
		END { if (latest != "") print latest }
	' "$log_file"
}

wait_for_new_online_service_generation() {
	prior="$1"
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		current="$(latest_online_service_generation)"
		case "$current" in
			''|*[!0-9]*) ;;
			*)
				if [ "$current" -gt "$prior" ]; then
					printf '%s' "$current"
					return
				fi
				;;
		esac
		count=$((count + 1))
		sleep 1
	done
	fail "Restart daemon did not advance online service generation beyond $prior"
}

assert_configuration() {
	config="$data_dir/desktop.json"
	[ -f "$config" ] && [ ! -L "$config" ] || fail 'desktop.json is not a real file'
	[ "$(stat -f '%Lp' "$config")" = 600 ] || fail 'desktop.json mode is not 600'
	plutil -lint "$config" >/dev/null
	for key in daemon_id workspace_id workspace_name workspace_slug workspace_url; do
		[ -n "$(plutil -extract "$key" raw -o - "$config")" ] || fail "desktop.json has an empty $key"
	done
	if grep -Eiq '"[^"[:space:]]*token[^"[:space:]]*"[[:space:]]*:' "$config"; then
		fail 'desktop.json contains a token-like field'
	fi
}

wait_for_connection() {
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		if [ -f "$data_dir/desktop.json" ] && security find-generic-password -s "$keychain_service" -a "$keychain_account" >/dev/null 2>&1; then
			assert_configuration
			return
		fi
		count=$((count + 1))
		sleep 1
	done
	fail 'browser handoff did not produce configuration plus a Keychain credential'
}

run_bounded_to_log() {
	label="$1"
	log="$2"
	shift 2
	timeout_marker="$log.timeout"
	rm -f "$timeout_marker"
	"$@" >>"$log" 2>&1 &
	command_pid=$!
	(
		sleep "$timeout_seconds"
		if kill -0 "$command_pid" 2>/dev/null; then
			printf '%s\n' "$label exceeded ${timeout_seconds}s" >"$timeout_marker"
			if kill -TERM "$command_pid" 2>/dev/null; then :; fi
			sleep 2
			if kill -0 "$command_pid" 2>/dev/null; then
				if kill -KILL "$command_pid" 2>/dev/null; then :; fi
			fi
		fi
	) &
	timeout_pid=$!
	if wait "$command_pid"; then
		bounded_status=0
	else
		bounded_status=$?
	fi
	if kill -0 "$timeout_pid" 2>/dev/null; then
		if kill -TERM "$timeout_pid" 2>/dev/null; then :; fi
	fi
	if wait "$timeout_pid" 2>/dev/null; then :; fi
	[ ! -e "$timeout_marker" ] || fail "$label exceeded CODESK_MACOS_ACCEPT_TIMEOUT=${timeout_seconds}s"
}

run_driver_action() {
	driver_path="$1"
	driver_stage="$2"
	driver_action="$3"
	planned_relative="$4"
	planned_sha256="$5"
	expected_driver_sha256="$(expected_driver_hash "$driver_path")"
	[ "$(hash_file "$driver_path")" = "$expected_driver_sha256" ] || fail "$driver_stage driver changed before $driver_action"
	driver_result_dir="$evidence_dir/driver-$driver_stage"
	case "$driver_action" in
		run|plan)
			[ ! -e "$driver_result_dir" ] && [ ! -L "$driver_result_dir" ] || fail "driver result already exists for $driver_stage"
			mkdir -p "$driver_result_dir"
			;;
		trigger)
			[ -d "$driver_result_dir" ] && [ ! -L "$driver_result_dir" ] || fail "sync plan result is missing for $driver_stage"
			;;
		*) fail "unsupported driver action $driver_action" ;;
	esac
	case "$driver_action" in
		run) driver_receipt="$driver_result_dir/receipt.txt" ;;
		*) driver_receipt="$driver_result_dir/receipt-$driver_action.txt" ;;
	esac
	[ ! -e "$driver_receipt" ] && [ ! -L "$driver_receipt" ] || fail "$driver_stage $driver_action receipt already exists"
	driver_log="$driver_result_dir/driver-$driver_action.log"
	[ ! -e "$driver_log" ] && [ ! -L "$driver_log" ] || fail "$driver_stage $driver_action log already exists"
	record "RUN bounded external driver stage=$driver_stage action=$driver_action timeout=${timeout_seconds}s path=$driver_path"
	(
		export CODESK_ACCEPT_STAGE="$driver_stage"
		export CODESK_ACCEPT_ACTION="$driver_action"
		export CODESK_ACCEPT_RUN_ID="$(read_value run-id)"
		export CODESK_ACCEPT_APP_PATH="$installed_app"
		export CODESK_ACCEPT_DATA_DIR="$data_dir"
		export CODESK_ACCEPT_LOGS_DIR="$logs_dir"
		export CODESK_ACCEPT_CACHE_DIR="$cache_dir"
		export CODESK_ACCEPT_RESULT_DIR="$driver_result_dir"
		export CODESK_ACCEPT_RELATIVE_PATH="$planned_relative"
		export CODESK_ACCEPT_SHA256="$planned_sha256"
		run_bounded_to_log "$driver_stage $driver_action driver" "$driver_log" "$driver_path"
		[ "$bounded_status" -eq 0 ] || exit "$bounded_status"
	) || fail "$driver_stage $driver_action driver failed; inspect $driver_log"
	[ "$(hash_file "$driver_path")" = "$expected_driver_sha256" ] || fail "$driver_stage driver changed during $driver_action"
	[ -s "$driver_receipt" ] && [ ! -L "$driver_receipt" ] || fail "$driver_stage $driver_action driver must write a non-empty $(basename -- "$driver_receipt")"
	[ "$(stat -f '%z' "$driver_receipt")" -le 32768 ] || fail "$driver_stage $driver_action driver receipt is too large"
	record "PASS external native driver completed stage=$driver_stage action=$driver_action path=$driver_path sha256=$expected_driver_sha256"
}

run_driver() {
	run_driver_action "$1" "$2" run '' ''
}

run_sync_stage() {
	sync_stage="$1"
	case "$sync_stage" in initial|upgrade|restart) ;; *) fail "unsupported sync stage $sync_stage" ;; esac
	run_driver_action "$sync_driver" "$sync_stage" plan '' ''
	sync_result_dir="$evidence_dir/driver-$sync_stage"
	[ -f "$sync_result_dir/relative-path" ] && [ ! -L "$sync_result_dir/relative-path" ] || fail "$sync_stage plan did not write relative-path"
	[ -f "$sync_result_dir/sha256" ] && [ ! -L "$sync_result_dir/sha256" ] || fail "$sync_stage plan did not write sha256"
	relative="$(cat "$sync_result_dir/relative-path")"
	expected_hash="$(cat "$sync_result_dir/sha256")"
	reject_newline "$sync_stage relative path" "$relative"
	reject_newline "$sync_stage SHA-256" "$expected_hash"
	case "$relative" in
		''|/*|.|..|../*|*/../*|*/..|./*|*/./*|*/.|*//*|.notty|.notty/*) fail "$sync_stage driver returned an unsafe relative path" ;;
	esac
	case "$expected_hash" in
		*[!0-9a-f]*|'') fail "$sync_stage driver returned an invalid SHA-256" ;;
	esac
	[ "${#expected_hash}" -eq 64 ] || fail "$sync_stage driver returned an invalid SHA-256 length"
	for prior_stage in initial upgrade restart; do
		[ "$prior_stage" != "$sync_stage" ] || continue
		prior_path_file="$evidence_dir/sync-$prior_stage-relative-path"
		if [ -f "$prior_path_file" ] && [ ! -L "$prior_path_file" ]; then
			[ "$relative" != "$(read_value "sync-$prior_stage-relative-path")" ] ||
				fail "$sync_stage sync plan reused the $prior_stage relative path"
		fi
	done
	local_path="$data_dir/workspace/$relative"
	[ ! -e "$local_path" ] && [ ! -L "$local_path" ] || fail "$sync_stage sync target existed before its remote trigger"
	write_value "sync-$sync_stage-relative-path" "$relative"
	write_value "sync-$sync_stage-sha256" "$expected_hash"
	record "PASS remote-to-local sync plan stage=$sync_stage path=$relative was locally absent before trigger"
	run_driver_action "$sync_driver" "$sync_stage" trigger "$relative" "$expected_hash"
	[ "$(cat "$sync_result_dir/relative-path")" = "$relative" ] || fail "$sync_stage driver changed relative-path during trigger"
	[ "$(cat "$sync_result_dir/sha256")" = "$expected_hash" ] || fail "$sync_stage driver changed sha256 during trigger"
	count=0
	while [ "$count" -lt "$timeout_seconds" ]; do
		if [ -f "$local_path" ] && [ ! -L "$local_path" ] && [ "$(hash_file "$local_path")" = "$expected_hash" ]; then
			record "PASS remote-to-local sync stage=$sync_stage path=$relative sha256=$expected_hash"
			return
		fi
		count=$((count + 1))
		sleep 1
	done
	fail "remote-to-local sync did not materialize the expected $sync_stage file"
}

assert_saved_sync() {
	stage="$1"
	relative="$(read_value "sync-$stage-relative-path")"
	expected_hash="$(read_value "sync-$stage-sha256")"
	local_path="$data_dir/workspace/$relative"
	[ -f "$local_path" ] && [ ! -L "$local_path" ] || fail "$stage sync file was not preserved"
	[ "$(hash_file "$local_path")" = "$expected_hash" ] || fail "$stage sync file changed"
}

scan_secret_root() {
	secret_file="$1"
	root="$2"
	[ -e "$root" ] || return
	[ ! -L "$root" ] || fail "secret scan root is a symlink: $root"
	matches="$tmp_dir/secret-matches"
	errors="$tmp_dir/secret-errors"
	if LC_ALL=C grep -r -l -F -f "$secret_file" "$root" >"$matches" 2>"$errors"; then
		fail "Keychain credential appeared in plaintext under $root"
	else
		status=$?
		[ "$status" -eq 1 ] || fail "could not complete plaintext credential scan under $root"
	fi
}

assert_keychain_only_secret() {
	mode="$1"
	secret_file="$tmp_dir/keychain-secret"
	security find-generic-password -s "$keychain_service" -a "$keychain_account" -w >"$secret_file" 2>/dev/null ||
		fail 'could not read the Codesk credential from Keychain for acceptance verification'
	chmod 0600 "$secret_file"
	[ -s "$secret_file" ] || fail 'Keychain returned an empty credential'
	secret_hash="$(hash_file "$secret_file")"
	if [ "$mode" = record ]; then
		write_value keychain-secret-sha256 "$secret_hash"
	else
		[ "$secret_hash" = "$(read_value keychain-secret-sha256)" ] || fail 'Keychain credential changed across login or upgrade'
	fi
	for root in "$data_dir" "$logs_dir" "$cache_dir" "$evidence_dir"; do
		scan_secret_root "$secret_file" "$root"
	done
	rm -f "$secret_file"
	record 'PASS daemon token exists only in Keychain service com.getcodesk.desktop'
}

normal_quit() {
	pid="$(wait_for_single_app)"
	group="$(process_group "$pid")"
	menu_click 'Quit Codesk'
	wait_for_no_codesk
	wait_for_empty_group "$group"
	record "PASS normal Quit joined Codesk process group $group"
}

start_provider_for_app() {
	provider_stage="$1"
	owner_pid="$2"
	provider_pids_before="$tmp_dir/$provider_stage-pids-before"
	ps -axo pid= | awk '{$1=$1; if ($1 != "") print $1}' >"$provider_pids_before"
	provider_trigger_epoch="$(date -u '+%s')"
	provider_triggered_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	run_driver "$provider_driver" "$provider_stage"
	provider_result_dir="$evidence_dir/driver-$provider_stage"
	provider_result="$provider_result_dir/provider-pid"
	provider_expected_path_file="$provider_result_dir/provider-executable"
	provider_expected_hash_file="$provider_result_dir/provider-executable-sha256"
	[ -f "$provider_result" ] && [ ! -L "$provider_result" ] || fail "$provider_stage provider driver did not write provider-pid"
	[ -f "$provider_expected_path_file" ] && [ ! -L "$provider_expected_path_file" ] || fail "$provider_stage provider driver did not write provider-executable"
	[ -f "$provider_expected_hash_file" ] && [ ! -L "$provider_expected_hash_file" ] || fail "$provider_stage provider driver did not write provider-executable-sha256"
	provider_pid="$(cat "$provider_result")"
	provider_expected_path="$(cat "$provider_expected_path_file")"
	provider_expected_hash="$(cat "$provider_expected_hash_file")"
	reject_newline "$provider_stage provider PID" "$provider_pid"
	reject_newline "$provider_stage provider executable" "$provider_expected_path"
	reject_newline "$provider_stage provider executable SHA-256" "$provider_expected_hash"
	case "$provider_pid" in ''|*[!0-9]*) fail "$provider_stage provider driver returned an invalid PID" ;; esac
	case "$provider_expected_hash" in *[!0-9a-f]*|'') fail "$provider_stage provider driver returned an invalid executable SHA-256" ;; esac
	[ "${#provider_expected_hash}" -eq 64 ] || fail "$provider_stage provider driver returned an invalid executable SHA-256 length"
	[ "$provider_pid" -gt 1 ] && kill -0 "$provider_pid" 2>/dev/null || fail "$provider_stage provider process is not alive"
	if awk -v pid="$provider_pid" '$1 == pid { found=1 } END { exit found ? 0 : 1 }' "$provider_pids_before"; then
		fail "$provider_stage provider PID existed before its trigger"
	fi
	provider_expected_path="$(resolve_driver_path "$provider_expected_path")"
	provider_actual_paths="$(lsof -a -p "$provider_pid" -d txt -Fn 2>>"$transcript" | awk 'substr($0, 1, 1) == "n" { print substr($0, 2) }')"
	[ "$(printf '%s\n' "$provider_actual_paths" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||
		fail "$provider_stage provider did not expose exactly one program-text executable"
	provider_actual_path="$provider_actual_paths"
	reject_newline "$provider_stage inspected provider executable" "$provider_actual_path"
	[ -n "$provider_actual_path" ] || fail "$provider_stage provider executable could not be inspected"
	provider_actual_path="$(resolve_driver_path "$provider_actual_path")"
	[ "$provider_actual_path" = "$provider_expected_path" ] || fail "$provider_stage provider executable differs from the driver claim"
	[ "$provider_actual_path" != "$app_executable" ] || fail "$provider_stage driver returned the Codesk desktop executable instead of a provider"
	[ "$provider_actual_path" != "$helper_executable" ] || fail "$provider_stage driver returned the Codesk helper executable instead of a provider"
	provider_actual_hash="$(hash_file "$provider_actual_path")"
	[ "$provider_actual_hash" = "$provider_expected_hash" ] || fail "$provider_stage provider executable hash differs from the driver claim"

	provider_chain="$provider_pid"
	provider_ancestor="$provider_pid"
	provider_rooted=0
	provider_depth=0
	while [ "$provider_depth" -lt 64 ]; do
		provider_parent="$(ps -p "$provider_ancestor" -o ppid= | awk '{$1=$1; print}')"
		case "$provider_parent" in ''|*[!0-9]*) fail "$provider_stage provider ancestry became unavailable" ;; esac
		provider_chain="$provider_chain<-$provider_parent"
		if [ "$provider_parent" = "$owner_pid" ]; then
			provider_rooted=1
			break
		fi
		[ "$provider_parent" -gt 1 ] || break
		provider_ancestor="$provider_parent"
		provider_depth=$((provider_depth + 1))
	done
	[ "$provider_rooted" -eq 1 ] || fail "$provider_stage provider ancestry is not rooted in Codesk pid=$owner_pid (chain $provider_chain)"
	owner_group="$(process_group "$owner_pid")"
	provider_group="$(process_group "$provider_pid")"
	[ "$provider_group" = "$owner_group" ] || fail "$provider_stage provider escaped Codesk process group ($provider_group != $owner_group)"
	provider_started="$(LC_ALL=C ps -p "$provider_pid" -o lstart= | awk '{$1=$1; print}')"
	[ -n "$provider_started" ] || fail "$provider_stage provider start time is unavailable"
	provider_start_epoch="$(LC_ALL=C date -j -f '%a %b %e %T %Y' "$provider_started" '+%s' 2>>"$transcript")" ||
		fail "$provider_stage provider start time could not be parsed"
	[ "$provider_start_epoch" -ge "$provider_trigger_epoch" ] || fail "$provider_stage provider started before its trigger"
	write_value "$provider_stage-provider-executable" "$provider_actual_path"
	write_value "$provider_stage-provider-executable-sha256" "$provider_actual_hash"
	write_value "$provider_stage-provider-triggered-at" "$provider_triggered_at"
	write_value "$provider_stage-provider-start" "$provider_started"
	write_value "$provider_stage-provider-ancestry" "$provider_chain"
	record "PASS real provider generation stage=$provider_stage pid=$provider_pid executable=$provider_actual_path sha256=$provider_actual_hash triggered_at=$provider_triggered_at start='$provider_started' ancestry=$provider_chain pgid=$provider_group"
	printf '%s' "$provider_pid"
}

if [ "$phase" = prepare ]; then
	[ ! -e "$installed_app" ] && [ ! -L "$installed_app" ] || fail 'prepare requires /Applications/Codesk.app to be absent'
	for path in "$data_dir" "$logs_dir" "$cache_dir"; do
		[ ! -e "$path" ] && [ ! -L "$path" ] || fail "prepare requires a clean dedicated account; remove $path"
	done
	if security find-generic-password -s "$keychain_service" -a "$keychain_account" >/dev/null 2>&1; then
		fail 'prepare requires the Codesk Keychain credential to be absent'
	fi
	run_id="macos-$host_arch-$(date -u '+%Y%m%dT%H%M%SZ')-$$"
	write_value run-id "$run_id"

	install_from_dmg "$previous_release/Codesk_${previous_version}_macos_universal.dmg" \
		"$previous_version" previous "$previous_dmg_hash" "$previous_dmg_size" "$previous_app_tree_hash"
	app_pid="$(launch_app)"
	assert_menu_item "Version $previous_version"
	login_state="$(menu_mark_state 'Launch at login')" || fail 'could not inspect launch-at-login menu state'
	if [ "$login_state" = marked ]; then
		fail 'clean-account launch-at-login state is unexpectedly enabled'
	fi
	assert_private_root "$data_dir"
	assert_private_root "$logs_dir"
	assert_private_root "$cache_dir"
	assert_private_log

	menu_click 'Connect...'
	run_driver "$connect_driver" connect
	wait_for_connection
	assert_keychain_only_secret record
	write_value configuration-sha256 "$(hash_file "$data_dir/desktop.json")"
	record 'PASS browser connection handoff committed token-free metadata and Keychain credential'

	run_sync_stage initial

	wait_login_enabled
	write_value prepare-app-pid "$app_pid"
	normal_quit
	[ -z "$(codesk_all_pids)" ] || fail 'Codesk was still running at the login-cycle boundary'
	record 'PASS Codesk was explicitly quit and its process group joined before logout'
	write_value stage awaiting-login
	record 'ACTION REQUIRED: perform a real macOS logout/login now. Do not manually launch Codesk.'
	record 'Then run the resume phase with the identical arguments and driver environment.'
	exit 0
fi

# Resume begins by proving the login item launched the app before the harness did.
resume_pid="$(wait_for_single_app)"
[ "$resume_pid" != "$(read_value prepare-app-pid)" ] || fail 'Codesk PID did not change; a real logout/login cycle was not observed'
assert_native_process "$resume_pid"
assert_no_dock_or_windows
assert_menu_item "Version $previous_version"
[ "$(menu_mark_state 'Launch at login')" = marked ] || fail 'Codesk did not restore enabled login-item state after login'
assert_configuration
[ "$(hash_file "$data_dir/desktop.json")" = "$(read_value configuration-sha256)" ] || fail 'configuration changed across login'
assert_saved_sync initial
assert_keychain_only_secret verify
record 'PASS SMAppService launched Codesk across a real logout/login cycle'

menu_click 'Launch at login'
wait_login_disabled

# Exercise both LaunchServices and the application-level flock guard. Every
# launch is bounded, and an unrelated LaunchServices failure is not evidence.
baseline_codesk_pids="$(codesk_all_pids | sort -n)"
direct_log="$tmp_dir/second-instance-direct.log"
forged_home="$tmp_dir/forged-home"
mkdir -p "$forged_home"
run_bounded_to_log 'altered-HOME direct second Codesk instance' "$direct_log" env HOME="$forged_home" "$app_executable"
[ "$bounded_status" -eq 0 ] || fail "direct second Codesk instance exited $bounded_status; inspect $direct_log"
[ "$(wait_for_single_app)" = "$resume_pid" ] || fail 'direct second instance displaced the active app'
[ "$(codesk_all_pids | sort -n)" = "$baseline_codesk_pids" ] || fail 'direct second instance changed the Codesk process set'
for forged_path in "$forged_home/Library/Application Support/Codesk" "$forged_home/Library/Logs/Codesk" "$forged_home/Library/Caches/Codesk"; do
	[ ! -e "$forged_path" ] && [ ! -L "$forged_path" ] || fail "caller-controlled HOME created a desktop root: $forged_path"
done
record 'PASS account-record home kept paths and the application lock authoritative under an altered-HOME direct launch'

launch_services_log="$tmp_dir/second-instance-launchservices.log"
run_bounded_to_log 'LaunchServices second Codesk request' "$launch_services_log" env LC_ALL=C open -n "$installed_app"
launch_services_status="$bounded_status"
[ "$(wait_for_single_app)" = "$resume_pid" ] || fail 'LaunchServices second instance displaced the active app'
[ "$(codesk_all_pids | sort -n)" = "$baseline_codesk_pids" ] || fail 'LaunchServices second request changed the Codesk process set'
if [ "$launch_services_status" -eq 0 ]; then
	launch_services_outcome='routed-to-existing-instance'
elif grep -Eiq 'already running|another instance|multiple instances|LSMultipleInstancesProhibited' "$launch_services_log"; then
	launch_services_outcome='explicitly-prohibited'
else
	fail "LaunchServices second request failed for an unrelated reason (status $launch_services_status); inspect $launch_services_log"
fi
record "PASS LaunchServices multiple-instance outcome=$launch_services_outcome status=$launch_services_status active_pid=$resume_pid"

# A post-readiness watchdog death must fail closed and remove the app-owned
# process group, including a real provider descendant.
app_group="$(process_group "$resume_pid")"
watchdog_provider_pid="$(start_provider_for_app provider-watchdog-exit "$resume_pid")"
watchdog_pid="$(watchdog_pid_for_app "$resume_pid")"
kill -KILL "$watchdog_pid"
wait_for_no_codesk
wait_for_empty_group "$app_group"
if kill -0 "$watchdog_provider_pid" 2>/dev/null; then
	fail 'provider survived post-readiness watchdog death'
fi
record "PASS watchdog death pid=$watchdog_pid failed closed and removed provider pid=$watchdog_provider_pid group=$app_group"

# Relaunch and independently prove abnormal application-exit cleanup.
resume_pid="$(launch_app)"
assert_menu_item "Version $previous_version"
assert_configuration
assert_keychain_only_secret verify
app_group="$(process_group "$resume_pid")"
provider_pid="$(start_provider_for_app provider-app-exit "$resume_pid")"
kill -KILL "$resume_pid"
wait_for_no_codesk
wait_for_empty_group "$app_group"
if kill -0 "$provider_pid" 2>/dev/null; then
	fail 'provider survived abnormal Codesk termination'
fi
record "PASS abnormal app exit removed real provider pid=$provider_pid and process group=$app_group"

# Relaunch the previous build, then replace it with the candidate app.
previous_pid="$(launch_app)"
assert_menu_item "Version $previous_version"
assert_configuration
assert_keychain_only_secret verify
normal_quit
[ -n "$previous_pid" ] || fail 'previous app did not relaunch after abnormal-exit test'

install_from_dmg "$candidate_snapshot_dmg" "$candidate_version" candidate \
	"$candidate_dmg_hash" "$candidate_dmg_size" "$candidate_app_tree_hash"
candidate_pid="$(launch_app)"
assert_menu_item "Version $candidate_version"
candidate_login_state="$(menu_mark_state 'Launch at login')" || fail 'could not inspect candidate launch-at-login state'
if [ "$candidate_login_state" = marked ]; then
	fail 'disabled launch-at-login state did not survive app replacement'
fi
assert_configuration
[ "$(hash_file "$data_dir/desktop.json")" = "$(read_value configuration-sha256)" ] || fail 'configuration changed during app replacement'
assert_saved_sync initial
assert_keychain_only_secret verify
run_sync_stage upgrade

generation_before_restart="$(latest_online_service_generation)"
case "$generation_before_restart" in ''|*[!0-9]*) fail 'candidate app did not publish an online service generation' ;; esac
menu_click 'Restart daemon'
generation_after_restart="$(wait_for_new_online_service_generation "$generation_before_restart")"
[ "$(wait_for_single_app)" = "$candidate_pid" ] || fail 'Restart daemon replaced the desktop process'
run_sync_stage restart
initial_sync_path="$(read_value sync-initial-relative-path)"
upgrade_sync_path="$(read_value sync-upgrade-relative-path)"
restart_sync_path="$(read_value sync-restart-relative-path)"
[ "$initial_sync_path" != "$upgrade_sync_path" ] && [ "$initial_sync_path" != "$restart_sync_path" ] && [ "$upgrade_sync_path" != "$restart_sync_path" ] ||
	fail 'sync driver must use pairwise-unique paths for initial, upgrade, and restart rows'
record "PASS sync stages used pairwise-unique causally planned paths initial=$initial_sync_path upgrade=$upgrade_sync_path restart=$restart_sync_path"
record "PASS Restart daemon advanced online service generation $generation_before_restart->$generation_after_restart, preserved app pid=$candidate_pid, and resumed sync"
record 'PASS replacement upgrade preserved configuration, Keychain credential, prior sync state, and resumed sync'

normal_quit
[ -n "$candidate_pid" ] || fail 'candidate app did not launch'
data_before_uninstall="$(path_fingerprint "$data_dir")"
logs_before_uninstall="$(path_fingerprint "$logs_dir")"
cache_before_uninstall="$(path_fingerprint "$cache_dir")"
remove_guarded_app_path "$installed_app"
sleep 5
[ ! -e "$installed_app" ] && [ ! -L "$installed_app" ] || fail 'Codesk.app remained after uninstall'
[ -z "$(codesk_all_pids)" ] || fail 'Codesk process relaunched after uninstall'
[ "$data_before_uninstall" = "$(path_fingerprint "$data_dir")" ] || fail 'uninstall changed Application Support data'
[ "$logs_before_uninstall" = "$(path_fingerprint "$logs_dir")" ] || fail 'uninstall changed logs'
[ "$cache_before_uninstall" = "$(path_fingerprint "$cache_dir")" ] || fail 'uninstall changed caches'
assert_keychain_only_secret verify
[ "$(read_value legacy-before)" = "$(path_fingerprint "$legacy_dir")" ] || fail 'desktop lifecycle touched legacy ~/.notty state'
record 'PASS app-only uninstall removed Codesk.app and preserved user data/Keychain state'
record 'PASS legacy ~/.notty state remained byte-for-byte unchanged'

write_value stage complete
result="$evidence_dir/result.txt"
connect_driver_result_path="$(read_value connect-driver-path)"
connect_driver_result_hash="$(read_value connect-driver-sha256)"
sync_driver_result_path="$(read_value sync-driver-path)"
sync_driver_result_hash="$(read_value sync-driver-sha256)"
provider_driver_result_path="$(read_value provider-driver-path)"
provider_driver_result_hash="$(read_value provider-driver-sha256)"
watchdog_provider_path="$(read_value provider-watchdog-exit-provider-executable)"
watchdog_provider_hash="$(read_value provider-watchdog-exit-provider-executable-sha256)"
app_exit_provider_path="$(read_value provider-app-exit-provider-executable)"
app_exit_provider_hash="$(read_value provider-app-exit-provider-executable-sha256)"
cat >"$result" <<RESULT
result=PASS
source_revision=$candidate_revision
host_arch=$host_arch
evidence_scope=$evidence_scope
artifact_trust=$artifact_trust
publishable=$publishable
previous_version=$previous_version
previous_manifest_sha256=$previous_manifest_hash
previous_dmg_sha256=$previous_dmg_hash
previous_dmg_size=$previous_dmg_size
previous_app_tree_sha256=$previous_app_tree_hash
candidate_version=$candidate_version
candidate_manifest_sha256=$candidate_manifest_hash
candidate_dmg_sha256=$candidate_dmg_hash
candidate_dmg_size=$candidate_dmg_size
candidate_app_tree_sha256=$candidate_app_tree_hash
connect_driver_path=$connect_driver_result_path
connect_driver_sha256=$connect_driver_result_hash
sync_driver_path=$sync_driver_result_path
sync_driver_sha256=$sync_driver_result_hash
provider_driver_path=$provider_driver_result_path
provider_driver_sha256=$provider_driver_result_hash
sync_initial_path=$initial_sync_path
sync_upgrade_path=$upgrade_sync_path
sync_restart_path=$restart_sync_path
watchdog_provider_executable=$watchdog_provider_path
watchdog_provider_executable_sha256=$watchdog_provider_hash
app_exit_provider_executable=$app_exit_provider_path
app_exit_provider_executable_sha256=$app_exit_provider_hash
token_storage=Keychain:$keychain_service/$keychain_account
uninstall_data_policy=preserved
legacy_cli_state=unchanged
RESULT
if [ "$evidence_scope" = native-functional-only ]; then
	record "PASS complete native macOS functional-only evidence=$evidence_dir artifact_trust=NOT_ESTABLISHED publishable=false"
else
	record "PASS complete native macOS acceptance for host_arch=$host_arch evidence=$evidence_dir artifact_trust=verified publishable=requires-intel-arm64-review"
fi
record 'The dedicated test account still contains preserved Codesk data and Keychain state; clean it only after evidence capture.'
