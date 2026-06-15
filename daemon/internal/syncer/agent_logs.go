package syncer

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type agentLog struct {
	mu      sync.Mutex
	cfg     Config
	agentID string
	now     func() time.Time
}

var agentLogWriteMu sync.Mutex

func openAgentLog(cfg Config, agentID string) (*agentLog, error) {
	return openAgentLogWithClock(cfg, agentID, time.Now)
}

func openAgentLogWithClock(cfg Config, agentID string, now func() time.Time) (*agentLog, error) {
	if now == nil {
		now = time.Now
	}
	agentLog := &agentLog{cfg: cfg, agentID: strings.TrimSpace(agentID), now: now}
	at := now().UTC()
	path := agentLogPathAt(cfg, agentID, at)
	if err := appendAgentLogLine(path, agentLogLineAt(at, "log opened")); err != nil {
		return nil, err
	}
	return agentLog, nil
}

func appendAgentLog(cfg Config, agentID string, format string, args ...any) {
	appendAgentLogWithClock(cfg, agentID, time.Now, format, args...)
}

func appendAgentLogWithClock(cfg Config, agentID string, now func() time.Time, format string, args ...any) {
	if now == nil {
		now = time.Now
	}
	at := now().UTC()
	path := agentLogPathAt(cfg, agentID, at)
	if err := appendAgentLogLine(path, agentLogLineAt(at, format, args...)); err != nil {
		log.Printf("agent log append failed path=%s err=%v", path, err)
	}
}

func appendAgentLogLine(path string, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	agentLogWriteMu.Lock()
	defer agentLogWriteMu.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	return writeFullString(file, line)
}

func (l *agentLog) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	at := l.now().UTC()
	path := agentLogPathAt(l.cfg, l.agentID, at)
	if err := appendAgentLogLine(path, agentLogLineAt(at, format, args...)); err != nil {
		log.Printf("agent log append failed path=%s err=%v", path, err)
	}
}

func (l *agentLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	at := l.now().UTC()
	path := agentLogPathAt(l.cfg, l.agentID, at)
	if err := appendAgentLogLine(path, agentLogLineAt(at, "log closed")); err != nil {
		log.Printf("agent log close record failed path=%s err=%v", path, err)
	}
}

func agentLogLineAt(at time.Time, format string, args ...any) string {
	line := strings.TrimRight(fmt.Sprintf(format, args...), "\r\n")
	return fmt.Sprintf("%s %s\n", at.UTC().Format(time.RFC3339Nano), line)
}

func agentLogPath(cfg Config, agentID string) string {
	return agentLogPathAt(cfg, agentID, time.Now().UTC())
}

func agentLogPathAt(cfg Config, agentID string, at time.Time) string {
	return filepath.Join(agentLogDir(cfg, agentID), at.UTC().Format("2006-01-02")+".logs")
}

func agentLogDir(cfg Config, agentID string) string {
	return filepath.Join(agentDataDir(cfg), "daemons", safeInstallerName(firstNonEmptyText(cfg.WorkspaceID, "workspace")), "agents", safeInstallerName(firstNonEmptyText(agentID, "agent")))
}

func agentDataDir(cfg Config) string {
	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		return defaultNottyDataDir()
	}
	return dataDir
}

func safeInstallerName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "workspace"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return '-'
		}
	}, value)
}
