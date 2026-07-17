package desktoprelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSourceRevision = "0123456789abcdef0123456789abcdef01234567"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		raw              string
		allowDevelopment bool
		wantNumeric      string
		wantFailure      bool
	}{
		{raw: "1.2.3", wantNumeric: "1.2.3"},
		{raw: "0.0.0", wantNumeric: "0.0.0"},
		{raw: "dev", allowDevelopment: true, wantNumeric: "0.0.0"},
		{raw: "dev", wantFailure: true},
		{raw: "1.2", wantFailure: true},
		{raw: "01.2.3", wantFailure: true},
		{raw: "1.2.3-beta", wantFailure: true},
		{raw: "+1.2.3", wantFailure: true},
		{raw: "1.2.2147483648", wantFailure: true},
		{raw: "1.2.-1", wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			version, err := ParseVersion(test.raw, VersionPolicy{AllowDevelopment: test.allowDevelopment})
			if (err != nil) != test.wantFailure {
				t.Fatalf("ParseVersion() error = %v, wantFailure = %t", err, test.wantFailure)
			}
			if err == nil && version.Numeric != test.wantNumeric {
				t.Fatalf("Numeric = %q, want %q", version.Numeric, test.wantNumeric)
			}
		})
	}
}

func TestMetadataRequiresCanonicalSourceAndReleaseTrust(t *testing.T) {
	version, err := ParseVersion("1.2.3", VersionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewMetadata(version, testSourceRevision, true, TrustPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Validate(version, TrustPolicy{}); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{strings.Repeat("0", 40), strings.Repeat("A", 40), "abc"} {
		if _, err := NewMetadata(version, revision, true, TrustPolicy{}); err == nil {
			t.Fatalf("NewMetadata() accepted source revision %q", revision)
		}
	}
	unsigned := metadata
	unsigned.Signed = false
	if err := unsigned.Validate(version, TrustPolicy{}); err == nil {
		t.Fatal("Metadata.Validate() accepted unsigned release without an override")
	}
	if err := unsigned.Validate(version, TrustPolicy{Signature: SignatureOptional}); err != nil {
		t.Fatal(err)
	}
	if err := unsigned.Validate(version, TrustPolicy{Signature: SignatureForbidden}); err != nil {
		t.Fatalf("Metadata.Validate() rejected required unsigned evidence: %v", err)
	}
	if err := metadata.Validate(version, TrustPolicy{Signature: SignatureForbidden}); err == nil {
		t.Fatal("Metadata.Validate() accepted a signed release when unsigned evidence was required")
	}
	development, err := ParseVersion("dev", VersionPolicy{AllowDevelopment: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMetadata(development, testSourceRevision, true, TrustPolicy{Signature: SignatureOptional}); err == nil {
		t.Fatal("NewMetadata() accepted a signed development release")
	}
	if _, err := NewMetadata(development, testSourceRevision, true, TrustPolicy{
		Signature:              SignatureOptional,
		AllowSignedDevelopment: true,
	}); err != nil {
		t.Fatalf("NewMetadata() rejected policy-authorized signed development release: %v", err)
	}
}

func TestFlexibleArtifactVersionPreservesWindowsCompatibility(t *testing.T) {
	policy := VersionPolicy{AllowDevelopment: true, AllowFlexibleArtifact: true}
	tests := map[string]string{
		"v1.2.3":       "1.2.3.0",
		"1.2.3.4":      "1.2.3.4",
		"1.2.3-beta.1": "1.2.3.0",
		"dev":          "0.0.0.0",
		"1.70000":      "0.0.0.0",
	}
	for raw, resource := range tests {
		version, err := ParseVersion(raw, policy)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", raw, err)
		}
		if version.Artifact != raw || version.Resource != resource {
			t.Fatalf("ParseVersion(%q) = %#v, want resource %q", raw, version, resource)
		}
	}
	for _, raw := range []string{"", "../release", "-version", " version", "version ", "version/name", strings.Repeat("v", 129)} {
		if _, err := ParseVersion(raw, policy); err == nil {
			t.Fatalf("ParseVersion(%q) accepted an invalid flexible artifact version", raw)
		}
	}
}

func TestReadCanonicalJSONRejectsEquivalentNonCanonicalBytes(t *testing.T) {
	type document struct {
		Version string `json:"version"`
		Signed  bool   `json:"signed"`
	}
	want := document{Version: "1.2.3", Signed: true}
	canonical, err := MarshalCanonicalJSON(want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	var actual document
	if _, err := ReadCanonicalJSON(path, &actual); err != nil {
		t.Fatal(err)
	}
	for name, mutation := range map[string][]byte{
		"indentation":   []byte(strings.Replace(string(canonical), "  \"version\"", "    \"version\"", 1)),
		"extra newline": append(append([]byte(nil), canonical...), '\n'),
		"key order":     []byte("{\n  \"signed\": true,\n  \"version\": \"1.2.3\"\n}\n"),
		"unknown field": []byte("{\n  \"version\": \"1.2.3\",\n  \"signed\": true,\n  \"extra\": true\n}\n"),
		"second value":  append(append([]byte(nil), canonical...), []byte("{}\n")...),
	} {
		t.Run(name, func(t *testing.T) {
			data := []byte(mutation)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			var candidate document
			if _, err := ReadCanonicalJSON(path, &candidate); err == nil {
				t.Fatal("ReadCanonicalJSON() accepted noncanonical bytes")
			}
		})
	}
}

func TestVerifyChecksumsRequiresExactCanonicalBytes(t *testing.T) {
	expected := []Checksum{
		{SHA256: strings.Repeat("a", 64), File: "Codesk.exe"},
		{SHA256: strings.Repeat("b", 64), File: "manifest.json"},
	}
	canonical, err := MarshalChecksums(expected)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "SHA256SUMS")
	if err := os.WriteFile(path, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(path, expected); err != nil {
		t.Fatal(err)
	}
	for name, mutation := range map[string][]byte{
		"row order": []byte(strings.Repeat("b", 64) + "  manifest.json\n" + strings.Repeat("a", 64) + "  Codesk.exe\n"),
		"separator": []byte(strings.Replace(string(canonical), "  Codesk.exe", " Codesk.exe", 1)),
		"trailer":   append(append([]byte(nil), canonical...), '\n'),
		"uppercase": []byte(strings.ToUpper(string(canonical))),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, mutation, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := VerifyChecksums(path, expected); err == nil {
				t.Fatal("VerifyChecksums() accepted noncanonical bytes")
			}
		})
	}
	for _, invalid := range []Checksum{
		{SHA256: strings.Repeat("A", 64), File: "artifact"},
		{SHA256: strings.Repeat("a", 63), File: "artifact"},
		{SHA256: strings.Repeat("a", 64), File: "../artifact"},
		{SHA256: strings.Repeat("a", 64), File: `dir\artifact`},
		{SHA256: strings.Repeat("a", 64), File: "artifact name"},
	} {
		if _, err := MarshalChecksums([]Checksum{invalid}); err == nil {
			t.Fatalf("MarshalChecksums() accepted %#v", invalid)
		}
	}
	if _, err := MarshalChecksums([]Checksum{expected[0], expected[0]}); err == nil {
		t.Fatal("MarshalChecksums() accepted a duplicate file")
	}
}

func TestVerifyEntriesRequiresExactRealTypesAndPermissions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Codesk.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	expected := []Entry{
		{Name: "manifest.json", Kind: RegularFile, ForbidGroupOrOtherWrite: true},
		{Name: "Codesk.app", Kind: Directory, ForbidGroupOrOtherWrite: true},
	}
	if err := VerifyEntries(root, expected); err != nil {
		t.Fatal(err)
	}
	t.Run("unexpected", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "extra"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(root, "extra"))
		if err := VerifyEntries(root, expected); err == nil {
			t.Fatal("VerifyEntries() accepted an unexpected entry")
		}
	})
	t.Run("wrong type", func(t *testing.T) {
		if err := VerifyEntries(root, []Entry{{Name: "manifest.json", Kind: Directory}, {Name: "Codesk.app", Kind: Directory}}); err == nil {
			t.Fatal("VerifyEntries() accepted the wrong entry type")
		}
	})
	t.Run("writable", func(t *testing.T) {
		manifest := filepath.Join(root, "manifest.json")
		if err := os.Chmod(manifest, 0o666); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(manifest, 0o644)
		if err := VerifyEntries(root, expected); err == nil {
			t.Fatal("VerifyEntries() accepted a group/other-writable entry")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		manifest := filepath.Join(root, "manifest.json")
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(manifest); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, manifest); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		defer func() {
			_ = os.Remove(manifest)
			_ = os.WriteFile(manifest, []byte("fixture"), 0o644)
		}()
		if err := VerifyEntries(root, expected); err == nil {
			t.Fatal("VerifyEntries() accepted a symlink")
		}
	})
}

func TestFileSHA256AndWriteAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata", "manifest.json")
	data := []byte("canonical\n")
	if err := WriteAtomic(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(data) {
		t.Fatalf("stored bytes = %q, want %q", stored, data)
	}
	if hash, err := FileSHA256(path); err != nil || hash != SHA256(data) {
		t.Fatalf("FileSHA256() = %q, %v; want %q", hash, err, SHA256(data))
	}
	link := filepath.Join(filepath.Dir(path), "manifest-link.json")
	if err := os.Symlink(path, link); err == nil {
		if _, err := FileSHA256(link); err == nil {
			t.Fatal("FileSHA256() accepted a symlink")
		}
	}
}
