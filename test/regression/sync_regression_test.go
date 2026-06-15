//go:build regression

package regression

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

type regressionThreadAnchor struct {
	RelativeStart string `json:"relativeStart"`
	RelativeEnd   string `json:"relativeEnd"`
	Kind          string `json:"kind"`
	Excerpt       string `json:"excerpt"`
}

type regressionRootLoc struct {
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
}

type regressionRootEntry struct {
	DocumentID string
	Path       string
	Deleted    bool
}

func TestAppendOnlyFileSyncReconstructsBackend(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	lines := envInt("NOTTY_REGRESSION_STRESS_LINES", 10_000)
	path := uniquePath("stresstest", ".txt")
	stack.writeSequentialFile(t, path, lines, 1*time.Millisecond)
	stack.assertLocalSequence(t, path, lines)
	stack.waitForBackendSequence(t, path, lines, 90*time.Second)
	stack.syncFreshWebsocketClientByPath(t, path, sequentialContent(lines))
}

func TestAppendOnlyFileSyncSurvivesBackendRestart(t *testing.T) {
	if os.Getenv("NOTTY_REGRESSION_BACKEND_RESTART") != "1" {
		t.Skip("set NOTTY_REGRESSION_BACKEND_RESTART=1 to run the restart/lost-write diagnostic")
	}
	stack := newRegressionStack(t)
	stack.up(t)

	lines := envInt("NOTTY_REGRESSION_RESTART_LINES", 5_000)
	path := uniquePath("restart-stresstest", ".txt")
	cmd := stack.daemonWriterCommand(path, lines, 1*time.Millisecond)
	var writerOutput bytes.Buffer
	cmd.Stdout = &writerOutput
	cmd.Stderr = &writerOutput
	if err := cmd.Start(); err != nil {
		t.Fatalf("start writer: %v", err)
	}

	stack.waitForLocalLineCountAtLeast(t, path, lines/3, 45*time.Second)
	stack.run(t, "restart", "backend")
	stack.waitForBackend(t, 90*time.Second)

	if err := cmd.Wait(); err != nil {
		t.Fatalf("writer failed: %v\n%s", err, writerOutput.String())
	}
	stack.assertLocalSequence(t, path, lines)
	stack.waitForBackendSequence(t, path, lines, 120*time.Second)
}

func TestDotPathsIgnoredWhileOrdinaryRootLogFilesSync(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	stack.execService(t, "daemon", "mkdir -p "+shellQuote(stack.daemonWorkspaceDir())+" && cd "+shellQuote(stack.daemonWorkspaceDir())+" && mkdir -p .notty/cache && printf 'internal\\n' > .notty/cache/state.txt && printf 'hidden\\n' > .hidden.txt && printf 'line one\\nline two\\n' > notes.log")

	stack.waitForDocumentPath(t, "notes.log", 30*time.Second)
	paths := stack.documentPaths()
	if contains(paths, ".hidden.txt") {
		t.Fatalf("dotfile was incorrectly synced as a document: %v", paths)
	}
	if contains(paths, ".notty/cache/state.txt") {
		t.Fatalf(".notty cache file was incorrectly synced as a document: %v", paths)
	}
	stack.waitForBackendContentByPath(t, "notes.log", "line one\nline two\n", 30*time.Second)
}

func TestLocalCreateEditDeleteMultipleFiles(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	initial := map[string]string{
		uniquePath("multi-create-a", ".md"): "alpha\n",
		uniquePath("multi-create-b", ".md"): "bravo\n",
		uniquePath("multi-create-c", ".md"): "charlie\n",
	}
	for path, content := range initial {
		stack.writeLocalFile(t, path, content)
	}
	for path, content := range initial {
		stack.waitForBackendContentByPath(t, path, content, 60*time.Second)
	}

	edited := map[string]string{}
	for path, content := range initial {
		edited[path] = strings.TrimSuffix(content, "\n") + " edited\n"
		stack.writeLocalFile(t, path, edited[path])
	}
	for path, content := range edited {
		stack.waitForBackendContentByPath(t, path, content, 60*time.Second)
	}

	deletePaths := make([]string, 0, len(edited))
	for path := range edited {
		deletePaths = append(deletePaths, path)
	}
	sort.Strings(deletePaths)
	if len(deletePaths) > 0 {
		path := deletePaths[0]
		stack.removeLocalFile(t, path)
		stack.waitForDocumentPathGone(t, path, 60*time.Second)
	}
}

func TestLocalFilesystemLifecycleSyncsServerDatabase(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	path := uniquePath("fs-lifecycle", ".md")
	initial := "alpha\nbeta\ngamma\n"
	stack.writeLocalFile(t, path, initial)
	stack.waitForBackendContentByPath(t, path, initial, 90*time.Second)
	documentID, err := stack.documentIDForRootPath(path)
	if err != nil {
		t.Fatalf("lookup created document id: %v", err)
	}
	stack.waitForRootEntry(t, documentID, path, false, 30*time.Second)

	inserted := "HEAD\nalpha\nbe-MID-ta\ngamma\nTAIL\n"
	stack.writeLocalFile(t, path, inserted)
	stack.waitForRootEntry(t, documentID, path, false, 30*time.Second)
	stack.waitForBackendContent(t, documentID, inserted, 90*time.Second)

	deleted := "alpha\nbeta\ngamma\n"
	stack.writeLocalFile(t, path, deleted)
	stack.waitForRootEntry(t, documentID, path, false, 30*time.Second)
	stack.waitForBackendContent(t, documentID, deleted, 90*time.Second)

	moved := strings.TrimSuffix(uniquePath("fs-lifecycle-moved", ".md"), ".md") + "/renamed.md"
	stack.moveLocalFile(t, path, moved)
	stack.waitForRootEntry(t, documentID, moved, false, 90*time.Second)
	stack.waitForDocumentPathGone(t, path, 30*time.Second)
	stack.waitForBackendContent(t, documentID, deleted, 60*time.Second)

	editAfterMove := "INTRO: alpha\ngamma\ndelta\n"
	stack.writeLocalFile(t, moved, editAfterMove)
	stack.waitForRootEntry(t, documentID, moved, false, 30*time.Second)
	stack.waitForBackendContent(t, documentID, editAfterMove, 90*time.Second)

	stack.removeLocalFile(t, moved)
	stack.waitForRootEntry(t, documentID, moved, true, 90*time.Second)
	stack.waitForDocumentPathGone(t, moved, 30*time.Second)
	stack.waitForBackendContent(t, documentID, editAfterMove, 60*time.Second)
}

func TestBackendAPIDocumentLifecycleSyncsDatabaseAndDaemonFilesystem(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	pathA := uniquePath("api-complex-a", ".md")
	pathB := uniquePath("api-complex-b", ".md")
	documentA := stack.createDocument(t, pathA, "")
	documentB := stack.createDocument(t, pathB, "")
	stack.assertRootDocumentSynced(t, documentA, pathA, "", 60*time.Second)
	stack.assertRootDocumentSynced(t, documentB, pathB, "", 60*time.Second)

	docA := crdt.New(crdt.WithClientID(1101))
	docB := crdt.New(crdt.WithClientID(1102))

	stack.applyClientTextUpdate(t, documentA, docA, "a-initial-insert", func(txn *crdt.Transaction, text *crdt.YText) {
		text.Insert(txn, 0, "alpha\nbeta\ngamma\n", nil)
	})
	wantA := docA.GetText("content").ToString()
	stack.assertRootDocumentSynced(t, documentA, pathA, wantA, 60*time.Second)

	stack.applyClientTextUpdate(t, documentA, docA, "a-insert-beginning-middle-end", func(txn *crdt.Transaction, text *crdt.YText) {
		text.Insert(txn, 0, "HEAD\n", nil)
		text.Insert(txn, len("HEAD\nalpha\nbe"), "-MID-", nil)
		text.Insert(txn, text.LenInTxn(txn), "TAIL\n", nil)
	})
	wantA = docA.GetText("content").ToString()
	stack.assertRootDocumentSynced(t, documentA, pathA, wantA, 60*time.Second)

	deleteA := docA.GetText("content").ToString()
	requireContainsForRegression(t, deleteA, "TAIL\n", "-MID-")
	stack.applyClientTextUpdate(t, documentA, docA, "a-delete-beginning-middle-end", func(txn *crdt.Transaction, text *crdt.YText) {
		text.Delete(txn, strings.LastIndex(deleteA, "TAIL\n"), len("TAIL\n"))
		text.Delete(txn, strings.Index(deleteA, "-MID-"), len("-MID-"))
		text.Delete(txn, 0, len("HEAD\n"))
	})
	wantA = docA.GetText("content").ToString()
	stack.assertRootDocumentSynced(t, documentA, pathA, wantA, 60*time.Second)

	stack.applyClientTextUpdate(t, documentB, docB, "b-initial-insert", func(txn *crdt.Transaction, text *crdt.YText) {
		text.Insert(txn, 0, "one two three\n", nil)
	})
	wantB := docB.GetText("content").ToString()
	stack.assertRootDocumentSynced(t, documentB, pathB, wantB, 60*time.Second)

	stack.applyClientTextUpdate(t, documentB, docB, "b-delete-middle-insert-beginning-end", func(txn *crdt.Transaction, text *crdt.YText) {
		text.Delete(txn, len("one "), len("two "))
		text.Insert(txn, 0, "zero ", nil)
		text.Insert(txn, text.LenInTxn(txn), "four\n", nil)
	})
	wantB = docB.GetText("content").ToString()
	stack.assertRootDocumentSynced(t, documentB, pathB, wantB, 60*time.Second)

	movedA := uniquePath("api-complex-a-moved", ".md")
	movedB := strings.TrimSuffix(uniquePath("api-complex-b-moved", ".md"), ".md") + "/notes.md"
	stack.moveRootDocument(t, documentA, movedA)
	stack.assertLocalMissing(t, pathA, 60*time.Second)
	stack.assertRootDocumentSynced(t, documentA, movedA, wantA, 60*time.Second)
	stack.moveRootDocument(t, documentB, movedB)
	stack.assertLocalMissing(t, pathB, 60*time.Second)
	stack.assertRootDocumentSynced(t, documentB, movedB, wantB, 60*time.Second)

	editMovedA := docA.GetText("content").ToString()
	requireContainsForRegression(t, editMovedA, "beta\n")
	stack.applyClientTextUpdate(t, documentA, docA, "a-edit-after-move", func(txn *crdt.Transaction, text *crdt.YText) {
		text.Delete(txn, strings.Index(editMovedA, "beta\n"), len("beta\n"))
		text.Insert(txn, 0, "INTRO: ", nil)
		text.Insert(txn, text.LenInTxn(txn), "delta\n", nil)
	})
	wantA = docA.GetText("content").ToString()
	stack.assertRootDocumentSynced(t, documentA, movedA, wantA, 60*time.Second)

	editMovedB := docB.GetText("content").ToString()
	requireContainsForRegression(t, editMovedB, "zero ", "three")
	stack.applyClientTextUpdate(t, documentB, docB, "b-edit-after-move", func(txn *crdt.Transaction, text *crdt.YText) {
		text.Delete(txn, 0, len("zero "))
		insertAt := strings.Index(editMovedB, "three")
		text.Insert(txn, insertAt-len("zero "), "TWO ", nil)
		text.Insert(txn, text.LenInTxn(txn), "five\n", nil)
	})
	wantB = docB.GetText("content").ToString()
	stack.assertRootDocumentSynced(t, documentB, movedB, wantB, 60*time.Second)

	stack.tombstoneRootDocument(t, documentB)
	stack.waitForRootEntry(t, documentB, movedB, true, 60*time.Second)
	stack.assertLocalMissing(t, movedB, 60*time.Second)
	stack.waitForBackendContent(t, documentB, wantB, 30*time.Second)
}

func TestThreadCreationAcceptsClientRelativeAnchors(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	documentID := stack.createDocument(t, uniquePath("thread-anchor", ".md"), "alpha bravo charlie\n")
	wsURL := stack.documentWSURL(t, documentID)
	clientDoc := crdt.New(crdt.WithClientID(901))
	conn := dialDocumentWebsocket(t, wsURL, "thread-author", 901)
	defer conn.Close()
	syncDocumentClient(t, conn, clientDoc, "alpha bravo charlie\n")

	text := clientDoc.GetText("content")
	relativeStart := encodeRelativeAnchorForRegression(text, 6)
	relativeEnd := encodeRelativeAnchorForRegression(text, 11)
	payload, err := json.Marshal(map[string]any{
		"documentId":    documentID,
		"title":         "Check bravo",
		"body":          "Please review this anchored range.",
		"relativeStart": relativeStart,
		"relativeEnd":   relativeEnd,
		"start":         6,
		"end":           11,
		"line":          1,
		"excerpt":       "bravo",
	})
	if err != nil {
		t.Fatalf("marshal thread request: %v", err)
	}
	var created struct {
		Thread struct {
			ID     string                 `json:"id"`
			Anchor regressionThreadAnchor `json:"anchor"`
		} `json:"thread"`
	}
	stack.postJSON(t, stack.workspaceAPIPath("/threads"), stack.authToken, json.RawMessage(payload), http.StatusCreated, &created)
	if created.Thread.ID == "" {
		t.Fatal("expected thread id")
	}
	if created.Thread.Anchor.RelativeStart != relativeStart || created.Thread.Anchor.RelativeEnd != relativeEnd {
		t.Fatalf("relative anchors were not preserved: %#v", created.Thread.Anchor)
	}
	if created.Thread.Anchor.Kind != "text-range" || created.Thread.Anchor.Excerpt != "bravo" {
		t.Fatalf("expected text-range anchor metadata, got %#v", created.Thread.Anchor)
	}
}

func TestThreadCreationWithoutRelativeAnchorsCreatesDocumentThread(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	documentID := stack.createDocument(t, uniquePath("raw-thread-anchor", ".md"), "alpha bravo charlie\n")
	payload, err := json.Marshal(map[string]any{
		"documentId": documentID,
		"title":      "Raw offset should fail",
		"body":       "This should not create an unstable text-range thread.",
		"start":      6,
		"end":        11,
		"line":       1,
		"excerpt":    "bravo",
	})
	if err != nil {
		t.Fatalf("marshal thread request: %v", err)
	}
	var created struct {
		Thread struct {
			ID     string                 `json:"id"`
			Anchor regressionThreadAnchor `json:"anchor"`
		} `json:"thread"`
	}
	stack.postJSON(t, stack.workspaceAPIPath("/threads"), stack.authToken, json.RawMessage(payload), http.StatusCreated, &created)
	if created.Thread.ID == "" {
		t.Fatal("expected thread id")
	}
	if created.Thread.Anchor.Kind != "document" || created.Thread.Anchor.RelativeStart != "" || created.Thread.Anchor.RelativeEnd != "" {
		t.Fatalf("expected raw offsets to be ignored and stored as a document thread, got %#v", created.Thread.Anchor)
	}
}

func TestMultipleWebsocketRecipientsConverge(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	documentID := stack.createDocument(t, uniquePath("multi-recipient", ".md"), "start\n")
	wsURL := stack.documentWSURL(t, documentID)

	writerDoc := crdt.New(crdt.WithClientID(301))
	viewerOneDoc := crdt.New(crdt.WithClientID(302))
	viewerTwoDoc := crdt.New(crdt.WithClientID(303))
	writerConn := dialDocumentWebsocket(t, wsURL, "writer", 301)
	defer writerConn.Close()
	viewerOneConn := dialDocumentWebsocket(t, wsURL, "viewer-one", 302)
	defer viewerOneConn.Close()
	viewerTwoConn := dialDocumentWebsocket(t, wsURL, "viewer-two", 303)
	defer viewerTwoConn.Close()

	syncDocumentClient(t, writerConn, writerDoc, "start\n")
	syncDocumentClient(t, viewerOneConn, viewerOneDoc, "start\n")
	syncDocumentClient(t, viewerTwoConn, viewerTwoDoc, "start\n")

	writerText := writerDoc.GetText("content")
	firstUpdate := captureDocUpdate(t, writerDoc, "first-insert", func(txn *crdt.Transaction) {
		writerText.Insert(txn, writerText.LenInTxn(txn), "one\n", nil)
	})
	writeBinary(t, writerConn, yproto.BuildSyncUpdate(firstUpdate))
	waitForWebsocketContent(t, viewerOneConn, viewerOneDoc, "start\none\n")
	waitForWebsocketContent(t, viewerTwoConn, viewerTwoDoc, "start\none\n")

	secondUpdate := captureDocUpdate(t, writerDoc, "second-insert", func(txn *crdt.Transaction) {
		writerText.Insert(txn, writerText.LenInTxn(txn), "two\n", nil)
	})
	writeBinary(t, writerConn, yproto.BuildSyncUpdate(secondUpdate))
	waitForWebsocketContent(t, viewerOneConn, viewerOneDoc, "start\none\ntwo\n")
	waitForWebsocketContent(t, viewerTwoConn, viewerTwoDoc, "start\none\ntwo\n")
	stack.waitForBackendContent(t, documentID, "start\none\ntwo\n", 30*time.Second)
}

func TestConcurrentWebsocketWritersConverge(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	documentID := stack.createDocument(t, uniquePath("concurrent-writers", ".md"), "base\n")
	wsURL := stack.documentWSURL(t, documentID)

	writerOneDoc := crdt.New(crdt.WithClientID(601))
	writerTwoDoc := crdt.New(crdt.WithClientID(602))
	viewerDoc := crdt.New(crdt.WithClientID(603))
	writerOneConn := dialDocumentWebsocket(t, wsURL, "writer-one", 601)
	defer writerOneConn.Close()
	writerTwoConn := dialDocumentWebsocket(t, wsURL, "writer-two", 602)
	defer writerTwoConn.Close()
	viewerConn := dialDocumentWebsocket(t, wsURL, "viewer", 603)
	defer viewerConn.Close()

	syncDocumentClient(t, writerOneConn, writerOneDoc, "base\n")
	syncDocumentClient(t, writerTwoConn, writerTwoDoc, "base\n")
	syncDocumentClient(t, viewerConn, viewerDoc, "base\n")

	writerOneText := writerOneDoc.GetText("content")
	writerTwoText := writerTwoDoc.GetText("content")
	updateOne := captureDocUpdate(t, writerOneDoc, "writer-one", func(txn *crdt.Transaction) {
		writerOneText.Insert(txn, writerOneText.LenInTxn(txn), "writer-one\n", nil)
	})
	updateTwo := captureDocUpdate(t, writerTwoDoc, "writer-two", func(txn *crdt.Transaction) {
		writerTwoText.Insert(txn, writerTwoText.LenInTxn(txn), "writer-two\n", nil)
	})

	errs := make(chan error, 2)
	go func() { errs <- writerOneConn.WriteMessage(websocket.BinaryMessage, yproto.BuildSyncUpdate(updateOne)) }()
	go func() { errs <- writerTwoConn.WriteMessage(websocket.BinaryMessage, yproto.BuildSyncUpdate(updateTwo)) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("write concurrent update: %v", err)
		}
	}

	finalContent := stack.waitForBackendContentPredicate(t, documentID, 30*time.Second, func(content string) bool {
		return strings.HasPrefix(content, "base\n") &&
			strings.Count(content, "writer-one\n") == 1 &&
			strings.Count(content, "writer-two\n") == 1
	})
	waitForWebsocketContent(t, viewerConn, viewerDoc, finalContent)

	freshDoc := crdt.New(crdt.WithClientID(604))
	freshConn := dialDocumentWebsocket(t, wsURL, "fresh-concurrent-reader", 604)
	defer freshConn.Close()
	syncDocumentClient(t, freshConn, freshDoc, finalContent)
}

func TestDocumentWebsocketBroadcastsInsertUpdate(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	documentID := stack.createDocument(t, uniquePath("websocket", ".md"), "ab")
	wsURL := stack.documentWSURL(t, documentID)

	authorDoc := crdt.New(crdt.WithClientID(101))
	authorConn := dialDocumentWebsocket(t, wsURL, "writer", 101)
	defer authorConn.Close()
	viewerConn := dialDocumentWebsocket(t, wsURL, "viewer", 202)
	defer viewerConn.Close()

	syncDocumentClient(t, authorConn, authorDoc, "ab")

	authorText := authorDoc.GetText("content")
	insertUpdate := captureDocUpdate(t, authorDoc, "insert", func(txn *crdt.Transaction) {
		authorText.Insert(txn, authorText.LenInTxn(txn), "cd", nil)
	})
	writeBinary(t, authorConn, yproto.BuildSyncUpdate(insertUpdate))
	waitForSyncUpdate(t, viewerConn, insertUpdate)

	stack.waitForBackendContent(t, documentID, "abcd", 30*time.Second)
}

func TestDocumentWebsocketBroadcastsDeleteUpdate(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	documentID := stack.createDocument(t, uniquePath("websocket-delete", ".md"), "abcd")
	wsURL := stack.documentWSURL(t, documentID)

	authorDoc := crdt.New(crdt.WithClientID(701))
	viewerDoc := crdt.New(crdt.WithClientID(702))
	authorConn := dialDocumentWebsocket(t, wsURL, "writer", 701)
	defer authorConn.Close()
	viewerConn := dialDocumentWebsocket(t, wsURL, "viewer", 702)
	defer viewerConn.Close()

	syncDocumentClient(t, authorConn, authorDoc, "abcd")
	syncDocumentClient(t, viewerConn, viewerDoc, "abcd")

	authorText := authorDoc.GetText("content")
	deleteUpdate := captureDocUpdate(t, authorDoc, "delete", func(txn *crdt.Transaction) {
		authorText.Delete(txn, authorText.LenInTxn(txn)-1, 1)
	})
	writeBinary(t, authorConn, yproto.BuildSyncUpdate(deleteUpdate))

	waitForWebsocketContent(t, viewerConn, viewerDoc, "abc")
	stack.waitForBackendContent(t, documentID, "abc", 30*time.Second)
}

func TestLocalAndRemoteAppendMergeConvergesAcrossRecipients(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	path := uniquePath("local-remote-append", ".md")
	documentID := stack.createDocument(t, path, "base\n")
	stack.waitForLocalContent(t, path, "base\n", 30*time.Second)

	wsURL := stack.documentWSURL(t, documentID)
	remoteDoc := crdt.New(crdt.WithClientID(401))
	remoteConn := dialDocumentWebsocket(t, wsURL, "remote-writer", 401)
	defer remoteConn.Close()
	syncDocumentClient(t, remoteConn, remoteDoc, "base\n")

	stack.appendLocalFile(t, path, "local\n")
	remoteText := remoteDoc.GetText("content")
	remoteUpdate := captureDocUpdate(t, remoteDoc, "remote-append", func(txn *crdt.Transaction) {
		remoteText.Insert(txn, remoteText.LenInTxn(txn), "remote\n", nil)
	})
	writeBinary(t, remoteConn, yproto.BuildSyncUpdate(remoteUpdate))

	finalContent := stack.waitForBackendContentPredicate(t, documentID, 60*time.Second, func(content string) bool {
		return strings.HasPrefix(content, "base\n") &&
			strings.Count(content, "local\n") == 1 &&
			strings.Count(content, "remote\n") == 1
	})
	stack.waitForLocalContent(t, path, finalContent, 60*time.Second)

	freshDoc := crdt.New(crdt.WithClientID(402))
	freshConn := dialDocumentWebsocket(t, wsURL, "fresh-recipient", 402)
	defer freshConn.Close()
	syncDocumentClient(t, freshConn, freshDoc, finalContent)
}

func TestOverlappingLocalAndRemoteRewritePreservesLocalDivergence(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	path := uniquePath("overlap-rewrite", ".md")
	documentID := stack.createDocument(t, path, "title\nshared\n")
	stack.waitForLocalContent(t, path, "title\nshared\n", 30*time.Second)

	wsURL := stack.documentWSURL(t, documentID)
	remoteDoc := crdt.New(crdt.WithClientID(501))
	remoteConn := dialDocumentWebsocket(t, wsURL, "remote-rewriter", 501)
	defer remoteConn.Close()
	syncDocumentClient(t, remoteConn, remoteDoc, "title\nshared\n")

	stack.writeLocalFile(t, path, "title\nlocal rewrite\n")
	remoteText := remoteDoc.GetText("content")
	remoteUpdate := captureDocUpdate(t, remoteDoc, "remote-overlap", func(txn *crdt.Transaction) {
		remoteText.Delete(txn, len("title\n"), len("shared\n"))
		remoteText.Insert(txn, len("title\n"), "remote rewrite\n", nil)
	})
	writeBinary(t, remoteConn, yproto.BuildSyncUpdate(remoteUpdate))

	finalContent := stack.waitForBackendContentPredicate(t, documentID, 60*time.Second, func(content string) bool {
		return strings.HasPrefix(content, "title\n") &&
			strings.Contains(content, "remote rewrite\n") &&
			content != "title\nshared\n"
	})
	localContent := stack.waitForLocalContentPredicate(t, path, 60*time.Second, func(content string) bool {
		return strings.HasPrefix(content, "title\n") && strings.Contains(content, "local rewrite")
	})
	if localContent == finalContent {
		t.Fatalf("expected overlapping rewrite to remain locally divergent until resolved; both sides have %q", finalContent)
	}

	freshDoc := crdt.New(crdt.WithClientID(502))
	freshConn := dialDocumentWebsocket(t, wsURL, "fresh-overlap-reader", 502)
	defer freshConn.Close()
	syncDocumentClient(t, freshConn, freshDoc, finalContent)
}

type regressionStack struct {
	root        string
	composeFile string
	project     string
	authToken   string
	workspaceID string
	daemonToken string
}

func newRegressionStack(t *testing.T) *regressionStack {
	t.Helper()
	root := repoRoot(t)
	return &regressionStack{
		root:        root,
		composeFile: filepath.Join(root, "test", "regression", "docker-compose.yml"),
		project:     "notty_regression_" + strconv.FormatInt(time.Now().UnixNano(), 36),
	}
}

func (s *regressionStack) up(t *testing.T) {
	t.Helper()
	s.run(t, "up", "-d", "--build", "postgres", "backend")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := s.command(ctx, "down", "-v", "--remove-orphans")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Logf("compose down failed: %v\n%s", err, output)
		}
	})
	s.waitForBackend(t, 2*time.Minute)
	s.bootstrapWorkspace(t)
	s.runWithEnv(t, map[string]string{
		"NOTTY_WORKSPACE_ID": s.workspaceID,
		"NOTTY_DAEMON_TOKEN": s.daemonToken,
	}, "up", "-d", "--build", "daemon")
}

func (s *regressionStack) backendURL(t *testing.T) string {
	t.Helper()
	output := strings.TrimSpace(s.runOutput(t, "port", "backend", "8080"))
	if output == "" {
		t.Fatal("backend port was empty")
	}
	return "http://" + output
}

func (s *regressionStack) bootstrapWorkspace(t *testing.T) {
	t.Helper()
	email := "regression-" + s.project + "@example.test"
	var auth struct {
		Token string `json:"token"`
	}
	s.postJSON(t, "/api/auth/register", "", map[string]string{
		"email":       email,
		"password":    "regression-password",
		"displayName": "Regression User",
	}, http.StatusCreated, &auth)
	if auth.Token == "" {
		t.Fatal("registration returned empty token")
	}
	s.authToken = auth.Token

	var workspaceResponse struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}
	s.postJSON(t, "/api/workspaces", s.authToken, map[string]string{
		"name": "Regression Workspace",
		"slug": s.project,
	}, http.StatusCreated, &workspaceResponse)
	if workspaceResponse.Workspace.ID == "" {
		t.Fatal("workspace creation returned empty id")
	}
	s.workspaceID = workspaceResponse.Workspace.ID

	var daemonResponse struct {
		Token string `json:"token"`
	}
	s.postJSON(t, s.workspaceAPIPath("/daemons"), s.authToken, map[string]string{
		"name": "Regression daemon",
	}, http.StatusCreated, &daemonResponse)
	if daemonResponse.Token == "" {
		t.Fatal("daemon token creation returned empty token")
	}
	s.daemonToken = daemonResponse.Token
}

func (s *regressionStack) workspaceAPIPath(path string) string {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "/api/workspaces/" + url.PathEscape(s.workspaceID) + path
}

func (s *regressionStack) documentWSURL(t *testing.T, documentID string) string {
	t.Helper()
	baseURL := s.backendURL(t)
	u, err := url.Parse("ws" + strings.TrimPrefix(baseURL, "http") + "/ws/workspaces/" + url.PathEscape(s.workspaceID) + "/documents/" + url.PathEscape(documentID))
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("token", s.authToken)
	u.RawQuery = query.Encode()
	return u.String()
}

func (s *regressionStack) daemonWorkspaceDir() string {
	return "/workspace/notty/" + s.workspaceID
}

func (s *regressionStack) postJSON(t *testing.T, path string, bearer string, body any, wantStatus int, out any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, s.backendURL(t)+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		response, _ := io.ReadAll(resp.Body)
		t.Fatalf("post %s status %d, want %d: %s", path, resp.StatusCode, wantStatus, response)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode post %s response: %v", path, err)
		}
	}
}

func (s *regressionStack) getJSON(t *testing.T, path string, bearer string, wantStatus int, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.backendURL(t)+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		response, _ := io.ReadAll(resp.Body)
		t.Fatalf("get %s status %d, want %d: %s", path, resp.StatusCode, wantStatus, response)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode get %s response: %v", path, err)
		}
	}
}

func (s *regressionStack) waitForBackend(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		url := s.backendURL(t) + "/healthz"
		resp, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("backend did not become healthy: %v", lastErr)
}

func (s *regressionStack) command(ctx context.Context, args ...string) *exec.Cmd {
	fullArgs := append([]string{"compose", "-f", s.composeFile, "-p", s.project}, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	cmd.Dir = s.root
	return cmd
}

func (s *regressionStack) run(t *testing.T, args ...string) {
	t.Helper()
	_ = s.runOutput(t, args...)
}

func (s *regressionStack) runWithEnv(t *testing.T, env map[string]string, args ...string) {
	t.Helper()
	_ = s.runOutputWithEnv(t, env, args...)
}

func (s *regressionStack) runOutput(t *testing.T, args ...string) string {
	t.Helper()
	return s.runOutputWithEnv(t, nil, args...)
}

func (s *regressionStack) runOutputWithEnv(t *testing.T, env map[string]string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := s.command(ctx, args...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func (s *regressionStack) execService(t *testing.T, service string, command string) string {
	t.Helper()
	return s.runOutput(t, "exec", "-T", service, "sh", "-lc", command)
}

func (s *regressionStack) writeSequentialFile(t *testing.T, path string, lines int, delay time.Duration) {
	t.Helper()
	cmd := s.daemonWriterCommand(path, lines, delay)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("write sequential file: %v\n%s", err, output)
	}
}

func (s *regressionStack) daemonWriterCommand(path string, lines int, delay time.Duration) *exec.Cmd {
	delaySeconds := fmt.Sprintf("%.3f", delay.Seconds())
	dir := filepath.ToSlash(filepath.Dir(path))
	mkdir := ""
	if dir != "." && dir != "" {
		mkdir = "mkdir -p " + shellQuote(dir) + " && "
	}
	script := fmt.Sprintf(
		"mkdir -p %s && cd %s && %s: > %s && i=1 && while [ \"$i\" -le %d ]; do printf \"%%s\\n\" \"$i\" >> %s; sleep %s; i=$((i+1)); done",
		shellQuote(s.daemonWorkspaceDir()),
		shellQuote(s.daemonWorkspaceDir()),
		mkdir,
		shellQuote(path),
		lines,
		shellQuote(path),
		shellQuote(delaySeconds),
	)
	ctx := context.Background()
	return s.command(ctx, "exec", "-T", "daemon", "sh", "-lc", script)
}

func (s *regressionStack) assertLocalSequence(t *testing.T, path string, lines int) {
	t.Helper()
	script := fmt.Sprintf(
		"cd %s && awk -v want=%d 'NR != $1 { printf(\"first_mismatch line=%%d value=%%s\\n\", NR, $1); exit 1 } END { if (NR != want) { printf(\"bad_count got=%%d want=%%d\\n\", NR, want); exit 1 }; printf(\"local_ok lines=%%d last=%%s\\n\", NR, $1) }' %s",
		shellQuote(s.daemonWorkspaceDir()),
		lines,
		shellQuote(path),
	)
	s.execService(t, "daemon", script)
}

func (s *regressionStack) writeLocalFile(t *testing.T, path string, content string) {
	t.Helper()
	dir := filepath.ToSlash(filepath.Dir(path))
	mkdir := ""
	if dir != "." && dir != "" {
		mkdir = "mkdir -p " + shellQuote(dir) + " && "
	}
	s.execService(t, "daemon", "mkdir -p "+shellQuote(s.daemonWorkspaceDir())+" && cd "+shellQuote(s.daemonWorkspaceDir())+" && "+mkdir+fmt.Sprintf("printf %%s %s > %s", shellQuote(content), shellQuote(path)))
}

func (s *regressionStack) appendLocalFile(t *testing.T, path string, content string) {
	t.Helper()
	dir := filepath.ToSlash(filepath.Dir(path))
	mkdir := ""
	if dir != "." && dir != "" {
		mkdir = "mkdir -p " + shellQuote(dir) + " && "
	}
	s.execService(t, "daemon", "mkdir -p "+shellQuote(s.daemonWorkspaceDir())+" && cd "+shellQuote(s.daemonWorkspaceDir())+" && "+mkdir+fmt.Sprintf("printf %%s %s >> %s", shellQuote(content), shellQuote(path)))
}

func (s *regressionStack) removeLocalFile(t *testing.T, path string) {
	t.Helper()
	s.execService(t, "daemon", "cd "+shellQuote(s.daemonWorkspaceDir())+" && rm -f "+shellQuote(path))
}

func (s *regressionStack) moveLocalFile(t *testing.T, from string, to string) {
	t.Helper()
	dir := filepath.ToSlash(filepath.Dir(to))
	mkdir := ""
	if dir != "." && dir != "" {
		mkdir = "mkdir -p " + shellQuote(dir) + " && "
	}
	s.execService(t, "daemon", "cd "+shellQuote(s.daemonWorkspaceDir())+" && "+mkdir+"mv "+shellQuote(from)+" "+shellQuote(to))
}

func (s *regressionStack) localFileContent(t *testing.T, path string) string {
	t.Helper()
	return s.execService(t, "daemon", fmt.Sprintf("if [ -d %s ]; then cd %s && if [ -f %s ]; then cat %s; fi; fi", shellQuote(s.daemonWorkspaceDir()), shellQuote(s.daemonWorkspaceDir()), shellQuote(path), shellQuote(path)))
}

func (s *regressionStack) localFileListing(t *testing.T) string {
	t.Helper()
	return s.execService(t, "daemon", fmt.Sprintf("if [ -d %s ]; then cd %s && find . -path './.notty' -prune -o -type f -print | sort; fi", shellQuote(s.daemonWorkspaceDir()), shellQuote(s.daemonWorkspaceDir())))
}

func (s *regressionStack) daemonLogs(t *testing.T) string {
	t.Helper()
	return s.runOutput(t, "logs", "--no-color", "--tail", "120", "daemon")
}

func (s *regressionStack) waitForLocalContent(t *testing.T, path string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.localFileContent(t, path)
		if last == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("local file %s did not converge: got %q want %q", path, last, want)
}

func (s *regressionStack) waitForLocalContentPredicate(t *testing.T, path string, timeout time.Duration, accept func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.localFileContent(t, path)
		if accept(last) {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("local file %s did not satisfy predicate: last=%q", path, last)
	return ""
}

func (s *regressionStack) assertLocalMissing(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.execService(t, "daemon", fmt.Sprintf("cd %s && if [ -e %s ]; then printf exists; else printf missing; fi", shellQuote(s.daemonWorkspaceDir()), shellQuote(path)))
		if strings.TrimSpace(last) == "missing" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("local file %s still exists after %s: last=%q", path, timeout, last)
}

func (s *regressionStack) waitForLocalLineCountAtLeast(t *testing.T, path string, lines int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		output := strings.TrimSpace(s.execService(t, "daemon", fmt.Sprintf("if [ -d %s ]; then cd %s && if [ -f %s ]; then wc -l < %s; fi; fi", shellQuote(s.daemonWorkspaceDir()), shellQuote(s.daemonWorkspaceDir()), shellQuote(path), shellQuote(path))))
		last = output
		if count, err := strconv.Atoi(output); err == nil && count >= lines {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("file %s did not reach %d lines, last count %q", path, lines, last)
}

func (s *regressionStack) waitForBackendSequence(t *testing.T, path string, lines int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		content, err := s.backendDocumentContentByPath(path)
		if err == nil {
			if err := assertSequentialContent(content, lines); err == nil {
				return
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("backend document %s did not converge to %d sequential lines: %v", path, lines, lastErr)
}

func (s *regressionStack) waitForBackendContent(t *testing.T, documentID string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		content, err := s.backendDocumentContent(documentID)
		if err == nil {
			last = content
			if content == want {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("backend document %s did not converge: got %q want %q", documentID, last, want)
}

func (s *regressionStack) waitForBackendContentByPath(t *testing.T, path string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		content, err := s.backendDocumentContentByPath(path)
		if err == nil {
			last = content
			if content == want {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("backend document path %s did not converge: got %q want %q", path, last, want)
}

func (s *regressionStack) waitForBackendContentPredicate(t *testing.T, documentID string, timeout time.Duration, accept func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		content, err := s.backendDocumentContent(documentID)
		if err == nil {
			last = content
			if accept(content) {
				return content
			}
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("backend document %s did not satisfy predicate: last=%q err=%v", documentID, last, lastErr)
	return ""
}

func (s *regressionStack) assertRootDocumentSynced(t *testing.T, documentID, path, content string, timeout time.Duration) {
	t.Helper()
	s.waitForRootEntry(t, documentID, path, false, timeout)
	s.waitForBackendContent(t, documentID, content, timeout)
	s.waitForLocalContent(t, path, content, timeout)
}

func (s *regressionStack) waitForRootEntry(t *testing.T, documentID, path string, deleted bool, timeout time.Duration) {
	t.Helper()
	rootID := s.workspaceRootDocumentIDForTest(t)
	deadline := time.Now().Add(timeout)
	var last regressionRootEntry
	var found bool
	var lastErr error
	for time.Now().Before(deadline) {
		entries, err := s.databaseRootEntries(rootID)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		last, found = entries[documentID]
		if found && last.Path == normalizeRegressionRootPath(path) && last.Deleted == deleted {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("root entry did not converge for %s: got=%#v found=%v want path=%q deleted=%v err=%v paths=%v localFiles=%q logs=%q",
		documentID, last, found, path, deleted, lastErr, s.documentPaths(), s.localFileListing(t), s.daemonLogs(t))
}

func (s *regressionStack) syncFreshWebsocketClientByPath(t *testing.T, path string, want string) {
	t.Helper()
	documentID, _, _, err := s.documentHeaderByPath(path)
	if err != nil {
		t.Fatalf("document %s not found for fresh websocket sync: %v", path, err)
	}
	wsURL := s.documentWSURL(t, documentID)
	clientID := uint64(time.Now().UnixNano())
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientID)))
	conn := dialDocumentWebsocket(t, wsURL, "fresh-stress-reader", clientID)
	defer conn.Close()
	syncDocumentClient(t, conn, doc, want)
}

func (s *regressionStack) waitForDocumentPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if contains(s.documentPaths(), path) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("document path %s did not appear; current paths: %v", path, s.documentPaths())
}

func (s *regressionStack) waitForDocumentPathGone(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var paths []string
	for time.Now().Before(deadline) {
		paths = s.documentPaths()
		if !contains(paths, path) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("document path %s did not disappear; current paths: %v", path, paths)
}

func (s *regressionStack) documentPaths() []string {
	rootID := s.workspaceRootDocumentID()
	if rootID == "" {
		return nil
	}
	entries, err := s.databaseRootEntries(rootID)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Deleted && entry.Path != "" {
			paths = append(paths, entry.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

func (s *regressionStack) backendDocumentContentByPath(path string) (string, error) {
	docID, err := s.documentIDForRootPath(path)
	if err != nil {
		return "", err
	}
	return s.backendDocumentContent(docID)
}

func (s *regressionStack) backendDocumentContent(documentID string) (string, error) {
	doc, err := s.backendDocumentDoc(documentID)
	if err != nil {
		return "", err
	}
	defer doc.Close()
	return doc.GetText("content").ToString(), nil
}

func (s *regressionStack) backendDocumentDoc(documentID string) (*crdt.Doc, error) {
	_, clientID, headID, err := s.documentHeaderByID(documentID)
	if err != nil {
		return nil, err
	}
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientID)))
	checkpointID, checkpoint, err := s.latestCheckpoint(documentID, headID)
	if err != nil {
		doc.Close()
		return nil, err
	}
	if len(checkpoint) > 0 {
		if err := crdt.ApplyUpdateV1(doc, checkpoint, "checkpoint"); err != nil {
			doc.Close()
			return nil, err
		}
	}
	updates, err := s.documentUpdates(documentID, checkpointID, headID)
	if err != nil {
		doc.Close()
		return nil, err
	}
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(doc, update, "tail"); err != nil {
			doc.Close()
			return nil, err
		}
	}
	return doc, nil
}

func (s *regressionStack) documentHeaderByPath(path string) (string, uint64, int64, error) {
	documentID, err := s.documentIDForRootPath(path)
	if err != nil {
		return "", 0, 0, err
	}
	return s.documentHeaderByID(documentID)
}

func (s *regressionStack) workspaceRootDocumentID() string {
	query := fmt.Sprintf(
		"SELECT id FROM documents WHERE workspace_id=%s AND path='.notty/root' LIMIT 1",
		sqlQuote(s.workspaceID),
	)
	return strings.TrimSpace(s.psql(query))
}

func (s *regressionStack) workspaceRootDocumentIDForTest(t *testing.T) string {
	t.Helper()
	var workspace struct {
		RootDocumentID string `json:"rootDocumentId"`
	}
	s.getJSON(t, s.workspaceAPIPath("/workspace"), s.authToken, http.StatusOK, &workspace)
	if workspace.RootDocumentID == "" {
		t.Fatal("workspace returned empty rootDocumentId")
	}
	return workspace.RootDocumentID
}

func (s *regressionStack) documentIDForRootPath(path string) (string, error) {
	rootID := s.workspaceRootDocumentID()
	if rootID == "" {
		return "", errors.New("root document id not found")
	}
	entries, err := s.databaseRootEntries(rootID)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.Deleted && entry.Path == normalizeRegressionRootPath(path) {
			return entry.DocumentID, nil
		}
	}
	return "", fmt.Errorf("root path %q not found", path)
}

func (s *regressionStack) databaseRootEntries(rootDocumentID string) (map[string]regressionRootEntry, error) {
	doc, err := s.backendDocumentDoc(rootDocumentID)
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	return decodeRegressionRootEntries(doc)
}

func decodeRegressionRootEntries(doc *crdt.Doc) (map[string]regressionRootEntry, error) {
	entries := map[string]regressionRootEntry{}
	root := doc.GetMap("root")
	if err := doc.Read(func(txn *crdt.Transaction) error {
		entriesMap, ok, err := root.GetMap(txn, "entriesById")
		if err != nil || !ok {
			return err
		}
		items, err := entriesMap.Entries(txn)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.ValueKind != crdt.YMapEntryMap || item.MapValue == nil {
				continue
			}
			entry, ok, err := decodeRegressionRootEntry(txn, item.MapValue)
			if err != nil {
				return err
			}
			if ok {
				entries[entry.DocumentID] = entry
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return entries, nil
}

func decodeRegressionRootEntry(txn *crdt.Transaction, entryMap *crdt.YMap) (regressionRootEntry, bool, error) {
	kind, ok, err := entryMap.GetString(txn, "kind")
	if err != nil {
		return regressionRootEntry{}, false, err
	}
	if ok && strings.TrimSpace(kind) != "file" {
		return regressionRootEntry{}, false, nil
	}
	documentID, ok, err := entryMap.GetString(txn, "contentDocumentId")
	if err != nil || !ok || strings.TrimSpace(documentID) == "" {
		return regressionRootEntry{}, false, err
	}
	locRaw, _, err := entryMap.GetString(txn, "loc")
	if err != nil {
		return regressionRootEntry{}, false, err
	}
	var loc regressionRootLoc
	_ = json.Unmarshal([]byte(locRaw), &loc)
	deleted, _, err := entryMap.GetString(txn, "deleted")
	if err != nil {
		return regressionRootEntry{}, false, err
	}
	path := normalizeRegressionRootPath(loc.Name)
	if parent := normalizeRegressionRootPath(loc.ParentID); parent != "" {
		path = normalizeRegressionRootPath(parent + "/" + path)
	}
	return regressionRootEntry{
		DocumentID: strings.TrimSpace(documentID),
		Path:       path,
		Deleted:    strings.EqualFold(strings.TrimSpace(deleted), "true"),
	}, true, nil
}

func (s *regressionStack) documentHeaderByID(documentID string) (string, uint64, int64, error) {
	query := fmt.Sprintf(
		"SELECT d.id || chr(9) || d.client_id_seed || chr(9) || h.update_id FROM documents d JOIN document_heads h ON h.workspace_id=d.workspace_id AND h.document_id=d.id WHERE d.workspace_id=%s AND d.id=%s",
		sqlQuote(s.workspaceID),
		sqlQuote(documentID),
	)
	return parseDocumentHeader(s.psql(query))
}

func parseDocumentHeader(output string) (string, uint64, int64, error) {
	line := strings.TrimSpace(output)
	if line == "" {
		return "", 0, 0, errors.New("document not found")
	}
	fields := strings.Split(line, "\t")
	if len(fields) != 3 {
		return "", 0, 0, fmt.Errorf("bad document header %q", line)
	}
	clientID, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return "", 0, 0, err
	}
	headID, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return "", 0, 0, err
	}
	return fields[0], clientID, headID, nil
}

func (s *regressionStack) latestCheckpoint(documentID string, headID int64) (int64, []byte, error) {
	query := fmt.Sprintf(
		"SELECT update_id || chr(9) || crdt_state FROM document_checkpoints WHERE workspace_id=%s AND document_id=%s AND update_id <= %d ORDER BY update_id DESC LIMIT 1",
		sqlQuote(s.workspaceID),
		sqlQuote(documentID),
		headID,
	)
	output := strings.TrimSpace(s.psql(query))
	if output == "" {
		return 0, nil, nil
	}
	fields := strings.Split(output, "\t")
	if len(fields) != 2 {
		return 0, nil, fmt.Errorf("bad checkpoint row %q", output)
	}
	updateID, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, nil, err
	}
	update, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return 0, nil, err
	}
	return updateID, update, nil
}

func (s *regressionStack) documentUpdates(documentID string, afterID int64, throughID int64) ([][]byte, error) {
	query := fmt.Sprintf(
		"SELECT id || chr(9) || encode(update, 'hex') FROM document_updates WHERE workspace_id=%s AND document_id=%s AND id > %d AND id <= %d ORDER BY id ASC",
		sqlQuote(s.workspaceID),
		sqlQuote(documentID),
		afterID,
		throughID,
	)
	output := strings.TrimSpace(s.psql(query))
	if output == "" {
		return nil, nil
	}
	updates := make([][]byte, 0)
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1024), 64*1024*1024)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 2 {
			return nil, fmt.Errorf("bad update row %q", scanner.Text())
		}
		update, err := hex.DecodeString(fields[1])
		if err != nil {
			return nil, err
		}
		updates = append(updates, update)
	}
	return updates, scanner.Err()
}

func (s *regressionStack) psql(query string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := s.command(ctx, "exec", "-T", "postgres", "psql", "-U", "notty", "-d", "notty", "-At", "-c", query).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(output)
}

func (s *regressionStack) createDocument(t *testing.T, path string, content string) string {
	t.Helper()
	var document struct {
		ID string `json:"id"`
	}
	s.postJSON(t, s.workspaceAPIPath("/documents"), s.authToken, map[string]any{}, http.StatusCreated, &document)
	if document.ID == "" {
		t.Fatal("created document has empty id")
	}
	s.upsertRootDocument(t, document.ID, path, false)
	s.waitForRootEntry(t, document.ID, path, false, 30*time.Second)
	if content != "" {
		doc := crdt.New(crdt.WithClientID(crdt.ClientID(time.Now().UnixNano())))
		text := doc.GetText("content")
		update := captureDocUpdate(t, doc, "initial-content", func(txn *crdt.Transaction) {
			text.Insert(txn, 0, content, nil)
		})
		s.sendDocumentUpdate(t, document.ID, update)
		s.waitForBackendContent(t, document.ID, content, 30*time.Second)
	}
	return document.ID
}

func (s *regressionStack) upsertRootDocument(t *testing.T, documentID, path string, deleted bool) {
	t.Helper()
	rootID := s.workspaceRootDocumentIDForTest(t)
	rootDoc, err := s.backendDocumentDoc(rootID)
	if err != nil {
		t.Fatalf("load root document: %v", err)
	}
	defer rootDoc.Close()
	root := rootDoc.GetMap("root")
	update, err := rootDoc.Update(func(txn *crdt.Transaction) error {
		entries, ok, err := root.GetMap(txn, "entriesById")
		if err != nil {
			return err
		}
		if !ok {
			entries, err = root.SetMap(txn, "entriesById")
			if err != nil {
				return err
			}
		}
		entry, ok, err := entries.GetMap(txn, documentID)
		if err != nil {
			return err
		}
		if !ok {
			entry, err = entries.SetMap(txn, documentID)
			if err != nil {
				return err
			}
		}
		if err := entry.SetString(txn, "kind", "file"); err != nil {
			return err
		}
		if err := entry.SetString(txn, "contentDocumentId", documentID); err != nil {
			return err
		}
		if path != "" {
			if err := entry.SetString(txn, "loc", regressionRootEntryPathLoc(path)); err != nil {
				return err
			}
		}
		if deleted {
			return entry.SetString(txn, "deleted", "true")
		}
		return entry.SetString(txn, "deleted", "false")
	}, "regression-root")
	if err != nil {
		t.Fatalf("build root update: %v", err)
	}
	s.sendDocumentUpdate(t, rootID, update)
}

func (s *regressionStack) moveRootDocument(t *testing.T, documentID, path string) {
	t.Helper()
	s.upsertRootDocument(t, documentID, path, false)
	s.waitForRootEntry(t, documentID, path, false, 30*time.Second)
}

func (s *regressionStack) tombstoneRootDocument(t *testing.T, documentID string) {
	t.Helper()
	rootID := s.workspaceRootDocumentIDForTest(t)
	entries, err := s.databaseRootEntries(rootID)
	if err != nil {
		t.Fatalf("load root entries before tombstone: %v", err)
	}
	path := entries[documentID].Path
	s.upsertRootDocument(t, documentID, path, true)
}

func (s *regressionStack) sendDocumentUpdate(t *testing.T, documentID string, update []byte) {
	t.Helper()
	if len(update) == 0 {
		t.Fatal("expected non-empty CRDT update")
	}
	conn := dialDocumentWebsocket(t, s.documentWSURL(t, documentID), "regression-client", uint64(time.Now().UnixNano()))
	defer conn.Close()
	writeBinary(t, conn, yproto.BuildSyncUpdate(update))
}

func (s *regressionStack) applyClientTextUpdate(t *testing.T, documentID string, doc *crdt.Doc, origin string, mutate func(*crdt.Transaction, *crdt.YText)) {
	t.Helper()
	text := doc.GetText("content")
	update := captureDocUpdate(t, doc, origin, func(txn *crdt.Transaction) {
		mutate(txn, text)
	})
	s.sendDocumentUpdate(t, documentID, update)
}

func dialDocumentWebsocket(t *testing.T, rawURL string, actorID string, clientID uint64) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("actor_id", actorID)
	query.Set("actor_type", "test")
	query.Set("client_id", strconv.FormatUint(clientID, 10))
	u.RawQuery = query.Encode()
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	return conn
}

func writeBinary(t *testing.T, conn *websocket.Conn, payload []byte) {
	t.Helper()
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write websocket binary: %v", err)
	}
}

func syncDocumentClient(t *testing.T, conn *websocket.Conn, doc *crdt.Doc, want string) {
	t.Helper()
	writeBinary(t, conn, yproto.BuildSyncStep1FromStateVector(crdt.EncodeStateVectorV1(doc)))
	waitForWebsocketContent(t, conn, doc, want)
}

func encodeRelativeAnchorForRegression(text *crdt.YText, index int) string {
	assoc := 0
	if index >= text.Len() {
		assoc = -1
	}
	anchor, err := text.EncodeRelativeAnchor(index, assoc)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(anchor)
}

func waitForSyncUpdate(t *testing.T, conn *websocket.Conn, want []byte) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	var sawUpdates int
	for time.Now().Before(deadline) {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("timed out waiting for websocket sync update after seeing %d other sync update(s)", sawUpdates)
			}
			t.Fatalf("read websocket message: %v", err)
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		topLevel, reader, err := yproto.DecodeProtocolMessage(payload)
		if err != nil || topLevel != yproto.MessageSync {
			continue
		}
		syncType, data, err := yproto.DecodeSyncMessage(reader)
		if err != nil {
			t.Fatalf("decode sync message: %v", err)
		}
		if syncType == yproto.SyncUpdate {
			if bytes.Equal(data, want) {
				return
			}
			sawUpdates++
		}
	}
	t.Fatalf("timed out waiting for websocket sync update after seeing %d other sync update(s)", sawUpdates)
}

func waitForWebsocketContent(t *testing.T, conn *websocket.Conn, doc *crdt.Doc, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	var last string
	var lastEvent string
	for time.Now().Before(deadline) {
		if got := doc.GetText("content").ToString(); got == want {
			return
		} else {
			last = got
		}
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("timed out waiting for websocket content %q, last content %q, last event %s", want, last, lastEvent)
			}
			t.Fatalf("read websocket message: %v", err)
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		topLevel, reader, err := yproto.DecodeProtocolMessage(payload)
		if err != nil || topLevel != yproto.MessageSync {
			continue
		}
		syncType, data, err := yproto.DecodeSyncMessage(reader)
		if err != nil {
			t.Fatalf("decode sync message: %v", err)
		}
		if syncType == yproto.SyncStep1 {
			lastEvent = "sync-step1"
			continue
		}
		before := doc.GetText("content").ToString()
		if err := crdt.ApplyUpdateV1(doc, data, "regression-ws"); err != nil {
			t.Fatalf("apply sync update: %v", err)
		}
		after := doc.GetText("content").ToString()
		lastEvent = fmt.Sprintf("sync-type=%d bytes=%d before=%q after=%q", syncType, len(data), before, after)
	}
	t.Fatalf("timed out waiting for websocket content %q, last content %q, last event %s", want, last, lastEvent)
}

func captureDocUpdate(t *testing.T, doc *crdt.Doc, origin string, mutate func(*crdt.Transaction)) []byte {
	t.Helper()
	var update []byte
	unsubscribe := doc.OnUpdate(func(next []byte, gotOrigin any) {
		if gotOrigin == origin {
			update = append([]byte(nil), next...)
		}
	})
	doc.Transact(func(txn *crdt.Transaction) {
		mutate(txn)
	}, origin)
	unsubscribe()
	if len(update) == 0 {
		t.Fatalf("expected update for origin %q", origin)
	}
	return update
}

func assertSequentialContent(content string, lines int) error {
	if !strings.HasSuffix(content, "\n") {
		return errors.New("content does not end with newline")
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024), 64*1024*1024)
	count := 0
	for scanner.Scan() {
		count++
		if got, want := scanner.Text(), strconv.Itoa(count); got != want {
			return fmt.Errorf("first mismatch line=%d value=%q want=%q", count, got, want)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if count != lines {
		return fmt.Errorf("line count got=%d want=%d", count, lines)
	}
	return nil
}

func sequentialContent(lines int) string {
	var builder strings.Builder
	for i := 1; i <= lines; i++ {
		builder.WriteString(strconv.Itoa(i))
		builder.WriteByte('\n')
	}
	return builder.String()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("repo root not found")
		}
		wd = parent
	}
}

func uniquePath(prefix string, suffix string) string {
	return fmt.Sprintf("regression/%s-%d%s", prefix, time.Now().UnixNano(), suffix)
}

func normalizeRegressionRootPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	if path == "" {
		return ""
	}
	parts := make([]string, 0)
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}

func regressionRootEntryPathLoc(path string) string {
	encoded, _ := json.Marshal(regressionRootLoc{Name: normalizeRegressionRootPath(path)})
	return string(encoded)
}

func requireContainsForRegression(t *testing.T, value string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			t.Fatalf("expected %q to contain %q", value, needle)
		}
	}
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
