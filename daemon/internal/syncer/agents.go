package syncer

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"
)

type agent struct {
	ID              string `json:"id"`
	DaemonID        string `json:"daemonId"`
	Handle          string `json:"handle"`
	Name            string `json:"name"`
	Role            string `json:"role"`
	Kind            string `json:"kind"`
	SystemPrompt    string `json:"systemPrompt"`
	WorkspaceRoot   string `json:"workspaceRoot"`
	CurrentTurnID   string `json:"currentTurnId"`
	SessionID       string `json:"sessionId"`
	Status          string `json:"status"`
	CurrentTask     string `json:"currentTask"`
	CurrentActivity string `json:"currentActivity"`
	CurrentRunID    string `json:"currentRunId"`
}

// agentRun is retained as the authenticated tool context shape while the
// daemon migrates agents from short-lived runs to resident app-server sessions.
type agentRun struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agentId"`
	AgentHandle     string    `json:"agentHandle"`
	AgentName       string    `json:"agentName"`
	AgentKind       string    `json:"agentKind"`
	SystemPrompt    string    `json:"systemPrompt"`
	SessionID       string    `json:"sessionId"`
	WorkspaceID     string    `json:"workspaceId"`
	WorkspaceRoot   string    `json:"workspaceRoot"`
	WorkingDir      string    `json:"workingDirectory"`
	Prompt          string    `json:"prompt"`
	Status          string    `json:"status"`
	DesiredStatus   string    `json:"desiredStatus"`
	ProcessID       int       `json:"processId"`
	LaunchTime      time.Time `json:"launchTime"`
	LastHeartbeatAt time.Time `json:"lastHeartbeatAt"`
	CompletedAt     time.Time `json:"completedAt"`
	ExitCode        int       `json:"exitCode"`
	LastMessage     string    `json:"lastMessage"`
	LogTail         []string  `json:"logTail"`
	Error           string    `json:"error"`
	AssignedTaskRef string    `json:"assignedTaskRef"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// buildAgentToolEnv exposes the daemon's tool callback endpoint to a spawned
// runtime, regardless of which runtime it is.
func buildAgentToolEnv(cfg Config, toolToken string) []string {
	values := []string{}
	if cfg.AgentToolBaseURL != "" {
		values = append(values, "NOTTY_AGENT_TOOL_BASE_URL="+cfg.AgentToolBaseURL)
	}
	if toolToken != "" {
		values = append(values, "NOTTY_AGENT_TOOL_TOKEN="+toolToken)
	}
	return values
}

func newToolToken() (string, error) {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func safeAgentWorkspaceName(agentID string) string {
	trimmed := strings.TrimSpace(agentID)
	if trimmed == "" {
		return "agent"
	}
	name := strings.Map(func(r rune) rune {
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
			return '_'
		}
	}, trimmed)
	switch name {
	case ".":
		return "_"
	case "..":
		return "__"
	default:
		return name
	}
}

// Backend WorkspaceRoot is display metadata, never filesystem identity.
func agentWorkspacePath(cfg Config, agentID string) string {
	return filepath.Join(cfg.AgentWorkspaceRoot, safeAgentWorkspaceName(agentID))
}
