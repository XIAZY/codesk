package desktoprelease

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
	"strconv"
	"strings"
)

type Version struct {
	Artifact    string
	Numeric     string
	Resource    string
	Development bool
}

type VersionPolicy struct {
	AllowDevelopment      bool
	AllowFlexibleArtifact bool
}

func ParseVersion(raw string, policy VersionPolicy) (Version, error) {
	if raw == "dev" {
		if !policy.AllowDevelopment {
			return Version{}, errors.New("desktop release: dev is allowed only in explicit development mode")
		}
		return Version{Artifact: raw, Numeric: "0.0.0", Resource: "0.0.0.0", Development: true}, nil
	}
	if policy.AllowFlexibleArtifact {
		if err := validateFlexibleVersion(raw); err != nil {
			return Version{}, err
		}
		return Version{Artifact: raw, Resource: fourPartVersion(raw)}, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Version{}, errors.New("desktop release: version must be three dot-separated integers")
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, errors.New("desktop release: version components must be canonical non-negative integers")
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return Version{}, errors.New("desktop release: version components must be canonical non-negative integers")
			}
		}
		if _, err := strconv.ParseUint(part, 10, 31); err != nil {
			return Version{}, errors.New("desktop release: version component is out of range")
		}
	}
	return Version{Artifact: raw, Numeric: raw, Resource: fourPartVersion(raw)}, nil
}

func validateFlexibleVersion(version string) error {
	if version == "" || len(version) > 128 || version != strings.TrimSpace(version) {
		return errors.New("desktop release: invalid flexible artifact version")
	}
	for index, character := range version {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || index > 0 && strings.ContainsRune("._+-", character)
		if !valid {
			return errors.New("desktop release: invalid flexible artifact version")
		}
	}
	return nil
}

func fourPartVersion(version string) string {
	candidate := strings.TrimPrefix(version, "v")
	if boundary := strings.IndexAny(candidate, "+-"); boundary >= 0 {
		candidate = candidate[:boundary]
	}
	parts := strings.Split(candidate, ".")
	if len(parts) > 4 {
		return "0.0.0.0"
	}
	components := make([]string, 4)
	for index := range components {
		components[index] = "0"
	}
	for index, part := range parts {
		if part == "" {
			return "0.0.0.0"
		}
		value, err := strconv.ParseUint(part, 10, 16)
		if err != nil {
			return "0.0.0.0"
		}
		components[index] = strconv.FormatUint(value, 10)
	}
	return strings.Join(components, ".")
}

type Metadata struct {
	Version        string
	SourceRevision string
	Signed         bool
}

type SignaturePolicy uint8

const (
	SignatureRequired SignaturePolicy = iota
	SignatureOptional
	SignatureForbidden
)

type TrustPolicy struct {
	Signature              SignaturePolicy
	AllowSignedDevelopment bool
}

func NewMetadata(version Version, sourceRevision string, signed bool, policy TrustPolicy) (Metadata, error) {
	metadata := Metadata{
		Version:        version.Artifact,
		SourceRevision: sourceRevision,
		Signed:         signed,
	}
	if err := metadata.Validate(version, policy); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (metadata Metadata) Validate(version Version, policy TrustPolicy) error {
	if err := metadata.validateIdentity(version); err != nil {
		return err
	}
	switch policy.Signature {
	case SignatureRequired:
		if !metadata.Signed {
			return errors.New("desktop release: artifact is unsigned; allow unsigned only for construction evidence")
		}
	case SignatureOptional:
	case SignatureForbidden:
		if metadata.Signed {
			return errors.New("desktop release: artifact is signed, want unsigned construction evidence")
		}
	default:
		return errors.New("desktop release: invalid signature policy")
	}
	if metadata.Signed && version.Development && !policy.AllowSignedDevelopment {
		return errors.New("desktop release: development artifacts cannot be marked signed")
	}
	return nil
}

func (metadata Metadata) validateIdentity(version Version) error {
	if metadata.Version != version.Artifact {
		return fmt.Errorf("desktop release: manifest version %q, want %q", metadata.Version, version.Artifact)
	}
	if err := ValidateSourceRevision(metadata.SourceRevision); err != nil {
		return err
	}
	return nil
}

func ValidateSourceRevision(revision string) error {
	if len(revision) != 40 || revision != strings.ToLower(revision) || strings.Trim(revision, "0") == "" {
		return errors.New("desktop release: source revision must be a full lowercase Git SHA")
	}
	decoded, err := hex.DecodeString(revision)
	if err != nil || len(decoded) != 20 {
		return errors.New("desktop release: source revision must be a full lowercase Git SHA")
	}
	return nil
}

func MarshalCanonicalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("desktop release: encode canonical JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func ReadCanonicalJSON(path string, target any) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("desktop release: read canonical JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, fmt.Errorf("desktop release: decode canonical JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("desktop release: canonical JSON contains multiple values")
		}
		return nil, fmt.Errorf("desktop release: decode canonical JSON trailer: %w", err)
	}
	canonical, err := MarshalCanonicalJSON(target)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("desktop release: JSON is not canonical")
	}
	return data, nil
}

type Checksum struct {
	SHA256 string
	File   string
}

func MarshalChecksums(checksums []Checksum) ([]byte, error) {
	seen := make(map[string]struct{}, len(checksums))
	var output strings.Builder
	for index, checksum := range checksums {
		if err := validateSHA256(checksum.SHA256); err != nil {
			return nil, fmt.Errorf("desktop release: checksum %d: %w", index+1, err)
		}
		if !validChecksumFilename(checksum.File) {
			return nil, fmt.Errorf("desktop release: checksum %d has invalid file %q", index+1, checksum.File)
		}
		if _, duplicate := seen[checksum.File]; duplicate {
			return nil, fmt.Errorf("desktop release: duplicate checksum file %q", checksum.File)
		}
		seen[checksum.File] = struct{}{}
		fmt.Fprintf(&output, "%s  %s\n", checksum.SHA256, checksum.File)
	}
	return []byte(output.String()), nil
}

func VerifyChecksums(path string, expected []Checksum) error {
	want, err := MarshalChecksums(expected)
	if err != nil {
		return err
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("desktop release: read SHA256SUMS: %w", err)
	}
	if !bytes.Equal(actual, want) {
		return errors.New("desktop release: SHA256SUMS is not canonical")
	}
	return nil
}

func validateSHA256(value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || value != strings.ToLower(value) {
		return errors.New("invalid lowercase SHA-256")
	}
	return nil
}

func validChecksumFilename(name string) bool {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, character := range name {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

type EntryKind uint8

const (
	RegularFile EntryKind = iota + 1
	Directory
)

type Entry struct {
	Name                    string
	Kind                    EntryKind
	ForbidGroupOrOtherWrite bool
}

func VerifyEntries(root string, expected []Entry) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("desktop release: stat release root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("desktop release: release root is not a real directory")
	}
	want := make(map[string]Entry, len(expected))
	for _, specification := range expected {
		if !validChecksumFilename(specification.Name) {
			return fmt.Errorf("desktop release: invalid expected release entry %q", specification.Name)
		}
		if specification.Kind != RegularFile && specification.Kind != Directory {
			return fmt.Errorf("desktop release: invalid kind for release entry %q", specification.Name)
		}
		if _, duplicate := want[specification.Name]; duplicate {
			return fmt.Errorf("desktop release: duplicate expected release entry %q", specification.Name)
		}
		want[specification.Name] = specification
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("desktop release: read release root: %w", err)
	}
	if len(entries) != len(want) {
		return fmt.Errorf("desktop release: release root has %d entries, want %d", len(entries), len(want))
	}
	for _, entry := range entries {
		specification, found := want[entry.Name()]
		if !found {
			return fmt.Errorf("desktop release: unexpected release entry %q", entry.Name())
		}
		info, err := os.Lstat(filepath.Join(root, entry.Name()))
		if err != nil {
			return fmt.Errorf("desktop release: stat release entry %q: %w", entry.Name(), err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("desktop release: release entry %q is a symlink", entry.Name())
		}
		if specification.Kind == RegularFile && !info.Mode().IsRegular() {
			return fmt.Errorf("desktop release: release entry %q is not a regular file", entry.Name())
		}
		if specification.Kind == Directory && !info.IsDir() {
			return fmt.Errorf("desktop release: release entry %q is not a directory", entry.Name())
		}
		if specification.ForbidGroupOrOtherWrite && info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("desktop release: release entry %q is group/other writable", entry.Name())
		}
	}
	return nil
}

func FileSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("desktop release: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("desktop release: %s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("desktop release: open %s: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", fmt.Errorf("desktop release: %s changed while opening", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("desktop release: hash %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func SHA256(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func WriteAtomic(path string, data []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".codesk-release-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); returnErr == nil && closeErr != nil {
				returnErr = closeErr
			}
		}
		if removeErr := os.Remove(temporaryPath); returnErr == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = removeErr
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	return os.Rename(temporaryPath, path)
}
