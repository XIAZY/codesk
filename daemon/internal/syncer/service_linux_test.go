//go:build linux

package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestScanWorkspaceFilesSkipsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	fifoPath := filepath.Join(root, "document.md")
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("create FIFO: %v", err)
	}

	occupant, err := classifyWorkspacePathOccupant(fifoPath)
	if err != nil {
		t.Fatalf("classify FIFO: %v", err)
	}
	if occupant.Kind != workspacePathOther || occupant.Mode&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO occupant = %#v, want physical other/named pipe", occupant)
	}
	providesContent, err := workspacePathProvidesFileContent(fifoPath, occupant)
	if err != nil {
		t.Fatalf("classify FIFO content: %v", err)
	}
	if providesContent {
		t.Fatal("FIFO must not be classified as document content")
	}

	type scanResult struct {
		files map[string]string
		err   error
	}
	done := make(chan scanResult, 1)
	go func() {
		files, scanErr := scanWorkspaceFiles(root)
		done <- scanResult{files: files, err: scanErr}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("scan workspace with FIFO: %v", result.err)
		}
		if len(result.files) != 0 {
			t.Fatalf("scan workspace with FIFO = %#v, want no files", result.files)
		}
	case <-time.After(time.Second):
		// Release a mutant blocked in ReadFile so the test cannot leak a goroutine.
		fd, openErr := unix.Open(fifoPath, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if openErr == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("scan blocked while reading a non-regular FIFO occupant")
	}
}
