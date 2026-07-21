#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
. "$repo_dir/scripts/lib/testtmp.sh"
tmp_dir="$(notty_test_mktemp notty-build-deploy-contract)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

target_count() {
	awk -v target="$1" '$0 ~ ("^" target ":[[:space:]]*$") { count++ } END { print count + 0 }' "$repo_dir/Makefile"
}

for target in \
	linux-daemon-build linux-daemon-deploy \
	macos-daemon-build macos-daemon-deploy \
	windows-daemon-build windows-daemon-deploy \
	frontend-build frontend-deploy backend-build backend-deploy
do
	[ "$(target_count "$target")" -eq 1 ] || fail "$target must have exactly one Make definition"
done
for target in macos-gui-build macos-gui-deploy windows-gui-build windows-gui-deploy; do
	[ "$(target_count "$target")" -eq 2 ] || fail "$target must have exactly two host-conditional Make definitions"
done
pass 'exact build/deploy pairs are present'

for target in \
	release static all promote \
	daemon-build daemon-release daemon-release-all release-daemons \
	macos-gui-release windows-gui-release windows-gui-payloads \
	build-windows-builder-image windows-verify \
	build-frontend build-daemon build-static build-static-local build-backend-image \
	static-build static-build-local static-publish backend-image \
	publish publish-backend publish-frontend publish-static \
	deploy-backend deploy-frontend deploy-static daemon-checksums
do
	[ "$(target_count "$target")" -eq 0 ] || fail "obsolete public Make target survived: $target"
done
pass 'obsolete public target vocabulary is absent'

deploy_recipe="$(awk '
	/^deploy:[[:space:]]*$/ { capture = 1; next }
	capture && /^[^[:space:]]/ { exit }
	capture && /^\t/ { print }
' "$repo_dir/Makefile")"
want_deploy_recipe="$(printf '\t$(MAKE) %s\n' \
	frontend-deploy linux-daemon-deploy macos-daemon-deploy windows-daemon-deploy backend-deploy)"
[ "$deploy_recipe" = "$want_deploy_recipe" ] || fail "aggregate deploy order changed:\n$deploy_recipe"
case "$deploy_recipe" in
	*gui*) fail 'aggregate deploy must not include desktop GUI targets' ;;
esac
pass 'aggregate deploy is ordered and excludes desktop GUI'

build_fixture="$tmp_dir/build-fixture"
mkdir -p "$build_fixture/scripts" "$build_fixture/deploy/daemons"
cp "$repo_dir/scripts/build-daemon-platform.sh" "$build_fixture/scripts/build-daemon-platform.sh"
cp "$repo_dir/scripts/read-version.sh" "$build_fixture/scripts/read-version.sh"
printf '1.2.3\n' >"$build_fixture/VERSION"
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
FIXTURE
chmod +x "$build_fixture/scripts/"*.sh
build_dist="$tmp_dir/daemon-dist"
build_log="$tmp_dir/build-routes.log"
for platform in linux macos windows; do
	BUILD_ROUTE_LOG="$build_log" DAEMON_DIST_ROOT="$build_dist" DAEMON_ARCHES='amd64 arm64' \
		"$build_fixture/scripts/build-daemon-platform.sh" "$platform" >/dev/null
done
want_build_routes="$(cat <<ROUTES
$build_dist/linux|$build_dist/linux|linux/amd64 linux/arm64
$build_dist/macos|$build_dist/macos|darwin/amd64 darwin/arm64
$build_dist/windows|$build_dist/windows|windows/amd64 windows/arm64
ROUTES
)"
[ "$(cat "$build_log")" = "$want_build_routes" ] || fail 'daemon platform build routes are not isolated or complete'
for installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
	[ -f "$build_dist/$installer" ] || fail "root daemon installer was not staged: $installer"
done
if BUILD_ROUTE_LOG="$build_log" DAEMON_DIST_ROOT="$build_dist" DAEMON_ARCHES='amd64 amd64' \
	"$build_fixture/scripts/build-daemon-platform.sh" linux >/dev/null 2>&1; then
	fail 'duplicate daemon architecture was accepted'
fi
pass 'daemon builds isolate platform manifests and require both unique architectures'

fake_bin="$tmp_dir/bin"
mkdir -p "$fake_bin"
cat >"$fake_bin/aws" <<'AWS'
#!/usr/bin/env sh
printf '%s\n' "$*" >>"$AWS_LOG"
if [ -n "${AWS_CAPTURE_DIR:-}" ] && [ "${4:-}" = cp ]; then
	aws_source="$5"
	aws_destination="$6"
	case "$aws_destination" in
		*"/desktop/windows/0.0.1/arm64/Codesk_0.0.1_windows_arm64.msi")
			cp "$aws_source" "$AWS_CAPTURE_DIR/arm64.msi"
			;;
		*"/desktop/windows/0.0.1/arm64/SHA256SUMS")
			cp "$aws_source" "$AWS_CAPTURE_DIR/arm64.SHA256SUMS"
			;;
		*"/desktop/windows/0.0.1/manifest.json")
			cp "$aws_source" "$AWS_CAPTURE_DIR/manifest.json"
			;;
	esac
fi
AWS
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
chmod +x "$fake_bin/aws" "$fake_bin/powershell.exe"
aws_log="$tmp_dir/aws.log"

fixture_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	else
		shasum -a 256 "$1" | awk '{ print $1 }'
	fi
}

write_windows_bundle() {
	bundle_root="$1"
	bundle_arch="$2"
	bundle_head="$3"
	bundle_base="$4"
	bundle_publishable="$5"
	case "$bundle_arch" in
		amd64) bundle_native=AMD64; bundle_installer=x64 ;;
		arm64) bundle_native=ARM64; bundle_installer=arm64 ;;
		*) fail "unsupported Windows fixture architecture: $bundle_arch" ;;
	esac
	bundle_dir="$bundle_root/$bundle_arch"
	bundle_msi="Codesk_0.0.1_windows_${bundle_arch}.msi"
	mkdir -p "$bundle_dir"
	printf 'msi-%s\n' "$bundle_arch" >"$bundle_dir/$bundle_msi"
	bundle_msi_sha="$(fixture_sha256 "$bundle_dir/$bundle_msi")"
	bundle_msi_size="$(wc -c <"$bundle_dir/$bundle_msi" | tr -d ' ')"
	printf '%s\n' "{\"schemaVersion\":2,\"source\":{\"repository\":\"XIAZY/notty\",\"event\":\"push\",\"checkoutCommit\":\"$bundle_head\",\"sourceHead\":\"$bundle_head\",\"sourceBase\":\"$bundle_base\",\"sourceBaseResolution\":\"event\",\"workflowRef\":\"local/scripts/run-windows-gui-target.ps1@$bundle_head\",\"runId\":\"local\",\"runAttempt\":\"1\"},\"runner\":{\"os\":\"Windows\",\"architecture\":\"$bundle_native\"},\"target\":{\"architecture\":\"$bundle_native\",\"goArchitecture\":\"$bundle_arch\",\"installerPlatform\":\"$bundle_installer\",\"buildMode\":\"release\",\"publishable\":$bundle_publishable},\"packages\":[{\"role\":\"release\",\"version\":\"0.0.1\",\"canonicalFile\":\"$bundle_msi\",\"canonicalSha256\":\"$bundle_msi_sha\",\"canonicalSize\":$bundle_msi_size}],\"productCodeDerivation\":{\"algorithm\":\"UUIDv5-SHA1\",\"name\":\"0.0.1+$bundle_arch\"}}" >"$bundle_dir/provenance.json"
	bundle_provenance_sha="$(fixture_sha256 "$bundle_dir/provenance.json")"
	printf '%s  %s\r\n%s  provenance.json\r\n' \
		"$bundle_msi_sha" "$bundle_msi" "$bundle_provenance_sha" >"$bundle_dir/SHA256SUMS"
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

static_dist="$tmp_dir/static"
mkdir -p "$static_dist/homepage" "$static_dist/app"
printf '<html>home</html>\n' >"$static_dist/homepage/index.html"
printf '<html>app</html>\n' >"$static_dist/app/index.html"
PATH="$fake_bin:$PATH" AWS_LOG="$aws_log" STATIC_DIST_DIR="$static_dist" UPLOAD_TARGET=frontend \
	R2_ENDPOINT_URL=https://example.invalid R2_HOMEPAGE_BUCKET=homepage R2_APP_BUCKET=app \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null

daemon_dist="$tmp_dir/daemons"
mkdir -p "$daemon_dist"
for installer in install.sh uninstall.sh install.ps1 uninstall.ps1; do
	printf '%s\n' "$installer" >"$daemon_dist/$installer"
done
for platform in linux macos windows; do
	version_dir="$daemon_dist/$platform/0.0.1"
	mkdir -p "$version_dir"
	printf '{"version":"0.0.1"}\n' >"$version_dir/manifest.json"
	printf '%064d  artifact\n' 0 >"$version_dir/SHA256SUMS"
	printf 'artifact\n' >"$version_dir/artifact"
	PATH="$fake_bin:$PATH" AWS_LOG="$aws_log" DAEMON_DIST_ROOT="$daemon_dist" \
		UPLOAD_TARGET=daemon UPLOAD_PLATFORM="$platform" R2_ENDPOINT_URL=https://example.invalid \
		R2_DAEMONS_BUCKET=static R2_DAEMONS_PREFIX=daemons \
		"$repo_dir/scripts/upload-r2.sh" >/dev/null
done

macos_dist="$tmp_dir/macos"
mkdir -p "$macos_dist/0.0.1"
printf 'dmg\n' >"$macos_dist/0.0.1/Codesk_0.0.1_macos_universal.dmg"
printf '{"version":"0.0.1"}\n' >"$macos_dist/0.0.1/manifest.json"
printf '%064d  Codesk_0.0.1_macos_universal.dmg\n' 0 >"$macos_dist/0.0.1/SHA256SUMS"
PATH="$fake_bin:$PATH" AWS_LOG="$aws_log" MACOS_GUI_DIST_DIR="$macos_dist" UPLOAD_TARGET=macos-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null

windows_dist="$tmp_dir/windows"
source_head="$(git -C "$repo_dir" rev-parse --verify HEAD)"
source_base="$(git -C "$repo_dir" rev-parse --verify 'HEAD^1')"
for arch in amd64 arm64; do
	write_windows_bundle "$windows_dist" "$arch" "$source_head" "$source_base" true
done
PATH="$fake_bin:$PATH" AWS_LOG="$aws_log" WINDOWS_GUI_MSI_ROOT="$windows_dist" UPLOAD_TARGET=windows-gui \
	R2_ENDPOINT_URL=https://example.invalid R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null

toctou_dist="$tmp_dir/windows-post-staging-mutation"
for arch in amd64 arm64; do
	write_windows_bundle "$toctou_dist" "$arch" "$source_head" "$source_base" true
done
toctou_source_before="$(fixture_sha256 "$toctou_dist/arm64/Codesk_0.0.1_windows_arm64.msi")"
toctou_aws_log="$tmp_dir/windows-post-staging-mutation.aws.log"
toctou_capture="$tmp_dir/windows-post-staging-mutation.capture"
mkdir "$toctou_capture"
: >"$toctou_aws_log"
PATH="$fake_bin:$PATH" AWS_LOG="$toctou_aws_log" AWS_CAPTURE_DIR="$toctou_capture" \
	WINDOWS_GUI_MUTATE_SOURCE_ROOT="$toctou_dist" WINDOWS_GUI_MSI_ROOT="$toctou_dist" \
	UPLOAD_TARGET=windows-gui R2_ENDPOINT_URL=https://example.invalid \
	R2_DESKTOP_BUCKET=static R2_DESKTOP_PREFIX=desktop \
	"$repo_dir/scripts/upload-r2.sh" >/dev/null
toctou_source_after="$(fixture_sha256 "$toctou_dist/arm64/Codesk_0.0.1_windows_arm64.msi")"
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
printf '%064d  Codesk_0.0.1_windows_amd64.msi\r\n%s  provenance.json\r\n' \
	0 "$wrong_hash_provenance" >"$wrong_hash_dist/amd64/SHA256SUMS"
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
	's3://static/daemons/linux/0.0.1/' \
	's3://static/daemons/macos/0.0.1/' \
	's3://static/daemons/windows/0.0.1/' \
	's3://static/daemons/linux/latest/manifest.json' \
	's3://static/daemons/macos/latest/manifest.json' \
	's3://static/daemons/windows/latest/manifest.json' \
	's3://static/desktop/macos/0.0.1/' \
	's3://static/desktop/macos/latest/manifest.json' \
	's3://static/desktop/windows/0.0.1/amd64/Codesk_0.0.1_windows_amd64.msi' \
	's3://static/desktop/windows/0.0.1/arm64/Codesk_0.0.1_windows_arm64.msi' \
	's3://static/desktop/windows/latest/manifest.json'
do
	grep -Fq "$required" "$aws_log" || fail "shared R2 uploader missed route: $required"
done
if grep -Fq 's3://static/daemons/latest/manifest.json' "$aws_log"; then
	fail 'daemon upload retained the cross-platform latest manifest collision'
fi
pass 'one shared R2 uploader routes frontend, isolated daemons, and both desktop GUIs'

grep -Fq 'artifact_base="$static_base/$artifact_platform"' "$repo_dir/deploy/daemons/install.sh" ||
	fail 'POSIX installer does not select the platform-scoped daemon root'
grep -Fq '$artifactBase = Join-RemotePath $StaticBase "windows"' "$repo_dir/deploy/daemons/install.ps1" ||
	fail 'PowerShell installer does not select the Windows daemon root'
pass 'daemon installers consume platform-scoped latest metadata'

printf '%s\n' 'All build/deploy contract tests passed.'
