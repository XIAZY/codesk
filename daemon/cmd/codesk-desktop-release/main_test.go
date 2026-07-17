package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
		if actual := resourceVersion(version); actual != expected {
			t.Errorf("resourceVersion(%q) = %q, want %q", version, actual, expected)
		}
	}
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
	manifest, err := readReleaseManifest(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Toolchain != canonicalReleaseToolchain {
		t.Fatalf("toolchain = %#v, want %#v", manifest.Toolchain, canonicalReleaseToolchain)
	}
	if manifest.SourceRevision != testSourceRevision {
		t.Fatalf("source revision = %q, want %q", manifest.SourceRevision, testSourceRevision)
	}
	checksums, err := readChecksums(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	manifestHash, err := fileSHA256(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if checksums["manifest.json"] != manifestHash {
		t.Fatalf("manifest checksum = %q, want %q", checksums["manifest.json"], manifestHash)
	}
}

func TestValidateSourceRevisionRejectsZeroAndNonCanonicalValues(t *testing.T) {
	for _, revision := range []string{strings.Repeat("0", 40), strings.Repeat("A", 40), "abc"} {
		if err := validateSourceRevision(revision); err == nil {
			t.Fatalf("validateSourceRevision(%q) unexpectedly succeeded", revision)
		}
	}
	if err := validateSourceRevision(testSourceRevision); err != nil {
		t.Fatalf("validateSourceRevision(valid) error = %v", err)
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
