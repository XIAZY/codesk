#!/usr/bin/env bash
# Contract for deploy-frontend.sh's preflight and live verification.
# The property under test: a deploy that did not reach users MUST NOT report success. On
# 2026-07-30 `rclone sync --no-check-dest` transferred zero bytes for two days while exiting 0,
# and separately "/" and "/index.html" resolved to different objects — so a deploy can fail in
# ways that every intermediate step calls fine.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

mkdir -p "$TMP_DIR/scripts/lib" "$TMP_DIR/dist/static/app/assets" "$TMP_DIR/bin"
cp "$ROOT_DIR/scripts/deploy-frontend.sh" "$TMP_DIR/scripts/"
cp "$ROOT_DIR/scripts/lib/deploy-env.sh" "$TMP_DIR/scripts/lib/"
printf '#!/bin/sh\nexit 0\n' > "$TMP_DIR/scripts/build-frontend.sh"

# The uploader either moves the served object or silently doesn't — the whole point of the check.
# It also records that it ran, so precondition failures can be proven to publish nothing.
cat > "$TMP_DIR/scripts/upload-r2.sh" <<'FAKEUPLOAD'
#!/bin/sh
echo ran >> "$FAKE_UPLOAD_LOG"
if [ "${FAKE_UPLOAD_WORKS:-1}" = "1" ]; then
  cat "$FAKE_BUILT_REF" > "$FAKE_SERVED_STATE"
  cp "$FAKE_BUILT_ASSET" "$FAKE_SERVED_ASSET"
fi
exit 0
FAKEUPLOAD

# Serves "/" (an index referencing the current served bundle) and "/assets/<ref>" (its bytes).
cat > "$TMP_DIR/bin/curl" <<'FAKECURL'
#!/bin/sh
# A stub cannot enforce a real curl timeout, so it refuses any probe that omits one. Without this
# the "bounded" retry is unbounded — one hung connection outlasts any attempt budget — and no
# behavioural assertion in this file could ever notice the flags going missing.
case " $* " in *" --max-time "*) ;; *) echo "curl invoked without --max-time" >&2; exit 64 ;; esac
case " $* " in *" --connect-timeout "*) ;; *) echo "curl invoked without --connect-timeout" >&2; exit 64 ;; esac
url=""
for arg in "$@"; do case "$arg" in http*) url="$arg" ;; esac; done
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
case "$url" in
  */assets/*)
    [ "${FAKE_ASSET_REACHABLE:-1}" = "1" ] || exit 22
    cat "$FAKE_SERVED_ASSET" ;;
  *) printf '<script src="/assets/%s"></script>\n' "$(cat "$FAKE_SERVED_STATE")" ;;
esac
exit 0
FAKECURL

cat > "$TMP_DIR/bin/git" <<'FAKEGIT'
#!/bin/sh
case "$*" in
  *"rev-parse --short HEAD"*) echo "abc1234"; exit 0 ;;
  *"rev-parse --git-dir"*) echo ".git"; exit 0 ;;
  *"rev-parse --verify --quiet origin/main"*) echo "def5678"; exit 0 ;;
  *fetch*) exit "${FAKE_FETCH_FAILS:-0}" ;;
  *"merge-base --is-ancestor"*) exit "${FAKE_BEHIND:-0}" ;;
  *"rev-list --count"*) echo "3"; exit 0 ;;
esac
exit 0
FAKEGIT
chmod +x "$TMP_DIR/scripts/build-frontend.sh" "$TMP_DIR/scripts/upload-r2.sh" \
	"$TMP_DIR/bin/curl" "$TMP_DIR/bin/git"

export FAKE_SERVED_STATE="$TMP_DIR/served.txt"
export FAKE_BUILT_REF="$TMP_DIR/built.txt"
export FAKE_SERVED_ASSET="$TMP_DIR/served-asset.js"
export FAKE_BUILT_ASSET="$TMP_DIR/built-asset.js"
export FAKE_UPLOAD_LOG="$TMP_DIR/uploads.log"
export FAKE_CURL_LOG="$TMP_DIR/curl.log"
export NOTTY_FRONTEND_ORIGIN="https://app.example.test"
export NOTTY_DEPLOY_VERIFY_ATTEMPTS=2
export NOTTY_DEPLOY_VERIFY_INTERVAL=0
export NOTTY_DEPLOY_ENV_FILE="$TMP_DIR/absent.env"

run_case() {
	built="$1"; served="$2"; shift 2
	printf '%s' "$built" > "$FAKE_BUILT_REF"
	printf '%s' "$served" > "$FAKE_SERVED_STATE"
	printf 'BUILT-%s' "$built" > "$FAKE_BUILT_ASSET"
	printf 'SERVED-%s' "$served" > "$FAKE_SERVED_ASSET"
	if [ "${FAKE_BREAK_BUILT_INDEX:-0}" = "1" ]; then
		printf '<html>no bundle reference</html>\n' > "$TMP_DIR/dist/static/app/index.html"
	else
		printf '<script src="/assets/%s"></script>\n' "$built" > "$TMP_DIR/dist/static/app/index.html"
	fi
	rm -f "$TMP_DIR/dist/static/app/assets/"*
	cp "$FAKE_BUILT_ASSET" "$TMP_DIR/dist/static/app/assets/$built"
	: > "$FAKE_UPLOAD_LOG"
	: > "$FAKE_CURL_LOG"
	set +e
	CASE_OUT="$(env PATH="$TMP_DIR/bin:$PATH" "$@" sh "$TMP_DIR/scripts/deploy-frontend.sh" 2>&1)"
	CASE_RC=$?
	set -e
}

fail() { echo "deploy-frontend contract: $1" >&2; echo "$CASE_OUT" >&2; exit 1; }
assert_no_upload() { [ ! -s "$FAKE_UPLOAD_LOG" ] || fail "$1 — it published before failing"; }

# Upload works and content changed: the happy path, and it must name what moved.
run_case index-NEW.js index-OLD.js FAKE_UPLOAD_WORKS=1
[ "$CASE_RC" -eq 0 ] || fail "successful deploy should exit 0, got $CASE_RC"
grep -q "index-OLD.js -> index-NEW.js" <<< "$CASE_OUT" || fail "must name the before and after bundle"
grep -q "digest matches" <<< "$CASE_OUT" || fail "must confirm the referenced asset, not just the filename"

# THE CORE PROPERTY: uploader exits 0 having shipped nothing.
run_case index-NEW.js index-OLD.js FAKE_UPLOAD_WORKS=0
[ "$CASE_RC" -ne 0 ] || fail "a deploy that shipped nothing MUST fail, got exit 0"
grep -q "did NOT reach users" <<< "$CASE_OUT" || fail "silent-no-op deploy must say so plainly"

# Content genuinely identical: not a failure, but never silent — the stale-checkout tell.
run_case index-SAME.js index-SAME.js FAKE_UPLOAD_WORKS=1
[ "$CASE_RC" -eq 0 ] || fail "an identical redeploy should succeed, got $CASE_RC"
grep -q "UNCHANGED" <<< "$CASE_OUT" || fail "an unchanged deploy must state that it was unchanged"
grep -q "abc1234" <<< "$CASE_OUT" || fail "the unchanged message must name the deploying SHA"

# REVERSE PARTIAL SYNC: the index moved but its bundle is unreachable — a white screen that a
# filename-only comparison calls a success.
run_case index-NEW.js index-OLD.js FAKE_UPLOAD_WORKS=1 FAKE_ASSET_REACHABLE=0
[ "$CASE_RC" -ne 0 ] || fail "live index referencing an unreachable bundle must fail, got exit 0"
grep -q "blank page" <<< "$CASE_OUT" || fail "must explain that users would see nothing"

# Same shape, subtler: the asset is reachable but is not the one we built.
printf '%s' index-NEW.js > "$FAKE_BUILT_REF"
run_case index-NEW.js index-NEW.js FAKE_UPLOAD_WORKS=0
[ "$CASE_RC" -ne 0 ] || fail "a stale asset under a matching filename must fail, got exit 0"
grep -q "partial sync" <<< "$CASE_OUT" || fail "digest mismatch must be named as a partial sync"

# Preflight: a stale checkout rebuilds old code into a byte-identical bundle and uploads it
# successfully. Nothing downstream can see it, so it must abort BEFORE publishing.
run_case index-NEW.js index-OLD.js FAKE_BEHIND=1
[ "$CASE_RC" -ne 0 ] || fail "a stale checkout must fail, got exit 0"
grep -q "git pull" <<< "$CASE_OUT" || fail "stale checkout must say how to fix it"
assert_no_upload "stale checkout"

# An unverifiable remote is not evidence the checkout is current: fail closed.
run_case index-NEW.js index-OLD.js FAKE_FETCH_FAILS=1
[ "$CASE_RC" -ne 0 ] || fail "an unfetchable origin/main must fail closed, got exit 0"
assert_no_upload "failed fetch"

# ...but an explicit, named override still exists for a genuinely offline deploy.
run_case index-NEW.js index-OLD.js FAKE_FETCH_FAILS=1 NOTTY_DEPLOY_ALLOW_STALE_REMOTE=1
[ "$CASE_RC" -eq 0 ] || fail "explicit stale-remote override should proceed, got $CASE_RC"
grep -q "WARNING" <<< "$CASE_OUT" || fail "the override must be loud"

# Verification that cannot run is a failure — and must be known BEFORE anything is published.
run_case index-NEW.js index-OLD.js NOTTY_FRONTEND_ORIGIN=
[ "$CASE_RC" -ne 0 ] || fail "unverifiable deploy must fail, got exit 0"
assert_no_upload "missing origin"

# curl absent is knowable before publishing. Without the explicit precheck the deploy still fails
# closed — but with a bare "command not found" instead of naming the tool, which is exactly the
# diagnosis you do not want at 2am. So the message is asserted, not just the exit code.
# A PATH that genuinely lacks curl: every other tool the script needs, symlinked, nothing else.
# Prepending a fake dir to the real PATH would not work — curl is still found further along it.
curl_less_bin="$TMP_DIR/nocurl"
mkdir -p "$curl_less_bin"
cp "$TMP_DIR/bin/git" "$curl_less_bin/git"
for needed in sh dirname basename pwd date grep head sha256sum shasum mktemp rm sleep cat cp sed printf wc tr cut ln mkdir touch env; do
	needed_path="$(command -v "$needed" 2>/dev/null || true)"
	[ -n "$needed_path" ] && ln -sf "$needed_path" "$curl_less_bin/$needed"
done
[ -x "$curl_less_bin/curl" ] && fail "the no-curl fixture still provides curl"
run_case index-NEW.js index-OLD.js PATH="$curl_less_bin"
[ "$CASE_RC" -ne 0 ] || fail "a deploy with no curl must fail, got exit 0"
assert_no_upload "missing curl"
grep -q "curl not found" <<< "$CASE_OUT" || fail "a missing curl must be named, not left as 'command not found'"

# A build that produced no usable index is knowable after the build and before the upload.
FAKE_BREAK_BUILT_INDEX=1 run_case index-NEW.js index-OLD.js
FAKE_BREAK_BUILT_INDEX=0
[ "$CASE_RC" -ne 0 ] || fail "a malformed built index must fail, got exit 0"
assert_no_upload "malformed built index"
grep -q "no bundle reference found" <<< "$CASE_OUT" || fail "a malformed built index must say what is wrong with it"

# A machine with no hash tool must not deploy. Before this was preflighted, `file_digest` fell
# through to `shasum | cut` — and a pipeline ending in `cut` exits 0 even when the hash command is
# missing, so BOTH digests became empty and "" = "" reported "digest matches". A verifier that can
# assert a match without computing a digest is this script's own failure domain in miniature.
hashless_bin="$TMP_DIR/nohash"
mkdir -p "$hashless_bin"
for needed in sh dirname basename pwd date grep head mktemp rm sleep cat cp sed printf wc tr cut ln mkdir touch env; do
	needed_path="$(command -v "$needed" 2>/dev/null || true)"
	[ -n "$needed_path" ] && ln -sf "$needed_path" "$hashless_bin/$needed"
done
cp "$TMP_DIR/bin/curl" "$hashless_bin/curl"
cp "$TMP_DIR/bin/git" "$hashless_bin/git"
[ -x "$hashless_bin/sha256sum" ] || [ -x "$hashless_bin/shasum" ] && fail "the hashless fixture still provides a digest tool"
run_case index-NEW.js index-OLD.js PATH="$hashless_bin"
[ "$CASE_RC" -ne 0 ] || fail "a deploy with no digest tool must fail, got exit 0"
assert_no_upload "missing digest tool"
grep -qE "sha256sum nor shasum" <<< "$CASE_OUT" || fail "a missing digest tool must be named"

# A hash tool that EXISTS but emits garbage is the subtler half: `cut` still exits 0, so only the
# output shape can catch it. 8 hex chars followed by 56 arbitrary bytes is the specific case a
# prefix-glob validator admits.
bad_hash_bin="$TMP_DIR/badhash"
mkdir -p "$bad_hash_bin"
for needed in sh dirname basename pwd date grep head mktemp rm sleep cat cp sed printf wc tr cut ln mkdir touch env; do
	needed_path="$(command -v "$needed" 2>/dev/null || true)"
	[ -n "$needed_path" ] && ln -sf "$needed_path" "$bad_hash_bin/$needed"
done
cp "$TMP_DIR/bin/curl" "$bad_hash_bin/curl"
cp "$TMP_DIR/bin/git" "$bad_hash_bin/git"
printf '#!/bin/sh\nprintf "deadbeefZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ  %%s\\n" "$1"\n' > "$bad_hash_bin/sha256sum"
chmod +x "$bad_hash_bin/sha256sum"
run_case index-NEW.js index-OLD.js PATH="$bad_hash_bin"
[ "$CASE_RC" -ne 0 ] || fail "a malformed digest must fail, got exit 0"
assert_no_upload "malformed digest"
grep -q "not a sha256" <<< "$CASE_OUT" || fail "a malformed digest must be named as such"

# A bad probe budget must be rejected outright rather than silently making the bound meaningless.
run_case index-NEW.js index-OLD.js NOTTY_DEPLOY_VERIFY_ATTEMPTS=0
[ "$CASE_RC" -ne 0 ] || fail "a non-positive attempt budget must be rejected, got exit 0"
assert_no_upload "invalid attempts"

# The probe must read "/" — the URL users hit — not "/index.html". On 2026-07-30 those resolved
# to DIFFERENT objects, and a check against the named path called that deploy verified while every
# real user saw a two-day-old app. Pinning the probed path is what makes the check about users.
run_case index-NEW.js index-OLD.js FAKE_UPLOAD_WORKS=1
grep -q '/index.html' "$FAKE_CURL_LOG" && fail "the verifier probed /index.html — it must read '/', the URL users actually hit"
grep -qE '^https://app\.example\.test/(\?|$)' "$FAKE_CURL_LOG" || fail "the verifier never probed the bare '/' path"
echo "deploy-frontend verification contract passed."
