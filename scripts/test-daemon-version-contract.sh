#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/notty-daemon-version-contract.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
expect_fail() {
	expect_name="$1"
	shift
	if "$@" >"$tmp_dir/output" 2>&1; then
		fail "$expect_name"
	fi
	pass "$expect_name"
}

[ -f "$repo_dir/DAEMON_VERSION" ] || fail 'root DAEMON_VERSION is missing'
for obsolete_version_path in \
	"$repo_dir/VERSION" \
	"$repo_dir/scripts/read-version.sh" \
	"$repo_dir/scripts/read-version.ps1"
do
	[ ! -e "$obsolete_version_path" ] || fail "obsolete version path survived: $obsolete_version_path"
done
[ -n "$("$repo_dir/scripts/read-daemon-version.sh")" ] ||
	fail 'root DAEMON_VERSION did not pass the strict reader'
pass 'root daemon-version file and reader names are explicit'

new_fixture() {
	new_fixture_dir="$1"
	rm -rf "$new_fixture_dir"
	mkdir -p "$new_fixture_dir/scripts/lib"
	cp "$repo_dir/Makefile" "$new_fixture_dir/Makefile"
	cp "$repo_dir/scripts/read-git-sha.sh" "$new_fixture_dir/scripts/read-git-sha.sh"
	cp "$repo_dir/scripts/read-daemon-version.sh" "$new_fixture_dir/scripts/read-daemon-version.sh"
	cp "$repo_dir/scripts/read-daemon-version.ps1" "$new_fixture_dir/scripts/read-daemon-version.ps1"
	cp "$repo_dir/scripts/lib/deploy-env.sh" "$new_fixture_dir/scripts/lib/deploy-env.sh"
	cp "$repo_dir/scripts/lib/testtmp.sh" "$new_fixture_dir/scripts/lib/testtmp.sh"
	for script in \
		build-daemon-release.sh build-daemon-platform.sh build-macos-desktop-release.sh \
		build-windows-desktop-payloads.sh build-backend-image.sh build-frontend.sh \
		build-homepage.sh push-backend-image.sh upload-r2.sh deploy-backend.sh \
		deploy-daemon.sh deploy-frontend.sh deploy-homepage.sh deploy-macos-gui.sh
	do
		cp "$repo_dir/scripts/$script" "$new_fixture_dir/scripts/$script"
	done
}

fixture="$tmp_dir/repo"
new_fixture "$fixture"
mkdir -p "$tmp_dir/invalid"

for version in 0.0.0 1.2.3 255.255.65535; do
	printf '%s\n' "$version" >"$fixture/DAEMON_VERSION"
	actual="$("$fixture/scripts/read-daemon-version.sh")"
	[ "$actual" = "$version" ] || fail "POSIX reader changed valid $version"
	pass "POSIX reader accepts $version"
done
printf '1.2.3\r\n' >"$fixture/DAEMON_VERSION"
actual="$("$fixture/scripts/read-daemon-version.sh")"
[ "$actual" = 1.2.3 ] || fail 'POSIX reader changed valid CRLF-terminated version'
pass 'POSIX reader accepts a CRLF-terminated version'

check_invalid() {
	invalid_name="$1"
	shift
	"$@" >"$fixture/DAEMON_VERSION"
	cp "$fixture/DAEMON_VERSION" "$tmp_dir/invalid/$invalid_name"
	expect_fail "POSIX reader rejects $invalid_name" "$fixture/scripts/read-daemon-version.sh"
}

rm -f "$fixture/DAEMON_VERSION"
expect_fail 'POSIX reader rejects missing DAEMON_VERSION' "$fixture/scripts/read-daemon-version.sh"
check_invalid empty printf ''
check_invalid leading-zero printf '01.2.3\n'
check_invalid prefix printf 'v1.2.3\n'
check_invalid suffix printf '1.2.3-rc1\n'
check_invalid leading-whitespace printf ' 1.2.3\n'
check_invalid trailing-whitespace printf '1.2.3 \n'
check_invalid NUL printf '1.2.3\000\n'
check_invalid non-ASCII printf '1.2.\200\n'
check_invalid multiline printf '1.2.3\n2.3.4\n'
check_invalid missing-LF printf '1.2.3'
check_invalid major-overflow printf '256.0.0\n'
check_invalid minor-overflow printf '0.256.0\n'
check_invalid build-overflow printf '0.0.65536\n'

printf '1.2.3\n' >"$fixture/DAEMON_VERSION"
[ "$(DAEMON_VERSION=injected "$fixture/scripts/read-daemon-version.sh")" = 1.2.3 ] ||
	fail 'environment DAEMON_VERSION overrode the repository daemon-version file'
pass 'environment DAEMON_VERSION cannot override the repository daemon-version file'

make_fake_bin="$tmp_dir/make-bin"
make_go_log="$tmp_dir/make-go.log"
mkdir -p "$make_fake_bin"
cat >"$make_fake_bin/go" <<'GO'
#!/usr/bin/env sh
set -eu
printf '%s\n' "$*" >>"$MAKE_GO_LOG"
GO
cat >"$fixture/scripts/build-yffi.sh" <<'BUILD_YFFI'
#!/usr/bin/env sh
set -eu
BUILD_YFFI
chmod +x "$make_fake_bin/go" "$fixture/scripts/build-yffi.sh"

for version in 0.0.0 1.2.3 255.255.65535; do
	printf '%s\n' "$version" >"$fixture/DAEMON_VERSION"
	: >"$make_go_log"
	PATH="$make_fake_bin:$PATH" MAKE_GO_LOG="$make_go_log" \
		make -s -C "$fixture" _build-daemon-host
	[ "$(grep -Fc "Version=$version" "$make_go_log")" -eq 2 ] ||
		fail "Make did not bind $version into both host daemon binaries"
	pass "Make resolves $version lazily for host daemon binaries"
done
printf '1.2.3\n' >"$fixture/DAEMON_VERSION"
: >"$make_go_log"
PATH="$make_fake_bin:$PATH" MAKE_GO_LOG="$make_go_log" \
	make -s -C "$fixture" _build-daemon-host DAEMON_VERSION=9.9.9 FILE_VERSION=8.8.8
[ "$(grep -Fc 'Version=1.2.3' "$make_go_log")" -eq 2 ] ||
	fail 'Make caller version variables overrode the repository daemon-version file'
pass 'Make caller version variables cannot override the repository daemon-version file'

rm -f "$fixture/DAEMON_VERSION"
: >"$make_go_log"
expect_fail 'Make host daemon build rejects missing DAEMON_VERSION' \
	env "PATH=$make_fake_bin:$PATH" "MAKE_GO_LOG=$make_go_log" \
	make -s -C "$fixture" _build-daemon-host
[ ! -s "$make_go_log" ] || fail 'Make invoked Go after the host version check failed'

backend_fake_bin="$tmp_dir/backend-bin"
backend_docker_log="$tmp_dir/backend-docker.log"
backend_ssh_log="$tmp_dir/backend-ssh.log"
frontend_aws_log="$tmp_dir/frontend-aws.log"
mkdir -p "$backend_fake_bin"
cat >"$backend_fake_bin/git" <<'GIT'
#!/usr/bin/env sh
set -eu
[ "$#" -eq 5 ] && [ "$1" = -C ] && [ "$3" = rev-parse ] &&
	[ "$4" = --short ] && [ "$5" = HEAD ] || exit 64
printf '%s\n' "${FAKE_GIT_SHA:-deadbee}"
GIT
cat >"$backend_fake_bin/docker" <<'DOCKER'
#!/usr/bin/env sh
set -eu
printf '%s\n' "$*" >>"$BACKEND_DOCKER_LOG"
DOCKER
cat >"$backend_fake_bin/ssh" <<'SSH'
#!/usr/bin/env sh
set -eu
printf '%s\n' "$*" >>"$BACKEND_SSH_LOG"
SSH
cat >"$backend_fake_bin/scp" <<'SCP'
#!/usr/bin/env sh
set -eu
SCP
cat >"$backend_fake_bin/aws" <<'AWS'
#!/usr/bin/env sh
set -eu
printf '%s\n' "$*" >>"$FRONTEND_AWS_LOG"
AWS
cat >"$backend_fake_bin/npm" <<'NPM'
#!/usr/bin/env sh
set -eu
[ "$#" -eq 2 ] && [ "$1" = run ] && [ "$2" = build ] || exit 64
NPM
chmod +x "$backend_fake_bin/git" "$backend_fake_bin/docker" \
	"$backend_fake_bin/ssh" "$backend_fake_bin/scp" "$backend_fake_bin/aws" \
	"$backend_fake_bin/npm"

[ "$(PATH="$backend_fake_bin:$PATH" FAKE_GIT_SHA=deadbee "$fixture/scripts/read-git-sha.sh")" = deadbee ] ||
	fail 'Git SHA reader changed the resolved short commit'
expect_fail 'Git SHA reader rejects non-hex output' \
	env "PATH=$backend_fake_bin:$PATH" FAKE_GIT_SHA=not-a-sha \
	"$fixture/scripts/read-git-sha.sh"

for backend_script in build-backend-image.sh push-backend-image.sh deploy-backend.sh; do
	grep -Fq 'scripts/read-git-sha.sh' "$fixture/scripts/$backend_script" ||
		fail "$backend_script does not use the shared Git SHA reader"
	if grep -Fq 'scripts/read-daemon-version.sh' "$fixture/scripts/$backend_script"; then
		fail "$backend_script still reads the repository DAEMON_VERSION"
	fi
done
: >"$backend_docker_log"
: >"$backend_ssh_log"
PATH="$backend_fake_bin:$PATH" BACKEND_DOCKER_LOG="$backend_docker_log" \
	BACKEND_SSH_LOG="$backend_ssh_log" DOCKER_REPO=example/notty DAEMON_VERSION=9.9.9 \
	make -s -C "$fixture" backend-deploy >/dev/null
grep -Fq -- '-t example/notty:backend-deadbee' "$backend_docker_log" ||
	fail 'backend deploy did not build the commit-addressed image'
grep -Fq -- '-t example/notty:backend-latest' "$backend_docker_log" ||
	fail 'backend deploy did not update the convenience latest image'
grep -Fq 'example/notty:backend-deadbee' "$backend_ssh_log" ||
	fail 'backend deploy did not restart Compose with the commit-addressed image'
pass 'backend build and deploy use Git SHA without reading DAEMON_VERSION'

for frontend_script in build-frontend.sh deploy-frontend.sh build-homepage.sh deploy-homepage.sh; do
	if grep -Fq 'scripts/read-daemon-version.sh' "$fixture/scripts/$frontend_script"; then
		fail "$frontend_script still reads the repository DAEMON_VERSION"
	fi
done
mkdir -p "$fixture/frontend/node_modules" "$fixture/frontend/dist" "$fixture/homepage"
printf '<html>app</html>\n' >"$fixture/frontend/dist/index.html"
printf '<html>home</html>\n' >"$fixture/homepage/index.html"
for browser_asset in \
	favicon.svg favicon.ico favicon-32x32.png favicon-16x16.png \
	apple-touch-icon.png safari-pinned-tab.svg
do
	printf '%s\n' "$browser_asset" >"$fixture/frontend/dist/$browser_asset"
	printf '%s\n' "$browser_asset" >"$fixture/homepage/$browser_asset"
done
: >"$frontend_aws_log"
PATH="$backend_fake_bin:$PATH" FRONTEND_AWS_LOG="$frontend_aws_log" \
	R2_ENDPOINT_URL=https://example.invalid R2_HOMEPAGE_BUCKET=homepage \
	R2_APP_BUCKET=app "$fixture/scripts/deploy-frontend.sh" >/dev/null
[ -f "$fixture/dist/static/app/index.html" ] &&
	[ -f "$fixture/dist/static/homepage/index.html" ] ||
	fail 'frontend deploy did not stage app and homepage assets'
[ -s "$frontend_aws_log" ] || fail 'frontend deploy did not invoke the R2 uploader'
pass 'frontend build and deploy work without a DAEMON_VERSION file'

# The homepage-only deploy must publish the homepage bucket without needing the app bundle or an
# R2_APP_BUCKET, so a copy edit ships without a Vite rebuild.
rm -rf "$fixture/dist/static"
: >"$frontend_aws_log"
PATH="$backend_fake_bin:$PATH" FRONTEND_AWS_LOG="$frontend_aws_log" \
	R2_ENDPOINT_URL=https://example.invalid R2_HOMEPAGE_BUCKET=homepage \
	"$fixture/scripts/deploy-homepage.sh" >/dev/null
[ -f "$fixture/dist/static/homepage/index.html" ] ||
	fail 'homepage deploy did not stage homepage assets'
[ -d "$fixture/dist/static/app" ] && fail 'homepage deploy staged app assets'
[ -s "$frontend_aws_log" ] || fail 'homepage deploy did not invoke the R2 uploader'
pass 'homepage deploy publishes the homepage alone without an app bundle'

for invalid_file in "$tmp_dir/invalid"/*; do
	name="$(basename -- "$invalid_file")"
	cp "$invalid_file" "$fixture/DAEMON_VERSION"
	: >"$make_go_log"
	expect_fail "Make host daemon build rejects invalid $name file" \
		env "PATH=$make_fake_bin:$PATH" "MAKE_GO_LOG=$make_go_log" \
		make -s -C "$fixture" _build-daemon-host
	[ ! -s "$make_go_log" ] || fail "Make invoked Go after rejecting invalid $name"
	for script in build-daemon-release.sh build-macos-desktop-release.sh build-windows-desktop-payloads.sh; do
		expect_fail "$script rejects invalid $name file before build work" sh "$fixture/scripts/$script"
	done
done

rm -f "$fixture/DAEMON_VERSION"
for script in build-daemon-release.sh build-macos-desktop-release.sh build-windows-desktop-payloads.sh; do
	expect_fail "$script rejects missing DAEMON_VERSION before build work" sh "$fixture/scripts/$script"
done

for script in build-daemon-release.sh build-macos-desktop-release.sh; do
	expect_fail "$script rejects excess positional arguments" sh "$fixture/scripts/$script" one two
	grep -q 'usage:' "$tmp_dir/output" || fail "$script excess-arity failure did not report usage"
done
expect_fail 'upload-r2.sh rejects every positional argument' sh "$fixture/scripts/upload-r2.sh" 1.2.3
grep -q 'usage:' "$tmp_dir/output" || fail 'upload-r2.sh excess-arity failure did not report usage'
expect_fail 'build-frontend.sh rejects every positional argument' sh "$fixture/scripts/build-frontend.sh" 1.2.3
grep -q 'usage:' "$tmp_dir/output" || fail 'build-frontend.sh excess-arity failure did not report usage'

if command -v pwsh >/dev/null 2>&1; then
	for version in 0.0.0 1.2.3 255.255.65535; do
		printf '%s\n' "$version" >"$fixture/DAEMON_VERSION"
		actual="$(pwsh -NoLogo -NoProfile -NonInteractive -File "$fixture/scripts/read-daemon-version.ps1")"
		[ "$actual" = "$version" ] || fail "PowerShell reader changed valid $version"
		pass "PowerShell reader accepts $version"
	done
	printf '1.2.3\r\n' >"$fixture/DAEMON_VERSION"
	actual="$(pwsh -NoLogo -NoProfile -NonInteractive -File "$fixture/scripts/read-daemon-version.ps1")"
	[ "$actual" = 1.2.3 ] || fail 'PowerShell reader changed valid CRLF-terminated version'
	pass 'PowerShell reader accepts a CRLF-terminated version'
	rm -f "$fixture/DAEMON_VERSION"
	expect_fail 'PowerShell reader rejects missing DAEMON_VERSION' pwsh -NoLogo -NoProfile -NonInteractive -File "$fixture/scripts/read-daemon-version.ps1"
	for invalid_file in "$tmp_dir/invalid"/*; do
		name="$(basename -- "$invalid_file")"
		cp "$invalid_file" "$fixture/DAEMON_VERSION"
		expect_fail "PowerShell reader rejects $name" pwsh -NoLogo -NoProfile -NonInteractive -File "$fixture/scripts/read-daemon-version.ps1"
	done
else
	printf '%s\n' 'SKIP: PowerShell runtime assertions (pwsh unavailable)'
fi

printf '%s\n' 'All daemon version contract tests passed.'
