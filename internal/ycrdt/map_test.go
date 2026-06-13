package ycrdt

import "testing"

func TestYMapStringAndNestedMapRoundTrip(t *testing.T) {
	source := New(WithClientID(101))
	defer source.Close()
	root := source.GetMap("root")
	update, err := source.Update(func(txn *Transaction) error {
		entries, err := root.SetMap(txn, "entriesById")
		if err != nil {
			return err
		}
		entry, err := entries.SetMap(txn, "entry_1")
		if err != nil {
			return err
		}
		if err := entry.SetString(txn, "kind", "file"); err != nil {
			return err
		}
		if err := entry.SetString(txn, "loc", `{"parentId":"","name":"docs/a.md"}`); err != nil {
			return err
		}
		return entry.SetString(txn, "contentDocumentId", "doc_content")
	}, "test")
	if err != nil {
		t.Fatalf("update source map: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected map update")
	}

	replica := New(WithClientID(202))
	defer replica.Close()
	if err := ApplyUpdateV1(replica, update, "sync"); err != nil {
		t.Fatalf("apply map update: %v", err)
	}

	replicaRoot := replica.GetMap("root")
	err = replica.Read(func(txn *Transaction) error {
		entries, ok, err := replicaRoot.GetMap(txn, "entriesById")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing entriesById")
		}
		entry, ok, err := entries.GetMap(txn, "entry_1")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing entry map")
		}
		got, ok, err := entry.GetString(txn, "loc")
		if err != nil {
			return err
		}
		if !ok || got != `{"parentId":"","name":"docs/a.md"}` {
			t.Fatalf("loc = %q/%t", got, ok)
		}
		items, err := entry.Entries(txn)
		if err != nil {
			return err
		}
		if len(items) != 3 {
			t.Fatalf("entry count = %d, want 3", len(items))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read replica: %v", err)
	}
}

func TestYMapConcurrentAtomicLocConvergesToWholeValue(t *testing.T) {
	left := New(WithClientID(111))
	defer left.Close()
	right := New(WithClientID(222))
	defer right.Close()
	root := left.GetMap("root")
	initial, err := left.Update(func(txn *Transaction) error {
		entries, err := root.SetMap(txn, "entriesById")
		if err != nil {
			return err
		}
		entry, err := entries.SetMap(txn, "entry_1")
		if err != nil {
			return err
		}
		return entry.SetString(txn, "loc", `{"parentId":"","name":"a.md"}`)
	}, "seed")
	if err != nil {
		t.Fatalf("seed left: %v", err)
	}
	if err := ApplyUpdateV1(right, initial, "seed"); err != nil {
		t.Fatalf("seed right: %v", err)
	}

	leftRoot := left.GetMap("root")
	rightRoot := right.GetMap("root")
	leftUpdate, err := left.Update(func(txn *Transaction) error {
		entries, ok, err := leftRoot.GetMap(txn, "entriesById")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing entries")
		}
		entry, ok, err := entries.GetMap(txn, "entry_1")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing entry")
		}
		return entry.SetString(txn, "loc", `{"parentId":"docs","name":"a.md"}`)
	}, "left")
	if err != nil {
		t.Fatalf("left move: %v", err)
	}
	rightUpdate, err := right.Update(func(txn *Transaction) error {
		entries, ok, err := rightRoot.GetMap(txn, "entriesById")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing entries")
		}
		entry, ok, err := entries.GetMap(txn, "entry_1")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing entry")
		}
		return entry.SetString(txn, "loc", `{"parentId":"notes","name":"b.md"}`)
	}, "right")
	if err != nil {
		t.Fatalf("right move: %v", err)
	}

	if err := ApplyUpdateV1(left, rightUpdate, "right"); err != nil {
		t.Fatalf("apply right to left: %v", err)
	}
	if err := ApplyUpdateV1(right, leftUpdate, "left"); err != nil {
		t.Fatalf("apply left to right: %v", err)
	}

	readLoc := func(doc *Doc) string {
		var loc string
		docRoot := doc.GetMap("root")
		if err := doc.Read(func(txn *Transaction) error {
			entries, _, err := docRoot.GetMap(txn, "entriesById")
			if err != nil {
				return err
			}
			entry, _, err := entries.GetMap(txn, "entry_1")
			if err != nil {
				return err
			}
			loc, _, err = entry.GetString(txn, "loc")
			return err
		}); err != nil {
			t.Fatalf("read loc: %v", err)
		}
		return loc
	}
	leftLoc := readLoc(left)
	rightLoc := readLoc(right)
	if leftLoc != rightLoc {
		t.Fatalf("loc diverged: left=%q right=%q", leftLoc, rightLoc)
	}
	if leftLoc != `{"parentId":"docs","name":"a.md"}` && leftLoc != `{"parentId":"notes","name":"b.md"}` {
		t.Fatalf("loc merged into unexpected value: %q", leftLoc)
	}
}
