#!/usr/bin/env bash
# Contract for deploy-frontend.sh's preflight and live verification.
# The property under test: a deploy that ships nothing MUST NOT report success. On 2026-07-30
# `rclone sync --no-check-dest` transferred zero bytes for days while exiting 0, and the only
# thing that caught it was a human fetching the served bundle by hand.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

mkdir -p "$TMP_DIR/scripts/lib" "$TMP_DIR/dist/static/app" "$TMP_DIR/bin"
cp "$ROOT_DIR/scripts/deploy-frontend.sh" "$TMP_DIR/scripts/"
cp "$ROOT_DIR/scripts/lib/deploy-env.sh" "$TMP_DIR/scripts/lib/"
printf '#!/bin/sh\nexit 0\n' > "$TMP_DIR/scripts/build-frontend.sh"

# The uploader either moves the served object or silently doesn't — the whole point of the check.
cat > "$TMP_DIR/scripts/upload-r2.sh" <<'FAKEUPLOAD'
#!/bin/sh
if [ "${FAKE_UPLOAD_WORKS:-1}" = "1" ]; then
  cat "$FAKE_BUILT_REF" > "$FAKE_SERVED_STATE"
fi
exit 0
FAKEUPLOAD

cat > "$TMP_DIR/bin/curl" <<'FAKECURL'
#!/bin/sh
printf '<script src="/assets/%s"></script>\n' "$(cat "$FAKE_SERVED_STATE")"
exit 0
FAKECURL

cat > "$TMP_DIR/bin/git" <<'FAKEGIT'
#!/bin/sh
case "$*" in
  *"rev-parse --short HEAD"*) echo "abc1234"; exit 0 ;;
  *"rev-parse --git-dir"*) echo ".git"; exit 0 ;;
  *"rev-parse --verify --quiet origin/main"*) echo "def5678"; exit 0 ;;
  *"merge-base --is-ancestor"*) exit "${FAKE_BEHIND:-0}" ;;
  *"rev-list --count"*) echo "3"; exit 0 ;;
  *fetch*) exit 0 ;;
esac
exit 0
FAKEGIT
chmod +x "$TMP_DIR/scripts/build-frontend.sh" "$TMP_DIR/scripts/upload-r2.sh" \
	"$TMP_DIR/bin/curl" "$TMP_DIR/bin/git"

export FAKE_SERVED_STATE="$TMP_DIR/served.txt"
export FAKE_BUILT_REF="$TMP_DIR/built.txt"
export NOTTY_FRONTEND_ORIGIN="https://app.example.test"
export NOTTY_DEPLOY_VERIFY_ATTEMPTS=2
export NOTTY_DEPLOY_VERIFY_INTERVAL=0
export NOTTY_DEPLOY_ENV_FILE="$TMP_DIR/absent.env"

run_case() {
	built="$1"; served="$2"
	printf '%s' "$built" > "$FAKE_BUILT_REF"
	printf '%s' "$served" > "$FAKE_SERVED_STATE"
	printf '<script src="/assets/%s"></script>\n' "$built" > "$TMP_DIR/dist/static/app/index.html"
	shift 2
	set +e
	CASE_OUT="$(env PATH="$TMP_DIR/bin:$PATH" "$@" sh "$TMP_DIR/scripts/deploy-frontend.sh" 2>&1)"
	CASE_RC=$?
	set -e
}

fail() { echo "deploy-frontend contract: $1" >&2; echo "$CASE_OUT" >&2; exit 1; }

# The upload works and the content changed: the happy path, and it must say what moved.
run_case index-NEW.js index-OLD.js FAKE_UPLOAD_WORKS=1
[ "$CASE_RC" -eq 0 ] || fail "successful deploy should exit 0, got $CASE_RC"
grep -q "verified live" <<< "$CASE_OUT" || fail "successful deploy must report the live verification"
grep -q "index-OLD.js -> index-NEW.js" <<< "$CASE_OUT" || fail "must name the before and after bundle"

# THE CORE PROPERTY: uploader exits 0 having shipped nothing. This is the July-30 outage.
run_case index-NEW.js index-OLD.js FAKE_UPLOAD_WORKS=0
[ "$CASE_RC" -ne 0 ] || fail "a deploy that shipped nothing MUST fail, got exit 0"
grep -q "did NOT reach users" <<< "$CASE_OUT" || fail "silent-no-op deploy must say so plainly"
grep -q "index-NEW.js" <<< "$CASE_OUT" || fail "failure must name the bundle that was expected"

# Content genuinely identical: not a failure, but never silent — it's the stale-checkout tell.
run_case index-SAME.js index-SAME.js FAKE_UPLOAD_WORKS=1
[ "$CASE_RC" -eq 0 ] || fail "an identical redeploy should succeed, got $CASE_RC"
grep -q "UNCHANGED" <<< "$CASE_OUT" || fail "an unchanged deploy must state that it was unchanged"
grep -q "abc1234" <<< "$CASE_OUT" || fail "the unchanged message must name the deploying SHA"

# Preflight: a checkout behind origin/main rebuilds old code into a byte-identical bundle and
# uploads it successfully. Nothing downstream can detect that, so it must be caught before build.
run_case index-NEW.js index-OLD.js FAKE_BEHIND=1
[ "$CASE_RC" -ne 0 ] || fail "a stale checkout must fail before building, got exit 0"
grep -q "behind origin/main" <<< "$CASE_OUT" || fail "stale checkout must say what is wrong"
grep -q "git pull" <<< "$CASE_OUT" || fail "stale checkout must say how to fix it"
grep -q "verified live" <<< "$CASE_OUT" && fail "preflight failure must abort before the upload"

# Verification that cannot run is a failure, not a pass — the deploy is unproven either way.
run_case index-NEW.js index-OLD.js NOTTY_FRONTEND_ORIGIN=
[ "$CASE_RC" -ne 0 ] || fail "unverifiable deploy must fail, got exit 0"
grep -q "cannot verify" <<< "$CASE_OUT" || fail "must say verification was impossible"

echo "deploy-frontend verification contract passed."
