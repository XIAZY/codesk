package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"notty/daemon/internal/desktopacceptance"
	"notty/daemon/internal/desktoprelease"
)

func TestVerifyWindowsReleaseBindsCanonicalSourceManifestAndArtifacts(t *testing.T) {
	input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
	release, err := verifyWindowsRelease(input, trustedArtifactInspector)
	if err != nil {
		t.Fatal(err)
	}
	if release.Platform != "windows" || release.Version != input.Version || release.SourceRevision != input.SourceRevision ||
		release.ManifestSHA256 == "" || release.SumsSHA256 == "" || len(release.Artifacts) != 2 {
		t.Fatalf("release = %+v", release)
	}
	for _, name := range []string{"go", "rustc", "cargo", "zig", "go-winres"} {
		if release.Toolchain[name] == "" {
			t.Fatalf("toolchain[%q] is empty: %+v", name, release.Toolchain)
		}
	}
}

func TestVerifyWindowsReleaseRejectsSourceMismatch(t *testing.T) {
	input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
	input.SourceRevision = strings.Repeat("b", 40)
	_, err := verifyWindowsRelease(input, trustedArtifactInspector)
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
	rewriteWindowsChecksums(t, input.Directory, input.Version, desktoprelease.SHA256(manifest))
	_, err = verifyWindowsRelease(input, trustedArtifactInspector)
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
	checksums := releaseChecksums(t, input.Directory, input.Version, desktoprelease.SHA256(manifest))
	checksums[0], checksums[2] = checksums[2], checksums[0]
	data, err := desktoprelease.MarshalChecksums(checksums)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.Directory, "SHA256SUMS"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = verifyWindowsRelease(input, trustedArtifactInspector)
	if err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("verifyWindowsRelease error = %v", err)
	}
}

func TestVerifyWindowsReleaseRejectsUnexpectedEntryAndArtifactReorder(t *testing.T) {
	t.Run("unexpected entry", func(t *testing.T) {
		input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
		if err := os.WriteFile(filepath.Join(input.Directory, "unreviewed.txt"), []byte("extra"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := verifyWindowsRelease(input, trustedArtifactInspector)
		if err == nil || !strings.Contains(err.Error(), "entries") {
			t.Fatalf("verifyWindowsRelease error = %v", err)
		}
	})

	t.Run("artifact order", func(t *testing.T) {
		input := writeWindowsRelease(t, "2.0.0", strings.Repeat("a", 40), true)
		manifestPath := filepath.Join(input.Directory, "manifest.json")
		var manifest windowsReleaseManifest
		if _, err := desktoprelease.ReadCanonicalJSON(manifestPath, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Artifacts[0], manifest.Artifacts[1] = manifest.Artifacts[1], manifest.Artifacts[0]
		data, err := desktoprelease.MarshalCanonicalJSON(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteWindowsChecksums(t, input.Directory, input.Version, desktoprelease.SHA256(data))
		_, err = verifyWindowsRelease(input, trustedArtifactInspector)
		if err == nil || !strings.Contains(err.Error(), "architecture") {
			t.Fatalf("verifyWindowsRelease error = %v", err)
		}
	})
}

func writeWindowsRelease(t *testing.T, version, sourceRevision string, signed bool) desktopacceptance.ReleaseInput {
	t.Helper()
	directory := t.TempDir()
	manifest := windowsReleaseManifest{
		Version:        version,
		SourceRevision: sourceRevision,
		Signed:         signed,
		Toolchain:      expectedWindowsReleaseToolchain,
	}
	for _, architecture := range []string{"amd64", "arm64"} {
		name := windowsSetupFilename(version, architecture)
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, minimalPE(architecture), 0o600); err != nil {
			t.Fatal(err)
		}
		hash, err := desktoprelease.FileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Artifacts = append(manifest.Artifacts, windowsReleaseArtifact{
			Arch: architecture, File: name, SHA256: hash, Signed: signed,
		})
	}
	manifestData, err := desktoprelease.MarshalCanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteWindowsChecksums(t, directory, version, desktoprelease.SHA256(manifestData))
	return desktopacceptance.ReleaseInput{Directory: directory, Version: version, SourceRevision: sourceRevision}
}

func rewriteWindowsChecksums(t *testing.T, directory, version, manifestHash string) {
	t.Helper()
	checksums := releaseChecksums(t, directory, version, manifestHash)
	data, err := desktoprelease.MarshalChecksums(checksums)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func releaseChecksums(t *testing.T, directory, version, manifestHash string) []desktoprelease.Checksum {
	t.Helper()
	checksums := make([]desktoprelease.Checksum, 0, 3)
	for _, architecture := range []string{"amd64", "arm64"} {
		name := windowsSetupFilename(version, architecture)
		hash, err := desktoprelease.FileSHA256(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		checksums = append(checksums, desktoprelease.Checksum{SHA256: hash, File: name})
	}
	return append(checksums, desktoprelease.Checksum{SHA256: manifestHash, File: "manifest.json"})
}

func trustedArtifactInspector(path string) (artifactInspection, error) {
	architecture, err := peArchitecture(path)
	return artifactInspection{Architecture: architecture, SignatureValid: true}, err
}

func minimalPE(architecture string) []byte {
	data := make([]byte, 0x98)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:], 0x80)
	copy(data[0x80:], "PE\x00\x00")
	machine := uint16(0x8664)
	if architecture == "arm64" {
		machine = 0xaa64
	}
	binary.LittleEndian.PutUint16(data[0x84:], machine)
	binary.LittleEndian.PutUint16(data[0x86:], 0)
	binary.LittleEndian.PutUint16(data[0x94:], 0)
	binary.LittleEndian.PutUint16(data[0x96:], 0x0002)
	return data
}
