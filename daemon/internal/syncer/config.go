package syncer

import (
	"os"
	"path/filepath"
)

type Config struct {
	BackendURL         string
	WorkspaceID        string
	DaemonToken        string
	WorkspaceDir       string
	AgentWorkspaceRoot string
	AgentID            string
	CodexCommand       string
	RuntimeDir         string
	AgentToolBaseURL   string
	PprofAddr          string
	CacheDir           string
}

func LoadConfig() Config {
	runtimeDir := getenv("NOTTY_RUNTIME_DIR", "/runtime/notty")
	cacheDir := getenv("NOTTY_CACHE_DIR", "")
	if cacheDir == "" {
		cacheDir = filepath.Join(runtimeDir, ".notty", "documents")
	}
	return Config{
		BackendURL:         getenv("NOTTY_BACKEND_URL", "http://backend:8080"),
		WorkspaceID:        getenv("NOTTY_WORKSPACE_ID", ""),
		DaemonToken:        getenv("NOTTY_DAEMON_TOKEN", ""),
		WorkspaceDir:       getenv("NOTTY_WORKSPACE_DIR", "/workspace/notty"),
		AgentWorkspaceRoot: getenv("NOTTY_AGENT_WORKSPACE_ROOT", "/workspace/agents"),
		AgentID:            getenv("NOTTY_AGENT_ID", "daemon_agent"),
		CodexCommand:       getenv("NOTTY_CODEX_COMMAND", "codex"),
		RuntimeDir:         runtimeDir,
		AgentToolBaseURL:   getenv("NOTTY_AGENT_TOOL_BASE_URL", "http://127.0.0.1:7778"),
		PprofAddr:          getenv("NOTTY_PPROF_ADDR", ""),
		CacheDir:           cacheDir,
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
