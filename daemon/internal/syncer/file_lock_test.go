package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestReadFileLockedWaitsForExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("stable"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	defer file.Close()
	if err := lockFile(file, syscall.LOCK_EX); err != nil {
		t.Fatalf("lock file: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		content, err := readFileLocked(path)
		if err == nil && string(content) != "stable" {
			err = errors.New("unexpected locked read content")
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("locked read completed before exclusive lock was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlockFile(file)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("locked read after unlock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("locked read did not complete after exclusive lock was released")
	}
}

func TestAppendFileLockedWaitsForExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	defer file.Close()
	if err := lockFile(file, syscall.LOCK_EX); err != nil {
		t.Fatalf("lock file: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- appendFileLocked(path, "append\n")
	}()

	select {
	case err := <-done:
		t.Fatalf("append completed before exclusive lock was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlockFile(file)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("append after unlock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("append did not complete after exclusive lock was released")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(content) != "base\nappend\n" {
		t.Fatalf("unexpected file content: %q", content)
	}
}

func TestAgentLogConcurrentWritersKeepCompleteRecords(t *testing.T) {
	workdir := t.TempDir()
	logPath := agentLogPath(workdir, "agent-name")
	if logPath != filepath.Join(workdir, "agent-name.log") {
		t.Fatalf("agent log path must stay at workspace root, got %s", logPath)
	}
	logger, err := openAgentLog(workdir, "agent-name")
	if err != nil {
		t.Fatalf("open agent log: %v", err)
	}
	defer logger.Close()

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			logger.Printf("persistent record %02d", i)
		}(i)
		go func(i int) {
			defer wg.Done()
			appendAgentLog(workdir, "agent-name", "oneoff record %02d", i)
		}(i)
	}
	wg.Wait()
	logger.Close()

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read agent log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 52 {
		t.Fatalf("expected 52 complete log records, got %d", len(lines))
	}
	for _, line := range lines {
		if len(line) < len("2006-01-02T15:04:05Z ") || line[4] != '-' || line[10] != 'T' || !strings.Contains(line[:min(len(line), 40)], "Z ") {
			t.Fatalf("malformed log record: %q", line)
		}
	}
}
