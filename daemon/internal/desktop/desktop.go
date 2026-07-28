package desktop

import (
	"errors"
	"fmt"
	"path/filepath"

	"notty/daemon/internal/desktopstate"
	"notty/daemon/internal/syncer"
)

type LoginItem interface {
	Enable() error
	Disable() error
	IsEnabled() (bool, error)
}

func loginItemRegistrationMatches(actual, expected string) bool {
	return actual != "" && actual == expected
}

type InstanceLock interface {
	Acquire() (bool, error)
	Release() error
}

type OpenURL interface {
	Open(url string) error
}

type Dirs struct {
	Data  string
	Logs  string
	Cache string
}

func (d Dirs) Validate() error {
	if err := requireAbsolute("data", d.Data); err != nil {
		return err
	}
	if err := requireAbsolute("logs", d.Logs); err != nil {
		return err
	}
	return requireAbsolute("cache", d.Cache)
}

func requireAbsolute(name, path string) error {
	return desktopstate.RequireAbsolute(name, path)
}

func errNoAppDir(reason string) error {
	return fmt.Errorf("desktop: cannot determine app directory: %s", reason)
}

var ErrUnsupportedPlatform = errors.New("desktop: unsupported platform")

func SyncerConfig(dirs Dirs, backendURL, workspaceID, daemonToken string) (syncer.Config, error) {
	if err := dirs.Validate(); err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		BackendURL:         backendURL,
		WorkspaceID:        workspaceID,
		DaemonToken:        daemonToken,
		ClientKind:         syncer.DaemonClientKindGUI,
		DataDir:            dirs.Data,
		WorkspaceDir:       filepath.Join(dirs.Data, "workspace"),
		AgentWorkspaceRoot: filepath.Join(dirs.Data, "agents"),
		CodexCommand:       "codex",
		ClaudeCommand:      "claude",
		AgentToolBaseURL:   "http://127.0.0.1:7778",
	}, nil
}
