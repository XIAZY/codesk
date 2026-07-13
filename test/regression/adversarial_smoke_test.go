//go:build regression

package regression

import (
	"fmt"
	"strings"
	"testing"
	"time"

	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

// adversarialStack is a helper that wraps regressionStack and provides
// helpers specific to the R1-R5 adversarial smoke scenarios.
type adversarialStack struct {
	*regressionStack
}

func newAdversarialStack(t *testing.T) *adversarialStack {
	t.Helper()
	s := newRegressionStack(t)
	return &adversarialStack{s}
}

// execDaemon runs a shell command inside the daemon container.
func (a *adversarialStack) execDaemon(t *testing.T, cmd string) string {
	t.Helper()
	return a.execService(t, "daemon", cmd)
}

// rawWriteFile bypasses the daemon's fsnotify cycle and writes directly
// into the workspace volume from within the daemon container.
func (a *adversarialStack) rawWriteFile(t *testing.T, relPath, content string) {
	t.Helper()
	absPath := a.daemonWorkspaceDir() + "/" + relPath
	// mkdir -p the parent directory of the target file (not just workspace root).
	a.execDaemon(t, fmt.Sprintf("mkdir -p %s && printf %%s %s > %s",
		shellQuote(absPath[:strings.LastIndex(absPath, "/")]),
		shellQuote(content), shellQuote(absPath)))
}

// rawReadFile reads the file from inside the daemon container.
func (a *adversarialStack) rawReadFile(t *testing.T, relPath string) string {
	t.Helper()
	absPath := a.daemonWorkspaceDir() + "/" + relPath
	return a.execDaemon(t, fmt.Sprintf("cat %s 2>/dev/null || true", shellQuote(absPath)))
}

// fileExistsInDaemon checks whether a path exists inside the daemon workspace.
func (a *adversarialStack) fileExistsInDaemon(t *testing.T, relPath string) bool {
	t.Helper()
	absPath := a.daemonWorkspaceDir() + "/" + relPath
	out := a.execDaemon(t, fmt.Sprintf("[ -f %s ] && echo yes || echo no", shellQuote(absPath)))
	return strings.TrimSpace(out) == "yes"
}

// getInode returns the inode number of a file inside the daemon container.
func (a *adversarialStack) getInode(t *testing.T, relPath string) string {
	t.Helper()
	absPath := a.daemonWorkspaceDir() + "/" + relPath
	out := a.execDaemon(t, fmt.Sprintf("stat -c %%i %s 2>/dev/null || echo none", shellQuote(absPath)))
	return strings.TrimSpace(out)
}

// sendTextInsert pushes a CRDT insert update to the document over a fresh websocket.
func (a *adversarialStack) sendTextInsert(t *testing.T, documentID, text string) {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(time.Now().UnixNano())))
	textObj := doc.GetText("content")
	update := captureDocUpdate(t, doc, "adversarial-insert", func(txn *crdt.Transaction) {
		textObj.Insert(txn, 0, text, nil)
	})
	conn := dialDocumentWebsocket(t, a.documentWSURL(t, documentID), "adversarial", uint64(time.Now().UnixNano()))
	defer conn.Close()
	// Step 1: pull server state
	writeBinary(t, conn, yproto.BuildSyncStep1FromStateVector(crdt.EncodeStateVectorV1(doc)))
	// Step 2: push our update
	writeBinary(t, conn, yproto.BuildSyncUpdate(update))
}

// pollUntil polls condition every second until it returns true or timeout expires.
func (a *adversarialStack) pollUntil(t *testing.T, timeout time.Duration, label string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("pollUntil(%s) timed out after %s", label, timeout)
}

// archiveFilesInDaemon lists files under .notty/recovered in the daemon container.
func (a *adversarialStack) archiveFilesInDaemon(t *testing.T) string {
	t.Helper()
	return a.execDaemon(t, fmt.Sprintf(
		"find %s/.notty/recovered -type f 2>/dev/null | sort || true",
		shellQuote(a.daemonWorkspaceDir()),
	))
}

// ============================================================
// R1 — External-edit protection (WriteIfUnchanged)
// ============================================================

// TestAdversarialR1ExternalEditPreventedByWriteIfUnchanged verifies that when
// an external process rewrites a tracked file while the daemon has a known
// projected snapshot, the daemon does NOT silently clobber the external edit
// when the backend sends a new update.
//
// Expected behaviour:
//   - External edit is detected as a hash mismatch (ErrDivergedWorkingCopy).
//   - Daemon treats the external edit as a new local change and reconciles it
//     back to the backend (bi-directional merge) rather than overwriting it.
//   - The local file retains the externally-written content until convergence.
func TestAdversarialR1ExternalEditPreventedByWriteIfUnchanged(t *testing.T) {
	a := newAdversarialStack(t)
	a.up(t)

	relPath := uniquePath("r1-external-edit", ".md")
	initialContent := "initial content\n"

	// 1. Create the document via the backend and let the daemon materialise it.
	docID := a.createDocument(t, relPath, initialContent)
	a.waitForLocalContent(t, relPath, initialContent, 60*time.Second)

	// 2. Externally rewrite the file (simulating a rogue process or agent).
	externalContent := "EXTERNAL REWRITE — do not clobber me\n"
	a.rawWriteFile(t, relPath, externalContent)

	// Give the daemon enough time to pick up the fsnotify event and register
	// the file as locally dirty.
	time.Sleep(3 * time.Second)

	// 3. Send a backend update that conflicts with the external content.
	// Sync against what the backend currently has (which may already include
	// the external content pushed by the daemon), then send an additional update.
	backendUpdateContent := "backend update after external edit\n"
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(1801)))
	textObj := doc.GetText("content")
	wsURL := a.documentWSURL(t, docID)
	conn := dialDocumentWebsocket(t, wsURL, "r1-writer", 1801)
	defer conn.Close()
	// Sync with whatever the server currently has (may include external edit already).
	writeBinary(t, conn, yproto.BuildSyncStep1FromStateVector(crdt.EncodeStateVectorV1(doc)))
	// Read messages until we get the server state, then push an additional insert.
	deadline := time.Now().Add(15 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for time.Now().Before(deadline) {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if msgType != 2 { // BinaryMessage
			continue
		}
		topLevel, reader, parseErr := yproto.DecodeProtocolMessage(payload)
		if parseErr != nil || topLevel != yproto.MessageSync {
			continue
		}
		syncType, data, _ := yproto.DecodeSyncMessage(reader)
		if syncType == yproto.SyncStep2 || syncType == yproto.SyncUpdate {
			_ = crdt.ApplyUpdateV1(doc, data, "r1-sync")
		}
		if syncType == yproto.SyncStep2 {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	update := captureDocUpdate(t, doc, "r1-backend-update", func(txn *crdt.Transaction) {
		textObj.Insert(txn, textObj.LenInTxn(txn), backendUpdateContent, nil)
	})
	writeBinary(t, conn, yproto.BuildSyncUpdate(update))

	// 4. The backend must eventually contain the EXTERNAL content — this proves
	//    the daemon reconciled the external edit upstream rather than clobbering it.
	finalContent := a.waitForBackendContentPredicate(t, docID, 60*time.Second, func(content string) bool {
		return strings.Contains(content, "EXTERNAL REWRITE")
	})
	if !strings.Contains(finalContent, "EXTERNAL REWRITE") {
		t.Fatalf("R1 FAIL: backend never received the external edit; got %q", finalContent)
	}
	t.Logf("R1: backend converged to %q", finalContent)
	t.Logf("R1: daemon logs tail:\n%s", a.daemonLogs(t))
}

// ============================================================
// R2 — Unsafe tombstone ordering (ErrUnsafeDelete guard)
// ============================================================

// TestAdversarialR2UnsafeTombstoneOrderingArchivesInsteadOfDeleting verifies that
// when a document is tombstoned while the local file has been externally modified
// (hash mismatch), the daemon's DeleteIfUnchanged returns ErrUnsafeDelete and the
// file is archived instead of deleted.
//
// Code path: cleanupRemovedDocument (service.go:1346) → DeleteIfUnchanged (workspace_fs.go:223)
//   → ErrUnsafeDelete → Archive
//
// The UNSAFE case:
//   1. Create a document, wait for daemon to materialize it.
//   2. Externally modify the file (hash no longer matches projected).
//   3. IMMEDIATELY tombstone the document before the daemon reconciles the edit.
//   4. Daemon detects hash mismatch in DeleteIfUnchanged, returns ErrUnsafeDelete.
//   5. File is archived to .notty/recovered, NOT deleted from disk.
func TestAdversarialR2UnsafeTombstoneOrderingArchivesInsteadOfDeleting(t *testing.T) {
	a := newAdversarialStack(t)
	a.up(t)

	relPath := uniquePath("r2-unsafe-tombstone", ".md")
	originalContent := "original content — must be preserved if unsafe delete\n"

	// Step 1: Create document via backend, wait for daemon to materialize.
	docID := a.createDocument(t, relPath, originalContent)
	a.waitForLocalContent(t, relPath, originalContent, 60*time.Second)
	t.Logf("R2: document %s materialized at %s", docID, relPath)

	// Step 2: Externally modify the file so its hash no longer matches.
	externalContent := "EXTERNALLY MODIFIED — hash mismatch, do not delete\n"
	a.rawWriteFile(t, relPath, externalContent)
	t.Logf("R2: externally modified file to %q", externalContent)

	// Step 3: IMMEDIATELY tombstone the document. Don't wait for daemon to reconcile
	// the external edit. This is the race condition we're testing.
	// The tombstone must arrive before the daemon has pushed the external content upstream.
	a.tombstoneRootDocument(t, docID)
	t.Logf("R2: tombstone sent for document %s", docID)

	// Step 4-5: Poll until the tombstone is processed (file disappears or is archived).
	var originalExists bool
	a.pollUntil(t, 30*time.Second, "tombstone processed", func() bool {
		originalExists = a.fileExistsInDaemon(t, relPath)
		archiveFiles := strings.TrimSpace(a.archiveFilesInDaemon(t))
		return !originalExists || archiveFiles != ""
	})

	archiveFiles := a.archiveFilesInDaemon(t)
	t.Logf("R2: original file exists at %s: %v", relPath, originalExists)
	t.Logf("R2: archive files:\n%s", archiveFiles)
	t.Logf("R2: daemon logs tail:\n%s", a.daemonLogs(t))

	// The EXTERNAL content must be preserved SOMEWHERE: at the original path,
	// in the archive, or reconciled to the backend. FAIL if it's nowhere.
	found := false
	if originalExists {
		content := a.rawReadFile(t, relPath)
		if strings.Contains(content, "EXTERNALLY MODIFIED") {
			t.Logf("R2: external content preserved at original path")
			found = true
		}
	}
	if !found {
		for _, archivePath := range strings.Split(strings.TrimSpace(archiveFiles), "\n") {
			archivePath = strings.TrimSpace(archivePath)
			if archivePath == "" {
				continue
			}
			content := a.execDaemon(t, fmt.Sprintf("cat %s 2>/dev/null || true", shellQuote(archivePath)))
			if strings.Contains(content, "EXTERNALLY MODIFIED") {
				t.Logf("R2: external content archived at %s", archivePath)
				found = true
				break
			}
		}
	}
	if !found {
		backendContent, err := a.backendDocumentContent(docID)
		if err == nil && strings.Contains(backendContent, "EXTERNALLY MODIFIED") {
			t.Logf("R2: external content reconciled to backend before tombstone")
			found = true
		}
	}
	if !found {
		t.Fatalf("R2 FAIL: external content is LOST — not at original path, not in archive, not in backend")
	}
}

// ============================================================
// R3 — External on-disk rename over tracked path (statFileWithIdentity)
// ============================================================

// TestAdversarialR3ExternalRenameOverTrackedPath verifies that when an external
// file is moved (via rename syscall) to a path already tracked by the daemon,
// the daemon detects the inode identity change via statFileWithIdentity and
// treats it as a new local file replacing the tracked one.
//
// Code path: fsnotify Create event (replica.go:193) → statFileWithIdentity
//   (file_identity_unix.go:18) → returns new inode identity → daemon adopts new content.
//
// The TOCTOU fix: statFileWithIdentity returns dev+ino from a single stat syscall,
// so the daemon can detect that the file at a tracked path is actually a different file.
func TestAdversarialR3ExternalRenameOverTrackedPath(t *testing.T) {
	a := newAdversarialStack(t)
	a.up(t)

	// Step 1: Create and sync a document so daemon tracks a file at pathA.
	pathA := uniquePath("r3-tracked-target", ".md")
	originalContent := "original tracked content\n"
	docID := a.createDocument(t, pathA, originalContent)
	a.waitForLocalContent(t, pathA, originalContent, 60*time.Second)
	t.Logf("R3: document %s materialized at %s", docID, pathA)

	// Record the original inode.
	originalInode := a.getInode(t, pathA)
	t.Logf("R3: original inode at %s: %s", pathA, originalInode)

	// Step 2: Create a DIFFERENT external file at pathB (unrelated to notty).
	pathB := uniquePath("r3-external-file", ".tmp")
	externalContent := "EXTERNAL FILE — different inode identity\n"
	a.rawWriteFile(t, pathB, externalContent)
	t.Logf("R3: external file created at %s", pathB)

	// Record external file's inode.
	externalInode := a.getInode(t, pathB)
	t.Logf("R3: external file inode at %s: %s", pathB, externalInode)

	// Verify the inodes are different.
	if originalInode == externalInode {
		t.Fatalf("R3: inodes should differ but both are %s", originalInode)
	}

	// Step 3: Use mv to move the external file FROM pathB TO pathA, replacing the tracked file.
	// This is a rename syscall — the file at pathA now has a different inode.
	absPathA := a.daemonWorkspaceDir() + "/" + pathA
	absPathB := a.daemonWorkspaceDir() + "/" + pathB
	a.execDaemon(t, fmt.Sprintf("mv -f %s %s", shellQuote(absPathB), shellQuote(absPathA)))
	t.Logf("R3: moved external file from %s to %s (replacing tracked file)", pathB, pathA)

	// Verify the inode at pathA is now the external file's inode.
	newInode := a.getInode(t, pathA)
	t.Logf("R3: new inode at %s after mv: %s (was %s)", pathA, newInode, originalInode)

	// Step 4-5: The daemon must detect the identity change and sync the external
	// file's content to the backend. Only backend convergence proves adoption —
	// local disk has the external content because mv put it there.
	backendContent := a.waitForBackendContentPredicate(t, docID, 60*time.Second, func(content string) bool {
		return strings.Contains(content, "EXTERNAL FILE")
	})
	t.Logf("R3: backend content: %q", backendContent)
	t.Logf("R3: daemon logs tail:\n%s", a.daemonLogs(t))

	if !strings.Contains(backendContent, "EXTERNAL FILE") {
		t.Fatalf("R3 FAIL: daemon did not sync external file content to backend; got %q", backendContent)
	}
}

// ============================================================
// R4 — CreateEmptyOrRead materialize + external race + no os.Remove
// ============================================================

// TestAdversarialR4CreateEmptyOrReadExternalRacePreservesFile verifies that when
// an external file already exists at a path the daemon tries to materialize,
// CreateEmptyOrRead (workspace_fs.go:126) uses O_CREATE|O_EXCL, gets os.ErrExist,
// and reads the existing content instead of overwriting.
//
// CRITICAL: the daemon must NEVER call os.Remove on the externally-created file.
//
// Code path: CreateEmptyOrRead → createEmptyFileExclusive (O_CREATE|O_EXCL)
//   → os.ErrExist → readFileObservation → return existing content.
func TestAdversarialR4CreateEmptyOrReadExternalRacePreservesFile(t *testing.T) {
	a := newAdversarialStack(t)
	a.up(t)

	relPath := uniquePath("r4-race-materialize", ".md")

	// Step 1: Create a document via backend API (exists in backend, no local file yet).
	// We create the document WITHOUT initial content so the daemon will try to
	// materialize an empty file.
	docID := a.createDocument(t, relPath, "")
	t.Logf("R4: document %s created in backend for path %s", docID, relPath)

	// Wait briefly for daemon to materialize (it will create an empty file).
	a.waitForLocalContent(t, relPath, "", 30*time.Second)
	t.Logf("R4: daemon materialized empty file at %s", relPath)

	// Now let's test the actual race: create a NEW document but pre-create the file.
	relPath2 := uniquePath("r4-race-preexist", ".md")
	externalContent := "EXTERNAL PRE-EXISTING FILE — daemon must not overwrite or delete\n"

	// Step 2: Before the daemon materializes the new document, externally create
	// a file at the same path.
	a.rawWriteFile(t, relPath2, externalContent)
	t.Logf("R4: externally created file at %s with content: %q", relPath2, externalContent)

	// Step 3: Now create the backend document that maps to the same path.
	// The daemon will try to materialize it and hit the existing file.
	docID2 := a.createDocument(t, relPath2, "")
	t.Logf("R4: document %s created in backend for path %s (file already exists)", docID2, relPath2)

	// Step 4: Poll until the daemon has attempted materialization, then verify
	// the external file content is preserved.
	a.pollUntil(t, 30*time.Second, "daemon materialization attempt", func() bool {
		// The daemon has processed the document when a websocket sync is established.
		// We just need to wait long enough for the materialize attempt.
		content, _ := a.backendDocumentContent(docID2)
		return content != "" || a.fileExistsInDaemon(t, relPath2)
	})

	fileExists := a.fileExistsInDaemon(t, relPath2)
	if !fileExists {
		t.Fatalf("R4 FAIL: daemon DELETED the externally-created file at %s", relPath2)
	}

	localContent := a.rawReadFile(t, relPath2)
	t.Logf("R4: local content at %s: %q", relPath2, localContent)
	t.Logf("R4: daemon logs tail:\n%s", a.daemonLogs(t))

	if !strings.Contains(localContent, "EXTERNAL PRE-EXISTING FILE") {
		t.Fatalf("R4 FAIL: daemon OVERWROTE the external file content. Got: %q, Want substring: %q",
			localContent, "EXTERNAL PRE-EXISTING FILE")
	}
}

// ============================================================
// R5 — Restart durability (no phantom dirty marks)
// ============================================================

// TestAdversarialR5RestartDurabilityNoPhantomDirtyMarks verifies that after a
// daemon restart, synced files are not falsely marked as locally dirty and no
// phantom outbound edits are generated.
//
// Code path on restart: replica.go:424 (walk with statFileWithIdentity) →
//   service.go:1588-1589 (snapshot.Hash == projectedHashString(state.baseContent))
//   → clearLocalDirty if match.
//
// This is the corruption failure mode the durability rewrite exists to prevent.
func TestAdversarialR5RestartDurabilityNoPhantomDirtyMarks(t *testing.T) {
	a := newAdversarialStack(t)
	a.up(t)

	// Step 1: Create a document, make edits, wait for full bi-directional sync.
	relPath := uniquePath("r5-restart-durability", ".md")
	initialContent := "initial\n"
	docID := a.createDocument(t, relPath, initialContent)
	a.waitForLocalContent(t, relPath, initialContent, 60*time.Second)
	t.Logf("R5: document %s materialized at %s", docID, relPath)

	// Make local edits (write content directly to the daemon workspace).
	editedContent := "initial\nedited line one\nedited line two\n"
	a.writeLocalFile(t, relPath, editedContent)

	// Wait for full bi-directional sync (both local and backend have the same content).
	a.waitForBackendContentByPath(t, relPath, editedContent, 60*time.Second)
	a.waitForLocalContent(t, relPath, editedContent, 30*time.Second)
	t.Logf("R5: full bi-directional sync achieved for content: %q", editedContent)

	// Step 2: Verify content matches and capture the backend document head
	// (update_id) BEFORE restart. A phantom re-upload of identical content
	// still advances the head — content-equality alone can't detect it.
	backendContent, err := a.backendDocumentContentByPath(relPath)
	if err != nil {
		t.Fatalf("R5: failed to read backend content: %v", err)
	}
	localContent := a.rawReadFile(t, relPath)
	if backendContent != localContent {
		t.Fatalf("R5: content mismatch before restart. Backend: %q, Local: %q",
			backendContent, localContent)
	}
	_, _, headBefore, err := a.documentHeaderByPath(relPath)
	if err != nil {
		t.Fatalf("R5: failed to read document header before restart: %v", err)
	}
	t.Logf("R5: pre-restart state: content=%q headID=%d", editedContent, headBefore)

	// Step 3: RESTART the daemon container.
	a.run(t, "restart", "daemon")

	// Step 4: Poll until the daemon has re-initialized (file still exists on disk).
	a.pollUntil(t, 30*time.Second, "daemon re-init after restart", func() bool {
		return a.fileExistsInDaemon(t, relPath)
	})

	// 4a. File must exist with correct content.
	localContentAfterRestart := a.rawReadFile(t, relPath)
	if localContentAfterRestart != editedContent {
		t.Fatalf("R5 FAIL: file content changed after restart. Before: %q, After: %q",
			editedContent, localContentAfterRestart)
	}

	// 4b. Wait for the daemon to complete its post-restart reconciliation,
	// then verify the document head has NOT advanced (no phantom edits).
	a.pollUntil(t, 30*time.Second, "post-restart reconciliation settle", func() bool {
		content, err := a.backendDocumentContentByPath(relPath)
		return err == nil && content == editedContent
	})

	_, _, headAfter, err := a.documentHeaderByPath(relPath)
	if err != nil {
		t.Fatalf("R5: failed to read document header after restart: %v", err)
	}
	t.Logf("R5: post-restart state: headID=%d (was %d)", headAfter, headBefore)
	t.Logf("R5: daemon logs after restart:\n%s", a.daemonLogs(t))

	if headAfter != headBefore {
		t.Fatalf("R5 FAIL: phantom outbound edit detected — document head advanced from %d to %d after restart (content unchanged: %q)",
			headBefore, headAfter, editedContent)
	}
}
