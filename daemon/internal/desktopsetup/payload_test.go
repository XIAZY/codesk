package desktopsetup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	testPayloadVersion = "1.2.3-test"
	testPayloadArch    = "amd64"
)

type testZIPEntry struct {
	name string
	data []byte
}

func testPayloadFiles() map[string][]byte {
	return map[string][]byte{
		"Codesk.exe":           []byte("desktop-binary"),
		"notty-agent-tool.exe": []byte("agent-tool-binary"),
		"codesk.ico":           []byte("icon-binary"),
	}
}

func writePayloadSources(t *testing.T, root string, files map[string][]byte) map[string]string {
	t.Helper()
	sources := make(map[string]string, len(files))
	for name, data := range files {
		path := filepath.Join(root, "sources", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		sources[name] = path
	}
	return sources
}

func createPayloadExecutable(t *testing.T, stub []byte, archivePath string) string {
	t.Helper()
	root := t.TempDir()
	stubPath := filepath.Join(root, "setup-stub.exe")
	outputPath := filepath.Join(root, "CodeskSetup.exe")
	if err := os.WriteFile(stubPath, stub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AppendPayload(stubPath, archivePath, outputPath); err != nil {
		t.Fatal(err)
	}
	return outputPath
}

func createValidPayloadExecutable(t *testing.T, stub []byte) (string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	files := testPayloadFiles()
	sources := writePayloadSources(t, root, files)
	archivePath := filepath.Join(root, "payload.zip")
	if err := CreatePayloadArchive(archivePath, testPayloadVersion, testPayloadArch, sources); err != nil {
		t.Fatal(err)
	}
	return createPayloadExecutable(t, stub, archivePath), files
}

func TestPayloadRoundTripAndExtract(t *testing.T) {
	executablePath, expectedFiles := createValidPayloadExecutable(t, []byte("setup-stub"))
	payload, err := OpenPayload(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := payload.Verify(testPayloadVersion, testPayloadArch)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != testPayloadVersion || manifest.Arch != testPayloadArch || len(manifest.Files) != len(payloadFileNames) {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}

	destination := filepath.Join(t.TempDir(), "staging")
	if err := payload.Extract(destination); err != nil {
		t.Fatal(err)
	}
	for name, expected := range expectedFiles {
		actual, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("extracted %s = %q, want %q", name, actual, expected)
		}
	}
	manifestData, err := os.ReadFile(filepath.Join(destination, "payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	var extractedManifest PayloadManifest
	if err := json.Unmarshal(manifestData, &extractedManifest); err != nil {
		t.Fatal(err)
	}
	if extractedManifest.Version != testPayloadVersion || extractedManifest.Arch != testPayloadArch {
		t.Fatalf("extracted manifest = %#v", extractedManifest)
	}
}

func TestCreatePayloadArchiveIsDeterministic(t *testing.T) {
	root := t.TempDir()
	sources := writePayloadSources(t, root, testPayloadFiles())
	first := filepath.Join(root, "first.zip")
	second := filepath.Join(root, "second.zip")
	if err := CreatePayloadArchive(first, testPayloadVersion, testPayloadArch, sources); err != nil {
		t.Fatal(err)
	}
	if err := CreatePayloadArchive(second, testPayloadVersion, testPayloadArch, sources); err != nil {
		t.Fatal(err)
	}
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstData, secondData) {
		t.Fatal("payload archive is not deterministic")
	}
}

func TestOpenPayloadRejectsTruncationAndHashMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "truncated footer",
			mutate: func(data []byte) []byte {
				return data[:len(data)-1]
			},
		},
		{
			name: "payload byte",
			mutate: func(data []byte) []byte {
				footer := data[len(data)-payloadFooterSize:]
				length := int(binary.LittleEndian.Uint64(footer[16:24]))
				data[len(data)-payloadFooterSize-length] ^= 0xff
				return data
			},
		},
		{
			name: "footer digest",
			mutate: func(data []byte) []byte {
				data[len(data)-1] ^= 0xff
				return data
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			executablePath, _ := createValidPayloadExecutable(t, []byte("setup-stub"))
			data, err := os.ReadFile(executablePath)
			if err != nil {
				t.Fatal(err)
			}
			data = test.mutate(append([]byte(nil), data...))
			if err := os.WriteFile(executablePath, data, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenPayload(executablePath); err == nil {
				t.Fatal("OpenPayload() accepted mutated executable")
			}
		})
	}
}

func TestOpenPayloadFindsFooterBeforeCertificateTable(t *testing.T) {
	stub := minimalPE64Stub()
	executablePath, _ := createValidPayloadExecutable(t, stub)
	data, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	padding := (8 - len(data)%8) % 8
	certificateOffset := len(data) + padding
	certificate := bytes.Repeat([]byte{0xa5}, 16)
	optionalOffset := 0x80 + 24
	securityEntry := optionalOffset + 112 + 4*8
	binary.LittleEndian.PutUint32(data[securityEntry:securityEntry+4], uint32(certificateOffset))
	binary.LittleEndian.PutUint32(data[securityEntry+4:securityEntry+8], uint32(len(certificate)))
	data = append(data, make([]byte, padding)...)
	data = append(data, certificate...)
	if err := os.WriteFile(executablePath, data, 0o700); err != nil {
		t.Fatal(err)
	}

	payload, err := OpenPayload(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payload.Verify(testPayloadVersion, testPayloadArch); err != nil {
		t.Fatal(err)
	}
}

func minimalPE64Stub() []byte {
	stub := make([]byte, 512)
	copy(stub[:2], "MZ")
	binary.LittleEndian.PutUint32(stub[0x3c:0x40], 0x80)
	copy(stub[0x80:0x84], "PE\x00\x00")
	binary.LittleEndian.PutUint16(stub[0x84:0x86], 0x8664)
	binary.LittleEndian.PutUint16(stub[0x94:0x96], 240)
	optionalOffset := 0x80 + 24
	binary.LittleEndian.PutUint16(stub[optionalOffset:optionalOffset+2], 0x20b)
	binary.LittleEndian.PutUint32(stub[optionalOffset+108:optionalOffset+112], 16)
	return stub
}

func TestVerifyRejectsDuplicateUnknownAndTraversalEntries(t *testing.T) {
	validEntries := validTestZIPEntries(t)
	for _, test := range []struct {
		name    string
		entries []testZIPEntry
	}{
		{"duplicate", append(append([]testZIPEntry(nil), validEntries...), testZIPEntry{name: "Codesk.exe", data: []byte("duplicate")})},
		{"unknown", append(append([]testZIPEntry(nil), validEntries...), testZIPEntry{name: "extra.dll", data: []byte("extra")})},
		{"traversal", append(append([]testZIPEntry(nil), validEntries...), testZIPEntry{name: "../Codesk.exe", data: []byte("escape")})},
	} {
		t.Run(test.name, func(t *testing.T) {
			archivePath := writeTestZIP(t, test.entries)
			executablePath := createPayloadExecutable(t, []byte("stub"), archivePath)
			payload, err := OpenPayload(executablePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := payload.Verify(testPayloadVersion, testPayloadArch); err == nil {
				t.Fatal("Verify() accepted unsafe ZIP entries")
			}
		})
	}
}

func TestVerifyRejectsManifestAndFileMismatch(t *testing.T) {
	entries := validTestZIPEntries(t)
	for index := range entries {
		if entries[index].name == "Codesk.exe" {
			entries[index].data = []byte("substituted desktop")
		}
	}
	archivePath := writeTestZIP(t, entries)
	executablePath := createPayloadExecutable(t, []byte("stub"), archivePath)
	payload, err := OpenPayload(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payload.Verify(testPayloadVersion, testPayloadArch); err == nil {
		t.Fatal("Verify() accepted a file that did not match the manifest")
	}
	if _, err := payload.Verify("different", testPayloadArch); err == nil {
		t.Fatal("Verify() accepted a different version")
	}
}

func TestExtractRequiresVerificationAndFreshAbsoluteDestination(t *testing.T) {
	executablePath, _ := createValidPayloadExecutable(t, []byte("stub"))
	payload, err := OpenPayload(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := payload.Extract(filepath.Join(t.TempDir(), "unverified")); err == nil {
		t.Fatal("Extract() accepted an unverified payload")
	}
	if _, err := payload.Verify(testPayloadVersion, testPayloadArch); err != nil {
		t.Fatal(err)
	}
	if err := payload.Extract("relative"); err == nil {
		t.Fatal("Extract() accepted a relative destination")
	}
	existing := t.TempDir()
	if err := payload.Extract(existing); err == nil {
		t.Fatal("Extract() accepted an existing destination")
	}
}

func validTestZIPEntries(t *testing.T) []testZIPEntry {
	t.Helper()
	files := testPayloadFiles()
	manifest := PayloadManifest{Version: testPayloadVersion, Arch: testPayloadArch}
	for _, name := range payloadFileNames {
		digest := sha256.Sum256(files[name])
		manifest.Files = append(manifest.Files, PayloadFile{
			Name: name, Size: int64(len(files[name])), SHA256: hex.EncodeToString(digest[:]),
		})
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return []testZIPEntry{
		{name: "Codesk.exe", data: files["Codesk.exe"]},
		{name: "notty-agent-tool.exe", data: files["notty-agent-tool.exe"]},
		{name: "codesk.ico", data: files["codesk.ico"]},
		{name: "payload.json", data: append(manifestData, '\n')},
	}
}

func writeTestZIP(t *testing.T, entries []testZIPEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entry := range entries {
		writer, err := archive.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
