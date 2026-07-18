package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"notty/daemon/internal/desktoprelease"
)

const testProductRevision = "0ae65b90d2fcfba9eef9ceaf920760e801c449cc"

func TestRunWritesCanonicalRelease(t *testing.T) {
	fixture := newReleaseFixture(t, "0.0.2")
	withBuiltProductRevision(t, testProductRevision)

	if err := run(fixture.arguments()); err != nil {
		t.Fatalf("run: %v", err)
	}

	amd64MSIHash := desktoprelease.SHA256([]byte("amd64-msi"))
	arm64MSIHash := desktoprelease.SHA256([]byte("arm64-msi"))
	amd64CodeskHash := desktoprelease.SHA256([]byte("amd64-codesk"))
	arm64CodeskHash := desktoprelease.SHA256([]byte("arm64-codesk"))
	amd64AgentHash := desktoprelease.SHA256([]byte("amd64-agent"))
	arm64AgentHash := desktoprelease.SHA256([]byte("arm64-agent"))
	wantManifest := fmt.Sprintf(`{
  "schema_version": 1,
  "version": "0.0.2",
  "source_revision": "%s",
  "upgrade_code": "{0C8C0BBA-06EE-43BA-BC34-768B9B740A09}",
  "cross_architecture_policy": "converge",
  "signed": false,
  "toolchain": {
    "go": "go1.26.5",
    "rustc": "rustc 1.97.0 (2d8144b78 2026-07-07)",
    "cargo": "cargo 1.97.0 (c980f4866 2026-06-30)",
    "zig": "0.16.0",
    "dotnet": "8.0.419",
    "wix": "4.0.5"
  },
  "artifacts": [
    {
      "arch": "amd64",
      "file": "CodeskMSI_0.0.2_windows_amd64.msi",
      "sha256": "%s",
      "signed": false,
      "product_code": "{F7EFC1E1-CF36-4BAD-9188-5B8145D94289}",
      "codesk_sha256": "%s",
      "agent_tool_sha256": "%s"
    },
    {
      "arch": "arm64",
      "file": "CodeskMSI_0.0.2_windows_arm64.msi",
      "sha256": "%s",
      "signed": false,
      "product_code": "{3E947E2D-775C-4580-827D-4DC7368186F4}",
      "codesk_sha256": "%s",
      "agent_tool_sha256": "%s"
    }
  ]
}
`, testProductRevision, amd64MSIHash, amd64CodeskHash, amd64AgentHash,
		arm64MSIHash, arm64CodeskHash, arm64AgentHash)
	manifest := mustRead(t, filepath.Join(fixture.output, "manifest.json"))
	if string(manifest) != wantManifest {
		t.Fatalf("manifest mismatch\n got:\n%s\nwant:\n%s", manifest, wantManifest)
	}
	wantSums := fmt.Sprintf("%s  CodeskMSI_0.0.2_windows_amd64.msi\n%s  CodeskMSI_0.0.2_windows_arm64.msi\n%s  manifest.json\n",
		amd64MSIHash, arm64MSIHash, desktoprelease.SHA256(manifest))
	if sums := string(mustRead(t, filepath.Join(fixture.output, "SHA256SUMS"))); sums != wantSums {
		t.Fatalf("SHA256SUMS mismatch\n got:\n%s\nwant:\n%s", sums, wantSums)
	}
	if err := run(fixture.arguments()); err == nil || !strings.Contains(err.Error(), "has 4 entries, want 2") {
		t.Fatalf("second publication error = %v, want fail-closed existing output", err)
	}
}

func TestRunRejectsUnboundOrMismatchedSource(t *testing.T) {
	fixture := newReleaseFixture(t, "0.0.2")

	withBuiltProductRevision(t, "")
	if err := run(fixture.arguments()); err == nil || !strings.Contains(err.Error(), "not source-bound") {
		t.Fatalf("unbound error = %v", err)
	}

	withBuiltProductRevision(t, "1111111111111111111111111111111111111111")
	if err := run(fixture.arguments()); err == nil || !strings.Contains(err.Error(), "does not match producer-bound") {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestRunRejectsInvalidReleaseInputsBeforePublication(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*releaseFixture)
		match  string
	}{
		{
			name: "invalid dotnet version",
			mutate: func(fixture *releaseFixture) {
				fixture.dotnetVersion = "9.0.100"
			},
			match: "not a stable 8.0 SDK version",
		},
		{
			name: "duplicate ProductCode",
			mutate: func(fixture *releaseFixture) {
				fixture.arm64ProductCode = fixture.amd64ProductCode
			},
			match: "reused across architectures",
		},
		{
			name: "noncanonical ProductCode",
			mutate: func(fixture *releaseFixture) {
				fixture.arm64ProductCode = "3E947E2D-775C-4580-827D-4DC7368186F4"
			},
			match: "not a canonical uppercase MSI GUID",
		},
		{
			name: "wrong MSI basename",
			mutate: func(fixture *releaseFixture) {
				wrong := filepath.Join(fixture.output, "wrong.msi")
				if err := os.Rename(fixture.amd64MSI, wrong); err != nil {
					fixture.t.Fatal(err)
				}
				fixture.amd64MSI = wrong
			},
			match: "MSI is named",
		},
		{
			name: "unexpected output entry",
			mutate: func(fixture *releaseFixture) {
				writeFixtureFile(fixture.t, filepath.Join(fixture.output, "extra"), "extra")
			},
			match: "has 3 entries, want 2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReleaseFixture(t, "0.0.2")
			test.mutate(fixture)
			withBuiltProductRevision(t, testProductRevision)
			err := run(fixture.arguments())
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("error = %v, want substring %q", err, test.match)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.output, "manifest.json")); !os.IsNotExist(statErr) {
				t.Fatalf("manifest was published on failure: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.output, "SHA256SUMS")); !os.IsNotExist(statErr) {
				t.Fatalf("SHA256SUMS was published on failure: %v", statErr)
			}
		})
	}
}

func TestValidateMSIVersion(t *testing.T) {
	for _, value := range []string{"0.0.0", "255.255.65535", "1.2.3"} {
		if err := validateMSIVersion(value); err != nil {
			t.Errorf("validateMSIVersion(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "256.0.0", "0.256.0", "0.0.65536", " 1.2.3", "1.2.-1"} {
		if err := validateMSIVersion(value); err == nil {
			t.Errorf("validateMSIVersion(%q) succeeded", value)
		}
	}
}

type releaseFixture struct {
	t                *testing.T
	output           string
	version          string
	dotnetVersion    string
	amd64MSI         string
	arm64MSI         string
	amd64Codesk      string
	arm64Codesk      string
	amd64Agent       string
	arm64Agent       string
	amd64ProductCode string
	arm64ProductCode string
}

func newReleaseFixture(t *testing.T, version string) *releaseFixture {
	t.Helper()
	root := t.TempDir()
	output := filepath.Join(root, "release")
	if err := os.Mkdir(output, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &releaseFixture{
		t: t, output: output, version: version, dotnetVersion: "8.0.419",
		amd64MSI:         filepath.Join(output, msiFilename(version, "amd64")),
		arm64MSI:         filepath.Join(output, msiFilename(version, "arm64")),
		amd64Codesk:      filepath.Join(root, "payload", "amd64", "Codesk.exe"),
		arm64Codesk:      filepath.Join(root, "payload", "arm64", "Codesk.exe"),
		amd64Agent:       filepath.Join(root, "payload", "amd64", "notty-agent-tool.exe"),
		arm64Agent:       filepath.Join(root, "payload", "arm64", "notty-agent-tool.exe"),
		amd64ProductCode: "{F7EFC1E1-CF36-4BAD-9188-5B8145D94289}",
		arm64ProductCode: "{3E947E2D-775C-4580-827D-4DC7368186F4}",
	}
	writeFixtureFile(t, fixture.amd64MSI, "amd64-msi")
	writeFixtureFile(t, fixture.arm64MSI, "arm64-msi")
	writeFixtureFile(t, fixture.amd64Codesk, "amd64-codesk")
	writeFixtureFile(t, fixture.arm64Codesk, "arm64-codesk")
	writeFixtureFile(t, fixture.amd64Agent, "amd64-agent")
	writeFixtureFile(t, fixture.arm64Agent, "arm64-agent")
	return fixture
}

func (fixture *releaseFixture) arguments() []string {
	return []string{
		"--output", fixture.output,
		"--version", fixture.version,
		"--source-revision", testProductRevision,
		"--dotnet-version", fixture.dotnetVersion,
		"--amd64-msi", fixture.amd64MSI,
		"--amd64-product-code", fixture.amd64ProductCode,
		"--amd64-codesk", fixture.amd64Codesk,
		"--amd64-agent", fixture.amd64Agent,
		"--arm64-msi", fixture.arm64MSI,
		"--arm64-product-code", fixture.arm64ProductCode,
		"--arm64-codesk", fixture.arm64Codesk,
		"--arm64-agent", fixture.arm64Agent,
	}
}

func writeFixtureFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func withBuiltProductRevision(t *testing.T, revision string) {
	t.Helper()
	previous := builtProductRevision
	builtProductRevision = revision
	t.Cleanup(func() { builtProductRevision = previous })
}
