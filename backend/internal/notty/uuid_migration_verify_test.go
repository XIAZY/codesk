package notty

import (
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestVerifyUUIDMigrationSnapshotsAcceptsMappedDocumentAndRootEntries(t *testing.T) {
	before := uuidMigrationTestSnapshot("ws_old", "doc_root_old", "doc_old", "hello")
	after := uuidMigrationTestSnapshot("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "hello")
	mappings := []UUIDMigrationMapping{
		{Entity: "workspace", OldID: "ws_old", NewID: "11111111-1111-4111-8111-111111111111"},
		{Entity: "document", OldID: "doc_root_old", NewID: "22222222-2222-4222-8222-222222222222"},
		{Entity: "document", OldID: "doc_old", NewID: "33333333-3333-4333-8333-333333333333"},
	}

	if issues := VerifyUUIDMigrationSnapshots(before, after, mappings); len(issues) != 0 {
		t.Fatalf("VerifyUUIDMigrationSnapshots issues = %#v, want none", issues)
	}
}

func TestVerifyUUIDMigrationSnapshotsCatchesContentAndRootDrift(t *testing.T) {
	before := uuidMigrationTestSnapshot("ws_old", "doc_root_old", "doc_old", "hello")
	after := uuidMigrationTestSnapshot("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "changed")
	after.RootDocuments[0].Entries[0].DesiredPath = "docs/renamed.md"
	after.RowCounts["documents"] = 3
	mappings := []UUIDMigrationMapping{
		{Entity: "workspace", OldID: "ws_old", NewID: "11111111-1111-4111-8111-111111111111"},
		{Entity: "document", OldID: "doc_root_old", NewID: "22222222-2222-4222-8222-222222222222"},
		{Entity: "document", OldID: "doc_old", NewID: "33333333-3333-4333-8333-333333333333"},
	}

	issues := VerifyUUIDMigrationSnapshots(before, after, mappings)
	if !uuidMigrationHasIssue(issues, "row_count_changed") {
		t.Fatalf("missing row_count_changed in %#v", issues)
	}
	if !uuidMigrationHasIssue(issues, "document_content_changed") {
		t.Fatalf("missing document_content_changed in %#v", issues)
	}
	if !uuidMigrationHasIssue(issues, "root_entry_changed") {
		t.Fatalf("missing root_entry_changed in %#v", issues)
	}
	after.RootDocuments[0].Entries[0].EntryID = "doc_old"
	issues = VerifyUUIDMigrationSnapshots(before, after, mappings)
	if !uuidMigrationHasIssue(issues, "root_entry_key_changed") {
		t.Fatalf("missing root_entry_key_changed in %#v", issues)
	}
}

func TestVerifyUUIDMigrationSnapshotsRequiresCompleteMappings(t *testing.T) {
	before := uuidMigrationTestSnapshot("ws_old", "doc_root_old", "doc_old", "hello")
	after := uuidMigrationTestSnapshot("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "hello")

	issues := VerifyUUIDMigrationSnapshots(before, after, []UUIDMigrationMapping{
		{Entity: "workspace", OldID: "ws_old", NewID: "11111111-1111-4111-8111-111111111111"},
	})
	if !uuidMigrationHasIssue(issues, "missing_mapping") {
		t.Fatalf("missing mapping issue in %#v", issues)
	}
	if !uuidMigrationHasIssue(issues, "root_entry_mapping_missing") {
		t.Fatalf("missing root entry mapping issue in %#v", issues)
	}
}

func TestDecodeUUIDMigrationRootEntries(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()
	root := doc.GetMap("root")
	if _, err := doc.Update(func(txn *crdt.Transaction) error {
		entries, err := root.SetMap(txn, "entriesById")
		if err != nil {
			return err
		}
		entry, err := entries.SetMap(txn, "doc_1")
		if err != nil {
			return err
		}
		if err := entry.SetString(txn, "kind", "file"); err != nil {
			return err
		}
		if err := entry.SetString(txn, "contentDocumentId", "doc_1"); err != nil {
			return err
		}
		if err := entry.SetString(txn, "loc", `{"parentId":"docs","name":"spec.md"}`); err != nil {
			return err
		}
		return entry.SetString(txn, "deleted", "false")
	}); err != nil {
		t.Fatalf("seed root doc: %v", err)
	}

	entries, err := decodeUUIDMigrationRootEntries(doc)
	if err != nil {
		t.Fatalf("decode root entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1: %#v", len(entries), entries)
	}
	if got := entries[0]; got.EntryID != "doc_1" || got.ContentDocumentID != "doc_1" || got.DesiredPath != "docs/spec.md" || got.Deleted {
		t.Fatalf("decoded root entry = %#v", got)
	}
}

func uuidMigrationTestSnapshot(workspaceID, rootDocumentID, contentDocumentID, content string) *UUIDMigrationSnapshot {
	return &UUIDMigrationSnapshot{
		Version: UUIDMigrationSnapshotVersion,
		RowCounts: map[string]int64{
			"workspaces":       1,
			"documents":        2,
			"document_heads":   2,
			"document_updates": 2,
		},
		EntityIDs: map[string][]string{
			"workspace": {workspaceID},
			"document":  {rootDocumentID, contentDocumentID},
		},
		Documents: []UUIDMigrationDocumentSnapshot{
			{WorkspaceID: workspaceID, DocumentID: rootDocumentID, Path: legacyRootDocumentPath, Hidden: true, ContentHash: uuidMigrationContentHash("")},
			{WorkspaceID: workspaceID, DocumentID: contentDocumentID, Path: "", ContentHash: uuidMigrationContentHash(content)},
		},
		RootDocuments: []UUIDMigrationRootDocumentSnapshot{{
			WorkspaceID: workspaceID,
			DocumentID:  rootDocumentID,
			Entries: []UUIDMigrationRootEntrySnapshot{{
				EntryID:           contentDocumentID,
				ContentDocumentID: contentDocumentID,
				DesiredPath:       "docs/spec.md",
			}},
		}},
	}
}

func uuidMigrationHasIssue(issues []UUIDMigrationIssue, kind string) bool {
	for _, issue := range issues {
		if issue.Kind == kind {
			return true
		}
	}
	return false
}
