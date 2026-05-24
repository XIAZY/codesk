package rootmanifest

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	crdt "notty/internal/ycrdt"
)

func TestValidateRejectsHardDeletedEntry(t *testing.T) {
	previous := New()
	previous.EntriesByID["doc_a"] = Entry{
		ID:              "doc_a",
		Kind:            EntryKindFile,
		Loc:             NewLocation(RootEntryID, "a.md"),
		ContentStreamID: "doc_a",
	}
	next := New()

	err := Validate(previous, next)
	if err == nil {
		t.Fatal("expected hard-deleted entry to be rejected")
	}
	if !strings.Contains(err.Error(), "cannot be removed") {
		t.Fatalf("expected removal error, got %v", err)
	}

	tombstoned := Clone(previous)
	entry := tombstoned.EntriesByID["doc_a"]
	entry.Tombstone = &Tombstone{ActorID: "tester", ActorType: "daemon", At: "2026-05-23T00:00:00Z"}
	tombstoned.EntriesByID["doc_a"] = entry
	if err := Validate(previous, tombstoned); err != nil {
		t.Fatalf("tombstone should remain valid: %v", err)
	}
}

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

func TestApplyIntentsConcurrentRenameAndTombstonePreservesBothFacts(t *testing.T) {
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
	assertConcurrentRenameAndTombstonePreserved(t, baseUpdate)
}

func TestApplyIntentsLegacyBaseConcurrentRenameAndTombstonePreservesBothFacts(t *testing.T) {
	base := crdt.New(crdt.WithGUID("root"))
	defer base.Close()
	seedLegacyEntriesMap(t, base, map[string]Entry{
		RootEntryID: {
			ID:   RootEntryID,
			Kind: EntryKindDir,
			Loc:  nil,
		},
		"doc_a": {
			ID:              "doc_a",
			Kind:            EntryKindFile,
			Loc:             NewLocation(RootEntryID, "old.md"),
			ContentStreamID: "doc_a",
		},
	})
	baseUpdate := base.EncodeStateAsUpdate()
	assertConcurrentRenameAndTombstonePreserved(t, baseUpdate)
}

func TestReadOverlaysFieldMapsOnLegacyBase(t *testing.T) {
	doc := crdt.New(crdt.WithGUID("root"))
	defer doc.Close()
	seedLegacyEntriesMap(t, doc, map[string]Entry{
		RootEntryID: {
			ID:   RootEntryID,
			Kind: EntryKindDir,
			Loc:  nil,
		},
		"doc_a": {
			ID:              "doc_a",
			Kind:            EntryKindFile,
			Loc:             NewLocation(RootEntryID, "old.md"),
			ContentStreamID: "doc_a",
		},
	})
	locMap := doc.GetMap(LocMapName)
	_, err := doc.Update(func(txn *crdt.Transaction) error {
		payload, err := json.Marshal(NewLocation(RootEntryID, "new.md"))
		if err != nil {
			return err
		}
		return locMap.InsertJSON(txn, "doc_a", string(payload))
	}, "test")
	if err != nil {
		t.Fatalf("write loc overlay: %v", err)
	}
	manifest, err := ReadValidated(doc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	entry := manifest.EntriesByID["doc_a"]
	if entry.Loc == nil || entry.Loc.Name != "new.md" {
		t.Fatalf("expected loc overlay, got %#v", entry.Loc)
	}
	if entry.ContentStreamID != "doc_a" {
		t.Fatalf("expected contentStreamId from legacy base, got %#v", entry)
	}
}

func TestReadValidatedRejectsPartialFieldOnlyEntry(t *testing.T) {
	doc := crdt.New(crdt.WithGUID("root"))
	defer doc.Close()
	locMap := doc.GetMap(LocMapName)
	_, err := doc.Update(func(txn *crdt.Transaction) error {
		payload, err := json.Marshal(NewLocation(RootEntryID, "partial.md"))
		if err != nil {
			return err
		}
		return locMap.InsertJSON(txn, "doc_partial", string(payload))
	}, "test")
	if err != nil {
		t.Fatalf("write partial loc: %v", err)
	}
	manifest, err := Read(doc)
	if err != nil {
		t.Fatalf("parse partial manifest: %v", err)
	}
	if _, ok := manifest.EntriesByID["doc_partial"]; !ok {
		t.Fatalf("partial entry should remain visible for diagnostics: %#v", manifest.EntriesByID)
	}
	if _, err := ReadValidated(doc); err == nil {
		t.Fatal("expected validated read to reject missing kind")
	}
}

func TestApplyIntentsStoresFieldMapsWithoutLegacyWrites(t *testing.T) {
	doc := crdt.New()
	defer doc.Close()
	if _, err := ApplyIntents(doc, []Intent{{
		Type: "create-file",
		Entry: Entry{
			ID:              "doc_1",
			Kind:            EntryKindFile,
			Loc:             NewLocation(RootEntryID, "spec.md"),
			ContentStreamID: "doc_1",
		},
	}}); err != nil {
		t.Fatalf("apply root intent: %v", err)
	}
	rawLegacy, err := doc.GetMap(MapName).JSON()
	if err != nil {
		t.Fatalf("read legacy map: %v", err)
	}
	if strings.TrimSpace(rawLegacy) != "{}" {
		t.Fatalf("legacy entries map should stay empty, got %s", rawLegacy)
	}
	rawKind, err := doc.GetMap(KindMapName).JSON()
	if err != nil {
		t.Fatalf("read kind map: %v", err)
	}
	if !strings.Contains(rawKind, `"doc_1":"file"`) {
		t.Fatalf("kind field map missing doc_1: %s", rawKind)
	}
	rawContent, err := doc.GetMap(ContentStreamMapName).JSON()
	if err != nil {
		t.Fatalf("read content stream map: %v", err)
	}
	if !strings.Contains(rawContent, `"doc_1":"doc_1"`) {
		t.Fatalf("content stream field map missing doc_1: %s", rawContent)
	}
	manifest, err := ReadValidated(doc)
	if err != nil {
		t.Fatalf("read root manifest: %v", err)
	}
	if manifest.EntriesByID["doc_1"].ContentStreamID != "doc_1" {
		t.Fatalf("unexpected manifest entry: %#v", manifest.EntriesByID["doc_1"])
	}
}

func TestResolveSeededInvariants(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest Manifest
	}{
		{name: "duplicates", manifest: resolverSeedManifest("duplicates")},
		{name: "tombstone", manifest: resolverSeedManifest("tombstone")},
		{name: "orphan", manifest: resolverSeedManifest("orphan")},
		{name: "cycle", manifest: resolverSeedManifest("cycle")},
		{name: "nested", manifest: resolverSeedManifest("nested")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertResolveInvariants(t, tc.manifest)
		})
	}
}

func FuzzRootResolverInvariants(f *testing.F) {
	for _, seed := range []string{"duplicates", "tombstone", "orphan", "cycle", "nested", "abcdef"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed string) {
		assertResolveInvariants(t, resolverSeedManifest(seed))
	})
}

func assertConcurrentRenameAndTombstonePreserved(t *testing.T, baseUpdate []byte) {
	t.Helper()
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

	for _, updates := range [][][]byte{
		{baseUpdate, renameUpdate, deleteUpdate},
		{baseUpdate, deleteUpdate, renameUpdate},
	} {
		merged := crdt.New(crdt.WithGUID("root"))
		for _, update := range updates {
			if err := crdt.ApplyUpdateV1(merged, update, "merge"); err != nil {
				merged.Close()
				t.Fatalf("merge update: %v", err)
			}
		}
		manifest, err := ReadValidated(merged)
		if err != nil {
			merged.Close()
			t.Fatalf("read merged manifest: %v", err)
		}
		entry := manifest.EntriesByID["doc_a"]
		if entry.Loc == nil || entry.Loc.Name != "new.md" {
			merged.Close()
			t.Fatalf("concurrent tombstone overwrote rename: %#v", entry)
		}
		if entry.Tombstone == nil {
			merged.Close()
			t.Fatalf("concurrent rename resurrected tombstone: %#v", entry)
		}
		merged.Close()
	}
}

func assertResolveInvariants(t *testing.T, manifest Manifest) {
	t.Helper()
	projection := Resolve(Clone(manifest))
	signature := projectionSignature(projection)
	for i := 0; i < 10; i++ {
		if next := projectionSignature(Resolve(Clone(manifest))); next != signature {
			t.Fatalf("Resolve is not stable across runs:\nfirst=%s\n next=%s", signature, next)
		}
	}
	if _, ok := projection.EntryPath[RootEntryID]; ok {
		t.Fatalf("root should not be materialized as a child: %#v", projection.EntryPath)
	}
	paths := map[string]string{}
	for id, materialized := range projection.EntryPath {
		entry := manifest.EntriesByID[id]
		if entry.Tombstone != nil {
			t.Fatalf("tombstoned entry %q materialized at %q", id, materialized)
		}
		if owner, ok := paths[materialized]; ok {
			t.Fatalf("materialized path %q assigned to both %q and %q", materialized, owner, id)
		}
		paths[materialized] = id
	}
	reachable := reachableEntriesForInvariant(manifest)
	for id, entry := range manifest.EntriesByID {
		if id == RootEntryID || entry.Tombstone != nil {
			continue
		}
		materialized := projection.EntryPath[id]
		if strings.TrimSpace(materialized) == "" {
			t.Fatalf("live non-root entry %q has no materialized or recovery path", id)
		}
		if reachable[id] {
			if projection.Orphaned[id] {
				t.Fatalf("reachable entry %q incorrectly marked orphaned", id)
			}
			if entry.Loc != nil && entry.Loc.ParentID != RootEntryID {
				parentPath := projection.EntryPath[entry.Loc.ParentID]
				if parentPath != "" && !strings.HasPrefix(materialized, parentPath+"/") {
					t.Fatalf("entry %q path %q is not under parent path %q", id, materialized, parentPath)
				}
			}
		} else if !projection.Orphaned[id] {
			t.Fatalf("disconnected entry %q should be marked orphaned", id)
		}
	}
}

func reachableEntriesForInvariant(manifest Manifest) map[string]bool {
	live := map[string]Entry{}
	for id, entry := range manifest.EntriesByID {
		if entry.Tombstone == nil {
			live[id] = entry
		}
	}
	reachable := map[string]bool{}
	visiting := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if value, ok := reachable[id]; ok {
			return value
		}
		entry, ok := live[id]
		if !ok {
			reachable[id] = false
			return false
		}
		if id == RootEntryID {
			reachable[id] = entry.Kind == EntryKindDir && entry.Loc == nil
			return reachable[id]
		}
		if entry.Loc == nil || entry.Loc.ParentID == "" || visiting[id] {
			reachable[id] = false
			return false
		}
		visiting[id] = true
		parent, ok := live[entry.Loc.ParentID]
		value := ok && parent.Kind == EntryKindDir && visit(entry.Loc.ParentID)
		visiting[id] = false
		reachable[id] = value
		return value
	}
	for id := range live {
		visit(id)
	}
	return reachable
}

func projectionSignature(projection Projection) string {
	keys := make([]string, 0, len(projection.EntryPath))
	for id := range projection.EntryPath {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, id := range keys {
		builder.WriteString(id)
		builder.WriteByte('=')
		builder.WriteString(projection.EntryPath[id])
		if projection.Orphaned[id] {
			builder.WriteString(":orphan")
		}
		builder.WriteByte('\n')
	}
	return builder.String()
}

func resolverSeedManifest(seed string) Manifest {
	manifest := New()
	switch seed {
	case "duplicates":
		manifest.EntriesByID["doc_a"] = Entry{ID: "doc_a", Kind: EntryKindFile, Loc: NewLocation(RootEntryID, "same.md"), ContentStreamID: "doc_a"}
		manifest.EntriesByID["doc_b"] = Entry{ID: "doc_b", Kind: EntryKindFile, Loc: NewLocation(RootEntryID, "same.md"), ContentStreamID: "doc_b"}
	case "tombstone":
		manifest.EntriesByID["doc_live"] = Entry{ID: "doc_live", Kind: EntryKindFile, Loc: NewLocation(RootEntryID, "live.md"), ContentStreamID: "doc_live"}
		manifest.EntriesByID["doc_deleted"] = Entry{ID: "doc_deleted", Kind: EntryKindFile, Loc: NewLocation(RootEntryID, "deleted.md"), ContentStreamID: "doc_deleted", Tombstone: &Tombstone{ActorID: "actor", ActorType: "human", At: "2026-05-24T00:00:00Z"}}
	case "orphan":
		manifest.EntriesByID["doc_orphan"] = Entry{ID: "doc_orphan", Kind: EntryKindFile, Loc: NewLocation("missing_parent", "orphan.md"), ContentStreamID: "doc_orphan"}
	case "cycle":
		manifest.EntriesByID["dir_a"] = Entry{ID: "dir_a", Kind: EntryKindDir, Loc: NewLocation("dir_b", "a")}
		manifest.EntriesByID["dir_b"] = Entry{ID: "dir_b", Kind: EntryKindDir, Loc: NewLocation("dir_a", "b")}
	case "nested":
		manifest.EntriesByID["dir_docs"] = Entry{ID: "dir_docs", Kind: EntryKindDir, Loc: NewLocation(RootEntryID, "docs")}
		manifest.EntriesByID["doc_spec"] = Entry{ID: "doc_spec", Kind: EntryKindFile, Loc: NewLocation("dir_docs", "spec.md"), ContentStreamID: "doc_spec"}
	default:
		bytes := []byte(seed)
		if len(bytes) == 0 {
			bytes = []byte{0}
		}
		count := 1 + int(bytes[0])%8
		for i := 0; i < count; i++ {
			id := "entry_" + string(rune('a'+i))
			kind := EntryKindFile
			if bytes[i%len(bytes)]%3 == 0 {
				kind = EntryKindDir
			}
			parent := RootEntryID
			if i > 0 && bytes[(i+1)%len(bytes)]%4 != 0 {
				parent = "entry_" + string(rune('a'+int(bytes[(i+2)%len(bytes)])%count))
			}
			entry := Entry{ID: id, Kind: kind, Loc: NewLocation(parent, "name-"+id)}
			if kind == EntryKindFile {
				entry.ContentStreamID = id
			}
			if bytes[(i+3)%len(bytes)]%7 == 0 {
				entry.Tombstone = &Tombstone{ActorID: "actor", ActorType: "human", At: "2026-05-24T00:00:00Z"}
			}
			manifest.EntriesByID[id] = entry
		}
	}
	return manifest
}

func seedLegacyEntriesMap(t *testing.T, doc *crdt.Doc, entries map[string]Entry) {
	t.Helper()
	entriesMap := doc.GetMap(MapName)
	_, err := doc.Update(func(txn *crdt.Transaction) error {
		for id, entry := range entries {
			payload, err := json.Marshal(entry)
			if err != nil {
				return err
			}
			if err := entriesMap.InsertJSON(txn, id, string(payload)); err != nil {
				return err
			}
		}
		return nil
	}, "legacy-seed")
	if err != nil {
		t.Fatalf("seed legacy entries: %v", err)
	}
}
