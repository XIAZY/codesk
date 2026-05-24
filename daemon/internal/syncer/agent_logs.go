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
	fs   *WorkspaceFS
	path string
}

func openAgentLog(workdir string, name string) (*agentLog, error) {
	path := agentLogPath(workdir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	fs, err := OpenWorkspaceFS(workdir)
	if err != nil {
		return nil, err
	}
	agentLog := &agentLog{fs: fs, path: path}
	agentLog.Printf("log opened")
	return agentLog, nil
}

func appendAgentLog(workdir string, name string, format string, args ...any) {
	path := agentLogPath(workdir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("agent log mkdir failed workdir=%s err=%v", workdir, err)
		return
	}
	fs, err := OpenWorkspaceFS(workdir)
	if err != nil {
		log.Printf("agent log fs lock failed workdir=%s err=%v", workdir, err)
		return
	}
	defer fs.Close()
	if err := fs.Append(path, agentLogLine(format, args...)); err != nil {
		log.Printf("agent log append failed path=%s err=%v", path, err)
	}
}

func (l *agentLog) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fs.Append(l.path, agentLogLine(format, args...)); err != nil {
		log.Printf("agent log append failed path=%s err=%v", l.path, err)
	}
}

func (l *agentLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fs.Append(l.path, agentLogLine("log closed")); err != nil {
		log.Printf("agent log close record failed path=%s err=%v", l.path, err)
	}
	if err := l.fs.Close(); err != nil {
		log.Printf("agent log close failed path=%s err=%v", l.path, err)
	}
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
