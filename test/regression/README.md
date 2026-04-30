# Regression Tests

These tests exercise the real backend, daemon, filesystem projection, postgres CRDT history, and websocket path.

Run the fast regression tier:

```sh
go test -tags=regression ./test/regression
```

Useful knobs:

```sh
NOTTY_REGRESSION_STRESS_LINES=100000 go test -tags=regression ./test/regression -run AppendOnly
NOTTY_REGRESSION_BACKEND_RESTART=1 NOTTY_REGRESSION_RESTART_LINES=20000 go test -tags=regression ./test/regression -run BackendRestart
```

The suite uses a dedicated Docker Compose project and random localhost ports, so it can run beside the normal dev stack.

Known gap: delete/backspace peer-convergence should be added as a passing websocket regression after the current CRDT delete behavior is fixed. A synthetic synced Go CRDT peer currently reproduces a delete-range failure, so the committed websocket test covers insert broadcast/persistence only.

Known gap: the backend-restart append test is opt-in because it currently reproduces a lost-write/reconnect problem. During a 1000-line reduced run, backend reconstruction stopped at 143 lines after restart, indicating websocket write success is being treated as persistence without a server-level acknowledgement.

Merge/conflict coverage:

- Non-overlapping local filesystem append plus remote websocket append must converge across backend, daemon projection, and a fresh websocket recipient.
- Overlapping local/remote rewrites are treated as unresolved divergence for now: the regression asserts the local edit is not silently clobbered while backend websocket recipients stay consistent on the remote CRDT state.
