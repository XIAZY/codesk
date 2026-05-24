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

type regressionWorkspaceDocument struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	DesiredPath string `json:"desiredPath"`
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

func TestDotPathsIgnoredWhileRootLogsSync(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	stack.execService(t, "daemon", "mkdir -p "+shellQuote(stack.daemonWorkspaceDir())+" && cd "+shellQuote(stack.daemonWorkspaceDir())+" && mkdir -p .notty/cache && printf 'internal\\n' > .notty/cache/state.txt && printf 'hidden\\n' > .hidden.txt && printf 'line one\\nline two\\n' > codex-agent.log")

	stack.waitForDocumentPath(t, "codex-agent.log", 30*time.Second)
	paths := stack.documentPaths()
	if contains(paths, ".hidden.txt") {
		t.Fatalf("dotfile was incorrectly synced as a document: %v", paths)
	}
	if contains(paths, ".notty/cache/state.txt") {
		t.Fatalf(".notty cache file was incorrectly synced as a document: %v", paths)
	}
	stack.waitForBackendContentByPath(t, "codex-agent.log", "line one\nline two\n", 30*time.Second)
}

func TestPostStartLocalCreateThenLocalDeleteSyncsAsDelete(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	path := uniquePath("local-delete/docs/test3", ".md")
	stack.writeLocalFile(t, path, "alpha\n")

	documentID := stack.waitForDocumentPath(t, path, 90*time.Second)
	stack.waitForBackendContent(t, documentID, "alpha\n", 90*time.Second)
	stack.removeLocalPath(t, path)

	stack.waitForDocumentPathAbsent(t, path, 120*time.Second)
	stack.waitForLocalPathAbsent(t, path, 30*time.Second)
}

func TestTwoDaemonsSamePathEntriesConvergeToConflictPaths(t *testing.T) {
	stack := newRegressionStack(t)
	stack.upBackendOnly(t)

	env := map[string]string{
		"NOTTY_WORKSPACE_ID": stack.workspaceID,
		"NOTTY_DAEMON_TOKEN": stack.daemonToken,
	}
	stack.createDocument(t, "README.md", "from-a\n")
	stack.createDocument(t, "README.md", "from-b\n")

	stack.runWithEnv(t, env, "up", "-d", "--build", "daemon_a", "daemon_b")
	docs := stack.waitForConflictDocuments(t, "README.md", 90*time.Second)
	contents := map[string]string{}
	for _, doc := range docs {
		contents[doc.Path] = stack.waitForBackendContentPredicate(t, doc.ID, 60*time.Second, func(content string) bool {
			return content == "from-a\n" || content == "from-b\n"
		})
	}
	for _, service := range []string{"daemon_a", "daemon_b"} {
		for path, content := range contents {
			stack.waitForServiceLocalContent(t, service, path, content, 90*time.Second)
		}
	}
}

func TestOfflineLocalSamePathCreatesConvergeToConflictPaths(t *testing.T) {
	stack := newRegressionStack(t)
	stack.upBackendOnly(t)

	env := map[string]string{
		"NOTTY_WORKSPACE_ID": stack.workspaceID,
		"NOTTY_DAEMON_TOKEN": stack.daemonToken,
	}
	stack.runWithEnv(t, env, "build", "daemon_a", "daemon_b")
	stack.seedServiceLocalFile(t, env, "daemon_a", "README.md", "offline-a\n")
	stack.seedServiceLocalFile(t, env, "daemon_b", "README.md", "offline-b\n")

	stack.runWithEnv(t, env, "up", "-d", "daemon_a", "daemon_b")
	docs := stack.waitForConflictDocuments(t, "README.md", 120*time.Second)
	contents := map[string]string{}
	for _, doc := range docs {
		contents[doc.Path] = stack.waitForBackendContentPredicate(t, doc.ID, 90*time.Second, func(content string) bool {
			return content == "offline-a\n" || content == "offline-b\n"
		})
	}
	if len(uniqueStringValues(contents)) != 2 {
		t.Fatalf("expected both offline contents to survive, got %#v", contents)
	}
	for _, service := range []string{"daemon_a", "daemon_b"} {
		for path, content := range contents {
			stack.waitForServiceLocalContent(t, service, path, content, 120*time.Second)
		}
	}
}

func TestOfflineLocalEditAndRemoteRenamePreservesIdentityPathAndContent(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	oldPath := uniquePath("edit-rename/source", ".md")
	newPath := uniquePath("edit-rename/renamed", ".md")
	documentID := stack.createDocument(t, oldPath, "base\n")
	stack.waitForLocalContent(t, oldPath, "base\n", 30*time.Second)

	stack.stopService(t, "daemon")
	stack.seedServiceLocalFile(t, stack.daemonEnv(), "daemon", oldPath, "base\nlocal edit\n")
	renamed := stack.renameDocument(t, documentID, newPath)
	if renamed.ID != documentID || renamed.Path != newPath {
		t.Fatalf("unexpected rename response: %#v", renamed)
	}

	stack.startDaemon(t)
	stack.waitForBackendContent(t, documentID, "base\nlocal edit\n", 90*time.Second)
	stack.waitForLocalContent(t, newPath, "base\nlocal edit\n", 90*time.Second)
	stack.waitForLocalPathAbsent(t, oldPath, 60*time.Second)
	currentID, err := stack.documentIDByPath(newPath)
	if err != nil {
		t.Fatalf("renamed document not visible at %s: %v", newPath, err)
	}
	if currentID != documentID {
		t.Fatalf("rename changed document identity: got %s want %s", currentID, documentID)
	}
}

func TestOfflineLocalEditAndRemoteDeleteCreatesReplacementForDirtyBytes(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	path := uniquePath("delete-edit", ".md")
	deletedID := stack.createDocument(t, path, "base\n")
	stack.waitForLocalContent(t, path, "base\n", 30*time.Second)

	stack.stopService(t, "daemon")
	stack.seedServiceLocalFile(t, stack.daemonEnv(), "daemon", path, "base\nlocal after delete\n")
	stack.deleteDocument(t, deletedID)

	stack.startDaemon(t)
	replacement := stack.waitForDocumentPathAndDifferentID(t, path, deletedID, 120*time.Second)
	content := stack.waitForBackendContentPredicate(t, replacement.ID, 90*time.Second, func(content string) bool {
		return content == "base\nlocal after delete\n"
	})
	stack.waitForLocalContent(t, replacement.Path, content, 90*time.Second)
	if _, err := stack.streamHeadID(deletedID); err != nil {
		t.Fatalf("deleted stream head should remain for audit/history: %v", err)
	}
}

func TestRemoteRenameCollidingWithOfflineLocalUntrackedBytesPreservesBothFiles(t *testing.T) {
	stack := newRegressionStack(t)
	stack.up(t)

	oldPath := uniquePath("rename-collision/source", ".md")
	newPath := uniquePath("rename-collision/target", ".md")
	documentID := stack.createDocument(t, oldPath, "remote bytes\n")
	stack.waitForLocalContent(t, oldPath, "remote bytes\n", 30*time.Second)

	stack.stopService(t, "daemon")
	stack.seedServiceLocalFile(t, stack.daemonEnv(), "daemon", newPath, "local untracked bytes\n")
	stack.renameDocument(t, documentID, newPath)

	stack.startDaemon(t)
	docs := stack.waitForConflictDocuments(t, newPath, 120*time.Second)
	contents := map[string]string{}
	for _, doc := range docs {
		contents[doc.Path] = stack.waitForBackendContentPredicate(t, doc.ID, 90*time.Second, func(content string) bool {
			return content == "remote bytes\n" || content == "local untracked bytes\n"
		})
	}
	if len(uniqueStringValues(contents)) != 2 {
		t.Fatalf("expected both remote and local bytes to survive rename collision, got %#v", contents)
	}
	for path, content := range contents {
		stack.waitForLocalContent(t, path, content, 90*time.Second)
	}
}

func TestPrimaryAndAgentWorkspaceLocalEditsMergeToOneContentStream(t *testing.T) {
	stack := newRegressionStack(t)
	stack.upBackendOnly(t)
	agent := stack.createAgent(t, "reviewer")
	stack.startDaemon(t)

	path := uniquePath("primary-agent-merge", ".md")
	documentID := stack.createDocument(t, path, "base\n")
	stack.waitForLocalContent(t, path, "base\n", 60*time.Second)
	stack.waitForAgentLocalContent(t, agent.ID, path, "base\n", 90*time.Second)

	stack.appendLocalFile(t, path, "primary edit\n")
	stack.appendAgentLocalFile(t, agent.ID, path, "agent edit\n")

	finalContent := stack.waitForBackendContentPredicate(t, documentID, 120*time.Second, func(content string) bool {
		return strings.Count(content, "base\n") == 1 &&
			strings.Count(content, "primary edit\n") == 1 &&
			strings.Count(content, "agent edit\n") == 1
	})
	stack.waitForLocalContent(t, path, finalContent, 90*time.Second)
	stack.waitForAgentLocalContent(t, agent.ID, path, finalContent, 90*time.Second)
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
		return strings.Count(content, "base\n") == 1 &&
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
	daemonID    string
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
	s.upBackendOnly(t)
	s.runWithEnv(t, s.daemonEnv(), "up", "-d", "--build", "daemon")
}

func (s *regressionStack) upBackendOnly(t *testing.T) {
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
}

func (s *regressionStack) backendURL(t *testing.T) string {
	t.Helper()
	url := s.backendURLNoFatal()
	if url == "" {
		t.Fatal("backend port was empty")
	}
	return url
}

func (s *regressionStack) backendURLNoFatal() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := s.command(ctx, "port", "backend", "8080").CombinedOutput()
	if err != nil {
		return ""
	}
	port := strings.TrimSpace(string(output))
	if port == "" {
		return ""
	}
	return "http://" + port
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
		Daemon struct {
			ID string `json:"id"`
		} `json:"daemon"`
		Token string `json:"token"`
	}
	s.postJSON(t, s.workspaceAPIPath("/daemons"), s.authToken, map[string]string{
		"name": "Regression daemon",
	}, http.StatusCreated, &daemonResponse)
	if daemonResponse.Daemon.ID == "" {
		t.Fatal("daemon creation returned empty id")
	}
	if daemonResponse.Token == "" {
		t.Fatal("daemon token creation returned empty token")
	}
	s.daemonID = daemonResponse.Daemon.ID
	s.daemonToken = daemonResponse.Token
}

func (s *regressionStack) daemonEnv() map[string]string {
	return map[string]string{
		"NOTTY_WORKSPACE_ID": s.workspaceID,
		"NOTTY_DAEMON_TOKEN": s.daemonToken,
	}
}

func (s *regressionStack) startDaemon(t *testing.T) {
	t.Helper()
	s.runWithEnv(t, s.daemonEnv(), "up", "-d", "daemon")
}

func (s *regressionStack) stopService(t *testing.T, service string) {
	t.Helper()
	s.run(t, "stop", service)
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
	route := "/streams/"
	u, err := url.Parse("ws" + strings.TrimPrefix(baseURL, "http") + "/ws/workspaces/" + url.PathEscape(s.workspaceID) + route + url.PathEscape(documentID))
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

func (s *regressionStack) agentWorkspaceDir(agentID string) string {
	return "/workspace/agents/" + s.workspaceID + "/" + safeAgentWorkspaceNameForRegression(agentID)
}

func (s *regressionStack) postJSON(t *testing.T, path string, bearer string, body any, wantStatus int, out any) {
	t.Helper()
	s.requestJSON(t, http.MethodPost, path, bearer, body, wantStatus, out)
}

func (s *regressionStack) patchJSON(t *testing.T, path string, bearer string, body any, wantStatus int, out any) {
	t.Helper()
	s.requestJSON(t, http.MethodPatch, path, bearer, body, wantStatus, out)
}

func (s *regressionStack) deleteJSON(t *testing.T, path string, bearer string, wantStatus int, out any) {
	t.Helper()
	s.requestJSON(t, http.MethodDelete, path, bearer, nil, wantStatus, out)
}

func (s *regressionStack) requestJSON(t *testing.T, method string, path string, bearer string, body any, wantStatus int, out any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, s.backendURL(t)+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		response, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, response)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
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

func (s *regressionStack) removeLocalPath(t *testing.T, path string) {
	t.Helper()
	s.execService(t, "daemon", "cd "+shellQuote(s.daemonWorkspaceDir())+" && rm -f -- "+shellQuote(path))
}

func (s *regressionStack) appendAgentLocalFile(t *testing.T, agentID string, path string, content string) {
	t.Helper()
	dir := filepath.ToSlash(filepath.Dir(path))
	mkdir := ""
	if dir != "." && dir != "" {
		mkdir = "mkdir -p " + shellQuote(dir) + " && "
	}
	root := s.agentWorkspaceDir(agentID)
	s.execService(t, "daemon", "mkdir -p "+shellQuote(root)+" && cd "+shellQuote(root)+" && "+mkdir+fmt.Sprintf("printf %%s %s >> %s", shellQuote(content), shellQuote(path)))
}

func (s *regressionStack) seedServiceLocalFile(t *testing.T, env map[string]string, service string, path string, content string) {
	t.Helper()
	dir := filepath.ToSlash(filepath.Dir(path))
	mkdir := ""
	if dir != "." && dir != "" {
		mkdir = "mkdir -p " + shellQuote(dir) + " && "
	}
	script := "mkdir -p " + shellQuote(s.daemonWorkspaceDir()) + " && cd " + shellQuote(s.daemonWorkspaceDir()) + " && " + mkdir + fmt.Sprintf("printf %%s %s > %s", shellQuote(content), shellQuote(path))
	s.runOutputWithEnv(t, env, "run", "--rm", "--no-deps", "--entrypoint", "sh", "-T", service, "-lc", script)
}

func (s *regressionStack) localFileContent(t *testing.T, path string) string {
	t.Helper()
	return s.execService(t, "daemon", fmt.Sprintf("if [ -d %s ]; then cd %s && if [ -f %s ]; then cat %s; fi; fi", shellQuote(s.daemonWorkspaceDir()), shellQuote(s.daemonWorkspaceDir()), shellQuote(path), shellQuote(path)))
}

func (s *regressionStack) agentLocalFileContent(t *testing.T, agentID string, path string) string {
	t.Helper()
	root := s.agentWorkspaceDir(agentID)
	return s.execService(t, "daemon", fmt.Sprintf("if [ -d %s ]; then cd %s && if [ -f %s ]; then cat %s; fi; fi", shellQuote(root), shellQuote(root), shellQuote(path), shellQuote(path)))
}

func (s *regressionStack) localPathExists(t *testing.T, path string) bool {
	t.Helper()
	output := s.execService(t, "daemon", fmt.Sprintf("if [ -e %s/%s ]; then printf present; fi", shellQuote(s.daemonWorkspaceDir()), shellQuote(path)))
	return strings.TrimSpace(output) == "present"
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

func (s *regressionStack) waitForAgentLocalContent(t *testing.T, agentID string, path string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.agentLocalFileContent(t, agentID, path)
		if last == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("agent %s file %s did not converge: got %q want %q", agentID, path, last, want)
}

func (s *regressionStack) waitForLocalPathAbsent(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !s.localPathExists(t, path) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("local path %s still exists", path)
}

func (s *regressionStack) waitForServiceLocalContent(t *testing.T, service string, path string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.execService(t, service, fmt.Sprintf("if [ -d %s ]; then cd %s && if [ -f %s ]; then cat %s; fi; fi", shellQuote(s.daemonWorkspaceDir()), shellQuote(s.daemonWorkspaceDir()), shellQuote(path), shellQuote(path)))
		if last == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s file %s did not converge: got %q want %q", service, path, last, want)
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

func (s *regressionStack) syncFreshWebsocketClientByPath(t *testing.T, path string, want string) {
	t.Helper()
	documentID, err := s.documentIDByPath(path)
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

func (s *regressionStack) waitForDocumentPath(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, document := range s.workspaceDocuments() {
			if document.Path == path {
				return document.ID
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("document path %s did not appear; current paths: %v", path, s.documentPaths())
	return ""
}

func (s *regressionStack) waitForDocumentPathAbsent(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []string
	for time.Now().Before(deadline) {
		last = s.documentPaths()
		if !contains(last, path) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("document path %s still present; current paths: %v", path, last)
}

func (s *regressionStack) waitForDocumentPathAndDifferentID(t *testing.T, path string, oldID string, timeout time.Duration) regressionWorkspaceDocument {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []regressionWorkspaceDocument
	for time.Now().Before(deadline) {
		docs := s.workspaceDocuments()
		for _, doc := range docs {
			if doc.Path == path && doc.ID != oldID {
				return doc
			}
		}
		last = docs
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("replacement document for %s did not appear; old id %s current docs: %#v", path, oldID, last)
	return regressionWorkspaceDocument{}
}

func (s *regressionStack) waitForConflictDocuments(t *testing.T, canonicalPath string, timeout time.Duration) []regressionWorkspaceDocument {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []regressionWorkspaceDocument
	for time.Now().Before(deadline) {
		docs := s.workspaceDocuments()
		matches := make([]regressionWorkspaceDocument, 0, 2)
		for _, doc := range docs {
			if doc.Path == canonicalPath || isConflictPathFor(doc.Path, canonicalPath) {
				matches = append(matches, doc)
			}
		}
		if len(matches) == 2 && containsDocumentPath(matches, canonicalPath) {
			return matches
		}
		last = docs
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("conflict documents for %s did not appear; current docs: %#v", canonicalPath, last)
	return nil
}

func isConflictPathFor(pathValue string, canonicalPath string) bool {
	dir := filepath.ToSlash(filepath.Dir(canonicalPath))
	base := filepath.Base(canonicalPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	conflictBase := filepath.Base(pathValue)
	if dir != "." && dir != "" && filepath.ToSlash(filepath.Dir(pathValue)) != dir {
		return false
	}
	return strings.HasPrefix(conflictBase, stem+" (conflict ") && strings.HasSuffix(conflictBase, ")"+ext)
}

func containsDocumentPath(docs []regressionWorkspaceDocument, path string) bool {
	for _, doc := range docs {
		if doc.Path == path {
			return true
		}
	}
	return false
}

func uniqueStringValues(values map[string]string) map[string]struct{} {
	unique := map[string]struct{}{}
	for _, value := range values {
		unique[value] = struct{}{}
	}
	return unique
}

func (s *regressionStack) documentPaths() []string {
	documents := s.workspaceDocuments()
	paths := make([]string, 0, len(documents))
	for _, document := range documents {
		paths = append(paths, document.Path)
	}
	return paths
}

func (s *regressionStack) backendDocumentContentByPath(path string) (string, error) {
	docID, err := s.documentIDByPath(path)
	if err != nil {
		return "", err
	}
	return s.backendStreamContent(docID)
}

func (s *regressionStack) backendDocumentContent(documentID string) (string, error) {
	return s.backendStreamContent(documentID)
}

func (s *regressionStack) backendStreamContent(streamID string) (string, error) {
	headID, err := s.streamHeadID(streamID)
	if err != nil {
		return "", err
	}
	doc := crdt.New()
	checkpointID, checkpoint, err := s.latestStreamCheckpoint(streamID, headID)
	if err != nil {
		return "", err
	}
	if len(checkpoint) > 0 {
		if err := crdt.ApplyUpdateV1(doc, checkpoint, "stream-checkpoint"); err != nil {
			return "", err
		}
	}
	updates, err := s.streamUpdates(streamID, checkpointID, headID)
	if err != nil {
		return "", err
	}
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(doc, update, "stream-tail"); err != nil {
			return "", err
		}
	}
	return doc.GetText("content").ToString(), nil
}

func (s *regressionStack) streamHeadID(streamID string) (int64, error) {
	query := fmt.Sprintf(
		"SELECT update_id FROM crdt_stream_heads WHERE workspace_id=%s AND stream_id=%s",
		sqlQuote(s.workspaceID),
		sqlQuote(streamID),
	)
	output := strings.TrimSpace(s.psql(query))
	if output == "" {
		return 0, errors.New("stream head not found")
	}
	return strconv.ParseInt(output, 10, 64)
}

func (s *regressionStack) documentIDByPath(path string) (string, error) {
	if documents := s.workspaceDocuments(); len(documents) > 0 {
		for _, document := range documents {
			if document.Path == path {
				return document.ID, nil
			}
		}
	}
	return "", errors.New("document not found")
}

func (s *regressionStack) workspaceDocuments() []regressionWorkspaceDocument {
	baseURL := s.backendURLNoFatal()
	if baseURL == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+s.workspaceAPIPath("/workspace"), nil)
	if err != nil {
		return nil
	}
	if s.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	var payload struct {
		Documents []regressionWorkspaceDocument `json:"documents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	return payload.Documents
}

func (s *regressionStack) latestStreamCheckpoint(streamID string, headID int64) (int64, []byte, error) {
	query := fmt.Sprintf(
		"SELECT update_id || chr(9) || encode(crdt_state, 'hex') FROM crdt_stream_checkpoints WHERE workspace_id=%s AND stream_id=%s AND update_id <= %d ORDER BY update_id DESC LIMIT 1",
		sqlQuote(s.workspaceID),
		sqlQuote(streamID),
		headID,
	)
	output := strings.TrimSpace(s.psql(query))
	if output == "" {
		return 0, nil, nil
	}
	fields := strings.Split(output, "\t")
	if len(fields) != 2 {
		return 0, nil, fmt.Errorf("bad stream checkpoint row %q", output)
	}
	updateID, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, nil, err
	}
	update, err := hex.DecodeString(fields[1])
	if err != nil {
		return 0, nil, err
	}
	return updateID, update, nil
}

func (s *regressionStack) streamUpdates(streamID string, afterID int64, throughID int64) ([][]byte, error) {
	query := fmt.Sprintf(
		"SELECT id || chr(9) || encode(update, 'hex') FROM crdt_stream_updates WHERE workspace_id=%s AND stream_id=%s AND id > %d AND id <= %d ORDER BY id ASC",
		sqlQuote(s.workspaceID),
		sqlQuote(streamID),
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
			return nil, fmt.Errorf("bad stream update row %q", scanner.Text())
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
	s.postJSON(t, s.workspaceAPIPath("/documents"), s.authToken, map[string]string{"path": path, "content": content}, http.StatusCreated, &document)
	if document.ID == "" {
		t.Fatal("created document has empty id")
	}
	return document.ID
}

func (s *regressionStack) renameDocument(t *testing.T, documentID string, path string) regressionWorkspaceDocument {
	t.Helper()
	var document regressionWorkspaceDocument
	s.patchJSON(t, s.workspaceAPIPath("/documents/"+url.PathEscape(documentID)), s.authToken, map[string]string{"path": path}, http.StatusOK, &document)
	if document.ID == "" {
		t.Fatal("renamed document has empty id")
	}
	return document
}

func (s *regressionStack) deleteDocument(t *testing.T, documentID string) {
	t.Helper()
	var response struct {
		Status string `json:"status"`
	}
	s.deleteJSON(t, s.workspaceAPIPath("/documents/"+url.PathEscape(documentID)), s.authToken, http.StatusOK, &response)
	if response.Status != "deleted" {
		t.Fatalf("unexpected delete response: %#v", response)
	}
}

type regressionAgent struct {
	ID string `json:"id"`
}

func (s *regressionStack) createAgent(t *testing.T, handle string) regressionAgent {
	t.Helper()
	var agent regressionAgent
	s.postJSON(t, s.workspaceAPIPath("/daemons/"+url.PathEscape(s.daemonID)+"/agents"), s.authToken, map[string]string{
		"handle": handle,
		"name":   "Regression " + handle,
		"role":   "Exercise regression workspace sync.",
		"kind":   "codex",
	}, http.StatusCreated, &agent)
	if agent.ID == "" {
		t.Fatal("created agent has empty id")
	}
	return agent
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

func safeAgentWorkspaceNameForRegression(agentID string) string {
	trimmed := strings.TrimSpace(agentID)
	if trimmed == "" {
		return "agent"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return '_'
		}
	}, trimmed)
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
