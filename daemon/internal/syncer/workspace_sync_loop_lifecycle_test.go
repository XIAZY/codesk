package syncer

import (
	"context"
	"database/sql"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"notty/internal/rootmanifest"
	crdt "notty/internal/ycrdt"
)

const lifecycleRootStreamID = "root-stream"

type localLifecycleHarness struct {
	t      *testing.T
	ctx    context.Context
	root   string
	state  *WorkspaceStateDB
	fs     *WorkspaceFS
	loop   *WorkspaceSyncLoop
	ackID  int64
	queued []string
	closed bool
}

func newLocalLifecycleHarness(t *testing.T) *localLifecycleHarness {
	t.Helper()
	withPendingCreateStabilityDelay(t, 0)

	ctx := context.Background()
	root := t.TempDir()
	state, err := OpenWorkspaceStateDB(root)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	fs := NewWorkspaceFS(root)
	h := &localLifecycleHarness{
		t:     t,
		ctx:   ctx,
		root:  root,
		state: state,
		fs:    fs,
	}
	h.loop = &WorkspaceSyncLoop{
		State:        state,
		FS:           fs,
		RootStreamID: lifecycleRootStreamID,
		ActorID:      "daemon",
		ActorType:    "daemon",
		Capabilities: ScanCapabilities{FileKeyReliable: true, CTimeReliable: true, DirectoryMTimeReliable: true},
		NewID:        lifecycleID,
		Queue:        func(streamID string) { h.queued = append(h.queued, streamID) },
	}
	t.Cleanup(h.close)
	return h
}

func (h *localLifecycleHarness) close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.fs != nil {
		if err := h.fs.Close(); err != nil {
			h.t.Fatalf("close workspace fs: %v", err)
		}
	}
	if h.state != nil {
		if err := h.state.Close(); err != nil {
			h.t.Fatalf("close state db: %v", err)
		}
	}
}

func lifecycleID(kind string, relPath string) string {
	switch kind + ":" + relPath {
	case "dir:docs":
		return "dir_docs"
	case "file:docs/a.md":
		return "doc_a"
	case "file:docs/b.md":
		return "doc_b"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ".", "_", "-", "_", " ", "_")
	return strings.Trim(replacer.Replace(kind+"_"+relPath), "_")
}

func (h *localLifecycleHarness) writeFile(rel string, content string) {
	h.t.Helper()
	abs := filepath.Join(h.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		h.t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", rel, err)
	}
}

func (h *localLifecycleHarness) removeFile(rel string) {
	h.t.Helper()
	if err := os.Remove(filepath.Join(h.root, filepath.FromSlash(rel))); err != nil {
		h.t.Fatalf("remove %s: %v", rel, err)
	}
}

func (h *localLifecycleHarness) renameFile(oldRel string, newRel string) {
	h.t.Helper()
	newAbs := filepath.Join(h.root, filepath.FromSlash(newRel))
	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
		h.t.Fatalf("mkdir rename target %s: %v", newRel, err)
	}
	if err := os.Rename(filepath.Join(h.root, filepath.FromSlash(oldRel)), newAbs); err != nil {
		h.t.Fatalf("rename %s -> %s: %v", oldRel, newRel, err)
	}
}

func (h *localLifecycleHarness) reconcile(streamID string) {
	h.t.Helper()
	if err := h.loop.ReconcileOne(h.ctx, streamID); err != nil {
		h.t.Fatalf("reconcile %s: %v", streamID, err)
	}
}

func (h *localLifecycleHarness) ackNextSendable(streamID string) {
	h.t.Helper()
	row, err := h.state.NextSendableOutboxRow(h.ctx)
	if err != nil {
		h.t.Fatalf("next sendable outbox: %v", err)
	}
	if row == nil {
		h.t.Fatalf("expected sendable outbox for %s, got nil", streamID)
	}
	if row.StreamID != streamID {
		h.t.Fatalf("expected sendable outbox for %s, got %s (%s)", streamID, row.StreamID, row.MutationKey)
	}
	h.ackID++
	if err := h.state.MarkOutboxAcked(h.ctx, row.ID, h.ackID, time.Now()); err != nil {
		h.t.Fatalf("ack outbox %d: %v", row.ID, err)
	}
}

func (h *localLifecycleHarness) settleLocalCreate(streamID string) {
	h.t.Helper()
	h.reconcile(lifecycleRootStreamID)
	h.ackNextSendable(lifecycleRootStreamID)
	h.reconcile(streamID)
	h.ackNextSendable(streamID)
	h.reconcile(streamID)
}

func (h *localLifecycleHarness) seedSyncedFile(entryID string, rel string, content string) {
	h.t.Helper()
	h.writeFile(rel, content)

	rootDoc := crdt.New(crdt.WithGUID(lifecycleRootStreamID))
	defer rootDoc.Close()
	intents := []rootmanifest.Intent{}
	parentID := rootmanifest.RootEntryID
	parentPath := path.Dir(rel)
	if parentPath != "." && parentPath != "" {
		parentID = lifecycleID("dir", parentPath)
		intents = append(intents, rootmanifest.Intent{
			Type: "create-dir",
			Entry: rootmanifest.Entry{
				ID:   parentID,
				Kind: rootmanifest.EntryKindDir,
				Loc:  rootmanifest.NewLocation(rootmanifest.RootEntryID, path.Base(parentPath)),
			},
		})
	}
	intents = append(intents, rootmanifest.Intent{
		Type: "create-file",
		Entry: rootmanifest.Entry{
			ID:              entryID,
			Kind:            rootmanifest.EntryKindFile,
			Loc:             rootmanifest.NewLocation(parentID, path.Base(rel)),
			ContentStreamID: entryID,
		},
	})
	if _, err := rootmanifest.ApplyIntents(rootDoc, intents); err != nil {
		h.t.Fatalf("seed root manifest: %v", err)
	}
	if err := h.state.EnsureLocalStream(h.ctx, lifecycleRootStreamID, "root"); err != nil {
		h.t.Fatalf("ensure root stream: %v", err)
	}
	rootStateID, err := h.state.persistLatestStreamDocFixture(h.ctx, lifecycleRootStreamID, rootDoc, "")
	if err != nil {
		h.t.Fatalf("persist root stream: %v", err)
	}

	if parentPath != "." && parentPath != "" {
		stat, err := h.fs.Stat(h.ctx, parentPath)
		if err != nil {
			h.t.Fatalf("stat %s: %v", parentPath, err)
		}
		if err := h.state.UpsertManifestProjection(h.ctx, ManifestProjectionRow{
			EntryID:              parentID,
			Kind:                 rootmanifest.EntryKindDir,
			DesiredPath:          parentPath,
			MaterializedPath:     parentPath,
			Stat:                 stat,
			RootProjectedStateID: rootStateID,
		}); err != nil {
			h.t.Fatalf("seed dir projection: %v", err)
		}
	}

	stat, err := h.fs.Stat(h.ctx, rel)
	if err != nil {
		h.t.Fatalf("stat %s: %v", rel, err)
	}
	hash := contentSHA256([]byte(content))
	if err := h.state.UpsertManifestProjection(h.ctx, ManifestProjectionRow{
		EntryID:              entryID,
		Kind:                 rootmanifest.EntryKindFile,
		ContentStreamID:      entryID,
		DesiredPath:          rel,
		MaterializedPath:     rel,
		Stat:                 stat,
		LastCleanHash:        hash,
		RootProjectedStateID: rootStateID,
	}); err != nil {
		h.t.Fatalf("seed file projection: %v", err)
	}

	contentStateDoc := contentDoc(h.t, entryID, content)
	defer contentStateDoc.Close()
	contentStateID, err := h.state.persistLatestStreamDocFixture(h.ctx, entryID, contentStateDoc, hash)
	if err != nil {
		h.t.Fatalf("persist content stream: %v", err)
	}
	if err := h.state.UpdateProjectedStreamState(h.ctx, entryID, contentStateID); err != nil {
		h.t.Fatalf("project content stream: %v", err)
	}
	if err := h.state.UpsertContentProjection(h.ctx, ContentProjectionRow{
		StreamID:         entryID,
		EntryID:          entryID,
		MaterializedPath: rel,
		ProjectedStateID: sql.NullInt64{Int64: contentStateID, Valid: true},
		ProjectedHash:    hash,
		Stat:             stat,
		Dirty:            false,
	}); err != nil {
		h.t.Fatalf("seed content projection: %v", err)
	}
}

func (h *localLifecycleHarness) manifest() rootmanifest.Manifest {
	h.t.Helper()
	doc, _, err := h.state.LoadLatestStreamDoc(h.ctx, lifecycleRootStreamID, "root")
	if err != nil {
		h.t.Fatalf("load root doc: %v", err)
	}
	defer doc.Close()
	manifest, err := rootmanifest.Read(doc)
	if err != nil {
		h.t.Fatalf("read root manifest: %v", err)
	}
	return manifest
}

func (h *localLifecycleHarness) entryPath(entryID string) string {
	h.t.Helper()
	return rootmanifest.Resolve(h.manifest()).EntryPath[entryID]
}

func (h *localLifecycleHarness) contentText(streamID string) string {
	h.t.Helper()
	doc, _, err := h.state.LoadLatestStreamDoc(h.ctx, streamID, "content")
	if err != nil {
		h.t.Fatalf("load content doc %s: %v", streamID, err)
	}
	defer doc.Close()
	return doc.GetText("content").ToString()
}

func (h *localLifecycleHarness) count(query string, args ...any) int {
	h.t.Helper()
	var count int
	if err := h.state.DB().QueryRow(query, args...).Scan(&count); err != nil {
		h.t.Fatalf("count query %q: %v", query, err)
	}
	return count
}

func (h *localLifecycleHarness) streamStateCount(streamID string) int {
	h.t.Helper()
	return h.count(`SELECT COUNT(*) FROM stream_states WHERE stream_id = ?`, streamID)
}

func (h *localLifecycleHarness) fsJobCount(kind string, status string) int {
	h.t.Helper()
	return h.count(`SELECT COUNT(*) FROM fs_jobs WHERE kind = ? AND status = ?`, kind, status)
}

func (h *localLifecycleHarness) pendingCreateStatus(entryID string) string {
	h.t.Helper()
	var status string
	if err := h.state.DB().QueryRow(`SELECT status FROM pending_content_creates WHERE entry_id = ?`, entryID).Scan(&status); err != nil {
		h.t.Fatalf("read pending create %s: %v", entryID, err)
	}
	return status
}

func TestWorkspaceSyncLoopLocalCreateSettlesAndIdleIsNoop(t *testing.T) {
	h := newLocalLifecycleHarness(t)
	h.writeFile("docs/a.md", "alpha\n")

	h.settleLocalCreate("doc_a")

	if got := h.entryPath("doc_a"); got != "docs/a.md" {
		t.Fatalf("local create should materialize doc_a at docs/a.md, got %q", got)
	}
	if got := h.contentText("doc_a"); got != "alpha\n" {
		t.Fatalf("local create should initialize content stream, got %q", got)
	}
	if got := h.pendingCreateStatus("doc_a"); got != "completed" {
		t.Fatalf("pending create should complete after content projection, got %q", got)
	}

	rootStates := h.streamStateCount(lifecycleRootStreamID)
	contentStates := h.streamStateCount("doc_a")
	writeJobs := h.fsJobCount("write-content", "done")
	h.reconcile(lifecycleRootStreamID)
	h.reconcile("doc_a")
	h.reconcile(lifecycleRootStreamID)
	h.reconcile("doc_a")
	if got := h.streamStateCount(lifecycleRootStreamID); got != rootStates {
		t.Fatalf("idle root reconciliation should not create synthetic states: before=%d after=%d", rootStates, got)
	}
	if got := h.streamStateCount("doc_a"); got != contentStates {
		t.Fatalf("idle content reconciliation should not create synthetic states: before=%d after=%d", contentStates, got)
	}
	if got := h.fsJobCount("write-content", "done"); got != writeJobs {
		t.Fatalf("idle reconciliation should not create write-content jobs: before=%d after=%d", writeJobs, got)
	}
}

func TestWorkspaceSyncLoopLocalEditUpdatesContentOnly(t *testing.T) {
	h := newLocalLifecycleHarness(t)
	h.seedSyncedFile("doc_a", "docs/a.md", "alpha\n")

	h.writeFile("docs/a.md", "alpha\nbeta\n")
	h.reconcile("doc_a")

	if got := h.contentText("doc_a"); got != "alpha\nbeta\n" {
		t.Fatalf("local edit should update content stream, got %q", got)
	}
	if got := h.entryPath("doc_a"); got != "docs/a.md" {
		t.Fatalf("local edit should not change root path, got %q", got)
	}
	projection, err := h.state.GetContentProjection(h.ctx, "doc_a")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if projection == nil || projection.Dirty {
		t.Fatalf("content projection should be clean after applying local edit, got %#v", projection)
	}
	if got := h.count(`SELECT COUNT(*) FROM stream_outbox WHERE stream_id = ? AND mutation_key LIKE 'content:edit:%'`, "doc_a"); got != 1 {
		t.Fatalf("expected one content edit outbox row, got %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM stream_outbox WHERE stream_id = ?`, lifecycleRootStreamID); got != 0 {
		t.Fatalf("local edit should not create root outbox rows, got %d", got)
	}
}

func TestWorkspaceSyncLoopLocalMovePreservesContentStreamIdentity(t *testing.T) {
	h := newLocalLifecycleHarness(t)
	h.seedSyncedFile("doc_a", "docs/a.md", "alpha\n")

	h.renameFile("docs/a.md", "docs/b.md")
	h.reconcile(lifecycleRootStreamID)

	manifest := h.manifest()
	entry := manifest.EntriesByID["doc_a"]
	if entry.Tombstone != nil {
		t.Fatalf("local move should not tombstone doc_a: %#v", entry)
	}
	if entry.ContentStreamID != "doc_a" {
		t.Fatalf("local move should preserve content stream identity, got %q", entry.ContentStreamID)
	}
	if got := rootmanifest.Resolve(manifest).EntryPath["doc_a"]; got != "docs/b.md" {
		t.Fatalf("local move should update root location to docs/b.md, got %q", got)
	}
	if got := h.contentText("doc_a"); got != "alpha\n" {
		t.Fatalf("local move should preserve content bytes, got %q", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM stream_outbox WHERE stream_id = ? AND mutation_key LIKE 'content:edit:%'`, "doc_a"); got != 0 {
		t.Fatalf("local move should not create content edit outbox rows, got %d", got)
	}
}

func TestWorkspaceSyncLoopLocalDeleteTombstonesCleanTrackedFile(t *testing.T) {
	h := newLocalLifecycleHarness(t)
	h.seedSyncedFile("doc_a", "docs/a.md", "alpha\n")

	h.removeFile("docs/a.md")
	h.reconcile(lifecycleRootStreamID)

	entry := h.manifest().EntriesByID["doc_a"]
	if entry.Tombstone == nil {
		t.Fatalf("clean local delete should tombstone doc_a, got %#v", entry)
	}
	projection, err := h.state.GetManifestProjection(h.ctx, "doc_a")
	if err != nil {
		t.Fatalf("get manifest projection: %v", err)
	}
	if projection == nil || !projection.Tombstoned {
		t.Fatalf("manifest projection should be tombstoned after local delete, got %#v", projection)
	}
	contentProjection, err := h.state.GetContentProjection(h.ctx, "doc_a")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if contentProjection != nil && contentProjection.Dirty {
		t.Fatalf("clean local delete should not dirty content projection: %#v", contentProjection)
	}
}

func TestWorkspaceSyncLoopLocalCreateThenDeleteIgnoresAckedContentInitHistory(t *testing.T) {
	h := newLocalLifecycleHarness(t)
	h.writeFile("docs/a.md", "alpha\n")
	h.settleLocalCreate("doc_a")

	h.removeFile("docs/a.md")
	h.reconcile(lifecycleRootStreamID)

	entry := h.manifest().EntriesByID["doc_a"]
	if entry.Tombstone == nil {
		t.Fatalf("clean local delete after local-create sync should tombstone doc_a even though content:init is acked, got %#v", entry)
	}
	contentProjection, err := h.state.GetContentProjection(h.ctx, "doc_a")
	if err != nil {
		t.Fatalf("get content projection: %v", err)
	}
	if contentProjection != nil && contentProjection.Dirty {
		t.Fatalf("clean local delete after local-create sync should not dirty content projection: %#v", contentProjection)
	}
	if failed := h.fsJobCount("write-content", "failed"); failed != 0 {
		t.Fatalf("clean local delete after local-create sync should not create failed write-content jobs, got %d", failed)
	}
}
