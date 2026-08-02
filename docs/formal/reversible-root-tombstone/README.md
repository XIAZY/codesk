# Reversible Root Tombstone Model Check

This directory is the Phase 2 protocol gate for the reversible root tombstone
design. It contains a finite TLA+ model and an executable Go reference model in
`test/model/reversetombstone`. Neither artifact is production implementation.

## Verdict

The original one-row, latest-arrival rule is unsafe. After operation A1 has
been accepted and superseded by B1, a delayed A1 request can be accepted again
because the row no longer remembers that A1 was already used. That is an ABA
replay, not a legitimate new delete.

The corrected protocol is safe within the stated bounds when all of these are
load-bearing:

1. every accepted new tombstone advances one monotonic document generation;
2. a new operation compares an immutable expected generation with the current
   generation under the same transaction and row locks as the root mutation;
3. exact-current operation replay is identified before that CAS and changes no
   deadline, generation, consumption, or root state;
4. restore carries the accepted window generation and validates it in addition
   to the auth-derived origin and distinct durable tombstone and restore
   operation IDs; and
5. restore, expiry, status reads, projection, and ordinary root changes do not
   advance the generation.

Generation `0` is only the local/API sentinel for "no row or not yet captured."
A persisted backend window always has a positive generation. This matches the
Phase 1 storage representation where `0` maps to SQL `NULL`.

## ABA Counterexample

The complete delayed-request scenario is:

1. Replica A durably creates A1 with expected generation 0 and sends two exact
   requests.
2. One A1 request is accepted as generation 1; the other remains delayed.
3. A restores generation 1. A duplicate restore request remains delayed after
   response loss.
4. Replica B durably creates B1 with expected generation 1. B1 is accepted as
   generation 2 and tombstones the root again.
5. The delayed A1 tombstone arrives with expected generation 0.

Without the generation CAS, step 5 replaces B1 with A1 at generation 3 and
accepts A1 for a second time. The legacy TLC configuration finds the shorter
safety prefix `A1@1 -> B1@2 -> delayed A1@3` and stops when
`OperationAtMostOnce` fails. Without restore-generation binding, the delayed A1
restore can then reactivate the root across B1.

With the corrected rules, the step-5 request is neither an exact-current replay
nor a generation match. It is rejected without mutation; the delayed restore is
also rejected because its accepted generation is 1 while the current generation
is 2.

## Generation Capture

`BeginTombstone` in the models represents this concrete daemon boundary:

1. Read the current document generation from the authoritative backend. A
   generation-conflict response may provide the same non-secret value.
2. Revalidate that the local path is still absent and the namespace binding is
   still eligible.
3. In one local SQLite transaction, allocate a new tombstone operation UUID and
   persist that UUID, the immutable expected generation, and
   `tombstone_pending` before the first network attempt.
4. Retry response loss with the same operation UUID and expected generation.
5. On a generation conflict, terminalize that operation. If absence is still
   eligible, read the new generation and create a new operation UUID in a new
   local transaction. Never change the expected generation of an existing
   operation.
6. After acceptance, persist the positive accepted window generation returned
   by the backend. Every restore attempt carries that exact value.
7. Before the first restore network attempt, allocate and durably persist a
   restore operation UUID in a namespace distinct from the tombstone operation.
   Response-loss retries reuse that restore UUID; a later logical restore uses a
   new one.

The generation read and command are intentionally not atomic. The command CAS
is what makes an intervening tombstone safe.

## Backend Decision Order

Under the root-head lock and the reverse-window row lock, tombstone admission
uses this order:

1. If the current row has the same auth-derived origin and operation UUID,
   compare the complete request fingerprint, including expected generation.
   An exact match returns the stored result before time/CAS checks. A mismatch
   returns `operation_mismatch`. Neither path mutates the row.
2. Otherwise compare `expectedWindowGeneration` with the current generation
   (`0` only when no row exists). A mismatch returns
   `window_generation_conflict` and may return the current numeric generation,
   but never the current origin or operation.
3. Only after the CAS matches may semantic root/path validation run and a new
   tombstone be accepted at current generation plus one.

Restore admission first returns an exact consumed replay only when tombstone
operation, restore operation, auth-derived origin, and accepted generation all
match. A non-replay request must match the current tombstone operation, origin,
and generation before deadline, frontier, entry, and path validation.

## Transition Table

| Event | Required discriminator | Root/window mutation | Generation |
| --- | --- | --- | --- |
| First tombstone | No row, new op, expected `0`, semantic validation passes | Tombstone root; create open row | `0 -> 1` |
| Exact current tombstone retry | Same origin + op + full fingerprint | Return stored result; preserve deadline and consumption | unchanged |
| Same current op, different input | Same op but fingerprint differs | Reject `operation_mismatch` | unchanged |
| New tombstone | Different op, expected equals current `g`, validation passes | Tombstone root; replace row; clear consumption; set fresh deadline | `g -> g+1` |
| Stale/future tombstone | Different op, expected differs from current `g` | Reject `window_generation_conflict` | unchanged |
| Generation/status read | Authenticated document read | No mutation | unchanged |
| First restore | Current origin + tombstone op + distinct durable restore op + accepted generation; unconsumed; all restore guards pass | Activate root and persist the consumed restore identity atomically | unchanged |
| Exact consumed restore replay | Same origin + tombstone op + restore op + accepted generation | Return stored success before deadline check | unchanged |
| Wrong restore generation/op/origin | Not an exact consumed replay | Reject without root/window mutation | unchanged |
| Restore validation/storage failure | Current identity but deadline/frontier/entry/path/transaction fails | Reject or 5xx; preserve pre-request state | unchanged |
| Expiry | Server time reaches deadline | Derived status only; retain row and tombstone | unchanged |
| Ordinary root update or external tombstone | Existing CRDT path | No reverse-window mutation | unchanged |
| Local projection/restart | Replica-local state transition | No backend mutation | unchanged |

Only accepted new tombstones advance the generation. Restore and expiry do not.

## Component Contract

The exact language-level names may follow repository conventions, but Phase 2
must expose these semantic fields:

```text
ReadDocumentReverseGeneration(workspace, document) -> currentGeneration

OpenDocumentReverseWindowRequest {
    operationId
    expectedWindowGeneration
    entryId
    contentDocumentId
    expectedDesiredPath
}
OpenDocumentReverseWindowResult { windowGeneration, openedAt, reverseUntil, ... }

ConsumeDocumentReverseWindowRequest {
    tombstoneOperationId
    restoreOperationId
    windowGeneration
    contentStateVector
}
```

The local durable workflow needs immutable `expected_window_generation`, the
positive `accepted_window_generation`, and a distinct durable
`restore_operation_id`. A request containing generation 0 is valid only for the
first tombstone against an absent backend row; restore must reject 0.

## Required RED Tests

| ID | Required behavior | Causal mutation that must fail |
| --- | --- | --- |
| G1 | A1 accepted, B1 supersedes, delayed A1 remains rejected forever | Remove new-operation generation CAS |
| G2 | Exact current A1 retry returns the first result and deadline | Apply CAS/new-window path before exact-current replay |
| G3 | A CAS conflict can continue only with a new operation UUID | Rewrite expected generation in place on the existing op |
| G4 | Two transactions racing from expected `g` produce one `g+1` acceptance | Remove the locked conditional generation update |
| G5 | Restore commits only for its accepted positive generation | Remove restore-generation comparison |
| G6 | Exact consumed restore replay succeeds after deadline only for the full origin + tombstone op + restore op + generation identity | Check time first, omit restore op from replay identity, or reuse the tombstone op as the restore op |
| G7 | Wrong origin, missing frontier, and `serverNow >= deadline` each reject | Remove each corresponding guard independently |
| G8 | New tombstone is the only transition that increments generation | Increment on restore, expiry, read, or projection |
| G9 | Crash before response retries the same immutable op/expected generation | Persist operation or expected generation after network send |
| G10 | Crash after acceptance retains accepted generation for restore | Regenerate or infer the restore generation after restart |

The backend race rows must use two independent PostgreSQL connections. The
daemon crash rows must close and reopen real SQLite, not only reconstruct an
in-memory struct.

## Exhaustive Results

TLC2 `2026.07.31.184830` (revision `30cc360`, v1.8.0 release asset) ran
breadth-first with two replicas (`A`, `B`), two durable tombstone operations
(`A1`, `B1`), two distinct durable restore operations (`RA1`, `RB1`), at most
two copies of each request, a five-minute window abstracted to two ticks, and
the invariants `TypeOK`, `OperationAtMostOnce`, `RestoreSafety`, and
`ProjectionSafety`.

| Configuration | Additional bound | Generated | Distinct | Depth | Result |
| --- | --- | ---: | ---: | ---: | --- |
| core CAS | max generation 3 | 240,839 | 39,670 | 33 | GREEN, complete graph |
| legacy no-CAS | max generation 3 | 1,255 | 433 | 11 | RED, invariant counterexample |
| deadline | max generation 2, time 0..2 | 3,073,863 | 469,812 | 35 | GREEN, complete graph |
| namespace | max generation 2, matching/changed/conflicting | 874,875 | 119,010 | 34 | GREEN, complete graph |
| non-content | max generation 2, content/absent/non-content | 718,177 | 90,540 | 33 | GREEN, complete graph |
| external tombstone | max generation 2 | 404,165 | 62,918 | 34 | GREEN, complete graph |
| crash/restart | max generation 2, one restart per replica | 1,122,033 | 158,680 | 35 | GREEN, complete graph |

The legacy row is a shortest failing prefix, not a complete graph: TLC stops at
the invariant violation with 176 states still queued.

An earlier all-boundaries-at-once run was intentionally stopped after it had
already reached 15,674,985 generated / 2,059,138 distinct states with a growing
453,460-state queue. Those partial numbers are not evidence and are not included
in the table. The orthogonal complete configurations above avoid that accidental
cross-product while retaining each boundary.

The Go BFS checker separately explored 17,819 distinct states and 83,522
transitions through depth 10 with these exact bounds: max time 2, max generation
2, max messages 2, one send attempt, and no restart. Focused traces outside that
BFS exercise the complete delayed A1/B1 ABA prefix, response loss, deadline,
frontier, origin, restore generation, exact consumed replay versus a different
restore ID, stale projection, and crash durability.
The Go mutation tests are RED for no CAS, wrong restore generation, missing
frontier, wrong origin, expired restore, stale projection, and dropped durable
workflow.

## Reproduction

```bash
go test -v ./test/model/reversetombstone

cd docs/formal/reversible-root-tombstone
java -cp /path/to/tla2tools.jar tlc2.TLC -cleanup -workers auto \
  -config ReversibleRootTombstone.cfg ReversibleRootTombstone.tla
```

Replace the config with each `ReversibleRootTombstone_*.cfg` file. The legacy
configuration is expected to exit nonzero with `Invariant Safety is violated`;
all other configurations must exhaust the queue with no error.

## Limits

These are finite safety checks, not an unbounded proof or a liveness proof. The
model covers one content document and abstracts CRDT bytes, SQL locking, and
filesystem identity into their semantic outcomes. Production conformance must
therefore retain the real two-connection PostgreSQL, SQLite reopen, CRDT helper,
watcher/scan, and end-to-end tests in the design gate.
