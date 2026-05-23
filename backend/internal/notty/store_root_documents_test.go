package notty

import (
	"strings"
	"testing"
)

func TestDocumentMutationsMirrorToRootAndContentStreams(t *testing.T) {
	store, err := NewStore(postgresTestDSN(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := clearNottyTables(store.db); err != nil {
		t.Fatalf("clear postgres tables: %v", err)
	}
	if err := store.Reload(); err != nil {
		t.Fatalf("reload clean store: %v", err)
	}

	first, err := store.CreateDocument(CreateDocumentRequest{Path: "docs/spec.md", Content: "alpha"}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create first document: %v", err)
	}
	second, err := store.CreateDocument(CreateDocumentRequest{Path: "docs/spec.md", Content: "bravo"}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create duplicate desired path document: %v", err)
	}

	streamDocs, err := store.ListStreamDocumentMetadata()
	if err != nil {
		t.Fatalf("list stream documents: %v", err)
	}
	if len(streamDocs) != 2 {
		t.Fatalf("expected two stream documents, got %#v", streamDocs)
	}
	paths := map[string]string{}
	desired := map[string]string{}
	for _, doc := range streamDocs {
		paths[doc.ID] = doc.Path
		desired[doc.ID] = doc.DesiredPath
	}
	if desired[first.ID] != "docs/spec.md" || desired[second.ID] != "docs/spec.md" {
		t.Fatalf("duplicate desired paths should be preserved, got %#v", desired)
	}
	if paths[first.ID] == "" || paths[second.ID] == "" || paths[first.ID] == paths[second.ID] {
		t.Fatalf("materialized paths should be non-empty and unique, got %#v", paths)
	}
	if !strings.Contains(paths[first.ID]+paths[second.ID], "conflict") {
		t.Fatalf("expected one materialized conflict path, got %#v", paths)
	}

	doc, _, err := store.RestoreStreamDoc(first.ID)
	if err != nil {
		t.Fatalf("restore first content stream: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("unexpected first content stream text %q", got)
	}

	moved, _, err := store.MoveDocument(first.ID, "docs/renamed.md", OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("move document: %v", err)
	}
	if moved.Path != "docs/renamed.md" {
		t.Fatalf("legacy moved path mismatch: %#v", moved)
	}
	byPath, err := store.GetStreamDocumentMetadataByPath("docs/renamed.md")
	if err != nil {
		t.Fatalf("stream by moved path: %v", err)
	}
	if byPath.ID != first.ID || byPath.DesiredPath != "docs/renamed.md" {
		t.Fatalf("stream projection did not reflect move: %#v", byPath)
	}

	if _, err := store.DeleteDocument(first.ID, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("delete document: %v", err)
	}
	streamDocs, err = store.ListStreamDocumentMetadata()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(streamDocs) != 1 || streamDocs[0].ID != second.ID {
		t.Fatalf("expected tombstoned document to disappear from projection, got %#v", streamDocs)
	}
}

func TestDocumentCreateMirrorsRootLogPath(t *testing.T) {
	store, err := NewStore(postgresTestDSN(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := clearNottyTables(store.db); err != nil {
		t.Fatalf("clear postgres tables: %v", err)
	}
	if err := store.Reload(); err != nil {
		t.Fatalf("reload clean store: %v", err)
	}

	created, err := store.CreateDocument(CreateDocumentRequest{Path: "codex-agent.log", Content: "line one\nline two\n"}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create log document: %v", err)
	}
	if created.Path != "codex-agent.log" {
		t.Fatalf("created document path mismatch: %#v", created)
	}

	byPath, err := store.GetStreamDocumentMetadataByPath("codex-agent.log")
	if err != nil {
		t.Fatalf("stream by log path: %v", err)
	}
	if byPath.ID != created.ID || byPath.Path != "codex-agent.log" || byPath.DesiredPath != "codex-agent.log" {
		t.Fatalf("unexpected stream projection for log path: %#v", byPath)
	}
}
