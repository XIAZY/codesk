package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"notty/daemon/internal/desktopsetup"
)

var releaseArchitectures = []string{"amd64", "arm64"}

const (
	releaseGoVersion       = "go1.26.5"
	releaseRustcVersion    = "rustc 1.97.0 (2d8144b78 2026-07-07)"
	releaseCargoVersion    = "cargo 1.97.0 (c980f4866 2026-06-30)"
	releaseZigVersion      = "0.16.0"
	releaseGoWinresVersion = "v0.3.1"
)

type releaseManifest struct {
	Version   string            `json:"version"`
	Signed    bool              `json:"signed"`
	Toolchain releaseToolchain  `json:"toolchain"`
	Artifacts []releaseArtifact `json:"artifacts"`
}

type releaseToolchain struct {
	Go       string `json:"go"`
	Rustc    string `json:"rustc"`
	Cargo    string `json:"cargo"`
	Zig      string `json:"zig"`
	GoWinres string `json:"go_winres"`
}

var canonicalReleaseToolchain = releaseToolchain{
	Go:       releaseGoVersion,
	Rustc:    releaseRustcVersion,
	Cargo:    releaseCargoVersion,
	Zig:      releaseZigVersion,
	GoWinres: releaseGoWinresVersion,
}

type releaseArtifact struct {
	Arch   string `json:"arch"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Signed bool   `json:"signed"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "codesk-desktop-release: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("missing command (archive, append, manifest, resource-version, or verify)")
	}
	switch arguments[0] {
	case "archive":
		return runArchive(arguments[1:])
	case "append":
		return runAppend(arguments[1:])
	case "manifest":
		return runManifest(arguments[1:])
	case "resource-version":
		return runResourceVersion(arguments[1:])
	case "verify":
		return runVerify(arguments[1:])
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func runArchive(arguments []string) error {
	flags := flag.NewFlagSet("archive", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var output, version, arch, desktop, agent, icon string
	flags.StringVar(&output, "output", "", "payload ZIP output")
	flags.StringVar(&version, "version", "", "release version")
	flags.StringVar(&arch, "arch", "", "release architecture")
	flags.StringVar(&desktop, "desktop", "", "Codesk desktop executable")
	flags.StringVar(&agent, "agent", "", "agent tool executable")
	flags.StringVar(&icon, "icon", "", "Codesk icon")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("archive arguments: %w", err)
	}
	if flags.NArg() != 0 || output == "" || desktop == "" || agent == "" || icon == "" {
		return errors.New("archive requires --output, --version, --arch, --desktop, --agent, and --icon")
	}
	return desktopsetup.CreatePayloadArchive(output, version, arch, map[string]string{
		"Codesk.exe":           desktop,
		"notty-agent-tool.exe": agent,
		"codesk.ico":           icon,
	})
}

func runAppend(arguments []string) error {
	flags := flag.NewFlagSet("append", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var stub, payload, output string
	flags.StringVar(&stub, "stub", "", "setup stub")
	flags.StringVar(&payload, "payload", "", "payload ZIP")
	flags.StringVar(&output, "output", "", "combined setup output")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("append arguments: %w", err)
	}
	if flags.NArg() != 0 || stub == "" || payload == "" || output == "" {
		return errors.New("append requires --stub, --payload, and --output")
	}
	return desktopsetup.AppendPayload(stub, payload, output)
}

func runManifest(arguments []string) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var output, version, amd64Path, arm64Path string
	var signed bool
	flags.StringVar(&output, "output", "", "release output directory")
	flags.StringVar(&version, "version", "", "release version")
	flags.StringVar(&amd64Path, "amd64", "", "AMD64 setup executable")
	flags.StringVar(&arm64Path, "arm64", "", "ARM64 setup executable")
	flags.BoolVar(&signed, "signed", false, "artifacts are Authenticode signed")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("manifest arguments: %w", err)
	}
	if flags.NArg() != 0 || output == "" || amd64Path == "" || arm64Path == "" {
		return errors.New("manifest requires --output, --version, --amd64, and --arm64")
	}
	return writeReleaseMetadata(output, version, signed, map[string]string{
		"amd64": amd64Path,
		"arm64": arm64Path,
	})
}

func runResourceVersion(arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("resource-version requires exactly one release version")
	}
	if err := validateReleaseVersion(arguments[0]); err != nil {
		return err
	}
	fmt.Println(resourceVersion(arguments[0]))
	return nil
}

func runVerify(arguments []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var allowUnsigned bool
	flags.BoolVar(&allowUnsigned, "allow-unsigned", false, "allow construction-only unsigned artifacts")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("verify arguments: %w", err)
	}
	if flags.NArg() != 2 {
		return errors.New("verify requires [--allow-unsigned] <version-dir> <version>")
	}
	return verifyRelease(flags.Arg(0), flags.Arg(1), allowUnsigned)
}

func writeReleaseMetadata(output, version string, signed bool, paths map[string]string) error {
	if err := validateReleaseVersion(version); err != nil {
		return err
	}
	if len(paths) != len(releaseArchitectures) {
		return errors.New("release metadata requires both Windows architectures")
	}
	manifest := releaseManifest{
		Version:   version,
		Signed:    signed,
		Toolchain: canonicalReleaseToolchain,
		Artifacts: make([]releaseArtifact, 0, len(paths)),
	}
	var checksums strings.Builder
	for _, arch := range releaseArchitectures {
		path := paths[arch]
		expectedName := setupFilename(version, arch)
		if filepath.Base(path) != expectedName {
			return fmt.Errorf("windows/%s setup is named %q, want %q", arch, filepath.Base(path), expectedName)
		}
		hash, err := fileSHA256(path)
		if err != nil {
			return err
		}
		manifest.Artifacts = append(manifest.Artifacts, releaseArtifact{
			Arch: arch, File: expectedName, SHA256: hash, Signed: signed,
		})
		fmt.Fprintf(&checksums, "%s  %s\n", hash, expectedName)
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode release manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')
	if err := writeAtomic(filepath.Join(output, "manifest.json"), manifestData, 0o600); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(output, "SHA256SUMS"), []byte(checksums.String()), 0o600)
}

func setupFilename(version, arch string) string {
	return fmt.Sprintf("CodeskSetup_%s_windows_%s.exe", version, arch)
}

func validateReleaseVersion(version string) error {
	if version == "" || len(version) > 128 || version != strings.TrimSpace(version) {
		return errors.New("invalid release version")
	}
	for index, character := range version {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || index > 0 && strings.ContainsRune("._+-", character)
		if !valid {
			return errors.New("invalid release version")
		}
	}
	return nil
}

func resourceVersion(version string) string {
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

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".codesk-release-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}
