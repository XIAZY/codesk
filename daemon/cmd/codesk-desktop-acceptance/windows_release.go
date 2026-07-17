package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"notty/daemon/internal/desktopacceptance"
)

const (
	windowsMSIManifestSchema    = 1
	windowsMSIUpgradeCode       = "{0C8C0BBA-06EE-43BA-BC34-768B9B740A09}"
	windowsMSIMaxMetadataSize   = 1 << 20
	windowsMSIMaxArtifactSize   = 1 << 30
	windowsMSICrossArchConverge = "converge"
	windowsMSICrossArchBlock    = "block"
)

var windowsMSIGUIDPattern = regexp.MustCompile(`^\{[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}\}$`)

type windowsReleaseToolchain struct {
	Go     string `json:"go"`
	Rustc  string `json:"rustc"`
	Cargo  string `json:"cargo"`
	Zig    string `json:"zig"`
	Dotnet string `json:"dotnet"`
	WiX    string `json:"wix"`
}

var expectedWindowsReleaseToolchain = windowsReleaseToolchain{
	Go:    "go1.26.5",
	Rustc: "rustc 1.97.0 (2d8144b78 2026-07-07)",
	Cargo: "cargo 1.97.0 (c980f4866 2026-06-30)",
	Zig:   "0.16.0",
	WiX:   "4.0.5",
}

type windowsReleaseArtifact struct {
	Arch            string `json:"arch"`
	File            string `json:"file"`
	SHA256          string `json:"sha256"`
	Signed          bool   `json:"signed"`
	ProductCode     string `json:"product_code"`
	CodeskSHA256    string `json:"codesk_sha256"`
	AgentToolSHA256 string `json:"agent_tool_sha256"`
}

type windowsReleaseManifest struct {
	SchemaVersion           int                      `json:"schema_version"`
	Version                 string                   `json:"version"`
	SourceRevision          string                   `json:"source_revision"`
	UpgradeCode             string                   `json:"upgrade_code"`
	CrossArchitecturePolicy string                   `json:"cross_architecture_policy"`
	Signed                  bool                     `json:"signed"`
	Toolchain               windowsReleaseToolchain  `json:"toolchain"`
	Artifacts               []windowsReleaseArtifact `json:"artifacts"`
}

type artifactInspection struct {
	Architecture     string
	ProductName      string
	Manufacturer     string
	ProductVersion   string
	ProductCode      string
	UpgradeCode      string
	PerUser          bool
	SignaturePresent bool
	SignatureValid   bool
	SignatureError   string
}

func verifyWindowsRelease(
	input desktopacceptance.ReleaseInput,
	inspect func(string) (artifactInspection, error),
) (desktopacceptance.Release, error) {
	if err := validateMSIVersion(input.Version); err != nil {
		return desktopacceptance.Release{}, err
	}
	if inspect == nil {
		return desktopacceptance.Release{}, errors.New("native MSI inspector is required")
	}
	directory, err := filepath.Abs(input.Directory)
	if err != nil {
		return desktopacceptance.Release{}, fmt.Errorf("resolve release directory: %w", err)
	}
	expectedNames := map[string]string{
		"amd64": windowsMSIFilename(input.Version, "amd64"),
		"arm64": windowsMSIFilename(input.Version, "arm64"),
	}
	if err := verifyReleaseEntries(directory, []string{
		"manifest.json",
		"SHA256SUMS",
		expectedNames["amd64"],
		expectedNames["arm64"],
	}); err != nil {
		return desktopacceptance.Release{}, err
	}

	manifestPath := filepath.Join(directory, "manifest.json")
	var manifest windowsReleaseManifest
	manifestBytes, err := readCanonicalJSON(manifestPath, &manifest)
	if err != nil {
		return desktopacceptance.Release{}, err
	}
	if manifest.SchemaVersion != windowsMSIManifestSchema {
		return desktopacceptance.Release{}, fmt.Errorf("manifest schema version is %d, want %d", manifest.SchemaVersion, windowsMSIManifestSchema)
	}
	if manifest.Version != input.Version {
		return desktopacceptance.Release{}, fmt.Errorf("manifest version %q does not match %q", manifest.Version, input.Version)
	}
	if err := validateExactRevision(manifest.SourceRevision); err != nil {
		return desktopacceptance.Release{}, fmt.Errorf("manifest source revision: %w", err)
	}
	if input.SourceRevision != "" && manifest.SourceRevision != input.SourceRevision {
		return desktopacceptance.Release{}, fmt.Errorf("manifest source revision %q does not match %q", manifest.SourceRevision, input.SourceRevision)
	}
	if manifest.UpgradeCode != windowsMSIUpgradeCode || !validMSIGUID(manifest.UpgradeCode) {
		return desktopacceptance.Release{}, fmt.Errorf("manifest UpgradeCode %q is not the canonical Codesk MSI UpgradeCode", manifest.UpgradeCode)
	}
	if manifest.CrossArchitecturePolicy != windowsMSICrossArchConverge && manifest.CrossArchitecturePolicy != windowsMSICrossArchBlock {
		return desktopacceptance.Release{}, fmt.Errorf("manifest cross-architecture policy %q is invalid", manifest.CrossArchitecturePolicy)
	}
	if err := validateWindowsToolchain(manifest.Toolchain); err != nil {
		return desktopacceptance.Release{}, err
	}

	architectures := []string{"amd64", "arm64"}
	if len(manifest.Artifacts) != len(architectures) {
		return desktopacceptance.Release{}, fmt.Errorf("manifest has %d artifacts, want %d", len(manifest.Artifacts), len(architectures))
	}
	release := desktopacceptance.Release{
		Platform:                "windows",
		Version:                 manifest.Version,
		SourceRevision:          manifest.SourceRevision,
		UpgradeCode:             manifest.UpgradeCode,
		CrossArchitecturePolicy: manifest.CrossArchitecturePolicy,
		Signed:                  manifest.Signed,
		Toolchain: map[string]string{
			"go":     manifest.Toolchain.Go,
			"rustc":  manifest.Toolchain.Rustc,
			"cargo":  manifest.Toolchain.Cargo,
			"zig":    manifest.Toolchain.Zig,
			"dotnet": manifest.Toolchain.Dotnet,
			"wix":    manifest.Toolchain.WiX,
		},
		ManifestPath:   manifestPath,
		ManifestSHA256: hashBytes(manifestBytes),
		SumsPath:       filepath.Join(directory, "SHA256SUMS"),
	}

	checksums := make([]releaseChecksum, 0, len(architectures)+1)
	productCodes := make(map[string]struct{}, len(architectures))
	for index, architecture := range architectures {
		item := manifest.Artifacts[index]
		if item.Arch != architecture {
			return desktopacceptance.Release{}, fmt.Errorf("manifest artifact %d has architecture %q, want %q", index+1, item.Arch, architecture)
		}
		if item.File != expectedNames[architecture] {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s artifact is named %q, want %q", architecture, item.File, expectedNames[architecture])
		}
		if item.Signed != manifest.Signed {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s signed bit disagrees with manifest", architecture)
		}
		if !validMSIGUID(item.ProductCode) {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s ProductCode %q is not a canonical MSI GUID", architecture, item.ProductCode)
		}
		if _, exists := productCodes[item.ProductCode]; exists {
			return desktopacceptance.Release{}, fmt.Errorf("ProductCode %s is reused across architectures", item.ProductCode)
		}
		productCodes[item.ProductCode] = struct{}{}
		if !validSHA256(item.CodeskSHA256) || !validSHA256(item.AgentToolSHA256) {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s installed payload hashes are not canonical SHA-256 values", architecture)
		}

		path := filepath.Join(directory, item.File)
		info, err := os.Lstat(path)
		if err != nil {
			return desktopacceptance.Release{}, fmt.Errorf("inspect MSI artifact: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > windowsMSIMaxArtifactSize {
			return desktopacceptance.Release{}, fmt.Errorf("MSI artifact is not a bounded regular file: %s", path)
		}
		actualHash, err := fileSHA256(path)
		if err != nil {
			return desktopacceptance.Release{}, err
		}
		if item.SHA256 != actualHash {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s MSI SHA-256 mismatch", architecture)
		}
		native, err := inspect(path)
		if err != nil {
			return desktopacceptance.Release{}, fmt.Errorf("inspect native MSI %s: %w", path, err)
		}
		if native.Architecture != architecture || native.ProductName != "Codesk" || native.Manufacturer != "Codesk" ||
			native.ProductVersion != manifest.Version || !strings.EqualFold(native.ProductCode, item.ProductCode) ||
			!strings.EqualFold(native.UpgradeCode, manifest.UpgradeCode) || !native.PerUser {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s MSI metadata does not match the source-bound manifest", architecture)
		}
		if native.SignaturePresent != item.Signed || native.SignatureValid != item.Signed {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s MSI native signature state does not match the source-bound manifest", architecture)
		}
		checksums = append(checksums, releaseChecksum{SHA256: actualHash, File: item.File})
		release.Artifacts = append(release.Artifacts, desktopacceptance.ReleaseArtifact{
			Architecture:       architecture,
			Path:               path,
			SHA256:             actualHash,
			Size:               info.Size(),
			ProductCode:        item.ProductCode,
			CodeskSHA256:       item.CodeskSHA256,
			AgentToolSHA256:    item.AgentToolSHA256,
			ManifestSigned:     item.Signed,
			NativeFormat:       "msi",
			NativeArchitecture: native.Architecture,
			SignaturePresent:   native.SignaturePresent,
			SignatureValid:     native.SignatureValid,
			SignatureError:     native.SignatureError,
		})
	}
	checksums = append(checksums, releaseChecksum{SHA256: release.ManifestSHA256, File: "manifest.json"})
	if err := verifyChecksums(release.SumsPath, checksums); err != nil {
		return desktopacceptance.Release{}, err
	}
	release.SumsSHA256, err = fileSHA256(release.SumsPath)
	if err != nil {
		return desktopacceptance.Release{}, err
	}
	sort.Slice(release.Artifacts, func(i, j int) bool {
		return release.Artifacts[i].Architecture < release.Artifacts[j].Architecture
	})
	return release, nil
}

func windowsMSIFilename(version, architecture string) string {
	return fmt.Sprintf("CodeskMSI_%s_windows_%s.msi", version, architecture)
}

func validateMSIVersion(value string) error {
	if value == "" || value != strings.TrimSpace(value) {
		return errors.New("MSI version must be a canonical three-part numeric value")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return fmt.Errorf("MSI version %q must contain exactly three numeric fields", value)
	}
	limits := []int{255, 255, 65535}
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 || number > limits[index] || strconv.Itoa(number) != part {
			return fmt.Errorf("MSI version field %q is not canonical or exceeds %d", part, limits[index])
		}
	}
	return nil
}

func validateWindowsToolchain(toolchain windowsReleaseToolchain) error {
	if toolchain.Go != expectedWindowsReleaseToolchain.Go || toolchain.Rustc != expectedWindowsReleaseToolchain.Rustc ||
		toolchain.Cargo != expectedWindowsReleaseToolchain.Cargo || toolchain.Zig != expectedWindowsReleaseToolchain.Zig ||
		toolchain.WiX != expectedWindowsReleaseToolchain.WiX || !strings.HasPrefix(toolchain.Dotnet, "8.0.") {
		return fmt.Errorf("manifest toolchain %#v does not match the canonical Windows MSI construction toolchain", toolchain)
	}
	return nil
}

func validMSIGUID(value string) bool { return windowsMSIGUIDPattern.MatchString(value) }

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func validateExactRevision(value string) error {
	if len(value) != 40 || strings.Trim(value, "0") == "" {
		return errors.New("revision must contain exactly 40 lowercase hexadecimal characters")
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return errors.New("revision must contain exactly 40 lowercase hexadecimal characters")
		}
	}
	return nil
}

func readCanonicalJSON(path string, target any) ([]byte, error) {
	data, err := readBoundedRegularFile(path, windowsMSIMaxMetadataSize)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode %s: trailing JSON value", filepath.Base(path))
	}
	canonical, err := marshalCanonicalJSON(target)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, fmt.Errorf("%s is not canonical JSON", filepath.Base(path))
	}
	return data, nil
}

func marshalCanonicalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode canonical JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func verifyReleaseEntries(directory string, expected []string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect release directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release directory is not a real directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read release directory: %w", err)
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("release directory has %d entries, want %d", len(entries), len(expected))
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		expectedSet[name] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := expectedSet[entry.Name()]; !ok {
			return fmt.Errorf("release directory contains unexpected entry %q", entry.Name())
		}
		info, err := os.Lstat(filepath.Join(directory, entry.Name()))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("release entry %q is not a regular file", entry.Name())
		}
	}
	return nil
}

type releaseChecksum struct {
	SHA256 string
	File   string
}

func verifyChecksums(path string, expected []releaseChecksum) error {
	data, err := readBoundedRegularFile(path, windowsMSIMaxMetadataSize)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != len(expected) || !bytes.HasSuffix(data, []byte("\n")) {
		return errors.New("SHA256SUMS does not contain the exact canonical row count or final newline")
	}
	for index, checksum := range expected {
		want := checksum.SHA256 + "  " + checksum.File
		if lines[index] != want {
			return fmt.Errorf("SHA256SUMS row %d is %q, want %q", index+1, lines[index], want)
		}
	}
	return nil
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > limit {
		return nil, fmt.Errorf("%s is not a bounded regular file", filepath.Base(path))
	}
	return os.ReadFile(path)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
