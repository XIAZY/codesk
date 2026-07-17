package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintLegacyTreeIsDeterministicAndByteSensitive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "legacy")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "state"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}

	baseline, err := fingerprintLegacyTree(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := fingerprintLegacyTree(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if baseline != repeated || !baseline.Present || baseline.EntryCount != 3 || baseline.ByteCount != 11 || baseline.DigestSHA256 == "" {
		t.Fatalf("legacy fingerprints baseline=%+v repeated=%+v", baseline, repeated)
	}
	if err := os.WriteFile(filepath.Join(root, "config"), []byte("frost"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutated, err := fingerprintLegacyTree(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutated.DigestSHA256 == baseline.DigestSHA256 || mutated.EntryCount != baseline.EntryCount || mutated.ByteCount != baseline.ByteCount {
		t.Fatalf("one-byte mutation baseline=%+v mutated=%+v", baseline, mutated)
	}
}

func TestFingerprintLegacyTreeDistinguishesAbsenceAndEmptyRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "legacy")
	absent, err := fingerprintLegacyTree(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if absent.Present || absent.DigestSHA256 != "" || absent.EntryCount != 0 || absent.ByteCount != 0 {
		t.Fatalf("absent fingerprint = %+v", absent)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	empty, err := fingerprintLegacyTree(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Present || empty.DigestSHA256 == "" || empty.EntryCount != 0 || empty.ByteCount != 0 {
		t.Fatalf("empty fingerprint = %+v", empty)
	}
}

func TestFingerprintLegacyTreeRejectsLinks(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "legacy")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := fingerprintLegacyTree(context.Background(), root, nil); err == nil {
		t.Fatal("linked legacy entry was accepted")
	}
}

func TestFingerprintLegacyTreePreservesCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "legacy")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fingerprintLegacyTree(ctx, root, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fingerprintLegacyTree error = %v, want context cancellation", err)
	}
}
