package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

var windowsMachines = map[string]uint16{
	"amd64": pe.IMAGE_FILE_MACHINE_AMD64,
	"arm64": pe.IMAGE_FILE_MACHINE_ARM64,
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: go run ./scripts/verify-windows-daemon-release.go <version-dir> <version>\n")
		os.Exit(2)
	}
	if err := verifyRelease(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "verify-windows-daemon-release: %v\n", err)
		os.Exit(1)
	}
}

func verifyRelease(versionDir, version string) error {
	manifest, err := readManifest(filepath.Join(versionDir, "manifest.json"))
	if err != nil {
		return err
	}
	if manifest.Version != version {
		return fmt.Errorf("manifest version %q, want %q", manifest.Version, version)
	}
	if len(manifest.Artifacts) != len(windowsMachines) {
		return fmt.Errorf("manifest has %d artifacts, want %d", len(manifest.Artifacts), len(windowsMachines))
	}

	checksums, err := readChecksums(filepath.Join(versionDir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	if len(checksums) != len(windowsMachines) {
		return fmt.Errorf("SHA256SUMS has %d entries, want %d", len(checksums), len(windowsMachines))
	}

	artifacts := make(map[string]releaseArtifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if artifact.OS != "windows" {
			return fmt.Errorf("manifest contains non-Windows artifact %q", artifact.File)
		}
		if _, ok := windowsMachines[artifact.Arch]; !ok {
			return fmt.Errorf("manifest contains unsupported Windows architecture %q", artifact.Arch)
		}
		if _, exists := artifacts[artifact.Arch]; exists {
			return fmt.Errorf("manifest contains duplicate windows/%s artifact", artifact.Arch)
		}
		artifacts[artifact.Arch] = artifact
	}

	for arch, machine := range windowsMachines {
		artifact, ok := artifacts[arch]
		if !ok {
			return fmt.Errorf("manifest is missing windows/%s", arch)
		}
		releaseName := fmt.Sprintf("notty-daemon_%s_windows_%s", version, arch)
		expectedFile := releaseName + ".zip"
		if artifact.File != expectedFile {
			return fmt.Errorf("windows/%s manifest file %q, want %q", arch, artifact.File, expectedFile)
		}
		archivePath := filepath.Join(versionDir, artifact.File)
		actualSum, err := fileSHA256(archivePath)
		if err != nil {
			return err
		}
		if artifact.SHA256 != actualSum {
			return fmt.Errorf("manifest hash for %s is %q, want %q", artifact.File, artifact.SHA256, actualSum)
		}
		if checksum, ok := checksums[artifact.File]; !ok || checksum != actualSum {
			return fmt.Errorf("SHA256SUMS hash for %s is %q, want %q", artifact.File, checksum, actualSum)
		}
		if err := verifyArchive(archivePath, releaseName, machine); err != nil {
			return err
		}
		fmt.Printf("verified windows/%s archive %s (PE machine 0x%04x)\n", arch, artifact.File, machine)
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
	checksums := make(map[string]string)
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("SHA256SUMS line %d is malformed", lineNumber+1)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != sha256.Size*2 {
			return nil, fmt.Errorf("SHA256SUMS line %d has invalid hash", lineNumber+1)
		}
		if _, exists := checksums[fields[1]]; exists {
			return nil, fmt.Errorf("SHA256SUMS contains duplicate file %q", fields[1])
		}
		checksums[fields[1]] = fields[0]
	}
	return checksums, nil
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

func verifyArchive(archivePath, releaseName string, machine uint16) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", archivePath, err)
	}
	defer archive.Close()

	files := make(map[string]*zip.File, len(archive.File))
	for _, file := range archive.File {
		if _, exists := files[file.Name]; exists {
			return fmt.Errorf("%s contains duplicate member %s", filepath.Base(archivePath), file.Name)
		}
		files[file.Name] = file
	}
	for _, relative := range []string{"README.md", "run-windows.ps1"} {
		name := releaseName + "/" + relative
		if _, ok := files[name]; !ok {
			return fmt.Errorf("%s is missing %s", filepath.Base(archivePath), name)
		}
	}
	for _, binary := range []string{"notty-daemon.exe", "notty-agent-tool.exe"} {
		name := releaseName + "/bin/" + binary
		file, ok := files[name]
		if !ok {
			return fmt.Errorf("%s is missing %s", filepath.Base(archivePath), name)
		}
		if err := verifyPEMachine(file, machine); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func verifyPEMachine(file *zip.File, expected uint16) error {
	reader, err := file.Open()
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	image, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse PE: %w", err)
	}
	defer image.Close()
	if image.FileHeader.Machine != expected {
		return fmt.Errorf("PE machine 0x%04x, want 0x%04x", image.FileHeader.Machine, expected)
	}
	return nil
}
