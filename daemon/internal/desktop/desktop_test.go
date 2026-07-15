package desktop

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncerConfigNeverReferencesCLIPaths(t *testing.T) {
	dirs := Dirs{
		Data:  "/desktop/data",
		Logs:  "/desktop/logs",
		Cache: "/desktop/cache",
	}

	cfg := SyncerConfig(dirs, "https://api.getcodesk.com", "ws-1", "tok-1", "1.0.0")

	cliPaths := []string{".notty", "notty"}
	fields := map[string]string{
		"DataDir":            cfg.DataDir,
		"WorkspaceDir":       cfg.WorkspaceDir,
		"AgentWorkspaceRoot": cfg.AgentWorkspaceRoot,
	}
	for name, value := range fields {
		for _, cli := range cliPaths {
			if strings.Contains(value, cli) {
				t.Errorf("Config.%s = %q contains CLI path %q", name, value, cli)
			}
		}
	}
}

func TestSyncerConfigAllPathsUnderDataDir(t *testing.T) {
	dirs := Dirs{
		Data:  "/app/Codesk",
		Logs:  "/app/Codesk/Logs",
		Cache: "/app/Codesk/Cache",
	}

	cfg := SyncerConfig(dirs, "https://api.getcodesk.com", "ws-1", "tok-1", "1.0.0")

	paths := map[string]string{
		"DataDir":            cfg.DataDir,
		"WorkspaceDir":       cfg.WorkspaceDir,
		"AgentWorkspaceRoot": cfg.AgentWorkspaceRoot,
	}
	for name, value := range paths {
		rel, err := filepath.Rel(dirs.Data, value)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("Config.%s = %q is not under data dir %q", name, value, dirs.Data)
		}
	}
}

func TestSyncerConfigFieldMapping(t *testing.T) {
	dirs := Dirs{
		Data:  "/test/data",
		Logs:  "/test/logs",
		Cache: "/test/cache",
	}

	cfg := SyncerConfig(dirs, "https://backend", "workspace-id", "secret-token", "2.0.0")

	if cfg.BackendURL != "https://backend" {
		t.Errorf("BackendURL = %q, want %q", cfg.BackendURL, "https://backend")
	}
	if cfg.WorkspaceID != "workspace-id" {
		t.Errorf("WorkspaceID = %q, want %q", cfg.WorkspaceID, "workspace-id")
	}
	if cfg.DaemonToken != "secret-token" {
		t.Errorf("DaemonToken = %q, want %q", cfg.DaemonToken, "secret-token")
	}
	if cfg.DaemonVersion != "2.0.0" {
		t.Errorf("DaemonVersion = %q, want %q", cfg.DaemonVersion, "2.0.0")
	}
	if cfg.DataDir != "/test/data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/test/data")
	}
	if cfg.WorkspaceDir != filepath.Join("/test/data", "workspace") {
		t.Errorf("WorkspaceDir = %q, want %q", cfg.WorkspaceDir, filepath.Join("/test/data", "workspace"))
	}
	if cfg.AgentWorkspaceRoot != filepath.Join("/test/data", "agents") {
		t.Errorf("AgentWorkspaceRoot = %q, want %q", cfg.AgentWorkspaceRoot, filepath.Join("/test/data", "agents"))
	}
}

func TestSyncerConfigDoesNotReadEnv(t *testing.T) {
	envVars := []string{
		"NOTTY_BACKEND_URL",
		"NOTTY_WORKSPACE_ID",
		"NOTTY_DAEMON_TOKEN",
		"NOTTY_DATA_DIR",
		"NOTTY_WORKSPACE_DIR",
		"NOTTY_AGENT_WORKSPACE_ROOT",
	}
	for _, key := range envVars {
		t.Setenv(key, "/poisoned/"+key)
	}

	dirs := Dirs{
		Data:  "/clean/data",
		Logs:  "/clean/logs",
		Cache: "/clean/cache",
	}
	cfg := SyncerConfig(dirs, "https://clean.api", "ws-clean", "tok-clean", "1.0.0")

	if strings.Contains(cfg.BackendURL, "poisoned") {
		t.Errorf("BackendURL read from env: %q", cfg.BackendURL)
	}
	if strings.Contains(cfg.DataDir, "poisoned") {
		t.Errorf("DataDir read from env: %q", cfg.DataDir)
	}
	if strings.Contains(cfg.WorkspaceDir, "poisoned") {
		t.Errorf("WorkspaceDir read from env: %q", cfg.WorkspaceDir)
	}
	if strings.Contains(cfg.AgentWorkspaceRoot, "poisoned") {
		t.Errorf("AgentWorkspaceRoot read from env: %q", cfg.AgentWorkspaceRoot)
	}
	if strings.Contains(cfg.WorkspaceID, "poisoned") {
		t.Errorf("WorkspaceID read from env: %q", cfg.WorkspaceID)
	}
	if strings.Contains(cfg.DaemonToken, "poisoned") {
		t.Errorf("DaemonToken read from env: %q", cfg.DaemonToken)
	}
}

func TestDirsValidate(t *testing.T) {
	tests := []struct {
		name    string
		dirs    Dirs
		wantErr bool
	}{
		{
			name:    "valid",
			dirs:    Dirs{Data: "/a", Logs: "/b", Cache: "/c"},
			wantErr: false,
		},
		{
			name:    "empty data",
			dirs:    Dirs{Data: "", Logs: "/b", Cache: "/c"},
			wantErr: true,
		},
		{
			name:    "empty logs",
			dirs:    Dirs{Data: "/a", Logs: "", Cache: "/c"},
			wantErr: true,
		},
		{
			name:    "empty cache",
			dirs:    Dirs{Data: "/a", Logs: "/b", Cache: ""},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.dirs.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultDirsUnsupportedPlatform(t *testing.T) {
	_, err := DefaultDirs()
	if err == nil {
		return
	}
	if err != ErrUnsupportedPlatform {
		t.Errorf("DefaultDirs() error = %v, want ErrUnsupportedPlatform or nil", err)
	}
}
