package notty

import (
	"strings"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestRootSemanticMutationTargetsOnlyValidatedEntry(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()
	if _, err := upsertRootEntryForTest(doc, "doc-a", "docs/a.md"); err != nil {
		t.Fatalf("seed target entry: %v", err)
	}
	if _, err := upsertRootEntryForTest(doc, "doc-b", "docs/b.md"); err != nil {
		t.Fatalf("seed sibling entry: %v", err)
	}

	entry, err := rootFileEntryForCommand(doc, "doc-a", "doc-a", "docs/./a.md")
	if err != nil {
		t.Fatalf("validate target entry: %v", err)
	}
	if entry.EntryID != "doc-a" || entry.ContentDocumentID != "doc-a" || entry.DesiredPath != "docs/a.md" || entry.Deleted {
		t.Fatalf("validated entry = %#v", entry)
	}

	update, applied, err := setRootFileDeleted(doc, entry, true)
	if err != nil {
		t.Fatalf("tombstone target: %v", err)
	}
	if !applied || len(update) == 0 {
		t.Fatalf("first tombstone applied=%v update=%v", applied, update)
	}
	if path, deleted := rootEntryForDocumentForTest(t, doc, "doc-a"); path != "docs/a.md" || !deleted {
		t.Fatalf("target after tombstone = path %q deleted %v", path, deleted)
	}
	if path, deleted := rootEntryForDocumentForTest(t, doc, "doc-b"); path != "docs/b.md" || deleted {
		t.Fatalf("sibling after target tombstone = path %q deleted %v", path, deleted)
	}

	if retry, retryApplied, err := setRootFileDeleted(doc, entry, true); err != nil {
		t.Fatalf("repeat tombstone: %v", err)
	} else if retryApplied || len(retry) != 0 {
		t.Fatalf("repeat tombstone applied=%v update=%v, want semantic no-op", retryApplied, retry)
	}

	entry.Deleted = true
	if _, applied, err := setRootFileDeleted(doc, entry, false); err != nil {
		t.Fatalf("restore target: %v", err)
	} else if !applied {
		t.Fatal("restore was not applied")
	}
	if path, deleted := rootEntryForDocumentForTest(t, doc, "doc-a"); path != "docs/a.md" || deleted {
		t.Fatalf("target after restore = path %q deleted %v", path, deleted)
	}
}

func TestRootFileEntryForCommandRejectsSemanticMismatch(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()
	if _, err := upsertRootEntryForTest(doc, "doc-a", "docs/a.md"); err != nil {
		t.Fatalf("seed target entry: %v", err)
	}

	for _, tc := range []struct {
		name      string
		entryID   string
		document  string
		path      string
		wantError string
	}{
		{name: "missing entry", entryID: "missing", document: "doc-a", path: "docs/a.md", wantError: "entry"},
		{name: "wrong document", entryID: "doc-a", document: "doc-b", path: "docs/a.md", wantError: "content document"},
		{name: "wrong path", entryID: "doc-a", document: "doc-a", path: "docs/b.md", wantError: "path"},
		{name: "hidden path", entryID: "doc-a", document: "doc-a", path: ".notty/root", wantError: "visible"},
		{name: "escaping path", entryID: "doc-a", document: "doc-a", path: "../a.md", wantError: "workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rootFileEntryForCommand(doc, tc.entryID, tc.document, tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want text %q", err, tc.wantError)
			}
		})
	}
}

func TestRootFileEntryForCommandRejectsMalformedFileEntry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		field     string
		value     string
		wantError string
	}{
		{name: "non-file kind", field: "kind", value: "folder", wantError: "not a file"},
		{name: "missing content document", field: "contentDocumentId", value: "", wantError: "no content document"},
		{name: "malformed location", field: "loc", value: "{", wantError: "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := crdt.New()
			defer doc.Close()
			if _, err := upsertRootEntryForTest(doc, "doc-a", "docs/a.md"); err != nil {
				t.Fatalf("seed target entry: %v", err)
			}
			root := doc.GetMap(rootMapName)
			if _, err := doc.Update(func(txn *crdt.Transaction) error {
				entry, err := rootCommandEntryMap(txn, root, "doc-a")
				if err != nil {
					return err
				}
				return entry.SetString(txn, tc.field, tc.value)
			}, "malformed-root-entry-test"); err != nil {
				t.Fatalf("mutate target entry: %v", err)
			}
			_, err := rootFileEntryForCommand(doc, "doc-a", "doc-a", "docs/a.md")
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want text %q", err, tc.wantError)
			}
		})
	}
}

func TestRootPathClaimedByOtherActiveEntryNormalizesPaths(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()
	if _, err := upsertRootEntryForTest(doc, "doc-a", "docs/a.md"); err != nil {
		t.Fatalf("seed target entry: %v", err)
	}
	if _, err := upsertRootEntryForTest(doc, "doc-b", "docs/b.md"); err != nil {
		t.Fatalf("seed sibling entry: %v", err)
	}

	claimed, err := rootPathClaimedByOtherActiveEntry(doc, "doc-a", "docs/./b.md")
	if err != nil {
		t.Fatalf("find path claim: %v", err)
	}
	if !claimed {
		t.Fatal("normalized sibling path claim was not detected")
	}
	claimed, err = rootPathClaimedByOtherActiveEntry(doc, "doc-a", "docs/a.md")
	if err != nil {
		t.Fatalf("exclude target claim: %v", err)
	}
	if claimed {
		t.Fatal("target entry claimed its own path")
	}

	entry, err := rootFileEntryForCommand(doc, "doc-b", "doc-b", "docs/b.md")
	if err != nil {
		t.Fatalf("validate sibling: %v", err)
	}
	if _, _, err := setRootFileDeleted(doc, entry, true); err != nil {
		t.Fatalf("tombstone sibling: %v", err)
	}
	claimed, err = rootPathClaimedByOtherActiveEntry(doc, "doc-a", "docs/b.md")
	if err != nil {
		t.Fatalf("check tombstoned sibling: %v", err)
	}
	if claimed {
		t.Fatal("tombstoned sibling retained an active path claim")
	}
}
