package notty

import (
	"strings"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestResolveMaterializedPathsDuplicateSiblingNamesAreStable(t *testing.T) {
	manifest := NewRootManifest()
	manifest.EntriesByID["doc_b"] = RootEntry{
		ID:              "doc_b",
		Kind:            RootEntryKindFile,
		Loc:             NewRootLocation(RootEntryID, "README.md"),
		ContentStreamID: "doc_b",
	}
	manifest.EntriesByID["doc_a"] = RootEntry{
		ID:              "doc_a",
		Kind:            RootEntryKindFile,
		Loc:             NewRootLocation(RootEntryID, "README.md"),
		ContentStreamID: "doc_a",
	}

	projection := ResolveMaterializedPaths(manifest)
	if got := projection.EntryPath["doc_a"]; got != "README.md" {
		t.Fatalf("lexically first entry should keep desired path, got %q", got)
	}
	if got := projection.EntryPath["doc_b"]; got != "README (conflict doc_b).md" {
		t.Fatalf("duplicate path should get stable projection-only conflict suffix, got %q", got)
	}
	if got := manifest.EntriesByID["doc_b"].Loc.Name; got != "README.md" {
		t.Fatalf("resolver must not write conflict suffix back into root, got loc name %q", got)
	}
}

func TestResolveMaterializedPathsSkipsTombstonesAndRecoversOrphans(t *testing.T) {
	manifest := NewRootManifest()
	manifest.EntriesByID["deleted"] = RootEntry{
		ID:              "deleted",
		Kind:            RootEntryKindFile,
		Loc:             NewRootLocation(RootEntryID, "deleted.md"),
		ContentStreamID: "deleted",
		Tombstone:       &RootTombstone{ActorID: "actor", ActorType: "human", At: "2026-05-23T00:00:00Z"},
	}
	manifest.EntriesByID["orphan"] = RootEntry{
		ID:              "orphan",
		Kind:            RootEntryKindFile,
		Loc:             NewRootLocation("missing_parent", "notes.md"),
		ContentStreamID: "orphan",
	}

	projection := ResolveMaterializedPaths(manifest)
	if _, ok := projection.EntryPath["deleted"]; ok {
		t.Fatalf("tombstoned entry materialized unexpectedly: %#v", projection.EntryPath)
	}
	if got := projection.EntryPath["orphan"]; got != "Recovered/orphans/orphan/notes.md" {
		t.Fatalf("orphan should materialize under recovered namespace, got %q", got)
	}
	if !projection.Orphaned["orphan"] {
		t.Fatalf("orphan marker missing: %#v", projection.Orphaned)
	}
}

func TestResolveMaterializedPathsCyclesDoNotCrash(t *testing.T) {
	manifest := NewRootManifest()
	manifest.EntriesByID["dir_a"] = RootEntry{
		ID:   "dir_a",
		Kind: RootEntryKindDir,
		Loc:  NewRootLocation("dir_b", "a"),
	}
	manifest.EntriesByID["dir_b"] = RootEntry{
		ID:   "dir_b",
		Kind: RootEntryKindDir,
		Loc:  NewRootLocation("dir_a", "b"),
	}

	projection := ResolveMaterializedPaths(manifest)
	if got := projection.EntryPath["dir_a"]; got != "Recovered/orphans/dir_a/a" {
		t.Fatalf("cycle member dir_a should be recovered, got %q", got)
	}
	if got := projection.EntryPath["dir_b"]; got != "Recovered/orphans/dir_b/b" {
		t.Fatalf("cycle member dir_b should be recovered, got %q", got)
	}
}

func TestResolveMaterializedPathsDirectoryRenameUpdatesDescendants(t *testing.T) {
	manifest := NewRootManifest()
	manifest.EntriesByID["dir_docs"] = RootEntry{
		ID:   "dir_docs",
		Kind: RootEntryKindDir,
		Loc:  NewRootLocation(RootEntryID, "docs"),
	}
	manifest.EntriesByID["doc_spec"] = RootEntry{
		ID:              "doc_spec",
		Kind:            RootEntryKindFile,
		Loc:             NewRootLocation("dir_docs", "spec.md"),
		ContentStreamID: "doc_spec",
	}
	if got := ResolveMaterializedPaths(manifest).EntryPath["doc_spec"]; got != "docs/spec.md" {
		t.Fatalf("expected initial descendant path, got %q", got)
	}

	dir := manifest.EntriesByID["dir_docs"]
	dir.Loc = NewRootLocation(RootEntryID, "reference")
	manifest.EntriesByID["dir_docs"] = dir
	if got := ResolveMaterializedPaths(manifest).EntryPath["doc_spec"]; got != "reference/spec.md" {
		t.Fatalf("directory rename should update descendant path, got %q", got)
	}
}

func TestValidateRootManifestAllowsDuplicatePathsRejectsImmutableChanges(t *testing.T) {
	previous := NewRootManifest()
	previous.EntriesByID["doc_a"] = RootEntry{
		ID:              "doc_a",
		Kind:            RootEntryKindFile,
		Loc:             NewRootLocation(RootEntryID, "same.md"),
		ContentStreamID: "doc_a",
	}
	next := NewRootManifest()
	next.EntriesByID["doc_a"] = previous.EntriesByID["doc_a"]
	next.EntriesByID["doc_b"] = RootEntry{
		ID:              "doc_b",
		Kind:            RootEntryKindFile,
		Loc:             NewRootLocation(RootEntryID, "same.md"),
		ContentStreamID: "doc_b",
	}
	if err := ValidateRootManifest(previous, next); err != nil {
		t.Fatalf("duplicate desired paths should validate: %v", err)
	}

	mutated := cloneRootManifest(next)
	entry := mutated.EntriesByID["doc_a"]
	entry.ContentStreamID = "other"
	mutated.EntriesByID["doc_a"] = entry
	if err := ValidateRootManifest(next, mutated); err == nil {
		t.Fatal("expected contentStreamId mutation to be rejected")
	}

	tombstoned := cloneRootManifest(next)
	entry = tombstoned.EntriesByID["doc_a"]
	entry.Tombstone = &RootTombstone{ActorID: "actor", ActorType: "human", At: "2026-05-23T00:00:00Z"}
	tombstoned.EntriesByID["doc_a"] = entry
	revived := cloneRootManifest(tombstoned)
	entry = revived.EntriesByID["doc_a"]
	entry.Tombstone = nil
	revived.EntriesByID["doc_a"] = entry
	if err := ValidateRootManifest(tombstoned, revived); err == nil {
		t.Fatal("expected tombstone removal to be rejected")
	}
}

func TestApplyRootIntentsStoresFieldMapsWithoutLegacyWrites(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()
	if _, err := ApplyRootIntents(doc, []RootIntent{{
		Type: "create-file",
		Entry: RootEntry{
			ID:              "doc_1",
			Kind:            RootEntryKindFile,
			Loc:             NewRootLocation(RootEntryID, "spec.md"),
			ContentStreamID: "doc_1",
		},
	}}); err != nil {
		t.Fatalf("apply root intent: %v", err)
	}
	if got := strings.TrimSpace(doc.GetText(RootManifestTextName).ToString()); got != "" {
		t.Fatalf("legacy root manifest text should remain empty, got %q", got)
	}
	rawLegacy, err := doc.GetMap(RootManifestMapName).JSON()
	if err != nil {
		t.Fatalf("read entries map json: %v", err)
	}
	if strings.TrimSpace(rawLegacy) != "{}" {
		t.Fatalf("legacy entries map should stay empty, got %s", rawLegacy)
	}
	rawKind, err := doc.GetMap(RootManifestKindMapName).JSON()
	if err != nil {
		t.Fatalf("read kind field map json: %v", err)
	}
	if !strings.Contains(rawKind, `"doc_1":"file"`) {
		t.Fatalf("kind field map missing doc_1: %s", rawKind)
	}
	rawContent, err := doc.GetMap(RootManifestContentStreamMapName).JSON()
	if err != nil {
		t.Fatalf("read content stream field map json: %v", err)
	}
	if !strings.Contains(rawContent, `"doc_1":"doc_1"`) {
		t.Fatalf("content stream field map missing doc_1: %s", rawContent)
	}
	manifest, err := ReadRootManifest(doc)
	if err != nil {
		t.Fatalf("read root manifest: %v", err)
	}
	if manifest.EntriesByID["doc_1"].ContentStreamID != "doc_1" {
		t.Fatalf("unexpected manifest entry: %#v", manifest.EntriesByID["doc_1"])
	}
}
