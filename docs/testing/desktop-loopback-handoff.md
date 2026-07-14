# Desktop loopback handoff gate

This gate proves one narrow contract before the Codesk desktop app is built:

1. A native process pre-binds `127.0.0.1:0` and generates a 256-bit nonce in the callback path.
2. The normal HTTPS Codesk web app authenticates the human, asks for an explicit workspace, and calls the existing daemon-create API.
3. A top-level `application/x-www-form-urlencoded` form POST sends the returned daemon ID, opaque token, and workspace metadata to the callback.
4. The loopback listener accepts the first valid POST, flushes a success page, and stops accepting connections.

The token is allowed only in the daemon-create HTTPS response, the temporary hidden form value, the loopback POST body, and the receiver's in-memory result. It must not appear in a URL, referrer, browser history entry, console, HAR file, screenshot, command argument, command output, or log.

This is an unshipped feasibility harness. It is not the product `/desktop/connect` route.

## Local gates

From the repository root:

```sh
./scripts/build-yffi.sh
cd frontend
npm ci
cd ..

gofmt -w daemon/internal/desktop/handoff/*.go daemon/cmd/desktop-handoff-spike/*.go
go test ./daemon/internal/desktop/handoff ./daemon/cmd/desktop-handoff-spike
go test -race ./daemon/internal/desktop/handoff ./daemon/cmd/desktop-handoff-spike
go vet ./daemon/internal/desktop/handoff ./daemon/cmd/desktop-handoff-spike
go test ./...
go vet ./...

cd frontend
npm test
npm run build
test ! -e dist/desktop-handoff-spike.html
npm run build:desktop-handoff-spike
test -e .test-dist/desktop-handoff/desktop-handoff-spike.html
```

The Go tests cover timeout, cancellation, wrong host/path/nonce, query rejection, malformed and oversized bodies, missing/duplicate/unknown fields, opaque-token preservation, first-valid-wins concurrency, second-request rejection, and output redaction. Do not replace these deterministic tests with manual `curl` commands containing a real daemon token.

The ordinary frontend build must omit `desktop-handoff-spike.html`. Only the explicit spike build emits it under `.test-dist/desktop-handoff`.

## Prepare one exact head

Use the same Git commit for the HTTPS harness deployment and each native command binary:

```sh
git rev-parse HEAD
git status --short
go version
node --version
npm --version
```

Build the native spike command with `-trimpath`, then hash it.

macOS:

```sh
mkdir -p artifacts
go build -trimpath -o artifacts/desktop-handoff-spike ./daemon/cmd/desktop-handoff-spike
shasum -a 256 artifacts/desktop-handoff-spike
sw_vers
```

Windows PowerShell:

```powershell
New-Item -ItemType Directory -Force artifacts | Out-Null
go build -trimpath -o artifacts\desktop-handoff-spike.exe ./daemon/cmd/desktop-handoff-spike
Get-FileHash -Algorithm SHA256 artifacts\desktop-handoff-spike.exe
```

Build the test-only web artifact and record its file hashes:

```sh
cd frontend
npm ci
npm run build:desktop-handoff-spike
find .test-dist/desktop-handoff -type f -print0 | sort -z | xargs -0 sha256sum
```

Overlay that directory onto an authorized branch/test deployment of the normal Codesk frontend. The harness must be served as `https://<codesk-test-origin>/desktop-handoff-spike.html` on the exact same origin as the login page and API session. Do not deploy the harness to production, and do not use an HTTP or localhost page for an acceptance row.

## Run a browser row

Each browser row needs a fresh receiver and creates a fresh daemon token. Start the native command without redirecting output to a shared log:

```sh
./artifacts/desktop-handoff-spike \
  --connect-page https://<codesk-test-origin>/desktop-handoff-spike.html \
  --timeout 10m
```

On Windows, use `artifacts\desktop-handoff-spike.exe` with the same flags.

1. Copy only the printed `connect_url` into the browser under test. It contains the loopback callback nonce, never the daemon token.
2. If prompted, open Codesk sign-in in the new tab, sign in normally, return to the harness, and select **Check again**.
3. Select one workspace explicitly, keep or edit the desktop name, and select **Create and connect** once.
4. Record any Local Network Access prompt, insecure-form warning, or browser interstitial before proceeding.
5. The browser must navigate to the literal `http://127.0.0.1:<port>/desktop/connect/<nonce>` success page. The command must report `handoff_accepted=true` and `token_received=true` without printing the token.
6. In developer tools, record only the callback request method, URL, response status, and content type. Do not capture or export its form payload. Never save a HAR: HAR request bodies contain the credential.
7. Verify the callback request has no `Referer` header, browser history contains no token, the console contains no token, and command output contains no token.
8. Confirm a second request cannot reach the listener. The automated test is authoritative; do not replay the real body.
9. Delete the disconnected test daemon through the Codesk management UI after the evidence is recorded.

The required rows are:

- Current stable Microsoft Edge on native Windows.
- Current stable Google Chrome on native Windows.
- Current stable Safari on native macOS.

Playwright Chromium/WebKit, a cross-compiled binary, a VM on a different OS, a localhost-origin page, or an HTTP-origin page is diagnostic evidence only.

## Evidence record

Create one redacted Markdown record per browser. Hash the finished record before attaching it to the task thread.

```text
Git commit:
Git tree clean: yes/no
Native binary SHA-256:
Harness file SHA-256 values:
Codesk HTTPS origin:
OS edition/build:
Browser name/version:
Local Network Access policy/prompt:
Insecure-form warning/interstitial:
Callback method: POST
Callback URL: http://127.0.0.1:<port>/desktop/connect/<redacted-nonce>
Callback content type: application/x-www-form-urlencoded
Callback response: 200
Success page rendered: yes/no
Command accepted: yes/no
Second request rejected: yes/no
Token absent from URL/history: yes/no
Referer absent: yes/no
Token absent from console/output/logs: yes/no
Saved HAR or payload screenshot: no
Result: PASS/BLOCKED
Notes (redacted):
```

For browser version evidence, use `edge://version`, `chrome://version`, or **Safari > About Safari**. For Windows build evidence use `winver`; for macOS use `sw_vers`.

## Stop conditions

If a required browser blocks or rewrites the top-level POST, preserve the redacted browser version, policy state, visible error, and whether the request reached the listener. Report the row as `BLOCKED` and stop the desktop program for redesign.

Do not substitute a query-string token, plaintext handoff file, `fetch`/CORS workaround, browser extension, localhost control API, human JWT, or new backend bootstrap/auth protocol.

## Protocol references

- [HTML form submission](https://html.spec.whatwg.org/multipage/form-control-infrastructure.html#form-submission-algorithm) serializes URL-encoded POST fields into the request body.
- [Mixed Content](https://www.w3.org/TR/mixed-content/) treats top-level document navigation separately and permits user-agent warnings for insecure form submission.
- [Chrome Local Network Access](https://developer.chrome.com/blog/local-network-access) documents the shipped permission boundary for local-network requests. Native browser results remain the acceptance authority.
