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
	mu   sync.Mutex
	file *os.File
	path string
}

func openAgentLog(workdir string, name string) (*agentLog, error) {
	path := agentLogPath(workdir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	agentLog := &agentLog{file: file, path: path}
	agentLog.Printf("log opened")
	return agentLog, nil
}

func appendAgentLog(workdir string, name string, format string, args ...any) {
	path := agentLogPath(workdir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("agent log mkdir failed workdir=%s err=%v", workdir, err)
		return
	}
	if err := appendFileLocked(path, agentLogLine(format, args...)); err != nil {
		log.Printf("agent log append failed path=%s err=%v", path, err)
	}
}

func (l *agentLog) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	if err := appendOpenFileLocked(l.file, agentLogLine(format, args...)); err != nil {
		log.Printf("agent log append failed path=%s err=%v", l.path, err)
	}
}

func (l *agentLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	if err := appendOpenFileLocked(l.file, agentLogLine("log closed")); err != nil {
		log.Printf("agent log close record failed path=%s err=%v", l.path, err)
	}
	_ = l.file.Close()
	l.file = nil
}

func agentLogLine(format string, args ...any) string {
	line := strings.TrimRight(fmt.Sprintf(format, args...), "\r\n")
	return fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), line)
}

func agentLogPath(workdir string, name string) string {
	return filepath.Join(workdir, safeAgentWorkspaceName(firstNonEmptyText(name, "agent"))+".log")
}

func agentLogName(current *agent) string {
	if current == nil {
		return "agent"
	}
	return firstNonEmptyText(current.Handle, current.Name, current.ID, "agent")
}
