package desktop

import (
	"errors"
	"fmt"
	"notty/daemon/internal/syncer"
	"path/filepath"
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
	if d.Data == "" {
		return fmt.Errorf("desktop: data directory is empty")
	}
	if d.Logs == "" {
		return fmt.Errorf("desktop: logs directory is empty")
	}
	if d.Cache == "" {
		return fmt.Errorf("desktop: cache directory is empty")
	}
	return nil
}

func errNoAppDir(reason string) error {
	return fmt.Errorf("desktop: cannot determine app directory: %s", reason)
}

var ErrUnsupportedPlatform = errors.New("desktop: unsupported platform")

func SyncerConfig(dirs Dirs, backendURL, workspaceID, daemonToken, daemonVersion string) syncer.Config {
	return syncer.Config{
		BackendURL:         backendURL,
		WorkspaceID:        workspaceID,
		DaemonToken:        daemonToken,
		DaemonVersion:      daemonVersion,
		DataDir:            dirs.Data,
		WorkspaceDir:       filepath.Join(dirs.Data, "workspace"),
		AgentWorkspaceRoot: filepath.Join(dirs.Data, "agents"),
	}
}
