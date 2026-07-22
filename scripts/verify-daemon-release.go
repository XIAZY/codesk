//go:build ignore

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type releaseManifest struct {
	Version   string            `json:"version"`
	Artifacts []releaseArtifact `json:"artifacts"`
}

type releaseArtifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type publicationManifest struct {
	Version string `json:"version"`
}

type releaseTarget struct {
	os   string
	arch string
	ext  string
}

var releaseTargets = []releaseTarget{
	{os: "linux", arch: "amd64", ext: ".tar.gz"},
	{os: "linux", arch: "arm64", ext: ".tar.gz"},
	{os: "darwin", arch: "amd64", ext: ".tar.gz"},
	{os: "darwin", arch: "arm64", ext: ".tar.gz"},
	{os: "windows", arch: "amd64", ext: ".zip"},
	{os: "windows", arch: "arm64", ext: ".zip"},
}

func main() {
	var err error
	switch {
	case len(os.Args) == 3:
		err = verifyRelease(os.Args[1], os.Args[2])
	case len(os.Args) == 5 && os.Args[1] == "publication-state":
		var state string
		state, err = publicationState(os.Args[2], os.Args[3], os.Args[4])
		if err == nil {
			fmt.Println(state)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  go run ./scripts/verify-daemon-release.go <version-dir> <version>")
		fmt.Fprintln(os.Stderr, "  go run ./scripts/verify-daemon-release.go publication-state <remote-manifest> <candidate-manifest> <version>")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-daemon-release: %v\n", err)
		os.Exit(1)
	}
}

func publicationState(remotePath, candidatePath, version string) (string, error) {
	remoteBytes, remote, err := readPublicationManifest(remotePath)
	if err != nil {
		return "", fmt.Errorf("remote latest: %w", err)
	}
	candidateBytes, candidate, err := readPublicationManifest(candidatePath)
	if err != nil {
		return "", fmt.Errorf("candidate: %w", err)
	}
	if candidate.Version != version {
		return "", fmt.Errorf("candidate manifest version %q, want %q", candidate.Version, version)
	}
	if remote.Version != version {
		return "different-version", nil
	}
	if bytes.Equal(remoteBytes, candidateBytes) {
		return "identical", nil
	}
	return "same-version-conflict", nil
}

func readPublicationManifest(path string) ([]byte, publicationManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, publicationManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest publicationManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&manifest); err != nil {
		return nil, publicationManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, publicationManifest{}, fmt.Errorf("decode manifest: trailing data")
	}
	if manifest.Version == "" {
		return nil, publicationManifest{}, fmt.Errorf("manifest version is empty")
	}
	return data, manifest, nil
}

func verifyRelease(versionDir, version string) error {
	expectedFiles := map[string]releaseTarget{}
	for _, target := range releaseTargets {
		name := archiveName(version, target)
		expectedFiles[name] = target
	}
	if err := verifyDirectoryInventory(versionDir, expectedFiles); err != nil {
		return err
	}

	manifest, err := readManifest(filepath.Join(versionDir, "manifest.json"))
	if err != nil {
		return err
	}
	if manifest.Version != version {
		return fmt.Errorf("manifest version %q, want %q", manifest.Version, version)
	}
	if len(manifest.Artifacts) != len(releaseTargets) {
		return fmt.Errorf("manifest has %d artifacts, want %d", len(manifest.Artifacts), len(releaseTargets))
	}

	checksums, err := readChecksums(filepath.Join(versionDir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	if len(checksums) != len(releaseTargets) {
		return fmt.Errorf("SHA256SUMS has %d entries, want %d", len(checksums), len(releaseTargets))
	}

	manifestFiles := make(map[string]struct{}, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		target, ok := expectedFiles[artifact.File]
		if !ok {
			return fmt.Errorf("manifest contains unexpected artifact %q", artifact.File)
		}
		if artifact.OS != target.os || artifact.Arch != target.arch {
			return fmt.Errorf("manifest identity for %q is %s/%s, want %s/%s", artifact.File, artifact.OS, artifact.Arch, target.os, target.arch)
		}
		if _, exists := manifestFiles[artifact.File]; exists {
			return fmt.Errorf("manifest contains duplicate artifact %q", artifact.File)
		}
		if !validSHA256(artifact.SHA256) {
			return fmt.Errorf("manifest contains invalid hash for %q", artifact.File)
		}
		manifestFiles[artifact.File] = struct{}{}

		actual, err := fileSHA256(filepath.Join(versionDir, artifact.File))
		if err != nil {
			return err
		}
		if artifact.SHA256 != actual {
			return fmt.Errorf("manifest hash for %q is %q, want %q", artifact.File, artifact.SHA256, actual)
		}
		if checksums[artifact.File] != actual {
			return fmt.Errorf("SHA256SUMS hash for %q is %q, want %q", artifact.File, checksums[artifact.File], actual)
		}
	}

	for name := range expectedFiles {
		if _, ok := manifestFiles[name]; !ok {
			return fmt.Errorf("manifest is missing %q", name)
		}
		if _, ok := checksums[name]; !ok {
			return fmt.Errorf("SHA256SUMS is missing %q", name)
		}
	}
	return nil
}

func archiveName(version string, target releaseTarget) string {
	return fmt.Sprintf("notty-daemon_%s_%s_%s%s", version, target.os, target.arch, target.ext)
}

func verifyDirectoryInventory(versionDir string, archives map[string]releaseTarget) error {
	entries, err := os.ReadDir(versionDir)
	if err != nil {
		return fmt.Errorf("read release directory: %w", err)
	}
	expected := make(map[string]struct{}, len(archives)+2)
	for name := range archives {
		expected[name] = struct{}{}
	}
	expected["manifest.json"] = struct{}{}
	expected["SHA256SUMS"] = struct{}{}

	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		actual = append(actual, entry.Name())
		if _, ok := expected[entry.Name()]; !ok {
			return fmt.Errorf("unexpected release entry %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect release entry %q: %w", entry.Name(), err)
		}
		if entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("release entry %q is not a real file", entry.Name())
		}
		if info.Size() == 0 {
			return fmt.Errorf("release entry %q is empty", entry.Name())
		}
	}
	if len(entries) != len(expected) {
		sort.Strings(actual)
		return fmt.Errorf("release inventory is incomplete: %v", actual)
	}
	return nil
}

func readManifest(path string) (releaseManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return releaseManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest releaseManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return releaseManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return releaseManifest{}, fmt.Errorf("decode manifest: trailing data")
	}
	return manifest, nil
}

func readChecksums(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" || strings.Contains(text, "\r") {
		return nil, fmt.Errorf("SHA256SUMS is empty or non-canonical")
	}
	checksums := make(map[string]string)
	for lineNumber, line := range strings.Split(text, "\n") {
		hash, name, ok := strings.Cut(line, "  ")
		if !ok || !validSHA256(hash) || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("SHA256SUMS line %d is malformed", lineNumber+1)
		}
		if _, exists := checksums[name]; exists {
			return nil, fmt.Errorf("SHA256SUMS contains duplicate file %q", name)
		}
		checksums[name] = hash
	}
	return checksums, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
