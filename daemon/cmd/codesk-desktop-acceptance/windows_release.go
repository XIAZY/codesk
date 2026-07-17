package main

import (
	"debug/pe"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"notty/daemon/internal/desktopacceptance"
	"notty/daemon/internal/desktoprelease"
)

type windowsReleaseToolchain struct {
	Go       string `json:"go"`
	Rustc    string `json:"rustc"`
	Cargo    string `json:"cargo"`
	Zig      string `json:"zig"`
	GoWinres string `json:"go_winres"`
}

var expectedWindowsReleaseToolchain = windowsReleaseToolchain{
	Go:       "go1.26.5",
	Rustc:    "rustc 1.97.0 (2d8144b78 2026-07-07)",
	Cargo:    "cargo 1.97.0 (c980f4866 2026-06-30)",
	Zig:      "0.16.0",
	GoWinres: "v0.3.1",
}

type windowsReleaseArtifact struct {
	Arch   string `json:"arch"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Signed bool   `json:"signed"`
}

type windowsReleaseManifest struct {
	Version        string                   `json:"version"`
	SourceRevision string                   `json:"source_revision"`
	Signed         bool                     `json:"signed"`
	Toolchain      windowsReleaseToolchain  `json:"toolchain"`
	Artifacts      []windowsReleaseArtifact `json:"artifacts"`
}

type artifactInspection struct {
	Architecture   string
	SignatureValid bool
	SignatureError string
}

var windowsReleaseVersionPolicy = desktoprelease.VersionPolicy{
	AllowDevelopment:      true,
	AllowFlexibleArtifact: true,
}

func verifyWindowsRelease(
	input desktopacceptance.ReleaseInput,
	inspect func(string) (artifactInspection, error),
) (desktopacceptance.Release, error) {
	version, err := desktoprelease.ParseVersion(input.Version, windowsReleaseVersionPolicy)
	if err != nil {
		return desktopacceptance.Release{}, err
	}
	if inspect == nil {
		return desktopacceptance.Release{}, errors.New("native artifact inspector is required")
	}
	directory, err := filepath.Abs(input.Directory)
	if err != nil {
		return desktopacceptance.Release{}, fmt.Errorf("resolve release directory: %w", err)
	}
	expectedNames := map[string]string{
		"amd64": windowsSetupFilename(version.Artifact, "amd64"),
		"arm64": windowsSetupFilename(version.Artifact, "arm64"),
	}
	if err := desktoprelease.VerifyEntries(directory, []desktoprelease.Entry{
		{Name: "manifest.json", Kind: desktoprelease.RegularFile},
		{Name: "SHA256SUMS", Kind: desktoprelease.RegularFile},
		{Name: expectedNames["amd64"], Kind: desktoprelease.RegularFile},
		{Name: expectedNames["arm64"], Kind: desktoprelease.RegularFile},
	}); err != nil {
		return desktopacceptance.Release{}, err
	}

	manifestPath := filepath.Join(directory, "manifest.json")
	var manifest windowsReleaseManifest
	manifestBytes, err := desktoprelease.ReadCanonicalJSON(manifestPath, &manifest)
	if err != nil {
		return desktopacceptance.Release{}, err
	}
	metadata := desktoprelease.Metadata{
		Version:        manifest.Version,
		SourceRevision: manifest.SourceRevision,
		Signed:         manifest.Signed,
	}
	if err := metadata.Validate(version, desktoprelease.TrustPolicy{
		Signature:              desktoprelease.SignatureOptional,
		AllowSignedDevelopment: true,
	}); err != nil {
		return desktopacceptance.Release{}, fmt.Errorf("manifest: %w", err)
	}
	if input.SourceRevision != "" && manifest.SourceRevision != input.SourceRevision {
		return desktopacceptance.Release{}, fmt.Errorf(
			"manifest source revision %q does not match %q",
			manifest.SourceRevision,
			input.SourceRevision,
		)
	}
	if manifest.Toolchain != expectedWindowsReleaseToolchain {
		return desktopacceptance.Release{}, fmt.Errorf(
			"manifest toolchain %#v does not match the canonical Windows release toolchain",
			manifest.Toolchain,
		)
	}
	architectures := []string{"amd64", "arm64"}
	if len(manifest.Artifacts) != len(architectures) {
		return desktopacceptance.Release{}, fmt.Errorf("manifest has %d artifacts, want %d", len(manifest.Artifacts), len(architectures))
	}

	release := desktopacceptance.Release{
		Platform:       "windows",
		Version:        metadata.Version,
		SourceRevision: metadata.SourceRevision,
		Signed:         metadata.Signed,
		Toolchain: map[string]string{
			"go":        manifest.Toolchain.Go,
			"rustc":     manifest.Toolchain.Rustc,
			"cargo":     manifest.Toolchain.Cargo,
			"zig":       manifest.Toolchain.Zig,
			"go-winres": manifest.Toolchain.GoWinres,
		},
		ManifestPath:   manifestPath,
		ManifestSHA256: desktoprelease.SHA256(manifestBytes),
		SumsPath:       filepath.Join(directory, "SHA256SUMS"),
	}
	checksums := make([]desktoprelease.Checksum, 0, len(architectures)+1)
	for index, architecture := range architectures {
		item := manifest.Artifacts[index]
		if item.Arch != architecture {
			return desktopacceptance.Release{}, fmt.Errorf(
				"manifest artifact %d has architecture %q, want %q",
				index+1,
				item.Arch,
				architecture,
			)
		}
		if item.File != expectedNames[architecture] {
			return desktopacceptance.Release{}, fmt.Errorf(
				"windows/%s artifact is named %q, want %q",
				architecture,
				item.File,
				expectedNames[architecture],
			)
		}
		if item.Signed != manifest.Signed {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s signed bit disagrees with manifest", architecture)
		}
		path := filepath.Join(directory, item.File)
		info, err := os.Lstat(path)
		if err != nil {
			return desktopacceptance.Release{}, fmt.Errorf("inspect release artifact: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 1<<30 {
			return desktopacceptance.Release{}, fmt.Errorf("release artifact is not a bounded regular file: %s", path)
		}
		actualHash, err := desktoprelease.FileSHA256(path)
		if err != nil {
			return desktopacceptance.Release{}, err
		}
		if item.SHA256 != actualHash {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s artifact SHA-256 mismatch", architecture)
		}
		machine, err := peArchitecture(path)
		if err != nil {
			return desktopacceptance.Release{}, err
		}
		if machine != architecture {
			return desktopacceptance.Release{}, fmt.Errorf("windows/%s artifact PE machine is %s", architecture, machine)
		}
		native, err := inspect(path)
		if err != nil {
			return desktopacceptance.Release{}, fmt.Errorf("inspect native artifact %s: %w", path, err)
		}
		if native.Architecture != architecture {
			return desktopacceptance.Release{}, fmt.Errorf(
				"native inspector reports %s for windows/%s",
				native.Architecture,
				architecture,
			)
		}
		checksums = append(checksums, desktoprelease.Checksum{SHA256: actualHash, File: item.File})
		release.Artifacts = append(release.Artifacts, desktopacceptance.ReleaseArtifact{
			Architecture:       architecture,
			Path:               path,
			SHA256:             actualHash,
			Size:               info.Size(),
			ManifestSigned:     item.Signed,
			NativeFormat:       "pe",
			NativeArchitecture: machine,
			SignatureValid:     native.SignatureValid,
			SignatureError:     native.SignatureError,
		})
	}
	checksums = append(checksums, desktoprelease.Checksum{
		SHA256: release.ManifestSHA256,
		File:   "manifest.json",
	})
	if err := desktoprelease.VerifyChecksums(release.SumsPath, checksums); err != nil {
		return desktopacceptance.Release{}, err
	}
	release.SumsSHA256, err = desktoprelease.FileSHA256(release.SumsPath)
	if err != nil {
		return desktopacceptance.Release{}, err
	}
	sort.Slice(release.Artifacts, func(i, j int) bool {
		return release.Artifacts[i].Architecture < release.Artifacts[j].Architecture
	})
	return release, nil
}

func windowsSetupFilename(version, architecture string) string {
	return fmt.Sprintf("CodeskSetup_%s_windows_%s.exe", version, architecture)
}

func peArchitecture(path string) (string, error) {
	file, err := pe.Open(path)
	if err != nil {
		return "", fmt.Errorf("open PE artifact: %w", err)
	}
	defer file.Close()
	switch file.FileHeader.Machine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "amd64", nil
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported PE machine 0x%x", file.FileHeader.Machine)
	}
}
