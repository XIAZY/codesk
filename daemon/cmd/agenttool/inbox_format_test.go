package main

import (
	"strings"
	"testing"
)

func TestFormatInboxAllBoxesLabeledShape(t *testing.T) {
	data := []byte(`{"items":[
		{"id":"a1","type":"thread.mentioned","box":"for_me","documentId":"d1","threadId":"t1","summary":"mentioned in intro","prompt":"you were mentioned","createdAt":"2026-07-10T02:40:00Z","updatedAt":"2026-07-10T02:41:00Z"},
		{"id":"g1","type":"document.updated","box":"general","documentId":"d2","summary":"spec.md changed 1 to 2","prompt":"review spec.md","createdAt":"2026-07-10T02:42:00Z","updatedAt":"2026-07-10T02:42:00Z"},
		{"id":"docinbox:muted:x","type":"document.updated","box":"muted","documentId":"d3","summary":"hello.md changed","prompt":"review hello.md","createdAt":"2026-07-10T02:43:00Z","updatedAt":"2026-07-10T02:43:00Z"}
	]}`)
	out, err := formatInbox(data, "")
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	// Per-box headers (proper singular/plural), all three, in order.
	for _, want := range []string{
		"you have 1 item in the for-me inbox:",
		"you have 1 item in the general inbox:",
		"you have 1 item in the muted inbox:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Labeled fields for the for-me item, including the conditional threadId and details<-prompt.
	for _, want := range []string{
		"- id: a1",
		"  type: thread.mentioned",
		"  documentId: d1",
		"  threadId: t1",
		"  summary: mentioned in intro",
		"  details: you were mentioned",
		"  created: 2026-07-10T02:40:00Z  updated: 2026-07-10T02:41:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// The trailer is AlphaToad's sentence, pointing at the two actions + --help.
	for _, want := range []string{
		"complete-inbox-item --item-id <id>",
		"mark-document-viewed --document-id <id>",
		"notty-agent-tool --help",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing trailer fragment %q in:\n%s", want, out)
		}
	}
	// Box order: for-me before general before muted.
	fi, gi, mi := strings.Index(out, "for-me inbox"), strings.Index(out, "general inbox"), strings.Index(out, "muted inbox")
	if !(fi >= 0 && fi < gi && gi < mi) {
		t.Fatalf("box order wrong (for-me=%d general=%d muted=%d):\n%s", fi, gi, mi, out)
	}
}

func TestFormatInboxSingleBoxAndEmpty(t *testing.T) {
	// --box general with no items → only the general section, the empty-box line, no bullets.
	out, err := formatInbox([]byte(`{"items":[]}`), "general")
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.Contains(out, "you have 0 items in the general inbox") {
		t.Fatalf("empty-box line missing in:\n%s", out)
	}
	if strings.Contains(out, "for-me inbox") || strings.Contains(out, "muted inbox") {
		t.Fatalf("--box general must print only the general section:\n%s", out)
	}
	if strings.Contains(out, "- id:") {
		t.Fatalf("an empty box must have no item bullets:\n%s", out)
	}
}
