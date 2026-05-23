package rootmanifest

import (
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestApplyIntentsConcurrentCreateDoesNotOverwriteRename(t *testing.T) {
	base := crdt.New(crdt.WithGUID("root"))
	defer base.Close()
	if _, err := ApplyIntents(base, []Intent{{
		Type: "create-file",
		Entry: Entry{
			ID:              "doc_a",
			Kind:            EntryKindFile,
			Loc:             NewLocation(RootEntryID, "old.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	baseUpdate := base.EncodeStateAsUpdate()

	renamer := crdt.New(crdt.WithGUID("root"))
	defer renamer.Close()
	if err := crdt.ApplyUpdateV1(renamer, baseUpdate, "base"); err != nil {
		t.Fatalf("load rename replica: %v", err)
	}
	renameUpdate, err := ApplyIntents(renamer, []Intent{{
		Type:    "loc",
		EntryID: "doc_a",
		Loc:     NewLocation(RootEntryID, "new.md"),
	}})
	if err != nil {
		t.Fatalf("rename entry: %v", err)
	}

	creator := crdt.New(crdt.WithGUID("root"))
	defer creator.Close()
	if err := crdt.ApplyUpdateV1(creator, baseUpdate, "base"); err != nil {
		t.Fatalf("load create replica: %v", err)
	}
	createUpdate, err := ApplyIntents(creator, []Intent{{
		Type: "create-file",
		Entry: Entry{
			ID:              "doc_b",
			Kind:            EntryKindFile,
			Loc:             NewLocation(RootEntryID, "local.md"),
			ContentStreamID: "doc_b",
		},
	}})
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}

	merged := crdt.New(crdt.WithGUID("root"))
	defer merged.Close()
	for _, update := range [][]byte{baseUpdate, createUpdate, renameUpdate} {
		if err := crdt.ApplyUpdateV1(merged, update, "merge"); err != nil {
			t.Fatalf("merge update: %v", err)
		}
	}
	manifest, err := Read(merged)
	if err != nil {
		t.Fatalf("read merged manifest: %v", err)
	}
	if got := manifest.EntriesByID["doc_a"].Loc.Name; got != "new.md" {
		t.Fatalf("concurrent create overwrote rename: doc_a path %q", got)
	}
	if _, ok := manifest.EntriesByID["doc_b"]; !ok {
		t.Fatalf("concurrent create missing from merged manifest: %#v", manifest.EntriesByID)
	}
}

func TestApplyIntentsConcurrentCreateDoesNotOverwriteTombstone(t *testing.T) {
	base := crdt.New(crdt.WithGUID("root"))
	defer base.Close()
	if _, err := ApplyIntents(base, []Intent{{
		Type: "create-file",
		Entry: Entry{
			ID:              "doc_a",
			Kind:            EntryKindFile,
			Loc:             NewLocation(RootEntryID, "old.md"),
			ContentStreamID: "doc_a",
		},
	}}); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	baseUpdate := base.EncodeStateAsUpdate()

	deleter := crdt.New(crdt.WithGUID("root"))
	defer deleter.Close()
	if err := crdt.ApplyUpdateV1(deleter, baseUpdate, "base"); err != nil {
		t.Fatalf("load delete replica: %v", err)
	}
	deleteUpdate, err := ApplyIntents(deleter, []Intent{{
		Type:      "tombstone",
		EntryID:   "doc_a",
		Tombstone: &Tombstone{ActorID: "peer", ActorType: "daemon", At: "2026-05-23T00:00:00Z"},
	}})
	if err != nil {
		t.Fatalf("tombstone entry: %v", err)
	}

	creator := crdt.New(crdt.WithGUID("root"))
	defer creator.Close()
	if err := crdt.ApplyUpdateV1(creator, baseUpdate, "base"); err != nil {
		t.Fatalf("load create replica: %v", err)
	}
	createUpdate, err := ApplyIntents(creator, []Intent{{
		Type: "create-file",
		Entry: Entry{
			ID:              "doc_b",
			Kind:            EntryKindFile,
			Loc:             NewLocation(RootEntryID, "local.md"),
			ContentStreamID: "doc_b",
		},
	}})
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}

	merged := crdt.New(crdt.WithGUID("root"))
	defer merged.Close()
	for _, update := range [][]byte{baseUpdate, createUpdate, deleteUpdate} {
		if err := crdt.ApplyUpdateV1(merged, update, "merge"); err != nil {
			t.Fatalf("merge update: %v", err)
		}
	}
	manifest, err := Read(merged)
	if err != nil {
		t.Fatalf("read merged manifest: %v", err)
	}
	if manifest.EntriesByID["doc_a"].Tombstone == nil {
		t.Fatalf("concurrent create resurrected tombstone: %#v", manifest.EntriesByID["doc_a"])
	}
	if _, ok := manifest.EntriesByID["doc_b"]; !ok {
		t.Fatalf("concurrent create missing from merged manifest: %#v", manifest.EntriesByID)
	}
}
