package notty

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestApplyCRDTUpdateConvergesAcrossPeers(t *testing.T) {
	left := crdt.New(crdt.WithClientID(1))
	right := crdt.New(crdt.WithClientID(2))
	leftText := left.GetText("content")
	rightText := right.GetText("content")

	left.Transact(func(txn *crdt.Transaction) {
		leftText.Insert(txn, 0, "A", nil)
	}, "left")
	right.Transact(func(txn *crdt.Transaction) {
		rightText.Insert(txn, 0, "B", nil)
	}, "right")

	leftUpdate := left.EncodeStateAsUpdate()
	rightUpdate := right.EncodeStateAsUpdate()

	if err := crdt.ApplyUpdateV1(left, rightUpdate, "remote"); err != nil {
		t.Fatalf("apply remote to left: %v", err)
	}
	if err := crdt.ApplyUpdateV1(right, leftUpdate, "remote"); err != nil {
		t.Fatalf("apply remote to right: %v", err)
	}

	if leftText.ToString() != rightText.ToString() {
		t.Fatalf("documents diverged: %q vs %q", leftText.ToString(), rightText.ToString())
	}
}

func TestStoreConvergesThreePeerOutOfOrderDuplicateUpdates(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "abcdef\n")
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}

	peerA, err := decodeCRDTState(document.CRDTState, 11)
	if err != nil {
		t.Fatalf("decode peer a: %v", err)
	}
	textA := peerA.GetText("content")
	updateA := captureDocUpdate(t, peerA, "peer-a", func(txn *crdt.Transaction) {
		textA.Insert(txn, 0, "A:", nil)
	})

	peerB, err := decodeCRDTState(document.CRDTState, 22)
	if err != nil {
		t.Fatalf("decode peer b: %v", err)
	}
	textB := peerB.GetText("content")
	updateB := captureDocUpdate(t, peerB, "peer-b", func(txn *crdt.Transaction) {
		textB.Delete(txn, 2, 2)
		textB.Insert(txn, 2, "XY", nil)
	})

	peerC, err := decodeCRDTState(document.CRDTState, 33)
	if err != nil {
		t.Fatalf("decode peer c: %v", err)
	}
	textC := peerC.GetText("content")
	updateC := captureDocUpdate(t, peerC, "peer-c", func(txn *crdt.Transaction) {
		textC.Insert(txn, textC.Len(), "Z\n", nil)
	})

	expectedDoc, err := decodeCRDTState(document.CRDTState, 99)
	if err != nil {
		t.Fatalf("decode expected doc: %v", err)
	}
	for _, update := range [][]byte{updateB, updateA, updateC, updateC, updateA} {
		if err := crdt.ApplyUpdateV1(expectedDoc, update, "expected"); err != nil {
			t.Fatalf("apply expected update: %v", err)
		}
	}
	expected := expectedDoc.GetText("content").ToString()

	for _, update := range [][]byte{updateC, updateA, updateB, updateA, updateC} {
		if _, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
			ActorID:   "peer",
			ActorType: "agent",
			Source:    "test",
		}); err != nil {
			t.Fatalf("apply store update: %v", err)
		}
	}
	updated, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get updated document: %v", err)
	}
	if updated.Content != expected {
		t.Fatalf("store diverged after out-of-order duplicate updates: got %q want %q", updated.Content, expected)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reloaded, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()
	reloadedDocument, err := reloaded.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get reloaded document: %v", err)
	}
	if reloadedDocument.Content != expected {
		t.Fatalf("reloaded store diverged: got %q want %q", reloadedDocument.Content, expected)
	}
}

func TestStoreAppliesFrontendStyleUpdates(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# notty\n\n")

	frontendDoc := crdt.New(crdt.WithClientID(77))
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	initial, err := decodeCRDTState(document.CRDTState, 77)
	if err != nil {
		t.Fatalf("decode initial state: %v", err)
	}
	frontendDoc = initial

	var update []byte
	text := frontendDoc.GetText("content")
	unsubscribe := frontendDoc.OnUpdate(func(next []byte, origin any) {
		update = append([]byte(nil), next...)
	})
	frontendDoc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), "CRDT", nil)
	}, "browser")
	unsubscribe()

	result, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
		ActorID:   "web_user",
		ActorType: "human",
		Source:    "ui",
	})
	if err != nil {
		t.Fatalf("apply update: %v", err)
	}
	if got := result.Content; got[len(got)-4:] != "CRDT" {
		t.Fatalf("unexpected content tail: %q", got)
	}
}

func TestStoreAppliesManySmallFrontendUpdatesExactlyOnce(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/typing.md", "")

	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	frontendDoc, err := decodeCRDTState(document.CRDTState, 77)
	if err != nil {
		t.Fatalf("decode frontend state: %v", err)
	}
	text := frontendDoc.GetText("content")
	var expected strings.Builder

	for i := 0; i < 200; i++ {
		chunk := fmt.Sprintf("%03d\n", i)
		update := captureDocUpdate(t, frontendDoc, "browser", func(txn *crdt.Transaction) {
			text.Insert(txn, text.Len(), chunk, nil)
		})
		expected.WriteString(chunk)
		result, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
			ActorID:   "web_user",
			ActorType: "human",
			Source:    "ui",
		})
		if err != nil {
			t.Fatalf("apply update %d: %v", i, err)
		}
		if result.Content != expected.String() {
			t.Fatalf("content diverged after update %d: got %q want %q", i, result.Content, expected.String())
		}
	}

	reloaded, err := NewStore(store.dataFile)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()
	reloadedDocument, err := reloaded.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get reloaded document: %v", err)
	}
	if reloadedDocument.Content != expected.String() {
		t.Fatalf("reloaded content diverged: got %q want %q", reloadedDocument.Content, expected.String())
	}
}

func TestDiffDocumentSupportsExplicitVersionsAndReload(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "one\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	initial, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get initial document: %v", err)
	}
	v1 := initial.UpdateID
	if _, _, err := store.ReplaceDocumentText(documentID, "one\ntwo\n", OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("second version: %v", err)
	}
	second, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get second document: %v", err)
	}
	v2 := second.UpdateID
	if _, _, err := store.ReplaceDocumentText(documentID, "zero\none\ntwo\n", OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("third version: %v", err)
	}
	third, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get third document: %v", err)
	}
	v3 := third.UpdateID

	diff, err := store.DiffDocument(agent.ID, documentID, strconv.FormatInt(v1, 10), strconv.FormatInt(v2, 10))
	if err != nil {
		t.Fatalf("diff explicit versions: %v", err)
	}
	if diff.FromContent != "one\n" || diff.ToContent != "one\ntwo\n" || !strings.Contains(diff.Unified, "+two\n") {
		t.Fatalf("unexpected explicit diff: %#v", diff)
	}

	if _, err := store.MarkDocumentViewed(agent.ID, documentID, MarkDocumentViewedRequest{UpdateID: v2}); err != nil {
		t.Fatalf("mark viewed: %v", err)
	}
	viewedDiff, err := store.DiffDocument(agent.ID, documentID, "last-viewed", "head")
	if err != nil {
		t.Fatalf("diff viewed to head: %v", err)
	}
	if viewedDiff.FromUpdateID != v2 || viewedDiff.ToUpdateID != v3 || !strings.Contains(viewedDiff.Unified, "+zero\n") {
		t.Fatalf("unexpected viewed diff: %#v", viewedDiff)
	}

	if _, err := store.DiffDocument(agent.ID, documentID, strconv.FormatInt(v3, 10), strconv.FormatInt(v2, 10)); err == nil {
		t.Fatal("expected newer-from version to fail")
	}
	if _, err := store.DiffDocument(agent.ID, documentID, "not-a-version", "head"); err == nil {
		t.Fatal("expected invalid version to fail")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reloaded, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()
	reloadedDiff, err := reloaded.DiffDocument(agent.ID, documentID, strconv.FormatInt(v2, 10), strconv.FormatInt(v3, 10))
	if err != nil {
		t.Fatalf("diff after reload: %v", err)
	}
	if reloadedDiff.FromContent != "one\ntwo\n" || reloadedDiff.ToContent != "zero\none\ntwo\n" {
		t.Fatalf("unexpected reloaded diff: %#v", reloadedDiff)
	}
}

func assertSharedAgentPrompt(t *testing.T, prompt, name, handle, role string) {
	t.Helper()
	for _, fragment := range []string{
		name,
		"@" + handle,
		role,
		"Your file changes sync to other peers through the shared workspace promptly",
		"Prefer direct edits to existing files when possible.",
		"triggered by direct mentions, document edits, or thread messages",
		"do not need to reply by default",
		"If you have comments about a specific part of a document, reply in the existing thread anchored there or create a new thread anchored to that document range.",
		"If you want help or input from other collaborators, mention them in the thread with their @handle.",
		"Respect other collaborators because this is a shared workspace.",
		"If you have doubts or are uncertain about a change, it is often better to ask for others' input in a thread before making the change.",
		"It is important to consult others' opinions before making edits, and preferably have everyone aligned in a thread before making substantial changes.",
		"Whenever possible, reuse an existing thread instead of opening a new one if the existing thread is already well aligned with the topic.",
		"If you are directly mentioned, you must reply with the thread tools.",
	} {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("expected system prompt to contain %q, got %q", fragment, prompt)
		}
	}
}

func TestProposalMergeRewritesDocumentThroughCRDT(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# draft\n")
	proposal, err := store.CreateProposal(CreateProposalRequest{
		DocumentID:   documentID,
		Author:       "refactor_agent",
		Title:        "Rewrite spec heading",
		ProposedText: "proposal body",
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	document, update, err := store.MergeProposal(proposal.ID, "reviewer")
	if err != nil {
		t.Fatalf("merge proposal: %v", err)
	}
	if document.Content != "proposal body" {
		t.Fatalf("unexpected merged content: %q", document.Content)
	}
	if len(update) == 0 {
		t.Fatal("expected incremental update bytes")
	}
}

func TestDocumentLifecycleOperations(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	created, err := store.CreateDocument(CreateDocumentRequest{
		Path:    "notes/weekly/roadmap.md",
		Content: "# roadmap\n",
	}, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	if created.Path != "notes/weekly/roadmap.md" {
		t.Fatalf("unexpected created path: %q", created.Path)
	}
	if created.Title != "Roadmap" {
		t.Fatalf("unexpected created title: %q", created.Title)
	}

	moved, oldPath, err := store.MoveDocument(created.ID, "notes/archive/roadmap-final.md", OperationMeta{
		ActorID:   "web_user",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("move document: %v", err)
	}
	if oldPath != "notes/weekly/roadmap.md" {
		t.Fatalf("unexpected old path: %q", oldPath)
	}
	if moved.Path != "notes/archive/roadmap-final.md" {
		t.Fatalf("unexpected moved path: %q", moved.Path)
	}
	if moved.Title != "Roadmap Final" {
		t.Fatalf("unexpected moved title: %q", moved.Title)
	}

	deleted, err := store.DeleteDocument(created.ID, OperationMeta{
		ActorID:   "web_user",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("delete document: %v", err)
	}
	if deleted.ID != created.ID {
		t.Fatalf("unexpected deleted id: %q", deleted.ID)
	}
	if _, err := store.GetDocument(created.ID); err != ErrNotFound {
		t.Fatalf("expected deleted document to be gone, got %v", err)
	}
}

func TestCreateDocumentRejectsInvalidPath(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	_, err = store.CreateDocument(CreateDocumentRequest{
		Path:    "../escape.md",
		Content: "nope",
	}, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err == nil {
		t.Fatal("expected invalid path error")
	}
}

func TestAgentRunLifecycle(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "codex-builder",
		Name:         "Codex Builder",
		Role:         "Implement requested changes",
		Kind:         "codex",
		SystemPrompt: "Stay inside the assigned working copy.",
	}, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	agent, run, err := store.StartAgentRun(StartAgentRunRequest{
		AgentID: agent.ID,
		Prompt:  "Update the API docs and run tests.",
	}, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("start agent run: %v", err)
	}
	if agent.Status != "queued" || run.Status != "queued" {
		t.Fatalf("unexpected queued state: agent=%s run=%s", agent.Status, run.Status)
	}
	assertSharedAgentPrompt(t, run.SystemPrompt, "Codex Builder", "codex-builder", "Implement requested changes")
	if run.AgentHandle != "codex-builder" {
		t.Fatalf("unexpected agent handle: %q", run.AgentHandle)
	}
	if run.WorkspaceRoot != "agents/"+agent.ID {
		t.Fatalf("unexpected workspace root: %q", run.WorkspaceRoot)
	}
	if run.WorkingDir != "." {
		t.Fatalf("unexpected working directory: %q", run.WorkingDir)
	}

	exitCode := 0
	updatedRun, updatedAgent, err := store.UpdateAgentRun(run.ID, UpdateAgentRunRequest{
		Status:          "running",
		ProcessID:       4242,
		LastHeartbeatAt: "2026-04-19T12:00:00Z",
		LastMessage:     "Scanning workspace",
		LogTail:         []string{"{\"type\":\"task.started\"}"},
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("update running run: %v", err)
	}
	if updatedRun.ProcessID != 4242 {
		t.Fatalf("unexpected pid: %d", updatedRun.ProcessID)
	}
	if updatedAgent.CurrentActivity != "Scanning workspace" {
		t.Fatalf("unexpected agent activity: %q", updatedAgent.CurrentActivity)
	}

	stoppedRun, err := store.StopAgentRun(run.ID, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("stop agent run: %v", err)
	}
	if stoppedRun.DesiredStatus != "stopped" {
		t.Fatalf("unexpected desired status: %q", stoppedRun.DesiredStatus)
	}

	completedRun, completedAgent, err := store.UpdateAgentRun(run.ID, UpdateAgentRunRequest{
		Status:      "completed",
		LastMessage: "Applied changes and finished",
		ExitCode:    &exitCode,
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("update completed run: %v", err)
	}
	if completedRun.Status != "completed" || completedRun.ExitCode != 0 {
		t.Fatalf("unexpected completed run: %#v", completedRun)
	}
	if completedAgent.Status != "completed" {
		t.Fatalf("unexpected completed agent status: %q", completedAgent.Status)
	}
}

func TestAgentCrudLifecycle(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	created, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "docs-agent",
		Name:         "Docs Agent",
		Role:         "Maintain documentation",
		Kind:         "codex",
		SystemPrompt: "Keep markdown tidy.",
	}, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if created.WorkspaceRoot != "agents/"+created.ID {
		t.Fatalf("unexpected workspace root: %q", created.WorkspaceRoot)
	}
	assertSharedAgentPrompt(t, created.SystemPrompt, "Docs Agent", "docs-agent", "Maintain documentation")
	if strings.Contains(created.SystemPrompt, "Keep markdown tidy.") {
		t.Fatalf("expected custom system prompt to be ignored, got %q", created.SystemPrompt)
	}

	updated, err := store.UpdateAgent(created.ID, UpdateAgentRequest{
		Handle:       "docs-steward",
		Name:         "Docs Steward",
		Role:         "Maintain docs and changelogs",
		SystemPrompt: "Prefer concise edits.",
	}, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("update agent: %v", err)
	}
	if updated.Name != "Docs Steward" || updated.Handle != "docs-steward" || updated.WorkspaceRoot != "agents/"+created.ID {
		t.Fatalf("unexpected updated agent: %#v", updated)
	}
	assertSharedAgentPrompt(t, updated.SystemPrompt, "Docs Steward", "docs-steward", "Maintain docs and changelogs")
	if strings.Contains(updated.SystemPrompt, "Prefer concise edits.") {
		t.Fatalf("expected custom system prompt to be ignored on update, got %q", updated.SystemPrompt)
	}

	deleted, err := store.DeleteAgent(created.ID, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if deleted.ID != created.ID {
		t.Fatalf("unexpected deleted agent: %#v", deleted)
	}
	if _, ok := store.Snapshot().Agents[created.ID]; ok {
		t.Fatal("expected deleted agent to be gone")
	}
}

func TestUpdateAgentSessionPersistsCodexThreadAndTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Review docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "web_user", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	updated, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{
		Status:          "working",
		CodexThreadID:   "thread_123",
		CurrentTurnID:   "turn_456",
		CurrentActivity: "Reviewing inbox",
	}, OperationMeta{ActorID: "daemon_agent", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	if updated.Status != "working" || updated.CodexThreadID != "thread_123" || updated.CurrentTurnID != "turn_456" {
		t.Fatalf("unexpected updated session: %#v", updated)
	}
	if updated.SessionID != "thread_123" || updated.CurrentRunID != "turn_456" {
		t.Fatalf("expected legacy mirrors for frontend compatibility, got %#v", updated)
	}

	if _, err := store.UpdateAgentSession(agent.ID, UpdateAgentSessionRequest{Status: "paused"}, OperationMeta{}); err == nil {
		t.Fatal("expected invalid agent session status to fail")
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	got := reloaded.Snapshot().Agents[agent.ID]
	if got == nil || got.CodexThreadID != "thread_123" || got.CurrentTurnID != "turn_456" || got.Status != "working" {
		t.Fatalf("expected session fields to persist, got %#v", got)
	}
}

func TestUserCrudAndMentionExtraction(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Initial draft.\n")

	user, err := store.CreateUser(CreateUserRequest{
		Name:   "Ada Lovelace",
		Handle: "ada",
		Role:   "Research lead",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.Handle != "ada" {
		t.Fatalf("unexpected handle: %q", user.Handle)
	}

	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "scribe",
		Name:         "Scribe Agent",
		Role:         "Document reviewer",
		Kind:         "codex",
		SystemPrompt: "Leave clear notes.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	_, _, err = store.ReplaceDocumentText(documentID, "Ping @ada and @scribe in the plan.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	snapshot := store.Snapshot()
	if len(snapshot.Mentions) != 2 {
		t.Fatalf("expected 2 mentions, got %d", len(snapshot.Mentions))
	}
	handles := []string{snapshot.Mentions[0].Handle, snapshot.Mentions[1].Handle}
	if !(containsString(handles, "ada") && containsString(handles, "scribe")) {
		t.Fatalf("unexpected mention handles: %#v", handles)
	}

	updated, err := store.UpdateUser(user.ID, UpdateUserRequest{
		Handle: "ada-team",
		Name:   "Ada Team",
		Role:   "Strategy",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("update user: %v", err)
	}
	if updated.Handle != "ada-team" {
		t.Fatalf("unexpected updated handle: %q", updated.Handle)
	}

	snapshot = store.Snapshot()
	if len(snapshot.Mentions) != 1 || snapshot.Mentions[0].Handle != agent.Handle {
		t.Fatalf("expected mention refresh after handle change, got %#v", snapshot.Mentions)
	}
}

func TestCreateThreadEnqueuesMentionedAgentEvent(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# notty\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "reviewer",
		Name:         "Reviewer Agent",
		Role:         "Reviews specs",
		Kind:         "codex",
		SystemPrompt: "Leave concise replies.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	thread, message, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Need review",
		Body:       "Please take a look @reviewer",
		Start:      0,
		End:        6,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.ID == "" || message.ID == "" {
		t.Fatal("expected thread and message ids")
	}
	if len(thread.Messages) != 1 {
		t.Fatalf("expected 1 thread message, got %d", len(thread.Messages))
	}

	snapshot := store.Snapshot()
	if len(snapshot.Threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(snapshot.Threads))
	}
	if len(snapshot.AgentEvents) != 1 {
		t.Fatalf("expected 1 agent event, got %d", len(snapshot.AgentEvents))
	}
	for _, event := range snapshot.AgentEvents {
		if event.AgentID != agent.ID || event.Type != "thread.mentioned" || event.ThreadID != thread.ID {
			t.Fatalf("unexpected agent event: %#v", event)
		}
	}
}

func TestLoadReconcilesMissingThreadMentionEvents(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "agent.log", "start\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews specs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	thread, message, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Need review",
		Body:       "Please take a look @reviewer",
		Start:      0,
		End:        6,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	store.mu.Lock()
	store.state.AgentEvents = map[string]*AgentEvent{}
	if err := store.persistLocked(); err != nil {
		store.mu.Unlock()
		t.Fatalf("persist store with missing agent event: %v", err)
	}
	store.mu.Unlock()

	reloaded, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	items, err := reloaded.ListAgentInbox(agent.ID, "for-me", "pending")
	if err != nil {
		t.Fatalf("list reloaded inbox: %v", err)
	}
	mention := findAgentEventByType(items, "thread.mentioned")
	if mention == nil || mention.ThreadID != thread.ID || mention.ThreadMessageID != message.ID {
		t.Fatalf("expected missing log thread mention to be reconciled on load, got %s", formatAgentEvents(items))
	}
}

func TestDocumentMentionEntersMentionedAgentForMeQueue(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Draft.\n")
	reviewer, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create reviewer: %v", err)
	}
	observer, err := store.CreateAgent(CreateAgentRequest{
		Handle: "observer",
		Name:   "Observer",
		Role:   "Watches general activity",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "Please review this @codex-agent.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	reviewerItems, err := store.ListAgentInbox(reviewer.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list reviewer for-me inbox: %v", err)
	}
	reviewerMention := findAgentEventByType(reviewerItems, "document.mentioned")
	if reviewerMention == nil || reviewerMention.DocumentID != documentID || reviewerMention.AnchorEnd <= reviewerMention.AnchorStart {
		t.Fatalf("expected document mention in reviewer for-me queue, got %s", formatAgentEvents(reviewerItems))
	}

	observerItems, err := store.ListAgentInbox(observer.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list observer for-me inbox: %v", err)
	}
	if observerMention := findAgentEventByType(observerItems, "document.mentioned"); observerMention != nil {
		t.Fatalf("expected observer not to receive document mention, got %#v", observerMention)
	}

	claimed, err := store.ClaimAgentEvent(ClaimAgentEventRequest{AgentID: reviewer.ID, ClaimedBy: "daemon"})
	if err != nil {
		t.Fatalf("claim reviewer event: %v", err)
	}
	if claimed.Type != "document.mentioned" || claimed.DocumentID != documentID || claimed.Status != "processing" {
		t.Fatalf("unexpected claimed document mention: %#v", claimed)
	}
}

func TestCreateDocumentQueuesInitialAgentMention(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	document, err := store.CreateDocument(CreateDocumentRequest{
		Path:    "docs/mentioned.md",
		Content: "Initial ask for @codex-agent.\n",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}

	items, err := store.ListAgentInbox(agent.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list agent inbox: %v", err)
	}
	mention := findAgentEventByType(items, "document.mentioned")
	if mention == nil || mention.DocumentID != document.ID {
		t.Fatalf("expected initial document mention in for-me queue, got %s", formatAgentEvents(items))
	}
}

func TestApplyCRDTUpdateQueuesHyphenatedDocumentMention(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Draft.\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	frontendDoc, err := decodeCRDTState(document.CRDTState, 404)
	if err != nil {
		t.Fatalf("decode frontend document: %v", err)
	}
	text := frontendDoc.GetText("content")
	update := captureDocUpdate(t, frontendDoc, "browser", func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), "Please review this @codex-agent.\n", nil)
	})

	result, err := store.ApplyCRDTUpdateWithResult(documentID, update, OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "ws",
	})
	if err != nil {
		t.Fatalf("apply crdt update: %v", err)
	}
	if !result.MentionsChanged {
		t.Fatal("expected CRDT document update to report changed mentions")
	}

	items, err := store.ListAgentInbox(agent.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list agent inbox: %v", err)
	}
	mention := findAgentEventByType(items, "document.mentioned")
	if mention == nil || mention.DocumentID != documentID || mention.AnchorEnd <= mention.AnchorStart {
		t.Fatalf("expected hyphenated document mention in for-me queue, got %s", formatAgentEvents(items))
	}
}

func TestDocumentMentionMoveDoesNotQueueAnotherDirectMention(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Ping @codex-agent.\n")
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	frontendDoc, err := decodeCRDTState(document.CRDTState, 505)
	if err != nil {
		t.Fatalf("decode frontend document: %v", err)
	}
	text := frontendDoc.GetText("content")
	update := captureDocUpdate(t, frontendDoc, "browser", func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "Intro. ", nil)
	})
	if _, err := store.ApplyCRDTUpdateWithResult(documentID, update, OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "ws",
	}); err != nil {
		t.Fatalf("apply crdt update: %v", err)
	}

	mentions := documentMentionsForDocument(store.Snapshot(), documentID)
	if len(mentions) != 1 || mentions[0].ID == "" || mentions[0].Start <= 5 {
		t.Fatalf("expected shifted durable mention overlay, got %#v", mentions)
	}

	var mentionEvents int
	for _, event := range store.Snapshot().AgentEvents {
		if event.AgentID == agent.ID && event.Type == "document.mentioned" {
			mentionEvents++
		}
	}
	if mentionEvents != 1 {
		t.Fatalf("expected shifted existing mention not to queue again, got %d document mention events", mentionEvents)
	}
}

func TestMultipleDocumentMentionsQueueSeparateEvents(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "First @codex-agent and later @codex-agent.\n")

	mentions := documentMentionsForDocument(store.Snapshot(), documentID)
	if len(mentions) != 2 || mentions[0].ID == mentions[1].ID {
		t.Fatalf("expected two distinct durable mention overlays, got %#v", mentions)
	}
	if got := countAgentEvents(store.Snapshot(), agent.ID, "document.mentioned"); got != 2 {
		t.Fatalf("expected one event per mention occurrence, got %d", got)
	}
}

func TestDeletingDocumentMentionMarksRecordInactive(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Ping @codex-agent.\n")

	if _, _, err := store.ReplaceDocumentText(documentID, "Ping nobody.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	snapshot := store.Snapshot()
	if mentions := documentMentionsForDocument(snapshot, documentID); len(mentions) != 0 {
		t.Fatalf("expected no active mention overlays after deletion, got %#v", mentions)
	}
	records := durableMentionsForDocument(snapshot, documentID)
	if len(records) != 1 || records[0].Status != "inactive" || records[0].DeletedAt.IsZero() {
		t.Fatalf("expected inactive durable mention record, got %#v", records)
	}
}

func TestRetypingDocumentMentionCreatesNewRecordAndNotification(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Ping @codex-agent.\n")
	firstMentions := documentMentionsForDocument(store.Snapshot(), documentID)
	if len(firstMentions) != 1 {
		t.Fatalf("expected initial mention overlay, got %#v", firstMentions)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "Ping nobody.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("remove mention: %v", err)
	}
	if _, _, err := store.ReplaceDocumentText(documentID, "Ping @codex-agent again.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("retype mention: %v", err)
	}

	secondMentions := documentMentionsForDocument(store.Snapshot(), documentID)
	if len(secondMentions) != 1 || secondMentions[0].ID == firstMentions[0].ID {
		t.Fatalf("expected retyped mention to get a fresh durable identity, before=%#v after=%#v", firstMentions, secondMentions)
	}
	if got := countAgentEvents(store.Snapshot(), agent.ID, "document.mentioned"); got != 2 {
		t.Fatalf("expected retyped mention to queue a new event, got %d", got)
	}
}

func TestDocumentMentionRecordsPersistAcrossReload(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Ping @codex-agent.\n")
	before := documentMentionsForDocument(store.Snapshot(), documentID)
	if len(before) != 1 || before[0].ID == "" {
		t.Fatalf("expected one durable mention before reload, got %#v", before)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reloaded, err := NewStore(dataFile)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	defer reloaded.Close()
	after := documentMentionsForDocument(reloaded.Snapshot(), documentID)
	if len(after) != 1 || after[0].ID != before[0].ID || after[0].Start != before[0].Start {
		t.Fatalf("expected durable mention identity to survive reload, before=%#v after=%#v", before, after)
	}
}

func TestExistingDocumentMentionClassifiesLaterUpdatesAsDocumentUpdated(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Ping @codex-agent.\n")
	claimed, err := store.ClaimAgentEvent(ClaimAgentEventRequest{AgentID: agent.ID, ClaimedBy: "daemon"})
	if err != nil {
		t.Fatalf("claim initial mention: %v", err)
	}
	if _, err := store.UpdateAgentEvent(claimed.ID, UpdateAgentEventRequest{Status: "completed"}, OperationMeta{
		ActorID:   agent.ID,
		ActorType: "agent",
		Source:    "test",
	}); err != nil {
		t.Fatalf("complete initial mention: %v", err)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "Intro.\nPing @codex-agent.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	items, err := store.ListAgentInbox(agent.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list agent inbox: %v", err)
	}
	item := findDocumentInboxItem(items, documentID, "for_me")
	if item == nil || item.Type != "document.updated" {
		t.Fatalf("expected later edit to be classified as document.updated, got %s", formatAgentEvents(items))
	}
}

func TestThreadMentionEntersMentionedAgentForMeQueue(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# spec\n")
	reviewer, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create reviewer: %v", err)
	}
	observer, err := store.CreateAgent(CreateAgentRequest{
		Handle: "observer",
		Name:   "Observer",
		Role:   "Watches general activity",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}

	thread, message, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Need review",
		Body:       "Can you check this @codex-agent?",
		Start:      0,
		End:        6,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	reviewerItems, err := store.ListAgentInbox(reviewer.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list reviewer for-me inbox: %v", err)
	}
	reviewerMention := findAgentEventByType(reviewerItems, "thread.mentioned")
	if reviewerMention == nil || reviewerMention.DocumentID != documentID || reviewerMention.ThreadID != thread.ID || reviewerMention.ThreadMessageID != message.ID {
		t.Fatalf("expected thread mention in reviewer for-me queue, got %s", formatAgentEvents(reviewerItems))
	}

	observerItems, err := store.ListAgentInbox(observer.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list observer for-me inbox: %v", err)
	}
	if observerMention := findAgentEventByType(observerItems, "thread.mentioned"); observerMention != nil {
		t.Fatalf("expected observer not to receive thread mention, got %#v", observerMention)
	}

	claimed, err := store.ClaimAgentEvent(ClaimAgentEventRequest{AgentID: reviewer.ID, ClaimedBy: "daemon"})
	if err != nil {
		t.Fatalf("claim reviewer event: %v", err)
	}
	if claimed.Type != "thread.mentioned" || claimed.ThreadID != thread.ID || claimed.ThreadMessageID != message.ID || claimed.Status != "processing" {
		t.Fatalf("unexpected claimed thread mention: %#v", claimed)
	}
}

func TestThreadReplyDirectMentionDoesNotAlsoQueueGenericReply(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# spec\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	thread, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Need review",
		Body:       "Initial ask @reviewer",
		Start:      0,
		End:        6,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, _, err := store.ReplyThread(thread.ID, ReplyThreadRequest{
		Body: "Follow-up direct ask @reviewer",
		Kind: "comment",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("reply thread: %v", err)
	}

	var mentioned, replied int
	for _, event := range store.Snapshot().AgentEvents {
		if event.AgentID != agent.ID {
			continue
		}
		switch event.Type {
		case "thread.mentioned":
			mentioned++
		case "thread.replied":
			replied++
		}
	}
	if mentioned != 2 || replied != 0 {
		t.Fatalf("expected only direct mention events, got thread.mentioned=%d thread.replied=%d", mentioned, replied)
	}
}

func TestThreadAnchorTracksRelativePositions(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha bravo charlie")
	thread, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Check bravo",
		Body:       "Anchor the middle word",
		Start:      6,
		End:        11,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.Anchor.RelativeStart == "" || thread.Anchor.RelativeEnd == "" {
		t.Fatalf("expected relative anchor positions, got %#v", thread.Anchor)
	}

	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	frontendDoc, err := decodeCRDTState(document.CRDTState, 88)
	if err != nil {
		t.Fatalf("decode document: %v", err)
	}
	text := frontendDoc.GetText("content")
	var update []byte
	unsubscribe := frontendDoc.OnUpdate(func(next []byte, origin any) {
		update = append([]byte(nil), next...)
	})
	frontendDoc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "wide ", nil)
	}, "browser")
	unsubscribe()

	if _, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("apply update: %v", err)
	}

	updated := store.Snapshot().Threads[thread.ID]
	if updated == nil {
		t.Fatal("expected thread in snapshot")
	}
	if updated.Anchor.Start != 11 || updated.Anchor.End != 16 {
		t.Fatalf("expected anchor to move with inserted text, got start=%d end=%d", updated.Anchor.Start, updated.Anchor.End)
	}
	if !strings.Contains(updated.Anchor.Excerpt, "bravo") {
		t.Fatalf("expected excerpt to follow anchored text, got %q", updated.Anchor.Excerpt)
	}
}

func TestStoreNotificationHelpersExposeAndUpdatePendingNotifications(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Draft.\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "scribe",
		Name:         "Scribe",
		Role:         "Tracks edits",
		Kind:         "codex",
		SystemPrompt: "Watch document changes.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, _, err := store.ReplaceDocumentText(documentID, "Please sync with @scribe.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	notifications, err := store.ListAgentNotifications(agent.ID, "pending")
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notifications) != 1 || notifications[0].Type != "document.mentioned" {
		t.Fatalf("unexpected notifications: %#v", notifications)
	}
	notification, err := store.GetAgentNotification(notifications[0].ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if notification.ID != notifications[0].ID {
		t.Fatalf("unexpected notification: %#v", notification)
	}
	updated, err := store.UpdateAgentNotification(notification.ID, UpdateAgentNotificationRequest{
		Status: "dismissed",
	}, OperationMeta{ActorID: agent.ID, ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("dismiss notification: %v", err)
	}
	if updated.Status != "dismissed" {
		t.Fatalf("unexpected updated notification: %#v", updated)
	}
	remaining, err := store.ListAgentNotifications(agent.ID, "pending")
	if err != nil {
		t.Fatalf("list notifications after dismiss: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected no pending notifications after dismiss, got %#v", remaining)
	}
}

func TestLogDocumentContentDoesNotGenerateAgentEventsButThreadMessagesDo(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	logDocumentID := mustCreateTestDocument(t, store, "agent.log", "start\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "scribe",
		Name:   "Scribe",
		Role:   "Reviews documents",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	if _, _, err := store.ReplaceDocumentText(logDocumentID, "start\n@scripte ignored typo\n@scribe should not notify\n", OperationMeta{
		ActorID:   "daemon",
		ActorType: "agent",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace log document: %v", err)
	}
	logDocument, err := store.GetDocument(logDocumentID)
	if err != nil {
		t.Fatalf("get log document: %v", err)
	}
	if !strings.Contains(logDocument.Content, "@scribe should not notify") || logDocument.UpdateID == 0 {
		t.Fatalf("expected log content to sync through CRDT, got %#v", logDocument)
	}

	for _, box := range []string{"for-me", "general"} {
		items, err := store.ListAgentInbox(agent.ID, box, "pending")
		if err != nil {
			t.Fatalf("list %s inbox after log content update: %v", box, err)
		}
		if len(items) != 0 {
			t.Fatalf("expected log content update to generate no %s inbox items, got %#v", box, items)
		}
	}

	thread, message, err := store.CreateThread(CreateThreadRequest{
		DocumentID: logDocumentID,
		Title:      "log thread",
		Body:       "Please inspect @scribe",
		Start:      0,
		End:        5,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create log thread: %v", err)
	}
	_, replyMessage, err := store.ReplyThread(thread.ID, ReplyThreadRequest{
		Body: "follow-up in log thread",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("reply log thread: %v", err)
	}

	notifications, err := store.ListAgentNotifications(agent.ID, "pending")
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notifications) != 2 {
		t.Fatalf("expected log thread messages to notify, got %#v", notifications)
	}

	forMe, err := store.ListAgentInbox(agent.ID, "for-me", "pending")
	if err != nil {
		t.Fatalf("list for-me inbox: %v", err)
	}
	mention := findAgentEventByType(forMe, "thread.mentioned")
	if mention == nil || mention.ThreadID != thread.ID || mention.ThreadMessageID != message.ID {
		t.Fatalf("expected log thread mention in for-me inbox, got %s", formatAgentEvents(forMe))
	}
	reply := findAgentEventByType(forMe, "thread.replied")
	if reply == nil || reply.ThreadID != thread.ID || reply.ThreadMessageID != replyMessage.ID {
		t.Fatalf("expected log thread reply in for-me inbox, got %s", formatAgentEvents(forMe))
	}

	general, err := store.ListAgentInbox(agent.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list general inbox: %v", err)
	}
	if len(general) != 0 {
		t.Fatalf("expected log thread messages to stay out of general inbox, got %#v", general)
	}
	if events := SortedAgentEvents(store.Snapshot()); len(events) != 2 {
		t.Fatalf("expected only log thread events in workspace events, got %s", formatAgentEvents(events))
	} else {
		for _, event := range events {
			if strings.HasPrefix(event.Type, "document.") {
				t.Fatalf("expected log document-derived events to stay filtered, got %s", formatAgentEvents(events))
			}
		}
	}
}

func TestAgentInboxRoutesDocumentUpdatesPerAgentParticipation(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha\n")
	reviewer, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create reviewer: %v", err)
	}
	observer, err := store.CreateAgent(CreateAgentRequest{
		Handle: "observer",
		Name:   "Observer",
		Role:   "Watches general changes",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}
	if _, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Reviewer context",
		Body:       "Please watch this area @reviewer",
		Start:      0,
		End:        5,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "alpha\nbeta\n", OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	reviewerForMe, err := store.ListAgentInbox(reviewer.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list reviewer for-me inbox: %v", err)
	}
	reviewerItem := findDocumentInboxItem(reviewerForMe, documentID, "for_me")
	if reviewerItem == nil || !strings.HasPrefix(reviewerItem.Type, "document.") || reviewerItem.ThreadID == "" {
		t.Fatalf("expected reviewer document update in for-me inbox with thread anchor, got %s", formatAgentEvents(reviewerForMe))
	}
	reviewerGeneral, err := store.ListAgentInbox(reviewer.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list reviewer general inbox: %v", err)
	}
	if item := findDocumentInboxItem(reviewerGeneral, documentID, "general"); item != nil {
		t.Fatalf("expected reviewer document update to stay out of general inbox, got %#v", item)
	}

	observerGeneral, err := store.ListAgentInbox(observer.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list observer general inbox: %v", err)
	}
	observerItem := findDocumentInboxItem(observerGeneral, documentID, "general")
	if observerItem == nil || observerItem.Type != "document.updated" {
		t.Fatalf("expected observer document update in general inbox, got %#v", observerGeneral)
	}
	observerForMe, err := store.ListAgentInbox(observer.ID, "for_me", "pending")
	if err != nil {
		t.Fatalf("list observer for-me inbox: %v", err)
	}
	if item := findDocumentInboxItem(observerForMe, documentID, "for_me"); item != nil {
		t.Fatalf("expected observer document update to stay out of for-me inbox, got %#v", item)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "alpha\nbeta\ngamma\n", OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("second edit: %v", err)
	}
	observerGeneralAgain, err := store.ListAgentInbox(observer.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list observer general inbox after second edit: %v", err)
	}
	observerItemAgain := findDocumentInboxItem(observerGeneralAgain, documentID, "general")
	if observerItemAgain == nil || observerItemAgain.ID != observerItem.ID || observerItemAgain.ToUpdateID <= observerItem.ToUpdateID {
		t.Fatalf("expected observer item to dedupe with advanced version: before=%#v after=%#v", observerItem, observerItemAgain)
	}
}

func TestAgentInboxDedupesDocumentUpdatesAndDiffsFromLastViewed(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "one\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	if items, err := store.ListAgentInbox(agent.ID, "general", "pending"); err != nil {
		t.Fatalf("list initial inbox: %v", err)
	} else if len(items) != 0 {
		t.Fatalf("expected new agent to start viewed at current documents, got %#v", items)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "one\ntwo\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	firstItems, err := store.ListAgentInbox(agent.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list inbox after first edit: %v", err)
	}
	if len(firstItems) != 1 || firstItems[0].Type != "document.updated" {
		t.Fatalf("unexpected first inbox: %#v", firstItems)
	}
	if firstItems[0].FromUpdateID == 0 || firstItems[0].ToUpdateID <= firstItems[0].FromUpdateID {
		t.Fatalf("expected version span, got %#v", firstItems[0])
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "one\ntwo\nthree\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("second edit: %v", err)
	}
	secondItems, err := store.ListAgentInbox(agent.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list inbox after second edit: %v", err)
	}
	if len(secondItems) != 1 {
		t.Fatalf("expected document edits to be deduped into one inbox item, got %#v", secondItems)
	}
	if secondItems[0].ID != firstItems[0].ID || secondItems[0].ToUpdateID <= firstItems[0].ToUpdateID {
		t.Fatalf("expected stable item id with advanced target version: first=%#v second=%#v", firstItems[0], secondItems[0])
	}

	diff, err := store.DiffDocument(agent.ID, documentID, "last-viewed", "head")
	if err != nil {
		t.Fatalf("diff document: %v", err)
	}
	if diff.FromUpdateID != secondItems[0].FromUpdateID || diff.ToUpdateID != secondItems[0].ToUpdateID {
		t.Fatalf("unexpected diff versions: diff=%#v item=%#v", diff, secondItems[0])
	}
	if !strings.Contains(diff.Unified, "+two\n") || !strings.Contains(diff.Unified, "+three\n") {
		t.Fatalf("expected unified diff to include inserted lines, got %q", diff.Unified)
	}

	if _, err := store.UpdateAgentInboxItem(secondItems[0].ID, UpdateAgentNotificationRequest{Status: "completed"}, OperationMeta{
		ActorID:   agent.ID,
		ActorType: "agent",
		Source:    "test",
	}); err != nil {
		t.Fatalf("complete inbox item: %v", err)
	}
	remaining, err := store.ListAgentInbox(agent.ID, "general", "pending")
	if err != nil {
		t.Fatalf("list inbox after complete: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected no pending document inbox after marking viewed, got %#v", remaining)
	}
}

func TestDocumentMentionEventClaimAndComplete(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Draft.\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "scribe",
		Name:         "Scribe",
		Role:         "Tracks edits",
		Kind:         "codex",
		SystemPrompt: "Watch document changes.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, _, err := store.ReplaceDocumentText(documentID, "Please sync with @scribe.\n", OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	claimed, err := store.ClaimAgentEvent(ClaimAgentEventRequest{
		AgentID:   agent.ID,
		ClaimedBy: "daemon",
	})
	if err != nil {
		t.Fatalf("claim agent event: %v", err)
	}
	if claimed.Type != "document.mentioned" || claimed.Status != "processing" {
		t.Fatalf("unexpected claimed event: %#v", claimed)
	}

	updated, err := store.UpdateAgentEvent(claimed.ID, UpdateAgentEventRequest{
		Status: "completed",
		RunID:  "run_demo",
	}, OperationMeta{ActorID: "daemon", ActorType: "agent", Source: "test"})
	if err != nil {
		t.Fatalf("update agent event: %v", err)
	}
	if updated.Status != "completed" || updated.RunID != "run_demo" || updated.CompletedAt.IsZero() {
		t.Fatalf("unexpected completed event: %#v", updated)
	}
}

func TestAgentSelfMentionDoesNotEnqueueDocumentEvent(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Draft.\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "scribe",
		Name:         "Scribe",
		Role:         "Tracks edits",
		Kind:         "codex",
		SystemPrompt: "Watch document changes.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "I updated this section myself @scribe.\n", OperationMeta{
		ActorID:   agent.Handle,
		ActorType: "agent",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	if got := len(store.Snapshot().AgentEvents); got != 0 {
		t.Fatalf("expected no agent event for self-mentioning edit, got %d", got)
	}
}

func TestAgentSelfReplyDoesNotEnqueueThreadEvent(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# spec\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "reviewer",
		Name:         "Reviewer",
		Role:         "Reviews specs",
		Kind:         "codex",
		SystemPrompt: "Review specs carefully.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	thread, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Need reviewer context",
		Body:       "Please review this @reviewer",
		Start:      0,
		End:        5,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if got := len(store.Snapshot().AgentEvents); got != 1 {
		t.Fatalf("expected initial mention event, got %d", got)
	}

	if _, _, err := store.ReplyThread(thread.ID, ReplyThreadRequest{
		Body: "Following up on my own thread @reviewer",
		Kind: "comment",
	}, OperationMeta{ActorID: agent.Handle, ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("reply thread: %v", err)
	}

	if got := len(store.Snapshot().AgentEvents); got != 1 {
		t.Fatalf("expected no extra event for self-authored reply, got %d", got)
	}
}

func TestAgentOwnDocumentEditDoesNotEnqueueDocumentEditedEvent(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "# spec\n")
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle:       "reviewer",
		Name:         "Reviewer",
		Role:         "Reviews specs",
		Kind:         "codex",
		SystemPrompt: "Review specs carefully.",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	thread, _, err := store.CreateThread(CreateThreadRequest{
		DocumentID: documentID,
		Title:      "Need reviewer context",
		Body:       "Please review this @reviewer",
		Start:      0,
		End:        5,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.ID == "" {
		t.Fatal("expected thread id")
	}
	if got := len(store.Snapshot().AgentEvents); got != 1 {
		t.Fatalf("expected initial mention event, got %d", got)
	}

	if _, _, err := store.ReplaceDocumentText(documentID, "# spec\nUpdated by reviewer.\n", OperationMeta{
		ActorID:   agent.Handle,
		ActorType: "agent",
		Source:    "test",
	}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}

	if got := len(store.Snapshot().AgentEvents); got != 1 {
		t.Fatalf("expected no document.edited event for self-authored edit, got %d", got)
	}
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func documentMentionsForDocument(state WorkspaceState, documentID string) []*Mention {
	mentions := make([]*Mention, 0)
	for _, mention := range state.Mentions {
		if mention != nil && mention.DocumentID == documentID {
			clone := *mention
			mentions = append(mentions, &clone)
		}
	}
	return mentions
}

func durableMentionsForDocument(state WorkspaceState, documentID string) []*DocumentMention {
	mentions := make([]*DocumentMention, 0)
	for _, mention := range state.DocumentMentions {
		if mention != nil && mention.DocumentID == documentID {
			clone := *mention
			mentions = append(mentions, &clone)
		}
	}
	return mentions
}

func countAgentEvents(state WorkspaceState, agentID string, eventType string) int {
	count := 0
	for _, event := range state.AgentEvents {
		if event != nil && event.AgentID == agentID && event.Type == eventType {
			count++
		}
	}
	return count
}

func mustCreateTestDocument(t *testing.T, store *Store, path string, content string) string {
	t.Helper()
	document, err := store.CreateDocument(CreateDocumentRequest{
		Path:    path,
		Content: content,
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create test document: %v", err)
	}
	return document.ID
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

func findDocumentInboxItem(items []*AgentEvent, documentID, box string) *AgentEvent {
	for _, item := range items {
		if item != nil && item.DocumentID == documentID && normalizeInboxBox(item.Box) == normalizeInboxBox(box) && strings.HasPrefix(item.Type, "document.") {
			return item
		}
	}
	return nil
}

func findAgentEventByType(items []*AgentEvent, eventType string) *AgentEvent {
	for _, item := range items {
		if item != nil && item.Type == eventType {
			return item
		}
	}
	return nil
}

func formatAgentEvents(items []*AgentEvent) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item == nil {
			parts = append(parts, "<nil>")
			continue
		}
		parts = append(parts, item.ID+" type="+item.Type+" box="+item.Box+" doc="+item.DocumentID+" anchor="+strconv.Itoa(item.AnchorStart)+"-"+strconv.Itoa(item.AnchorEnd))
	}
	return strings.Join(parts, "; ")
}
