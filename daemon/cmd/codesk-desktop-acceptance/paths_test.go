package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathWithinResolvedDetectsRedirectedProtectedPath(t *testing.T) {
	parent := t.TempDir()
	resetRoot := filepath.Join(parent, "reset-root")
	if err := os.Mkdir(resetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "outside-alias")
	if err := os.Symlink(resetRoot, alias); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(alias, "not-created-yet", "evidence")
	if inside, err := pathWithin(resetRoot, protected); err != nil || inside {
		t.Fatalf("lexical containment = %t, %v; want false", inside, err)
	}
	if inside, err := pathWithinResolved(resetRoot, protected); err != nil || !inside {
		t.Fatalf("resolved containment = %t, %v; want true", inside, err)
	}
}

func TestPathWithinResolvedKeepsIndependentPathOutside(t *testing.T) {
	parent := t.TempDir()
	resetRoot := filepath.Join(parent, "reset-root")
	protected := filepath.Join(parent, "evidence")
	if err := os.Mkdir(resetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if inside, err := pathWithinResolved(resetRoot, protected); err != nil || inside {
		t.Fatalf("resolved containment = %t, %v; want false", inside, err)
	}
}
