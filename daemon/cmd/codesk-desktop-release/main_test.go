package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"notty/daemon/internal/desktoprelease"
)

const testSourceRevision = "0123456789abcdef0123456789abcdef01234567"

func TestResourceVersion(t *testing.T) {
	tests := map[string]string{
		"v1.2.3":       "1.2.3.0",
		"1.2.3.4":      "1.2.3.4",
		"1.2.3-beta.1": "1.2.3.0",
		"dev":          "0.0.0.0",
		"1.70000":      "0.0.0.0",
	}
	for version, expected := range tests {
		parsed, err := desktoprelease.ParseVersion(version, windowsReleaseVersionPolicy)
		if err != nil {
			t.Fatal(err)
		}
		if actual := resourceVersion(parsed); actual != expected {
			t.Errorf("resourceVersion(%q) = %q, want %q", version, actual, expected)
		}
	}
}

func TestVerifyReleaseRejectsRehashedNonCanonicalMetadata(t *testing.T) {
	t.Run("manifest JSON", func(t *testing.T) {
		directory, manifest, manifestData := writeTestReleaseMetadata(t)
		manifestData = bytes.Replace(manifestData, []byte("  \"version\""), []byte("    \"version\""), 1)
		if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestData, 0o600); err != nil {
			t.Fatal(err)
		}
		checksums := make([]desktoprelease.Checksum, 0, len(manifest.Artifacts)+1)
		for _, artifact := range manifest.Artifacts {
			checksums = append(checksums, desktoprelease.Checksum{SHA256: artifact.SHA256, File: artifact.File})
		}
		checksums = append(checksums, desktoprelease.Checksum{SHA256: desktoprelease.SHA256(manifestData), File: "manifest.json"})
		checksumData, err := desktoprelease.MarshalChecksums(checksums)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), checksumData, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyRelease(directory, "dev", true); err == nil {
			t.Fatal("verifyRelease() accepted noncanonical manifest JSON with matching checksums")
		}
	})

	t.Run("SHA256SUMS", func(t *testing.T) {
		directory, _, _ := writeTestReleaseMetadata(t)
		path := filepath.Join(directory, "SHA256SUMS")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.Replace(data, []byte("  "), []byte(" "), 1)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyRelease(directory, "dev", true); err == nil {
			t.Fatal("verifyRelease() accepted noncanonical SHA256SUMS")
		}
	})
}

func TestVerifyReleaseRejectsRehashedArtifactOrder(t *testing.T) {
	directory, manifest, _ := writeTestReleaseMetadata(t)
	manifest.Artifacts[0], manifest.Artifacts[1] = manifest.Artifacts[1], manifest.Artifacts[0]
	manifestData, err := desktoprelease.MarshalCanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts := make(map[string]releaseArtifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		artifacts[artifact.Arch] = artifact
	}
	checksums := make([]desktoprelease.Checksum, 0, len(releaseArchitectures)+1)
	for _, arch := range releaseArchitectures {
		artifact := artifacts[arch]
		checksums = append(checksums, desktoprelease.Checksum{SHA256: artifact.SHA256, File: artifact.File})
	}
	checksums = append(checksums, desktoprelease.Checksum{SHA256: desktoprelease.SHA256(manifestData), File: "manifest.json"})
	checksumData, err := desktoprelease.MarshalChecksums(checksums)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), checksumData, 0o600); err != nil {
		t.Fatal(err)
	}
	err = verifyRelease(directory, "dev", true)
	if err == nil || !strings.Contains(err.Error(), "artifact 1") {
		t.Fatalf("verifyRelease() error = %v, want canonical artifact order rejection", err)
	}
}

func writeTestReleaseMetadata(t *testing.T) (string, releaseManifest, []byte) {
	t.Helper()
	directory := t.TempDir()
	paths := make(map[string]string, len(releaseArchitectures))
	for _, arch := range releaseArchitectures {
		path := filepath.Join(directory, setupFilename("dev", arch))
		if err := os.WriteFile(path, []byte("fixture "+arch), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[arch] = path
	}
	if err := writeReleaseMetadata(directory, "dev", testSourceRevision, false, paths); err != nil {
		t.Fatal(err)
	}
	var manifest releaseManifest
	manifestData, err := desktoprelease.ReadCanonicalJSON(filepath.Join(directory, "manifest.json"), &manifest)
	if err != nil {
		t.Fatal(err)
	}
	return directory, manifest, manifestData
}

func TestWriteReleaseMetadataRecordsCanonicalToolchain(t *testing.T) {
	directory := t.TempDir()
	paths := make(map[string]string, len(releaseArchitectures))
	for _, arch := range releaseArchitectures {
		path := filepath.Join(directory, setupFilename("dev", arch))
		if err := os.WriteFile(path, []byte("fixture "+arch), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[arch] = path
	}
	if err := writeReleaseMetadata(directory, "dev", testSourceRevision, false, paths); err != nil {
		t.Fatal(err)
	}
	var manifest releaseManifest
	manifestPath := filepath.Join(directory, "manifest.json")
	manifestData, err := desktoprelease.ReadCanonicalJSON(manifestPath, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Toolchain != canonicalReleaseToolchain {
		t.Fatalf("toolchain = %#v, want %#v", manifest.Toolchain, canonicalReleaseToolchain)
	}
	if manifest.SourceRevision != testSourceRevision {
		t.Fatalf("source revision = %q, want %q", manifest.SourceRevision, testSourceRevision)
	}
	expectedChecksums := make([]desktoprelease.Checksum, 0, len(manifest.Artifacts)+1)
	for _, artifact := range manifest.Artifacts {
		expectedChecksums = append(expectedChecksums, desktoprelease.Checksum{SHA256: artifact.SHA256, File: artifact.File})
	}
	expectedChecksums = append(expectedChecksums, desktoprelease.Checksum{SHA256: desktoprelease.SHA256(manifestData), File: "manifest.json"})
	if err := desktoprelease.VerifyChecksums(filepath.Join(directory, "SHA256SUMS"), expectedChecksums); err != nil {
		t.Fatal(err)
	}
}

func TestRootResourceTypes(t *testing.T) {
	const childOffset = 16 + 4*8
	data := make([]byte, childOffset+16)
	binary.LittleEndian.PutUint16(data[14:16], 4)
	for index, resourceType := range []uint32{3, 14, 16, 24} {
		entry := data[16+index*8 : 24+index*8]
		binary.LittleEndian.PutUint32(entry[:4], resourceType)
		binary.LittleEndian.PutUint32(entry[4:], 0x80000000|childOffset)
	}
	types, err := rootResourceTypes(data)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[uint32]bool{3: true, 14: true, 16: true, 24: true}
	if !reflect.DeepEqual(types, expected) {
		t.Fatalf("rootResourceTypes() = %#v, want %#v", types, expected)
	}

	data[16+4] = 0
	data[16+5] = 0
	data[16+6] = 0
	data[16+7] = 0
	if _, err := rootResourceTypes(data); err == nil {
		t.Fatal("rootResourceTypes() accepted a non-directory child")
	}

	binary.LittleEndian.PutUint32(data[16+4:16+8], 0x80000000|uint32(len(data)-15))
	if _, err := rootResourceTypes(data); err == nil {
		t.Fatal("rootResourceTypes() accepted a truncated child directory")
	}
}
