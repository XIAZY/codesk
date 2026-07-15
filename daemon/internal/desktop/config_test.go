package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfiguration() Configuration {
	return Configuration{
		DaemonID:      "daemon-1",
		WorkspaceID:   "workspace-1",
		WorkspaceName: "Product",
		WorkspaceSlug: "product",
		WorkspaceURL:  "https://app.getcodesk.com/w/product",
	}
}

func TestFileConfigurationStoreRoundTripContainsNoTokenField(t *testing.T) {
	store, err := NewFileConfigurationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := validConfiguration()
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(data)), "token") {
		t.Fatalf("configuration contains a token field: %s", data)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != config {
		t.Fatalf("Load() = %#v, want %#v", loaded, config)
	}
}

func TestFileConfigurationStoreRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, body := range []string{
		`{"daemon_id":"d","workspace_id":"w","workspace_name":"n","workspace_slug":"s","workspace_url":"https://app.getcodesk.com/w/s","token":"secret"}`,
		`{"daemon_id":"d","workspace_id":"w","workspace_name":"n","workspace_slug":"s","workspace_url":"https://app.getcodesk.com/w/s"} {}`,
	} {
		store, err := NewFileConfigurationStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatalf("Load() accepted %q", body)
		}
	}
}

func TestFileConfigurationStoreDeleteIsIdempotent(t *testing.T) {
	store, err := NewFileConfigurationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(validConfiguration()); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() error = %v, want os.ErrNotExist", err)
	}
}

func TestConfigurationValidateRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Configuration)
	}{
		{"empty ID", func(c *Configuration) { c.DaemonID = "" }},
		{"padded name", func(c *Configuration) { c.WorkspaceName = " Product" }},
		{"control slug", func(c *Configuration) { c.WorkspaceSlug = "bad\nslug" }},
		{"relative URL", func(c *Configuration) { c.WorkspaceURL = "/w/product" }},
		{"FTP URL", func(c *Configuration) { c.WorkspaceURL = "ftp://app.getcodesk.com/w/product" }},
		{"custom-scheme URL", func(c *Configuration) { c.WorkspaceURL = "codesk://app.getcodesk.com/w/product" }},
		{"credential URL", func(c *Configuration) { c.WorkspaceURL = "https://user@app.getcodesk.com/w/product" }},
		{"query URL", func(c *Configuration) { c.WorkspaceURL = "https://app.getcodesk.com/w/product?token=x" }},
		{"fragment URL", func(c *Configuration) { c.WorkspaceURL = "https://app.getcodesk.com/w/product#x" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validConfiguration()
			tt.mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatalf("Validate() accepted %#v", config)
			}
		})
	}
}

func TestNewFileConfigurationStoreRejectsRelativeDataDir(t *testing.T) {
	if _, err := NewFileConfigurationStore(filepath.Join("relative", "data")); err == nil {
		t.Fatal("NewFileConfigurationStore() accepted relative data dir")
	}
}
