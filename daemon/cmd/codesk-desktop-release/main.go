package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"notty/daemon/internal/desktoprelease"
	"notty/daemon/internal/desktopsetup"
)

var releaseArchitectures = []string{"amd64", "arm64"}

var windowsReleaseVersionPolicy = desktoprelease.VersionPolicy{
	AllowDevelopment:      true,
	AllowFlexibleArtifact: true,
}

const (
	releaseGoVersion       = "go1.26.5"
	releaseRustcVersion    = "rustc 1.97.0 (2d8144b78 2026-07-07)"
	releaseCargoVersion    = "cargo 1.97.0 (c980f4866 2026-06-30)"
	releaseZigVersion      = "0.16.0"
	releaseGoWinresVersion = "v0.3.1"
)

type releaseManifest struct {
	Version        string            `json:"version"`
	SourceRevision string            `json:"source_revision"`
	Signed         bool              `json:"signed"`
	Toolchain      releaseToolchain  `json:"toolchain"`
	Artifacts      []releaseArtifact `json:"artifacts"`
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
	if _, err := desktoprelease.ParseVersion(version, windowsReleaseVersionPolicy); err != nil {
		return err
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
	var output, version, sourceRevision, amd64Path, arm64Path string
	var signed bool
	flags.StringVar(&output, "output", "", "release output directory")
	flags.StringVar(&version, "version", "", "release version")
	flags.StringVar(&sourceRevision, "source-revision", "", "full source Git revision")
	flags.StringVar(&amd64Path, "amd64", "", "AMD64 setup executable")
	flags.StringVar(&arm64Path, "arm64", "", "ARM64 setup executable")
	flags.BoolVar(&signed, "signed", false, "artifacts are Authenticode signed")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("manifest arguments: %w", err)
	}
	if flags.NArg() != 0 || output == "" || sourceRevision == "" || amd64Path == "" || arm64Path == "" {
		return errors.New("manifest requires --output, --version, --source-revision, --amd64, and --arm64")
	}
	return writeReleaseMetadata(output, version, sourceRevision, signed, map[string]string{
		"amd64": amd64Path,
		"arm64": arm64Path,
	})
}

func runResourceVersion(arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("resource-version requires exactly one release version")
	}
	version, err := desktoprelease.ParseVersion(arguments[0], windowsReleaseVersionPolicy)
	if err != nil {
		return err
	}
	fmt.Println(resourceVersion(version))
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

func writeReleaseMetadata(output, version, sourceRevision string, signed bool, paths map[string]string) error {
	releaseVersion, err := desktoprelease.ParseVersion(version, windowsReleaseVersionPolicy)
	if err != nil {
		return err
	}
	metadata, err := desktoprelease.NewMetadata(releaseVersion, sourceRevision, signed, desktoprelease.TrustPolicy{
		Signature:              desktoprelease.SignatureOptional,
		AllowSignedDevelopment: true,
	})
	if err != nil {
		return err
	}
	if len(paths) != len(releaseArchitectures) {
		return errors.New("release metadata requires both Windows architectures")
	}
	manifest := releaseManifest{
		Version:        metadata.Version,
		SourceRevision: metadata.SourceRevision,
		Signed:         metadata.Signed,
		Toolchain:      canonicalReleaseToolchain,
		Artifacts:      make([]releaseArtifact, 0, len(paths)),
	}
	checksums := make([]desktoprelease.Checksum, 0, len(paths)+1)
	for _, arch := range releaseArchitectures {
		path := paths[arch]
		expectedName := setupFilename(releaseVersion.Artifact, arch)
		if filepath.Base(path) != expectedName {
			return fmt.Errorf("windows/%s setup is named %q, want %q", arch, filepath.Base(path), expectedName)
		}
		hash, err := desktoprelease.FileSHA256(path)
		if err != nil {
			return err
		}
		manifest.Artifacts = append(manifest.Artifacts, releaseArtifact{
			Arch: arch, File: expectedName, SHA256: hash, Signed: signed,
		})
		checksums = append(checksums, desktoprelease.Checksum{SHA256: hash, File: expectedName})
	}
	manifestData, err := desktoprelease.MarshalCanonicalJSON(manifest)
	if err != nil {
		return err
	}
	if err := desktoprelease.WriteAtomic(filepath.Join(output, "manifest.json"), manifestData, 0o600); err != nil {
		return err
	}
	checksums = append(checksums, desktoprelease.Checksum{SHA256: desktoprelease.SHA256(manifestData), File: "manifest.json"})
	checksumData, err := desktoprelease.MarshalChecksums(checksums)
	if err != nil {
		return err
	}
	return desktoprelease.WriteAtomic(filepath.Join(output, "SHA256SUMS"), checksumData, 0o600)
}

func setupFilename(version, arch string) string {
	return fmt.Sprintf("CodeskSetup_%s_windows_%s.exe", version, arch)
}

func resourceVersion(version desktoprelease.Version) string {
	return version.Resource
}
