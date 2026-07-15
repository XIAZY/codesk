package desktop

import (
	"errors"
	"fmt"
	"notty/daemon/internal/syncer"
	"path/filepath"
	"strings"
)

const SecretKeyDaemonToken = "codesk:daemon-token"

type SecretStore interface {
	Save(key string, secret []byte) error
	Load(key string) ([]byte, error)
	Delete(key string) error
}

type LoginItem interface {
	Enable() error
	Disable() error
	IsEnabled() (bool, error)
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
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("desktop: %s directory is empty", name)
	}
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("desktop: %s directory %q has surrounding whitespace", name, path)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("desktop: %s directory %q is not absolute", name, path)
	}
	if path != filepath.Clean(path) {
		return fmt.Errorf("desktop: %s directory %q is not clean (use %q)", name, path, filepath.Clean(path))
	}
	return nil
}

func errNoAppDir(reason string) error {
	return fmt.Errorf("desktop: cannot determine app directory: %s", reason)
}

var ErrUnsupportedPlatform = errors.New("desktop: unsupported platform")

func SyncerConfig(dirs Dirs, backendURL, workspaceID, daemonToken, daemonVersion string) (syncer.Config, error) {
	if err := dirs.Validate(); err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		BackendURL:         backendURL,
		WorkspaceID:        workspaceID,
		DaemonToken:        daemonToken,
		DaemonVersion:      daemonVersion,
		DataDir:            dirs.Data,
		WorkspaceDir:       filepath.Join(dirs.Data, "workspace"),
		AgentWorkspaceRoot: filepath.Join(dirs.Data, "agents"),
		CodexCommand:       "codex",
		ClaudeCommand:      "claude",
		AgentToolBaseURL:   "http://127.0.0.1:7778",
	}, nil
}
