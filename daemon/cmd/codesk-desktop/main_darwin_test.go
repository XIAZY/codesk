//go:build darwin && cgo

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDarwinPrivateDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureDarwinPrivateDirectory(directory); err != nil {
		t.Fatalf("ensureDarwinPrivateDirectory() error = %v", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("private directory mode = %04o, want 0700", mode)
	}

	realDirectory := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureDarwinPrivateDirectory(link); err == nil {
		t.Fatal("ensureDarwinPrivateDirectory() accepted a symlink")
	}
}

func TestOpenDarwinPrivateLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codesk.log")
	file, err := openDarwinPrivateLog(path)
	if err != nil {
		t.Fatalf("openDarwinPrivateLog() error = %v", err)
	}
	if _, err := file.WriteString("test\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("private log mode = %04o, want 0600", mode)
	}

	realLog := filepath.Join(t.TempDir(), "real.log")
	if err := os.WriteFile(realLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.log")
	if err := os.Symlink(realLog, link); err != nil {
		t.Fatal(err)
	}
	if file, err := openDarwinPrivateLog(link); err == nil {
		file.Close()
		t.Fatal("openDarwinPrivateLog() accepted a symlink")
	}
}
