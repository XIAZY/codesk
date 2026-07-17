package desktop

import (
	"notty/daemon/internal/syncer"
	"path/filepath"
	"strings"
	"testing"
)

func validDirs(t *testing.T) Dirs {
	t.Helper()
	root := t.TempDir()
	return Dirs{
		Data:  filepath.Join(root, "data"),
		Logs:  filepath.Join(root, "logs"),
		Cache: filepath.Join(root, "cache"),
	}
}

func mustSyncerConfig(t *testing.T, dirs Dirs, backendURL, workspaceID, daemonToken, daemonVersion string) syncer.Config {
	t.Helper()
	cfg, err := SyncerConfig(dirs, backendURL, workspaceID, daemonToken, daemonVersion)
	if err != nil {
		t.Fatalf("SyncerConfig() unexpected error: %v", err)
	}
	return cfg
}

func TestSyncerConfigNeverReferencesCLIPaths(t *testing.T) {
	dirs := validDirs(t)
	cfg := mustSyncerConfig(t, dirs, "https://api.getcodesk.com", "ws-1", "tok-1", "1.0.0")

	cliPaths := []string{".notty", string(filepath.Separator) + "notty"}
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

func TestLoginItemRegistrationRequiresExactNonemptyCommand(t *testing.T) {
	want := `"C:\\Users\\me\\AppData\\Local\\Codesk\\Codesk.exe"`
	for _, test := range []struct {
		name   string
		actual string
		want   bool
	}{
		{name: "exact command", actual: want, want: true},
		{name: "empty MSI sentinel is disabled", actual: "", want: false},
		{name: "stale executable path is disabled", actual: `"C:\\old\\Codesk.exe"`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := loginItemRegistrationMatches(test.actual, want); got != test.want {
				t.Fatalf("loginItemRegistrationMatches(%q, %q) = %t, want %t", test.actual, want, got, test.want)
			}
		})
	}
}

func TestSyncerConfigAllPathsUnderDataDir(t *testing.T) {
	dirs := validDirs(t)
	cfg := mustSyncerConfig(t, dirs, "https://api.getcodesk.com", "ws-1", "tok-1", "1.0.0")

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
	dirs := validDirs(t)
	cfg := mustSyncerConfig(t, dirs, "https://backend", "workspace-id", "secret-token", "2.0.0")

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
	if cfg.DataDir != dirs.Data {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, dirs.Data)
	}
	if cfg.WorkspaceDir != filepath.Join(dirs.Data, "workspace") {
		t.Errorf("WorkspaceDir = %q, want %q", cfg.WorkspaceDir, filepath.Join(dirs.Data, "workspace"))
	}
	if cfg.AgentWorkspaceRoot != filepath.Join(dirs.Data, "agents") {
		t.Errorf("AgentWorkspaceRoot = %q, want %q", cfg.AgentWorkspaceRoot, filepath.Join(dirs.Data, "agents"))
	}
	if cfg.CodexCommand != "codex" {
		t.Errorf("CodexCommand = %q, want %q", cfg.CodexCommand, "codex")
	}
	if cfg.ClaudeCommand != "claude" {
		t.Errorf("ClaudeCommand = %q, want %q", cfg.ClaudeCommand, "claude")
	}
	if cfg.AgentToolBaseURL != "http://127.0.0.1:7778" {
		t.Errorf("AgentToolBaseURL = %q, want %q", cfg.AgentToolBaseURL, "http://127.0.0.1:7778")
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
		t.Setenv(key, filepath.Join(t.TempDir(), "poisoned-"+key))
	}

	dirs := validDirs(t)
	cfg := mustSyncerConfig(t, dirs, "https://clean.api", "ws-clean", "tok-clean", "1.0.0")

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

func TestSyncerConfigRejectsInvalidDirs(t *testing.T) {
	base := validDirs(t)
	tests := []struct {
		name string
		dirs Dirs
	}{
		{"zero dirs", Dirs{}},
		{"whitespace data", Dirs{Data: "  ", Logs: base.Logs, Cache: base.Cache}},
		{"whitespace logs", Dirs{Data: base.Data, Logs: "\t", Cache: base.Cache}},
		{"whitespace cache", Dirs{Data: base.Data, Logs: base.Logs, Cache: " \n "}},
		{"relative data", Dirs{Data: "relative/path", Logs: base.Logs, Cache: base.Cache}},
		{"relative logs", Dirs{Data: base.Data, Logs: "relative", Cache: base.Cache}},
		{"relative cache", Dirs{Data: base.Data, Logs: base.Logs, Cache: "relative"}},
		{"padded data", Dirs{Data: " " + base.Data + " ", Logs: base.Logs, Cache: base.Cache}},
		{"padded logs", Dirs{Data: base.Data, Logs: " " + base.Logs + " ", Cache: base.Cache}},
		{"padded cache", Dirs{Data: base.Data, Logs: base.Logs, Cache: " " + base.Cache + " "}},
		{"trailing space data", Dirs{Data: base.Data + " ", Logs: base.Logs, Cache: base.Cache}},
		{"trailing space logs", Dirs{Data: base.Data, Logs: base.Logs + " ", Cache: base.Cache}},
		{"trailing space cache", Dirs{Data: base.Data, Logs: base.Logs, Cache: base.Cache + " "}},
		{"leading space data", Dirs{Data: " " + base.Data, Logs: base.Logs, Cache: base.Cache}},
		{"dotdot data", Dirs{Data: base.Data + "/../other", Logs: base.Logs, Cache: base.Cache}},
		{"dotdot logs", Dirs{Data: base.Data, Logs: base.Logs + "/../other", Cache: base.Cache}},
		{"dotdot cache", Dirs{Data: base.Data, Logs: base.Logs, Cache: base.Cache + "/../other"}},
		{"dot data", Dirs{Data: base.Data + "/./sub", Logs: base.Logs, Cache: base.Cache}},
		{"redundant sep data", Dirs{Data: base.Data + "//sub", Logs: base.Logs, Cache: base.Cache}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := SyncerConfig(tt.dirs, "https://api", "ws", "tok", "1.0")
			if err == nil {
				t.Errorf("SyncerConfig() should reject invalid dirs, got DataDir=%q", cfg.DataDir)
			}
		})
	}
}

func TestSyncerConfigSuccessInvariants(t *testing.T) {
	dirs := validDirs(t)
	cfg, err := SyncerConfig(dirs, "https://api", "ws", "tok", "1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		t.Error("successful SyncerConfig returned empty DataDir")
	}
	for name, path := range map[string]string{
		"DataDir":            cfg.DataDir,
		"WorkspaceDir":       cfg.WorkspaceDir,
		"AgentWorkspaceRoot": cfg.AgentWorkspaceRoot,
	} {
		if path != filepath.Clean(path) {
			t.Errorf("%s = %q is not lexically clean (want %q)", name, path, filepath.Clean(path))
		}
	}
}

func TestDirsValidate(t *testing.T) {
	base := validDirs(t)
	tests := []struct {
		name    string
		dirs    Dirs
		wantErr bool
	}{
		{
			name:    "valid",
			dirs:    base,
			wantErr: false,
		},
		{
			name:    "empty data",
			dirs:    Dirs{Data: "", Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "empty logs",
			dirs:    Dirs{Data: base.Data, Logs: "", Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "empty cache",
			dirs:    Dirs{Data: base.Data, Logs: base.Logs, Cache: ""},
			wantErr: true,
		},
		{
			name:    "whitespace data",
			dirs:    Dirs{Data: "  ", Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "relative data",
			dirs:    Dirs{Data: "relative", Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "relative logs",
			dirs:    Dirs{Data: base.Data, Logs: "relative", Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "relative cache",
			dirs:    Dirs{Data: base.Data, Logs: base.Logs, Cache: "relative"},
			wantErr: true,
		},
		{
			name:    "padded data",
			dirs:    Dirs{Data: " " + base.Data + " ", Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "padded logs",
			dirs:    Dirs{Data: base.Data, Logs: " " + base.Logs + " ", Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "padded cache",
			dirs:    Dirs{Data: base.Data, Logs: base.Logs, Cache: " " + base.Cache + " "},
			wantErr: true,
		},
		{
			name:    "trailing space data",
			dirs:    Dirs{Data: base.Data + " ", Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "leading space data",
			dirs:    Dirs{Data: " " + base.Data, Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "dotdot data",
			dirs:    Dirs{Data: base.Data + "/../other", Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "dot data",
			dirs:    Dirs{Data: base.Data + "/./sub", Logs: base.Logs, Cache: base.Cache},
			wantErr: true,
		},
		{
			name:    "redundant sep",
			dirs:    Dirs{Data: base.Data + "//sub", Logs: base.Logs, Cache: base.Cache},
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
