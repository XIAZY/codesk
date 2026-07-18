package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"notty/daemon/internal/desktoprelease"
)

const (
	manifestSchemaVersion = 1
	windowsUpgradeCode    = "{0C8C0BBA-06EE-43BA-BC34-768B9B740A09}"
	crossArchitecture     = "converge"

	releaseGoVersion    = "go1.26.5"
	releaseRustcVersion = "rustc 1.97.0 (2d8144b78 2026-07-07)"
	releaseCargoVersion = "cargo 1.97.0 (c980f4866 2026-06-30)"
	releaseZigVersion   = "0.16.0"
	releaseWiXVersion   = "4.0.5"
)

var (
	builtProductRevision string
	guidPattern          = regexp.MustCompile(`^\{[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}\}$`)
	dotnetVersionPattern = regexp.MustCompile(`^8\.0\.[0-9]+$`)
)

type releaseToolchain struct {
	Go     string `json:"go"`
	Rustc  string `json:"rustc"`
	Cargo  string `json:"cargo"`
	Zig    string `json:"zig"`
	Dotnet string `json:"dotnet"`
	WiX    string `json:"wix"`
}

type releaseArtifact struct {
	Arch            string `json:"arch"`
	File            string `json:"file"`
	SHA256          string `json:"sha256"`
	Signed          bool   `json:"signed"`
	ProductCode     string `json:"product_code"`
	CodeskSHA256    string `json:"codesk_sha256"`
	AgentToolSHA256 string `json:"agent_tool_sha256"`
}

type releaseManifest struct {
	SchemaVersion           int               `json:"schema_version"`
	Version                 string            `json:"version"`
	SourceRevision          string            `json:"source_revision"`
	UpgradeCode             string            `json:"upgrade_code"`
	CrossArchitecturePolicy string            `json:"cross_architecture_policy"`
	Signed                  bool              `json:"signed"`
	Toolchain               releaseToolchain  `json:"toolchain"`
	Artifacts               []releaseArtifact `json:"artifacts"`
}

type artifactInput struct {
	architecture string
	msiPath      string
	productCode  string
	codeskPath   string
	agentPath    string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "codesk-windows-msi-release: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("codesk-windows-msi-release", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var output, version, sourceRevision, dotnetVersion string
	var signed bool
	var amd64, arm64 artifactInput
	flags.StringVar(&output, "output", "", "release output directory")
	flags.StringVar(&version, "version", "", "three-part MSI version")
	flags.StringVar(&sourceRevision, "source-revision", "", "exact product Git revision")
	flags.StringVar(&dotnetVersion, "dotnet-version", "", "dotnet SDK version used to link the MSI")
	flags.BoolVar(&signed, "signed", false, "MSI artifacts are Authenticode signed")
	addArtifactFlags(flags, "amd64", &amd64)
	addArtifactFlags(flags, "arm64", &arm64)
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if output == "" || version == "" || sourceRevision == "" || dotnetVersion == "" {
		return errors.New("--output, --version, --source-revision, and --dotnet-version are required")
	}
	if builtProductRevision == "" {
		return errors.New("producer is not source-bound; compile it with builtProductRevision")
	}
	if err := desktoprelease.ValidateSourceRevision(builtProductRevision); err != nil {
		return fmt.Errorf("built product revision: %w", err)
	}
	if sourceRevision != builtProductRevision {
		return fmt.Errorf("source revision %q does not match producer-bound revision %q", sourceRevision, builtProductRevision)
	}
	if err := validateMSIVersion(version); err != nil {
		return err
	}
	if !dotnetVersionPattern.MatchString(dotnetVersion) {
		return fmt.Errorf("dotnet version %q is not a stable 8.0 SDK version", dotnetVersion)
	}
	return writeRelease(output, version, sourceRevision, dotnetVersion, signed, []artifactInput{amd64, arm64})
}

func addArtifactFlags(flags *flag.FlagSet, architecture string, input *artifactInput) {
	input.architecture = architecture
	flags.StringVar(&input.msiPath, architecture+"-msi", "", strings.ToUpper(architecture)+" MSI path")
	flags.StringVar(&input.productCode, architecture+"-product-code", "", strings.ToUpper(architecture)+" MSI ProductCode")
	flags.StringVar(&input.codeskPath, architecture+"-codesk", "", strings.ToUpper(architecture)+" Codesk.exe path")
	flags.StringVar(&input.agentPath, architecture+"-agent", "", strings.ToUpper(architecture)+" agent-tool path")
}

func writeRelease(output, version, sourceRevision, dotnetVersion string, signed bool, inputs []artifactInput) error {
	if len(inputs) != 2 || inputs[0].architecture != "amd64" || inputs[1].architecture != "arm64" {
		return errors.New("release inputs must be ordered amd64 then arm64")
	}
	outputPath, err := filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve release output: %w", err)
	}
	expectedEntries := make([]desktoprelease.Entry, 0, 2)
	seenProductCodes := make(map[string]struct{}, 2)
	for _, input := range inputs {
		if input.msiPath == "" || input.productCode == "" || input.codeskPath == "" || input.agentPath == "" {
			return fmt.Errorf("windows/%s requires MSI, ProductCode, Codesk.exe, and agent-tool inputs", input.architecture)
		}
		expectedName := msiFilename(version, input.architecture)
		if filepath.Base(input.msiPath) != expectedName {
			return fmt.Errorf("windows/%s MSI is named %q, want %q", input.architecture, filepath.Base(input.msiPath), expectedName)
		}
		msiPath, err := filepath.Abs(input.msiPath)
		if err != nil {
			return fmt.Errorf("resolve windows/%s MSI: %w", input.architecture, err)
		}
		if msiPath != filepath.Join(outputPath, expectedName) {
			return fmt.Errorf("windows/%s MSI must be the canonical file inside the release output", input.architecture)
		}
		if !guidPattern.MatchString(input.productCode) {
			return fmt.Errorf("windows/%s ProductCode %q is not a canonical uppercase MSI GUID", input.architecture, input.productCode)
		}
		if _, duplicate := seenProductCodes[input.productCode]; duplicate {
			return fmt.Errorf("ProductCode %s is reused across architectures", input.productCode)
		}
		seenProductCodes[input.productCode] = struct{}{}
		expectedEntries = append(expectedEntries, desktoprelease.Entry{Name: expectedName, Kind: desktoprelease.RegularFile})
	}
	if err := desktoprelease.VerifyEntries(outputPath, expectedEntries); err != nil {
		return fmt.Errorf("release input directory: %w", err)
	}

	manifest := releaseManifest{
		SchemaVersion:           manifestSchemaVersion,
		Version:                 version,
		SourceRevision:          sourceRevision,
		UpgradeCode:             windowsUpgradeCode,
		CrossArchitecturePolicy: crossArchitecture,
		Signed:                  signed,
		Toolchain: releaseToolchain{
			Go: releaseGoVersion, Rustc: releaseRustcVersion, Cargo: releaseCargoVersion,
			Zig: releaseZigVersion, Dotnet: dotnetVersion, WiX: releaseWiXVersion,
		},
		Artifacts: make([]releaseArtifact, 0, len(inputs)),
	}
	checksums := make([]desktoprelease.Checksum, 0, len(inputs)+1)
	for _, input := range inputs {
		msiHash, err := desktoprelease.FileSHA256(input.msiPath)
		if err != nil {
			return err
		}
		codeskHash, err := desktoprelease.FileSHA256(input.codeskPath)
		if err != nil {
			return err
		}
		agentHash, err := desktoprelease.FileSHA256(input.agentPath)
		if err != nil {
			return err
		}
		name := msiFilename(version, input.architecture)
		manifest.Artifacts = append(manifest.Artifacts, releaseArtifact{
			Arch: input.architecture, File: name, SHA256: msiHash, Signed: signed,
			ProductCode: input.productCode, CodeskSHA256: codeskHash, AgentToolSHA256: agentHash,
		})
		checksums = append(checksums, desktoprelease.Checksum{SHA256: msiHash, File: name})
	}
	manifestData, err := desktoprelease.MarshalCanonicalJSON(manifest)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(outputPath, "manifest.json")
	if err := desktoprelease.WriteAtomic(manifestPath, manifestData, 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	checksums = append(checksums, desktoprelease.Checksum{
		SHA256: desktoprelease.SHA256(manifestData), File: "manifest.json",
	})
	checksumData, err := desktoprelease.MarshalChecksums(checksums)
	if err != nil {
		return err
	}
	sumsPath := filepath.Join(outputPath, "SHA256SUMS")
	if err := desktoprelease.WriteAtomic(sumsPath, checksumData, 0o600); err != nil {
		_ = os.Remove(manifestPath)
		return fmt.Errorf("write SHA256SUMS: %w", err)
	}
	if err := desktoprelease.VerifyEntries(outputPath, []desktoprelease.Entry{
		{Name: msiFilename(version, "amd64"), Kind: desktoprelease.RegularFile},
		{Name: msiFilename(version, "arm64"), Kind: desktoprelease.RegularFile},
		{Name: "manifest.json", Kind: desktoprelease.RegularFile},
		{Name: "SHA256SUMS", Kind: desktoprelease.RegularFile},
	}); err != nil {
		_ = os.Remove(sumsPath)
		_ = os.Remove(manifestPath)
		return err
	}
	return nil
}

func msiFilename(version, architecture string) string {
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
