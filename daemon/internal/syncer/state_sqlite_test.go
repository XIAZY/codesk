package syncer

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceStateDBCreatesFullSchemaAndResetsStartupRows(t *testing.T) {
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	db := state.DB()
	defer state.Close()

	for _, table := range []string{
		"streams",
		"stream_states",
		"stream_inbox",
		"stream_outbox",
		"manifest_projection",
		"content_projection",
		"scan_hints",
		"directory_scan_cache",
		"scan_state",
		"pending_content_creates",
		"fs_jobs",
	} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("expected table %s: %v", table, err)
		}
	}

	var scanStateRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scan_state WHERE id = 1 AND capabilities_initialized = 0`).Scan(&scanStateRows); err != nil {
		t.Fatalf("query scan_state: %v", err)
	}
	if scanStateRows != 1 {
		t.Fatalf("expected singleton uninitialized scan_state, got %d rows", scanStateRows)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO pending_content_creates(entry_id, content_stream_id, materialized_path, root_mutation_key, status, created_at, updated_at) VALUES ('doc_a', 'doc_a', 'a.md', 'root:create:doc_a', 'reading', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert pending create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO fs_jobs(job_key, kind, status, created_at, updated_at) VALUES ('job_a', 'write-content', 'running', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert fs job: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close state db: %v", err)
	}

	state, err = OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("reopen state db: %v", err)
	}
	defer state.Close()
	db = state.DB()
	var pendingStatus, jobStatus string
	if err := db.QueryRow(`SELECT status FROM pending_content_creates WHERE entry_id = 'doc_a'`).Scan(&pendingStatus); err != nil {
		t.Fatalf("query pending status: %v", err)
	}
	if pendingStatus != "needs_bytes" {
		t.Fatalf("expected stale reading row reset to needs_bytes, got %q", pendingStatus)
	}
	if err := db.QueryRow(`SELECT status FROM fs_jobs WHERE job_key = 'job_a'`).Scan(&jobStatus); err != nil {
		t.Fatalf("query job status: %v", err)
	}
	if jobStatus != "pending" {
		t.Fatalf("expected stale running job reset to pending, got %q", jobStatus)
	}
}

func TestManifestProjectionTombstonedRowsDoNotReserveMaterializedPath(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:          "old",
		Kind:             "dir",
		DesiredPath:      "docs",
		MaterializedPath: "docs",
		Tombstoned:       true,
	}); err != nil {
		t.Fatalf("insert tombstoned projection: %v", err)
	}
	if err := state.UpsertManifestProjection(ctx, ManifestProjectionRow{
		EntryID:          "new",
		Kind:             "dir",
		DesiredPath:      "docs",
		MaterializedPath: "docs",
		Tombstoned:       false,
	}); err != nil {
		t.Fatalf("live projection should reuse tombstoned path: %v", err)
	}
}

func TestInitializeScanCapabilitiesRunsOnceAndInsertsFullHint(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	prober := &fakeScanCapabilityProber{
		fileKeyReliable:        false,
		directoryMTimeReliable: true,
		ctimeReliable:          false,
	}
	scanState, err := state.InitializeScanCapabilities(ctx, prober)
	if err != nil {
		t.Fatalf("initialize capabilities: %v", err)
	}
	if prober.calls != 1 {
		t.Fatalf("expected one probe run, got %d", prober.calls)
	}
	if !scanState.CapabilitiesInitialized {
		t.Fatal("expected capabilities initialized")
	}
	if !scanState.DirectoryMTimeReliable || scanState.FileKeyReliable || scanState.CTimeReliable {
		t.Fatalf("unexpected stored capabilities: %#v", scanState.Capabilities())
	}
	if !scanState.LastCapabilityProbeAt.Valid {
		t.Fatal("expected last capability probe timestamp")
	}
	var fullHints int
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM scan_hints WHERE kind = 'full' AND reason = 'capability-probe'`).Scan(&fullHints); err != nil {
		t.Fatalf("query full hints: %v", err)
	}
	if fullHints != 1 {
		t.Fatalf("expected one full scan hint after first probe, got %d", fullHints)
	}

	prober.fileKeyReliable = true
	prober.directoryMTimeReliable = false
	prober.ctimeReliable = true
	scanState, err = state.InitializeScanCapabilities(ctx, prober)
	if err != nil {
		t.Fatalf("initialize capabilities second time: %v", err)
	}
	if prober.calls != 1 {
		t.Fatalf("expected second call to reuse stored capabilities, probe calls=%d", prober.calls)
	}
	if !scanState.DirectoryMTimeReliable || scanState.FileKeyReliable || scanState.CTimeReliable {
		t.Fatalf("second initialization should not overwrite capabilities: %#v", scanState.Capabilities())
	}
	if err := state.DB().QueryRow(`SELECT COUNT(*) FROM scan_hints WHERE kind = 'full' AND reason = 'capability-probe'`).Scan(&fullHints); err != nil {
		t.Fatalf("query full hints second time: %v", err)
	}
	if fullHints != 1 {
		t.Fatalf("expected no duplicate full hint, got %d", fullHints)
	}
}

func TestWorkspaceStateDBEnforcesProjectionConstraints(t *testing.T) {
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()
	db := state.DB()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	if _, err := db.Exec(`INSERT INTO content_projection(stream_id, entry_id, materialized_path, updated_at) VALUES ('s1', 'e1', 'same.md', ?)`, now); err != nil {
		t.Fatalf("insert content projection: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO content_projection(stream_id, entry_id, materialized_path, updated_at) VALUES ('s2', 'e2', 'same.md', ?)`, now); err == nil {
		t.Fatal("expected unique materialized path constraint for content_projection")
	}

	_, err = db.Exec(`INSERT INTO pending_content_creates(entry_id, content_stream_id, materialized_path, observed_stat_valid, root_mutation_key, status, created_at, updated_at) VALUES ('bad', 'bad', 'bad.md', 1, 'root:create:bad', 'needs_bytes', ?, ?)`, now, now)
	if err == nil {
		t.Fatal("expected observed stat check constraint to reject incomplete stat tuple")
	}
}

type fakeScanCapabilityProber struct {
	fileKeyReliable        bool
	directoryMTimeReliable bool
	ctimeReliable          bool
	calls                  int
}

func (p *fakeScanCapabilityProber) TestFileKeyReliability(context.Context) bool {
	p.calls++
	return p.fileKeyReliable
}

func (p *fakeScanCapabilityProber) TestDirectoryMTimeReliability(context.Context) bool {
	return p.directoryMTimeReliable
}

func (p *fakeScanCapabilityProber) TestCTimeReliability(context.Context) bool {
	return p.ctimeReliable
}

func TestScanHintsOverflowCollapsesToSingleFullHint(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	tx, err := state.DB().Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO scan_hints(kind, path, reason, created_at) VALUES ('path', ?, 'seed', ?)`)
	if err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	for i := 0; i < MaxPendingScanHints; i++ {
		if _, err := stmt.Exec("file.md", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed hint %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	if err := state.InsertScanHint(ctx, ScanHintPath, "overflow.md", "overflow-test"); err != nil {
		t.Fatalf("insert overflow hint: %v", err)
	}
	var count int
	var kind, reason string
	if err := state.DB().QueryRow(`SELECT COUNT(*), kind, reason FROM scan_hints`).Scan(&count, &kind, &reason); err != nil {
		t.Fatalf("query overflow hint: %v", err)
	}
	if count != 1 || kind != string(ScanHintFull) || reason != "hint-overflow" {
		t.Fatalf("expected one full overflow hint, count=%d kind=%q reason=%q", count, kind, reason)
	}
}

func TestDirectoryScanCacheCaps(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	if err := state.StoreDirectoryScanCache(ctx, "docs", 1, 2, []string{"a.md", "b.md"}); err != nil {
		t.Fatalf("store cache: %v", err)
	}
	var entryCount int
	if err := state.DB().QueryRow(`SELECT entry_count FROM directory_scan_cache WHERE path = 'docs'`).Scan(&entryCount); err != nil {
		t.Fatalf("query cached dir: %v", err)
	}
	if entryCount != 2 {
		t.Fatalf("expected cached entry count 2, got %d", entryCount)
	}

	tooMany := make([]string, MaxCachedDirChildren+1)
	for i := range tooMany {
		tooMany[i] = "child.md"
	}
	if err := state.StoreDirectoryScanCache(ctx, "docs", 1, 2, tooMany); err != nil {
		t.Fatalf("store too-large cache: %v", err)
	}
	err = state.DB().QueryRow(`SELECT entry_count FROM directory_scan_cache WHERE path = 'docs'`).Scan(&entryCount)
	if err != sql.ErrNoRows {
		t.Fatalf("expected oversized child count to delete cache row, err=%v count=%d", err, entryCount)
	}

	largeJSON := []string{strings.Repeat("x", MaxCachedDirJSONBytes+1)}
	if err := state.StoreDirectoryScanCache(ctx, "huge", 1, 2, largeJSON); err != nil {
		t.Fatalf("store huge json cache: %v", err)
	}
	err = state.DB().QueryRow(`SELECT entry_count FROM directory_scan_cache WHERE path = 'huge'`).Scan(&entryCount)
	if err != sql.ErrNoRows {
		t.Fatalf("expected oversized json to skip cache row, err=%v count=%d", err, entryCount)
	}
}

func TestMaybeInsertPeriodicFullScanHint(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer state.Close()

	inserted, err := state.MaybeInsertPeriodicFullScanHint(ctx, PeriodicFullScanInterval)
	if err != nil {
		t.Fatalf("maybe insert first full scan: %v", err)
	}
	if !inserted {
		t.Fatal("expected missing last_full_scan_at to schedule a full scan")
	}
	hints, err := state.DrainScanHints(ctx, 10)
	if err != nil {
		t.Fatalf("drain hints: %v", err)
	}
	if len(hints) != 1 || hints[0].Kind != ScanHintFull || hints[0].Reason != "periodic-full-scan" {
		t.Fatalf("expected periodic full hint, got %#v", hints)
	}
	if err := state.MarkFullScanComplete(ctx); err != nil {
		t.Fatalf("mark full scan complete: %v", err)
	}
	inserted, err = state.MaybeInsertPeriodicFullScanHint(ctx, PeriodicFullScanInterval)
	if err != nil {
		t.Fatalf("maybe insert recent full scan: %v", err)
	}
	if inserted {
		t.Fatal("recent full scan should not schedule another full scan")
	}

	old := time.Now().UTC().Add(-PeriodicFullScanInterval - time.Minute).Format(time.RFC3339Nano)
	if _, err := state.DB().Exec(`UPDATE scan_state SET last_full_scan_at = ? WHERE id = 1`, old); err != nil {
		t.Fatalf("age full scan timestamp: %v", err)
	}
	inserted, err = state.MaybeInsertPeriodicFullScanHint(ctx, PeriodicFullScanInterval)
	if err != nil {
		t.Fatalf("maybe insert stale full scan: %v", err)
	}
	if !inserted {
		t.Fatal("stale full scan timestamp should schedule a full scan")
	}
}
