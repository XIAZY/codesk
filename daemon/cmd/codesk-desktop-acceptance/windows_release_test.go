package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"notty/daemon/internal/desktopacceptance"
)

const (
	testAMD64ProductCode = "{11111111-1111-4111-8111-111111111111}"
	testARM64ProductCode = "{22222222-2222-4222-8222-222222222222}"
)

func TestVerifyWindowsReleaseBindsCanonicalMSIManifestAndArtifacts(t *testing.T) {
	input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
	release, err := verifyWindowsRelease(input, testArtifactInspector("2.0.0", true))
	if err != nil {
		t.Fatal(err)
	}
	if release.Platform != "windows" || release.Version != input.Version || release.SourceRevision != input.SourceRevision ||
		release.UpgradeCode != windowsMSIUpgradeCode || release.CrossArchitecturePolicy != windowsMSICrossArchConverge ||
		release.ManifestSHA256 == "" || release.SumsSHA256 == "" || len(release.Artifacts) != 2 {
		t.Fatalf("release = %+v", release)
	}
	for _, name := range []string{"go", "rustc", "cargo", "zig", "dotnet", "wix"} {
		if release.Toolchain[name] == "" {
			t.Fatalf("toolchain[%q] is empty: %+v", name, release.Toolchain)
		}
	}
	for _, artifact := range release.Artifacts {
		if artifact.NativeFormat != "msi" || artifact.ProductCode == "" || artifact.CodeskSHA256 == "" ||
			artifact.AgentToolSHA256 == "" || !artifact.ManifestSigned || !artifact.SignaturePresent || !artifact.SignatureValid {
			t.Fatalf("artifact = %+v", artifact)
		}
	}
}

func TestVerifyWindowsReleaseRejectsSourceMismatch(t *testing.T) {
	input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
	input.SourceRevision = strings.Repeat("b", 40)
	_, err := verifyWindowsRelease(input, testArtifactInspector("2.0.0", true))
	if err == nil || !strings.Contains(err.Error(), "source revision") {
		t.Fatalf("verifyWindowsRelease error = %v", err)
	}
}

func TestVerifyWindowsReleaseRejectsRehashedNoncanonicalManifest(t *testing.T) {
	input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
	manifestPath := filepath.Join(input.Directory, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, ' ')
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteWindowsChecksums(t, input.Directory, input.Version, hashBytes(manifest))
	_, err = verifyWindowsRelease(input, testArtifactInspector("2.0.0", true))
	if err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("verifyWindowsRelease error = %v", err)
	}
}

func TestVerifyWindowsReleaseRejectsNoncanonicalChecksumOrder(t *testing.T) {
	input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
	manifest, err := os.ReadFile(filepath.Join(input.Directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	checksums := releaseChecksums(t, input.Directory, input.Version, hashBytes(manifest))
	checksums[0], checksums[2] = checksums[2], checksums[0]
	writeChecksums(t, input.Directory, checksums)
	_, err = verifyWindowsRelease(input, testArtifactInspector("2.0.0", true))
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS row") {
		t.Fatalf("verifyWindowsRelease error = %v", err)
	}
}

func TestVerifyWindowsReleaseRejectsUnexpectedEntryAndArtifactReorder(t *testing.T) {
	t.Run("unexpected entry", func(t *testing.T) {
		input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
		if err := os.WriteFile(filepath.Join(input.Directory, "unreviewed.txt"), []byte("extra"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := verifyWindowsRelease(input, testArtifactInspector("2.0.0", true))
		if err == nil || !strings.Contains(err.Error(), "entries") {
			t.Fatalf("verifyWindowsRelease error = %v", err)
		}
	})

	t.Run("artifact order", func(t *testing.T) {
		input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
		manifest := readTestManifest(t, input.Directory)
		manifest.Artifacts[0], manifest.Artifacts[1] = manifest.Artifacts[1], manifest.Artifacts[0]
		writeTestManifest(t, input.Directory, input.Version, manifest)
		_, err := verifyWindowsRelease(input, testArtifactInspector("2.0.0", true))
		if err == nil || !strings.Contains(err.Error(), "architecture") {
			t.Fatalf("verifyWindowsRelease error = %v", err)
		}
	})
}

func TestVerifyWindowsReleaseRejectsSymlinkedReleaseDirectory(t *testing.T) {
	input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
	link := filepath.Join(t.TempDir(), "release-link")
	if err := os.Symlink(input.Directory, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	input.Directory = link
	_, err := verifyWindowsRelease(input, testArtifactInspector("2.0.0", true))
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("verifyWindowsRelease error = %v", err)
	}
}

func TestVerifyWindowsReleaseRejectsUntrustedOrAmbiguousMSIIdentity(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*windowsReleaseManifest)
		inspect func(string) (artifactInspection, error)
		want    string
	}{
		{
			name: "duplicate ProductCode",
			mutate: func(manifest *windowsReleaseManifest) {
				manifest.Artifacts[1].ProductCode = manifest.Artifacts[0].ProductCode
			},
			inspect: testArtifactInspector("2.0.0", true),
			want:    "reused across architectures",
		},
		{
			name: "invalid payload digest",
			mutate: func(manifest *windowsReleaseManifest) {
				manifest.Artifacts[0].CodeskSHA256 = "not-a-digest"
			},
			inspect: testArtifactInspector("2.0.0", true),
			want:    "payload hashes",
		},
		{
			name:   "native metadata mismatch",
			mutate: func(*windowsReleaseManifest) {},
			inspect: func(string) (artifactInspection, error) {
				return artifactInspection{Architecture: "amd64", ProductName: "Other", SignaturePresent: true, SignatureValid: true}, nil
			},
			want: "metadata",
		},
		{
			name:    "native signature mismatch",
			mutate:  func(*windowsReleaseManifest) {},
			inspect: testArtifactInspector("2.0.0", false),
			want:    "signature state",
		},
		{
			name: "invalid cross architecture policy",
			mutate: func(manifest *windowsReleaseManifest) {
				manifest.CrossArchitecturePolicy = "hope"
			},
			inspect: testArtifactInspector("2.0.0", true),
			want:    "cross-architecture policy",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
			manifest := readTestManifest(t, input.Directory)
			test.mutate(&manifest)
			writeTestManifest(t, input.Directory, input.Version, manifest)
			_, err := verifyWindowsRelease(input, test.inspect)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyWindowsRelease error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateMSIVersion(t *testing.T) {
	for _, value := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.256.3", "1.2.65536", "1.2.-1"} {
		if err := validateMSIVersion(value); err == nil {
			t.Errorf("validateMSIVersion(%q) succeeded", value)
		}
	}
	if err := validateMSIVersion("255.255.65535"); err != nil {
		t.Fatal(err)
	}
}

func writeWindowsRelease(t *testing.T, version, sourceRevision string, signed bool) desktopacceptance.ReleaseInput {
	t.Helper()
	directory := t.TempDir()
	manifest := windowsReleaseManifest{
		SchemaVersion:           windowsMSIManifestSchema,
		Version:                 version,
		SourceRevision:          sourceRevision,
		UpgradeCode:             windowsMSIUpgradeCode,
		CrossArchitecturePolicy: windowsMSICrossArchConverge,
		Signed:                  signed,
		Toolchain:               expectedWindowsReleaseToolchain,
	}
	manifest.Toolchain.Dotnet = "8.0.419"
	for index, architecture := range []string{"amd64", "arm64"} {
		name := windowsMSIFilename(version, architecture)
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("test-msi-"+architecture), 0o600); err != nil {
			t.Fatal(err)
		}
		hash, err := fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		productCode := testAMD64ProductCode
		if architecture == "arm64" {
			productCode = testARM64ProductCode
		}
		manifest.Artifacts = append(manifest.Artifacts, windowsReleaseArtifact{
			Arch:            architecture,
			File:            name,
			SHA256:          hash,
			Signed:          signed,
			ProductCode:     productCode,
			CodeskSHA256:    strings.Repeat(string(rune('c'+index)), 64),
			AgentToolSHA256: strings.Repeat(string(rune('e'+index)), 64),
		})
	}
	writeTestManifest(t, directory, version, manifest)
	return desktopacceptance.ReleaseInput{Directory: directory, Version: version, SourceRevision: sourceRevision}
}

func readTestManifest(t *testing.T, directory string) windowsReleaseManifest {
	t.Helper()
	var manifest windowsReleaseManifest
	if _, err := readCanonicalJSON(filepath.Join(directory, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeTestManifest(t *testing.T, directory, version string, manifest windowsReleaseManifest) {
	t.Helper()
	data, err := marshalCanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteWindowsChecksums(t, directory, version, hashBytes(data))
}

func rewriteWindowsChecksums(t *testing.T, directory, version, manifestHash string) {
	t.Helper()
	writeChecksums(t, directory, releaseChecksums(t, directory, version, manifestHash))
}

func releaseChecksums(t *testing.T, directory, version, manifestHash string) []releaseChecksum {
	t.Helper()
	checksums := make([]releaseChecksum, 0, 3)
	for _, architecture := range []string{"amd64", "arm64"} {
		name := windowsMSIFilename(version, architecture)
		hash, err := fileSHA256(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		checksums = append(checksums, releaseChecksum{SHA256: hash, File: name})
	}
	return append(checksums, releaseChecksum{SHA256: manifestHash, File: "manifest.json"})
}

func writeChecksums(t *testing.T, directory string, checksums []releaseChecksum) {
	t.Helper()
	var data strings.Builder
	for _, checksum := range checksums {
		data.WriteString(checksum.SHA256)
		data.WriteString("  ")
		data.WriteString(checksum.File)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(data.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testArtifactInspector(version string, signed bool) func(string) (artifactInspection, error) {
	return func(path string) (artifactInspection, error) {
		architecture := "amd64"
		productCode := testAMD64ProductCode
		if strings.Contains(filepath.Base(path), "_arm64.msi") {
			architecture = "arm64"
			productCode = testARM64ProductCode
		}
		return artifactInspection{
			Architecture:     architecture,
			ProductName:      "Codesk",
			Manufacturer:     "Codesk",
			ProductVersion:   version,
			ProductCode:      productCode,
			UpgradeCode:      windowsMSIUpgradeCode,
			PerUser:          true,
			SignaturePresent: signed,
			SignatureValid:   signed,
		}, nil
	}
}
