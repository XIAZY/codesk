package syncer

import "os"

type DaemonClientKind string

const (
	DaemonClientKindCLI DaemonClientKind = "cli"
	DaemonClientKindGUI DaemonClientKind = "gui"
)

type Config struct {
	BackendURL         string
	WorkspaceID        string
	DaemonToken        string
	ClientKind         DaemonClientKind
	DataDir            string
	WorkspaceDir       string
	AgentWorkspaceRoot string
	AgentID            string
	CodexCommand       string
	ClaudeCommand      string
	AgentToolBaseURL   string
	PprofAddr          string
	RuntimeObserver    RuntimeObserver
}

func LoadConfig() Config {
	return Config{
		BackendURL:         getenv("NOTTY_BACKEND_URL", "http://backend:8080"),
		WorkspaceID:        getenv("NOTTY_WORKSPACE_ID", ""),
		DaemonToken:        getenv("NOTTY_DAEMON_TOKEN", ""),
		ClientKind:         DaemonClientKindCLI,
		DataDir:            getenv("NOTTY_DATA_DIR", defaultNottyDataDir()),
		WorkspaceDir:       getenv("NOTTY_WORKSPACE_DIR", "/workspace/notty"),
		AgentWorkspaceRoot: getenv("NOTTY_AGENT_WORKSPACE_ROOT", "/workspace/agents"),
		AgentID:            getenv("NOTTY_AGENT_ID", ""),
		CodexCommand:       getenv("NOTTY_CODEX_COMMAND", "codex"),
		ClaudeCommand:      getenv("NOTTY_CLAUDE_COMMAND", "claude"),
		AgentToolBaseURL:   getenv("NOTTY_AGENT_TOOL_BASE_URL", "http://127.0.0.1:7778"),
		PprofAddr:          getenv("NOTTY_PPROF_ADDR", ""),
	}
}

func defaultNottyDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".notty"
	}
	return home + string(os.PathSeparator) + ".notty"
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
