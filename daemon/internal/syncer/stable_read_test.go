package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadBytesStableReadsAndRevalidatesFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("stable"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	stat, err := fs.Stat(context.Background(), path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	read, ok, err := fs.ReadBytesStable(context.Background(), path, StableReadOptions{
		ExpectedStat: &stat,
		Capabilities: ScanCapabilities{FileKeyReliable: true, CTimeReliable: true},
	})
	if err != nil || !ok {
		t.Fatalf("stable read failed ok=%t err=%v", ok, err)
	}
	if string(read.Bytes) != "stable" {
		t.Fatalf("unexpected stable bytes %q", read.Bytes)
	}
	if read.OpenStat.FileKey == "" || read.FinalStat.FileKey == "" || read.OpenStat.FileKey != read.FinalStat.FileKey {
		t.Fatalf("expected matching FileKey before/after read, open=%#v final=%#v", read.OpenStat, read.FinalStat)
	}
}

func TestReadBytesStableRejectsPathReplacementAtFinish(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open old file: %v", err)
	}
	defer file.Close()
	openStat, err := fs.FStat(file, path)
	if err != nil {
		t.Fatalf("fstat old file: %v", err)
	}
	replacement := filepath.Join(root, "replacement.md")
	if err := os.WriteFile(replacement, []byte("old"), 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace path: %v", err)
	}

	_, ok, err := fs.finishStableRead(context.Background(), path, file, []byte("old"), openStat, StableReadOptions{
		Capabilities: ScanCapabilities{FileKeyReliable: true, CTimeReliable: true},
	})
	if err != nil {
		t.Fatalf("finish stable read: %v", err)
	}
	if ok {
		t.Fatal("expected stable read final validation to reject path replacement")
	}
}

func TestReadBytesStableHonorsMaxBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fs := NewWorkspaceFS(root)
	_, ok, err := fs.ReadBytesStable(context.Background(), path, StableReadOptions{
		Capabilities: ScanCapabilities{FileKeyReliable: true},
		MaxBytes:     3,
	})
	if !errors.Is(err, ErrFileTooLargeForSingleRead) {
		t.Fatalf("expected max bytes error, ok=%t err=%v", ok, err)
	}
}
