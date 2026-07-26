# Regression Tests

These tests exercise the real backend, daemon, filesystem projection, postgres CRDT history, and websocket path.

Run the fast regression tier:

```sh
go test -tags=regression ./test/regression
```

Useful knobs:

```sh
NOTTY_REGRESSION_DEADLINE_SCALE=4 go test -tags=regression ./test/regression
NOTTY_REGRESSION_STRESS_LINES=100000 go test -tags=regression ./test/regression -run AppendOnly
NOTTY_REGRESSION_BACKEND_RESTART=1 NOTTY_REGRESSION_RESTART_LINES=20000 go test -tags=regression ./test/regression -run BackendRestart
```

The suite uses a dedicated Docker Compose project and random localhost ports, so it can run beside the normal dev stack.
The Compose stack provides fake Mailgun settings so strict email configuration does not prevent backend boot; the harness marks its own throwaway account verified in Postgres before logging in.
`NOTTY_REGRESSION_DEADLINE_SCALE` multiplies convergence deadlines and Docker command timeouts for loaded CI runners without adding blanket sleeps; values below 1 are rejected so local/default runs stay strict.

Delete/backspace coverage is included at both levels: a CRDT-only regression verifies partial delete ranges inside larger text items, and a websocket regression verifies peer propagation plus backend persistence.

Root-native lifecycle coverage includes a backend-API-driven regression that allocates documents with `POST /documents`, writes root entries over the root CRDT stream, performs repeated content inserts/deletes over document websockets, moves documents through root CRDT updates, and verifies after each step that Postgres CRDT reconstruction and the daemon-managed filesystem match.

Daemon filesystem lifecycle coverage includes a local-filesystem-driven regression that creates a file in the daemon workspace, performs repeated inserts/deletes by rewriting the local file, moves the file, edits it after the move, deletes it, and verifies after each operation that Postgres root/content CRDT reconstruction matches the expected state.

Thread integrity coverage verifies that clients can create document threads with Yjs relative anchors, that the backend preserves those caller-supplied anchors without materializing document text, and that raw-offset text-range thread creation is rejected.

Model-profile coverage runs a deterministic local Codex app-server fixture through the real daemon, authenticated backend APIs, and Postgres. It verifies the exact seven-model catalog after a slow successful probe, explicit and inherited create-agent profiles, thread start/resume parameters, and zero-spawn rejection when a model vanishes, an effort is unsupported, or the runtime default moves.

Known gap: the backend-restart append test is opt-in because it currently reproduces a lost-write/reconnect problem. During a 1000-line reduced run, backend reconstruction stopped at 143 lines after restart, indicating websocket write success is being treated as persistence without a server-level acknowledgement.

Merge/conflict coverage:

- Non-overlapping local filesystem append plus remote websocket append must converge across backend, daemon projection, and a fresh websocket recipient.
- Overlapping local/remote whole-line rewrites must converge across backend, daemon projection, and a fresh websocket recipient without losing either complete rewritten line.
