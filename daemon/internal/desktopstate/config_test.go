package desktopstate

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
	actualFingerprint, err := store.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	wantFingerprint := fingerprint(data)
	if actualFingerprint != wantFingerprint {
		t.Fatalf("Fingerprint() = %#v, want exact persisted bytes %#v", actualFingerprint, wantFingerprint)
	}
	if !actualFingerprint.Present || strings.Contains(actualFingerprint.SHA256, "opaque") || actualFingerprint.Size != int64(len(data)) {
		t.Fatalf("Fingerprint() exposed content or wrong size: %#v", actualFingerprint)
	}
}

func TestFileConfigurationStoreFingerprintsExactValidatedBytes(t *testing.T) {
	store, err := NewFileConfigurationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("{\n  \"workspace_url\": \"https://app.getcodesk.com/w/product\",\n  \"workspace_slug\": \"product\",\n  \"workspace_name\": \"Product\",\n  \"workspace_id\": \"workspace-1\",\n  \"daemon_id\": \"daemon-1\"\n}\n")
	if err := os.WriteFile(store.path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	actual, err := store.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if want := fingerprint(body); actual != want {
		t.Fatalf("Fingerprint() = %#v, want exact validated persisted bytes %#v", actual, want)
	}
}

func TestFileConfigurationStoreFingerprintRejectsInvalidStateWithoutLeakingIt(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileConfigurationStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const sensitive = "must-not-leak-token-value"
	body := []byte(`{"daemon_id":"d","workspace_id":"w","workspace_name":"n","workspace_slug":"s","workspace_url":"https://app.getcodesk.com/w/s","` + sensitive + `":"x"}`)
	if err := os.WriteFile(store.path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fingerprint(); err == nil {
		t.Fatal("Fingerprint() accepted invalid configuration")
	} else if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), sensitive) {
		t.Fatalf("Fingerprint() error exposed path or persisted content: %v", err)
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
	fingerprint, err := store.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint.Present || fingerprint.SHA256 != "" || fingerprint.Size != 0 {
		t.Fatalf("Fingerprint() after delete = %#v, want absent", fingerprint)
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
