package main

import (
	"strings"
	"testing"
)

func TestFormatInboxItemSingleBlockWithHeader(t *testing.T) {
	data := []byte(`{"item":{"id":"9157","type":"document.updated","box":"general","status":"completed","documentId":"5a49","summary":"hello.md changed from version 18310 to 18401","prompt":"Review hello.md","createdAt":"2026-07-10T02:40:00Z","updatedAt":"2026-07-10T02:41:00Z"}}`)

	// get-inbox-item (no header): the block alone, with box + status lines.
	block, err := formatInboxItem(data, "")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	for _, want := range []string{"- id: 9157", "  type: document.updated", "  box: general", "  status: completed", "  documentId: 5a49", "  details: Review hello.md"} {
		if !strings.Contains(block, want) {
			t.Fatalf("missing %q in:\n%s", want, block)
		}
	}
	if strings.HasPrefix(block, "marked") {
		t.Fatalf("get-inbox-item must have no header:\n%s", block)
	}

	// complete-inbox-item: a state header precedes the same block.
	withHeader, err := formatInboxItem(data, "marked completed")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.HasPrefix(withHeader, "marked completed: 9157\n") {
		t.Fatalf("missing confirmation header:\n%s", withHeader)
	}
}

func TestFormatInboxItemAcceptsLegacyNotificationKey(t *testing.T) {
	data := []byte(`{"notification":{"id":"n1","type":"thread.mentioned","box":"for_me","status":"pending"}}`)
	out, err := formatInboxItem(data, "")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.Contains(out, "- id: n1") || !strings.Contains(out, "  box: for-me") {
		t.Fatalf("legacy {notification} not rendered:\n%s", out)
	}
}

func TestFormatSubscriptionsHeaderListAndIDOnly(t *testing.T) {
	data := []byte(`{"documents":[{"id":"5a49","path":"specs/api.md"},{"id":"gone","path":""}]}`)

	// subscribe: header names the target (path resolved from the list), then the full list; the pathless
	// doc degrades to an id-only line.
	out, err := formatSubscriptions(data, subscribeVerb, "5a49")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	for _, want := range []string{
		"subscribed to specs/api.md (id: 5a49)\n",
		// Subscribe confirmation copy (task #6): the notification contract is stated at opt-in.
		"you will now receive notifications about new edits and thread messages on this document\n",
		"you are subscribed to 2 documents:",
		"- specs/api.md (id: 5a49)",
		"- gone\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gone (id:") {
		t.Fatalf("pathless doc must render id-only:\n%s", out)
	}

	// list-subscriptions: no header, and no subscribe-confirmation copy (it belongs to the opt-in only).
	list, err := formatSubscriptions(data, "", "")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if strings.HasPrefix(list, "subscribed to") || strings.HasPrefix(list, "unsubscribed") {
		t.Fatalf("list-subscriptions must have no header:\n%s", list)
	}
	if strings.Contains(list, "you will now receive notifications") {
		t.Fatalf("list must not carry the subscribe confirmation copy:\n%s", list)
	}

	// zero case.
	empty, err := formatSubscriptions([]byte(`{"documents":[]}`), "", "")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if strings.TrimSpace(empty) != "you are subscribed to 0 documents" {
		t.Fatalf("zero case wrong: %q", empty)
	}
}

func TestFormatDiffHeaderThenUnified(t *testing.T) {
	data := []byte(`{"diff":{"documentId":"5a49","fromUpdateId":18310,"toUpdateId":18401,"unified":"+this is a test document\n"}}`)
	out, err := formatDiff(data)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if out != "document 5a49 changed from version 18310 to 18401\n+this is a test document\n" {
		t.Fatalf("unexpected diff render:\n%q", out)
	}
	// empty diff → explicit no-change note, still with the header.
	none, err := formatDiff([]byte(`{"diff":{"documentId":"5a49","fromUpdateId":1,"toUpdateId":1,"unified":""}}`))
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.Contains(none, "(no textual changes)") {
		t.Fatalf("empty diff note missing:\n%s", none)
	}
}

func TestFormatMarkViewedOneLine(t *testing.T) {
	out, err := formatMarkViewed([]byte(`{"view":{"documentId":"5a49","updateId":18401}}`))
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if out != "marked document 5a49 viewed at version 18401\n" {
		t.Fatalf("unexpected: %q", out)
	}
}

func TestFormatDocumentsListAndSingle(t *testing.T) {
	list, err := formatDocuments([]byte(`{"documents":[{"id":"5a49","path":"hello.md","updateId":18401},{"id":"7bc0","path":"notes/todo.md","updateId":3}]}`))
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	for _, want := range []string{"you have 2 documents:", "- hello.md (id: 5a49, version: 18401)", "- notes/todo.md (id: 7bc0, version: 3)"} {
		if !strings.Contains(list, want) {
			t.Fatalf("missing %q in:\n%s", want, list)
		}
	}

	one, err := formatDocument([]byte(`{"document":{"id":"5a49","path":"hello.md","updateId":18401}}`))
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if strings.TrimSpace(one) != "hello.md (id: 5a49, version: 18401)" {
		t.Fatalf("unexpected single-doc render: %q", one)
	}
	// content, when a response carries it, follows a separator.
	withContent, err := formatDocument([]byte(`{"document":{"id":"5a49","path":"hello.md","updateId":18401,"content":"# Title\n"}}`))
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.Contains(withContent, "---\n# Title") {
		t.Fatalf("content separator missing:\n%s", withContent)
	}
}

func TestFormatThreadAndMutation(t *testing.T) {
	threadJSON := `{"thread":{"id":"t1","documentId":"5a49","title":"Review intro","status":"open","messages":[{"authorHandle":"alphatoad","body":"please review","createdAt":"2026-07-10T02:40:00Z"}]}}`
	out, err := formatThread([]byte(threadJSON))
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	for _, want := range []string{"- id: t1", "  title: Review intro", "  status: open", "  documentId: 5a49", "  messages:", "    @alphatoad (2026-07-10T02:40:00Z): please review"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// create-thread confirmation header + block.
	created, err := formatThreadMutation([]byte(`{"thread":{"id":"t9","status":"open"}}`), "thread created:")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.HasPrefix(created, "thread created: t9\n") {
		t.Fatalf("missing create header:\n%s", created)
	}

	// queued mutation names the intent instead of a thread block.
	queued, err := formatThreadMutation([]byte(`{"queued":true,"intentId":"intent_42"}`), "thread created:")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.Contains(queued, "queued (intent intent_42) — posts when the document syncs") {
		t.Fatalf("queued render wrong:\n%s", queued)
	}
}

func TestFormatThreadsEmpty(t *testing.T) {
	out, err := formatThreads([]byte(`{"threads":[]}`))
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if strings.TrimSpace(out) != "you have 0 threads" {
		t.Fatalf("unexpected empty render: %q", out)
	}
}
