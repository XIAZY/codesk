package notty

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	crdt "notty/internal/ycrdt"
)

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func numberedLines(count int) string {
	var builder strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&builder, "%d\n", i)
	}
	return builder.String()
}

func assertSharedAgentPrompt(t *testing.T, prompt, name, handle, role string) {
	t.Helper()
	for _, fragment := range []string{
		name,
		"@" + handle,
		role,
		"Your file changes sync to other peers through the shared workspace promptly",
		"Prefer direct edits to existing files when possible.",
		"notified by direct thread mentions, document edits, thread messages, or an inbox check",
		"Plain @handle text inside markdown documents is regular document text, not a notification",
		"do not need to reply by default",
		"If you have comments about a specific part of a document, reply in the existing thread anchored there or create a new thread anchored to that document range.",
		"If you want help or input from other collaborators, mention them in the thread with their @handle.",
		"Respect other collaborators because this is a shared workspace.",
		"If you have doubts or are uncertain about a change, it is often better to ask for others' input in a thread before making the change.",
		"It is important to consult others' opinions before making edits, and preferably have everyone aligned in a thread before making substantial changes.",
		"Whenever possible, reuse an existing thread instead of opening a new one if the existing thread is already well aligned with the topic.",
		"If you are directly mentioned in a thread, you must reply with the thread tools.",
	} {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("expected system prompt to contain %q, got %q", fragment, prompt)
		}
	}
}

func mustCreateTestDocument(t *testing.T, store *Store, _ string, content string) string {
	t.Helper()
	document, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create test document: %v", err)
	}
	if content != "" {
		if _, _, err := store.ReplaceDocumentText(document.ID, content, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test-seed-content"}); err != nil {
			t.Fatalf("seed test document content: %v", err)
		}
	}
	return document.ID
}

func syncedDocumentTextForTest(t *testing.T, store *Store, documentID string) string {
	t.Helper()
	doc := crdt.New()
	defer doc.Close()
	_, updates, err := store.EncodeDocumentSyncUpdates(documentID, nil)
	if err != nil {
		t.Fatalf("sync document %s: %v", documentID, err)
	}
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(doc, update, "test-sync"); err != nil {
			t.Fatalf("apply sync update for %s: %v", documentID, err)
		}
	}
	return doc.GetText("content").ToString()
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

func currentDocumentContentForTest(t *testing.T, store *Store, documentID string) string {
	t.Helper()
	document, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document %s: %v", documentID, err)
	}
	return documentContentForTest(t, store, document)
}

func documentContentForTest(t *testing.T, store *Store, document *Document) string {
	t.Helper()
	content, err := documentContentAtUpdatePostgres(store.db, store.Snapshot().WorkspaceID, document, document.UpdateID)
	if err != nil {
		t.Fatalf("materialize document %s at %d: %v", document.ID, document.UpdateID, err)
	}
	return content
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

func findUserForTest(items []*User, userID string) *User {
	for _, item := range items {
		if item != nil && item.ID == userID {
			return item
		}
	}
	return nil
}

func getUserForTest(db *sql.DB, workspaceID string, userID string) (*User, error) {
	row := db.QueryRow(
		`SELECT id::text, handle, name, role, kind, status, created_at, updated_at
		   FROM users
		  WHERE workspace_id = $1::uuid AND id = $2::uuid`,
		workspaceID,
		userID,
	)
	user := &User{}
	if err := row.Scan(&user.ID, &user.Handle, &user.Name, &user.Role, &user.Kind, &user.Status, &user.CreatedAt, &user.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}

func containsAgentForTest(items []*Agent, agentID string) bool {
	for _, item := range items {
		if item != nil && item.ID == agentID {
			return true
		}
	}
	return false
}

func containsAgentRunForTest(items []*AgentRun, runID string) bool {
	for _, item := range items {
		if item != nil && item.ID == runID {
			return true
		}
	}
	return false
}

func formatAgentEvents(items []*AgentEvent) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item == nil {
			parts = append(parts, "<nil>")
			continue
		}
		parts = append(parts, item.ID+" type="+item.Type+" box="+item.Box+" doc="+item.DocumentID)
	}
	return strings.Join(parts, "; ")
}
