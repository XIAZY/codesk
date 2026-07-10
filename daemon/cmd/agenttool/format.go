package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CLI-layer rendering for the whole agent-tool surface (task #2 ergonomics). The gateway and backend keep
// returning JSON — goldens, contract tests, and any programmatic caller are untouched — and this binary
// renders that JSON into labeled blocks / headers an LLM reads without parsing JSON shape. One format
// language across every command: ids always visible, headers that state what happened, timestamps trailing.

// countNoun returns the noun with grammatical number: "1 item", "2 items".
func countNoun(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// --- single inbox item: get-inbox-item / complete-inbox-item / dismiss-inbox-item ---

// singleInboxItemResponse accepts both the current {item} shape and the legacy {notification} alias.
type singleInboxItemResponse struct {
	Item         *inboxItem `json:"item"`
	Notification *inboxItem `json:"notification"`
}

func (r singleInboxItemResponse) item() *inboxItem {
	if r.Item != nil {
		return r.Item
	}
	return r.Notification
}

// formatInboxItem renders one item as a labeled block (with box + status). header is "" for get-inbox-item
// (block alone) or a verb like "marked completed" for the mutating commands, which prefixes a state line.
func formatInboxItem(data []byte, header string) (string, error) {
	var resp singleInboxItemResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	item := resp.item()
	if item == nil {
		return "", fmt.Errorf("response contained no item")
	}
	var b strings.Builder
	if header != "" {
		b.WriteString(header + ": " + item.ID + "\n")
	}
	writeInboxItemBlock(&b, *item, true)
	return b.String(), nil
}

// --- document subscriptions: subscribe-document / unsubscribe-document / list-subscriptions ---

type subscribedDocumentLine struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type subscriptionsResponse struct {
	Documents []subscribedDocumentLine `json:"documents"`
}

// documentRef renders a document as "<path> (id: <id>)", or just "<id>" when the path is unknown (a
// deleted/renamed doc the projection no longer has).
func documentRef(path, id string) string {
	if strings.TrimSpace(path) != "" {
		return path + " (id: " + id + ")"
	}
	return id
}

// formatSubscriptions renders the post-change subscription list. verb is "" for list-subscriptions (no
// header) or "subscribed to" / "unsubscribed from" for the mutating commands, whose header names the
// document just changed (targetID); its path is resolved from the returned list when still present.
func formatSubscriptions(data []byte, verb, targetID string) (string, error) {
	var resp subscriptionsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	var b strings.Builder
	if verb != "" {
		path := ""
		for _, doc := range resp.Documents {
			if doc.ID == targetID {
				path = doc.Path
				break
			}
		}
		b.WriteString(verb + " " + documentRef(path, targetID) + "\n")
	}
	n := len(resp.Documents)
	b.WriteString(fmt.Sprintf("you are subscribed to %d %s", n, countNoun(n, "document")))
	if n == 0 {
		b.WriteString("\n")
		return b.String(), nil
	}
	b.WriteString(":\n")
	for _, doc := range resp.Documents {
		b.WriteString("- " + documentRef(doc.Path, doc.ID) + "\n")
	}
	return b.String(), nil
}

// --- diff-document ---

type diffResponse struct {
	Diff *struct {
		DocumentID   string `json:"documentId"`
		FromUpdateID int64  `json:"fromUpdateId"`
		ToUpdateID   int64  `json:"toUpdateId"`
		Unified      string `json:"unified"`
	} `json:"diff"`
}

func formatDiff(data []byte) (string, error) {
	var resp diffResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if resp.Diff == nil {
		return "", fmt.Errorf("response contained no diff")
	}
	header := fmt.Sprintf("document %s changed from version %d to %d\n", resp.Diff.DocumentID, resp.Diff.FromUpdateID, resp.Diff.ToUpdateID)
	unified := resp.Diff.Unified
	if strings.TrimSpace(unified) == "" {
		return header + "(no textual changes)\n", nil
	}
	if !strings.HasSuffix(unified, "\n") {
		unified += "\n"
	}
	return header + unified, nil
}

// --- mark-document-viewed ---

type viewResponse struct {
	View *struct {
		DocumentID string `json:"documentId"`
		UpdateID   int64  `json:"updateId"`
	} `json:"view"`
}

func formatMarkViewed(data []byte) (string, error) {
	var resp viewResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if resp.View == nil {
		return "", fmt.Errorf("response contained no view")
	}
	return fmt.Sprintf("marked document %s viewed at version %d\n", resp.View.DocumentID, resp.View.UpdateID), nil
}

// --- list-documents / get-document-by-path ---

type documentLine struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	UpdateID int64  `json:"updateId"`
	Content  string `json:"content"`
}

func (d documentLine) metadata() string {
	return fmt.Sprintf("%s (id: %s, version: %d)", d.Path, d.ID, d.UpdateID)
}

type documentsResponse struct {
	Documents []documentLine `json:"documents"`
}

func formatDocuments(data []byte) (string, error) {
	var resp documentsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	var b strings.Builder
	n := len(resp.Documents)
	b.WriteString(fmt.Sprintf("you have %d %s", n, countNoun(n, "document")))
	if n == 0 {
		b.WriteString("\n")
		return b.String(), nil
	}
	b.WriteString(":\n")
	for _, doc := range resp.Documents {
		b.WriteString("- " + doc.metadata() + "\n")
	}
	return b.String(), nil
}

type documentResponse struct {
	Document *documentLine `json:"document"`
}

func formatDocument(data []byte) (string, error) {
	var resp documentResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if resp.Document == nil {
		return "", fmt.Errorf("response contained no document")
	}
	out := resp.Document.metadata() + "\n"
	// Content is not part of the projection today (the document type carries only id/path/version); the
	// separator + body appear only if a future response starts including content.
	if strings.TrimSpace(resp.Document.Content) != "" {
		out += "---\n" + resp.Document.Content
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
	}
	return out, nil
}

// --- threads: get-thread / list-threads-for-document / create-thread / reply-thread ---

type threadMessageLine struct {
	AuthorHandle string `json:"authorHandle"`
	AuthorName   string `json:"authorName"`
	Body         string `json:"body"`
	CreatedAt    string `json:"createdAt"`
}

type threadObject struct {
	ID         string               `json:"id"`
	DocumentID string               `json:"documentId"`
	Title      string               `json:"title"`
	Status     string               `json:"status"`
	Messages   []*threadMessageLine `json:"messages"`
}

func writeThreadBlock(b *strings.Builder, t *threadObject) {
	b.WriteString("- id: " + t.ID + "\n")
	if t.Title != "" {
		b.WriteString("  title: " + t.Title + "\n")
	}
	b.WriteString("  status: " + t.Status + "\n")
	if t.DocumentID != "" {
		b.WriteString("  documentId: " + t.DocumentID + "\n")
	}
	if len(t.Messages) == 0 {
		return
	}
	b.WriteString("  messages:\n")
	for _, m := range t.Messages {
		if m == nil {
			continue
		}
		author := m.AuthorHandle
		if author == "" {
			author = m.AuthorName
		}
		b.WriteString("    @" + author + " (" + m.CreatedAt + "): " + m.Body + "\n")
	}
}

type singleThreadResponse struct {
	Thread *threadObject `json:"thread"`
}

func formatThread(data []byte) (string, error) {
	var resp singleThreadResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if resp.Thread == nil {
		return "", fmt.Errorf("response contained no thread")
	}
	var b strings.Builder
	writeThreadBlock(&b, resp.Thread)
	return b.String(), nil
}

type threadsResponse struct {
	Threads []*threadObject `json:"threads"`
}

func formatThreads(data []byte) (string, error) {
	var resp threadsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	var b strings.Builder
	n := len(resp.Threads)
	b.WriteString(fmt.Sprintf("you have %d %s", n, countNoun(n, "thread")))
	if n == 0 {
		b.WriteString("\n")
		return b.String(), nil
	}
	b.WriteString(":\n")
	for _, t := range resp.Threads {
		if t == nil {
			continue
		}
		writeThreadBlock(&b, t)
	}
	return b.String(), nil
}

type threadMutationResponse struct {
	Thread   *threadObject `json:"thread"`
	Queued   bool          `json:"queued"`
	IntentID string        `json:"intentId"`
}

// formatThreadMutation renders create-thread / reply-thread. verb is "thread created" or "reply posted to".
// A queued mutation (the document has not synced yet) prints the intent instead of a thread block.
func formatThreadMutation(data []byte, verb string) (string, error) {
	var resp threadMutationResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if resp.Queued || resp.Thread == nil {
		return fmt.Sprintf("queued (intent %s) — posts when the document syncs\n", resp.IntentID), nil
	}
	var b strings.Builder
	b.WriteString(verb + " " + resp.Thread.ID + "\n")
	writeThreadBlock(&b, resp.Thread)
	return b.String(), nil
}
