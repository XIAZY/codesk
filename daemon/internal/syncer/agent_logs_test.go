package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentLogPathUsesDaemonDataDirWorkspaceAndDate(t *testing.T) {
	cfg := Config{
		DataDir:     t.TempDir(),
		WorkspaceID: "workspace/one:two",
	}
	at := time.Date(2026, 5, 28, 23, 59, 0, 0, time.UTC)

	got := agentLogPathAt(cfg, "agent:one", at)
	want := filepath.Join(cfg.DataDir, "daemons", "workspace-one-two", "agents", "agent-one", "2026-05-28.logs")
	if got != want {
		t.Fatalf("agent log path mismatch:\ngot  %s\nwant %s", got, want)
	}
}

func TestAgentLogWritesOutsideAgentWorkspace(t *testing.T) {
	workdir := t.TempDir()
	cfg := Config{
		DataDir:     t.TempDir(),
		WorkspaceID: "workspace:test",
	}
	at := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

	appendAgentLogWithClock(cfg, "agent-name", func() time.Time { return at }, "record")

	if _, err := os.Stat(agentLogPathAt(cfg, "agent-name", at)); err != nil {
		t.Fatalf("expected daemon-local agent log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "agent-name.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace root log should not be written, stat err=%v", err)
	}
}

func TestAgentLogRotatesByUTCDate(t *testing.T) {
	cfg := Config{
		DataDir:     t.TempDir(),
		WorkspaceID: "workspace:test",
	}
	now := time.Date(2026, 5, 28, 23, 59, 59, 0, time.UTC)
	logger, err := openAgentLogWithClock(cfg, "agent-name", func() time.Time { return now })
	if err != nil {
		t.Fatalf("open agent log: %v", err)
	}

	logger.Printf("before midnight")
	now = now.Add(2 * time.Second)
	logger.Printf("after midnight")
	logger.Close()

	firstPath := agentLogPathAt(cfg, "agent-name", time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC))
	secondPath := agentLogPathAt(cfg, "agent-name", time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC))
	first, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("read first day log: %v", err)
	}
	second, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("read second day log: %v", err)
	}
	if !strings.Contains(string(first), "before midnight") {
		t.Fatalf("first day log missing pre-rotation record:\n%s", string(first))
	}
	if !strings.Contains(string(second), "after midnight") || !strings.Contains(string(second), "log closed") {
		t.Fatalf("second day log missing post-rotation records:\n%s", string(second))
	}
}

func TestAgentLogConcurrentWritersKeepCompleteRecords(t *testing.T) {
	cfg := Config{
		DataDir:     t.TempDir(),
		WorkspaceID: "workspace:test",
	}
	at := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	logPath := agentLogPathAt(cfg, "agent-name", at)
	logger, err := openAgentLogWithClock(cfg, "agent-name", func() time.Time { return at })
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
			appendAgentLogWithClock(cfg, "agent-name", func() time.Time { return at }, "oneoff record %02d", i)
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
