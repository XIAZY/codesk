package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type desktopManifestAsset struct {
	Filename  string                 `json:"filename"`
	SHA256    string                 `json:"sha256"`
	SizeBytes int64                  `json:"size_bytes"`
	Signing   map[string]interface{} `json:"signing"`
}

type desktopReleaseManifest struct {
	SchemaVersion int                             `json:"schema_version"`
	Version       string                          `json:"version"`
	ReleaseTag    string                          `json:"release_tag"`
	Assets        map[string]desktopManifestAsset `json:"assets"`
}

// writeAsset creates a fake asset file with deterministic content and returns its
// path plus the sha256 the manifest must report for it.
func writeAsset(t *testing.T, dir, name, content string) (string, string, int64) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	return p, sum, int64(len(content))
}

func runBuildManifest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"./build-release-manifest.sh"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestBuildReleaseManifestEmitsFrozenSchema verifies a full happy-path manifest:
// schema_version, normalized version, immutable release_tag, exactly the three
// frozen asset keys with correct versionless filenames, digests computed on the
// real bytes, positive sizes, and platform-appropriate signing state.
func TestBuildReleaseManifestEmitsFrozenSchema(t *testing.T) {
	dir := t.TempDir()
	macos, macosSum, macosSize := writeAsset(t, dir, "mac.dmg", "fake-universal-dmg-bytes")
	amd64, amd64Sum, amd64Size := writeAsset(t, dir, "amd64.msi", "fake-amd64-msi")
	arm64, arm64Sum, arm64Size := writeAsset(t, dir, "arm64.msi", "fake-arm64-msi-payload-xyz")
	outPath := filepath.Join(dir, "manifest.json")

	if out, err := runBuildManifest(t,
		"--version", "1.2.3", "--release-tag", "desktop-v1.2.3",
		"--macos-universal", macos, "--windows-amd64", amd64, "--windows-arm64", arm64,
		"--out", outPath,
	); err != nil {
		t.Fatalf("expected success, got error: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m desktopReleaseManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\n%s", err, raw)
	}

	if m.SchemaVersion != 1 {
		t.Errorf("schema_version: want 1, got %d", m.SchemaVersion)
	}
	if m.Version != "1.2.3" {
		t.Errorf("version: want 1.2.3, got %q", m.Version)
	}
	if m.ReleaseTag != "desktop-v1.2.3" {
		t.Errorf("release_tag: want desktop-v1.2.3, got %q", m.ReleaseTag)
	}
	if len(m.Assets) != 3 {
		t.Fatalf("want exactly 3 assets, got %d: %v", len(m.Assets), m.Assets)
	}

	expect := map[string]struct {
		filename string
		sum      string
		size     int64
		signKey  string
	}{
		"macos-universal": {"Codesk-macos-universal.dmg", macosSum, macosSize, "signed_and_notarized"},
		"windows-amd64":   {"Codesk-windows-amd64.msi", amd64Sum, amd64Size, "signed"},
		"windows-arm64":   {"Codesk-windows-arm64.msi", arm64Sum, arm64Size, "signed"},
	}
	for key, want := range expect {
		got, ok := m.Assets[key]
		if !ok {
			t.Errorf("missing asset key %q", key)
			continue
		}
		if got.Filename != want.filename {
			t.Errorf("%s filename: want %q, got %q", key, want.filename, got.Filename)
		}
		if got.SHA256 != want.sum {
			t.Errorf("%s sha256: want %q, got %q", key, want.sum, got.SHA256)
		}
		if got.SizeBytes != want.size {
			t.Errorf("%s size_bytes: want %d, got %d", key, want.size, got.SizeBytes)
		}
		v, ok := got.Signing[want.signKey]
		if !ok {
			t.Errorf("%s signing missing key %q: %v", key, want.signKey, got.Signing)
			continue
		}
		if b, isBool := v.(bool); !isBool || b != false {
			t.Errorf("%s signing.%s: want bool false, got %v", key, want.signKey, v)
		}
	}
}

// TestBuildReleaseManifestRefusesPartialRelease is the all-three-or-nothing
// guard: if any required asset is missing, the script must fail and write no
// manifest file (a partial manifest would produce a dead download link).
func TestBuildReleaseManifestRefusesPartialRelease(t *testing.T) {
	dir := t.TempDir()
	macos, _, _ := writeAsset(t, dir, "mac.dmg", "dmg")
	amd64, _, _ := writeAsset(t, dir, "amd64.msi", "amd64")
	missing := filepath.Join(dir, "does-not-exist.msi")
	outPath := filepath.Join(dir, "manifest.json")

	out, err := runBuildManifest(t,
		"--version", "1.2.3", "--release-tag", "desktop-v1.2.3",
		"--macos-universal", macos, "--windows-amd64", amd64, "--windows-arm64", missing,
		"--out", outPath,
	)
	if err == nil {
		t.Fatalf("expected failure for missing arm64 asset, but succeeded:\n%s", out)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("no manifest file must be written on partial release, but %s exists", outPath)
	}
}

// TestBuildReleaseManifestRejectsEmptyAsset guards against a zero-byte asset
// (e.g. a truncated build output) being published with size_bytes 0.
func TestBuildReleaseManifestRejectsEmptyAsset(t *testing.T) {
	dir := t.TempDir()
	macos, _, _ := writeAsset(t, dir, "mac.dmg", "dmg")
	amd64, _, _ := writeAsset(t, dir, "amd64.msi", "amd64")
	empty, _, _ := writeAsset(t, dir, "arm64.msi", "")
	outPath := filepath.Join(dir, "manifest.json")

	if out, err := runBuildManifest(t,
		"--version", "1.2.3", "--release-tag", "desktop-v1.2.3",
		"--macos-universal", macos, "--windows-amd64", amd64, "--windows-arm64", empty,
		"--out", outPath,
	); err == nil {
		t.Fatalf("expected failure for empty asset, but succeeded:\n%s", out)
	}
}

// TestBuildReleaseManifestRejectsBadSigningBool ensures the signing flags are
// strictly boolean so the emitted JSON keeps stable resolver types.
func TestBuildReleaseManifestRejectsBadSigningBool(t *testing.T) {
	dir := t.TempDir()
	macos, _, _ := writeAsset(t, dir, "mac.dmg", "dmg")
	amd64, _, _ := writeAsset(t, dir, "amd64.msi", "amd64")
	arm64, _, _ := writeAsset(t, dir, "arm64.msi", "arm64")
	outPath := filepath.Join(dir, "manifest.json")

	if out, err := runBuildManifest(t,
		"--version", "1.2.3", "--release-tag", "desktop-v1.2.3",
		"--macos-universal", macos, "--windows-amd64", amd64, "--windows-arm64", arm64,
		"--windows-signed", "yes",
		"--out", outPath,
	); err == nil {
		t.Fatalf("expected failure for non-boolean signing flag, but succeeded:\n%s", out)
	}
}
