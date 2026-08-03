package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const windowsProductCodeNamespace = "55a27873-bf9c-5dc3-aa8b-9d6f996041ef"

var canonicalMSIProductVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func TestGUIBuildTargetsRejectWrongKernel(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is unavailable")
	}
	tests := []struct {
		name       string
		targetOS   string
		target     string
		overrides  []string
		wantOutput string
	}{
		{
			name:       "macOS",
			targetOS:   "darwin",
			target:     "macos-gui-build",
			overrides:  []string{"HOST_OS=darwin", "MACOS_GUI_HOST_OS=darwin"},
			wantOutput: "macos-gui-build requires a real macOS host; no GUI was built",
		},
		{
			name:       "Windows",
			targetOS:   "windows",
			target:     "windows-gui-build",
			overrides:  []string{"WINDOWS_GUI_HOST_OS=windows"},
			wantOutput: "windows-gui-build requires a real Windows host; no GUI was built",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if runtime.GOOS == test.targetOS {
				t.Skipf("%s is the native host", test.targetOS)
			}
			args := append([]string{"-s", test.target}, test.overrides...)
			command := exec.Command("make", args...)
			command.Dir = ".."
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("make %s succeeded on %s", test.target, runtime.GOOS)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("make %s output = %q, want %q", test.target, output, test.wantOutput)
			}
		})
	}
}

func TestWindowsGUIMakeRoutesWithoutPOSIXTools(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is unavailable")
	}
	makeData, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	makeSource := normalizeSourceNewlines(string(makeData))

	for _, target := range []string{"windows-gui-build", "windows-gui-deploy"} {
		if err := runWindowsGUIDispatch(makeSource, target); err != nil {
			t.Fatalf("Windows-native Make %s dispatch failed: %v", target, err)
		}
	}
	old := `make.ps1 windows-gui-build "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`
	if strings.Count(makeSource, old) != 1 {
		t.Fatalf("Windows build route source count = %d, want 1", strings.Count(makeSource, old))
	}
	mutated := strings.Replace(makeSource, old, `make.ps1 windows-gui-build "WINDOWS_GUI_ARCHES=amd64" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`, 1)
	if err := runWindowsGUIDispatch(mutated, "windows-gui-build"); err == nil {
		t.Fatal("Make injected a spoofed build architecture into the PowerShell route")
	}
}

func TestWindowsMSIReleaseVersionAndProductCodeContract(t *testing.T) {
	for _, version := range []string{"0.0.0", "1.2.3", "255.255.65535"} {
		if err := validateMSIProductVersion(version); err != nil {
			t.Errorf("valid version %q rejected: %v", version, err)
		}
	}
	for _, version := range []string{
		"", "dev", "desktop-v1.2.3", "v1.2.3", "01.2.3", "1.02.3", "1.2.03",
		"256.2.3", "1.256.3", "1.2.65536", "1.2", "1.2.3.4", "1.2.3-rc.1",
		"1.2.3+build", "-1.2.3", "4294967296.1.1",
	} {
		if err := validateMSIProductVersion(version); err == nil {
			t.Errorf("invalid version %q accepted", version)
		}
	}

	vectors := map[string]string{
		"0.0.0+amd64":         "{EC007635-30D0-5F8F-9C0D-F9D361E5517F}",
		"0.0.1+amd64":         "{1FE313EB-FF6F-546C-9E89-C8E4B056905E}",
		"0.0.1+arm64":         "{890D863D-C26F-5A68-995B-08C75A43143E}",
		"1.2.3+amd64":         "{BA38D73A-53FE-5601-9279-0960877911B5}",
		"1.2.3+arm64":         "{DD1C3E13-0DB4-5670-9274-2E172E060E39}",
		"255.255.65535+amd64": "{E61B7D4A-91B9-5B4C-82BD-EC13B7D637A6}",
	}
	for name, want := range vectors {
		got, err := uuidV5(windowsProductCodeNamespace, name)
		if err != nil {
			t.Fatalf("derive ProductCode for %q: %v", name, err)
		}
		if got != want {
			t.Errorf("ProductCode for %q = %s, want %s", name, got, want)
		}
	}
}

func TestWindowsGUIPowerShellRejectsRemovedVersionArgument(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell defaults require Windows")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}

	command := exec.Command(
		powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", "scripts/run-windows-gui-target.ps1", "-Target", "windows-gui-deploy", "-Version", "invalid",
	)
	command.Dir = ".."
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid release version unexpectedly succeeded: %s", output)
	}
	text := string(output)
	if !strings.Contains(text, "Version") || !strings.Contains(text, "parameter") {
		t.Fatalf("removed version argument failure = %q, want parameter rejection", output)
	}
	for _, regression := range []string{
		"Windows GUI architecture list must not be empty",
		"Cannot bind argument to parameter 'Path' because it is an empty string",
	} {
		if strings.Contains(text, regression) {
			t.Fatalf("release default regressed with %q: %s", regression, output)
		}
	}
}

func TestWindowsGUIDeployRejectsArchitectureSubset(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell defaults require Windows")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}

	command := exec.Command(
		powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", "scripts/run-windows-gui-target.ps1", "-Target", "windows-gui-deploy", "-Architectures", "amd64",
	)
	command.Dir = ".."
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("single-architecture deploy unexpectedly succeeded: %s", output)
	}
	if !strings.Contains(string(output), "windows-gui-deploy requires exactly amd64 then arm64") {
		t.Fatalf("single-architecture deploy failure = %q", output)
	}
}

func TestPowerShellDaemonVersionReaderSourceContract(t *testing.T) {
	data, err := os.ReadFile("read-daemon-version.ps1")
	if err != nil {
		t.Fatal(err)
	}
	source := normalizeSourceNewlines(string(data))
	if err := checkPowerShellDaemonVersionReaderSource(source); err != nil {
		t.Fatal(err)
	}
	mutations := []struct{ name, old, new string }{
		{"text reader fallback", "[System.IO.File]::ReadAllBytes", "[System.IO.File]::ReadAllText"},
		{"trailing LF changed", "$bytes[$bytes.Length - 1] -ne 10", "$bytes[$bytes.Length - 1] -ne 13"},
		{"CRLF detection changed", "$bytes[$lineLength - 1] -eq 13", "$bytes[$lineLength - 1] -eq 10"},
		{"embedded newline scan shortened", "for ($index = 0; $index -lt $lineLength; $index++)", "for ($index = 0; $index -lt $lineLength - 1; $index++)"},
		{"embedded LF changed", "$bytes[$index] -eq 10", "$bytes[$index] -eq 13"},
		{"leading zeros accepted", "^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$", "^[0-9]+\\.[0-9]+\\.[0-9]+$"},
		{"major range widened", "$major -gt 255", "$major -gt 256"},
		{"minor range widened", "$minor -gt 255", "$minor -gt 256"},
		{"build range widened", "$build -gt 65535", "$build -gt 65536"},
		{"numeric overflow check removed", "[uint32]::TryParse($fields[0]", "[uint64]::TryParse($fields[0]"},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.old) != 1 {
				t.Fatalf("mutation source count for %q is not one", mutation.old)
			}
			mutated := strings.Replace(source, mutation.old, mutation.new, 1)
			if err := checkPowerShellDaemonVersionReaderSource(mutated); err == nil {
				t.Fatal("PowerShell reader mutation survived")
			}
		})
	}
}

func TestWindowsMSIReleaseSourceContract(t *testing.T) {
	buildData, err := os.ReadFile("build-windows-desktop-msi-artifact.ps1")
	if err != nil {
		t.Fatal(err)
	}
	orchestratorData, err := os.ReadFile("run-windows-gui-target.ps1")
	if err != nil {
		t.Fatal(err)
	}
	makeData, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	shimData, err := os.ReadFile("../make.ps1")
	if err != nil {
		t.Fatal(err)
	}
	build := normalizeSourceNewlines(string(buildData))
	orchestrator := normalizeSourceNewlines(string(orchestratorData))
	makefile := normalizeSourceNewlines(string(makeData))
	shim := normalizeSourceNewlines(string(shimData))
	if err := checkWindowsMSIReleaseSource(build, orchestrator, makefile, shim); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name            string
		buildOld        string
		buildNew        string
		orchestratorOld string
		orchestratorNew string
		makeOld         string
		makeNew         string
		shimOld         string
		shimNew         string
	}{
		{
			name:     "QA fixture becomes the default mode",
			buildOld: "[CmdletBinding(DefaultParameterSetName = 'Release')]",
			buildNew: "[CmdletBinding(DefaultParameterSetName = 'TestOnlyUpgradeQa')]",
		},
		{
			name:     "architecture removed from UUID name",
			buildOld: "$productCodeName = \"$ProductVersion+$GoArchitecture\"",
			buildNew: "$productCodeName = $ProductVersion",
		},
		{
			name:     "UUID namespace drift",
			buildOld: "55A27873-BF9C-5DC3-AA8B-9D6F996041EF",
			buildNew: "55A27873-BF9C-5DC3-AA8B-9D6F996041E0",
		},
		{
			name:     "UUID version nibble drift",
			buildOld: "-bor 0x50",
			buildNew: "-bor 0x40",
		},
		{
			name:     "release output loses requested version",
			buildOld: `$canonicalName = "Codesk_$($version.version)_windows_$GoArchitecture.msi"`,
			buildNew: `$canonicalName = "Codesk_release_windows_$GoArchitecture.msi"`,
		},
		{
			name:            "release invokes QA mode",
			orchestratorOld: "Release = $true",
			orchestratorNew: "PreviousProductCode = $version",
		},
		{
			name:            "Windows build host gate removed",
			orchestratorOld: "[System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT",
			orchestratorNew: "[System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT",
		},
		{
			name:            "Windows ARM64 host resolves to AMD64",
			orchestratorOld: "'ARM64' { return 'arm64' }",
			orchestratorNew: "'ARM64' { return 'amd64' }",
		},
		{
			name:    "macOS build host gate removed",
			makeOld: "macos-gui-build:\n\t@if [ \"$(MACOS_GUI_HOST_OS)\" != darwin ]; then",
			makeNew: "macos-gui-build:\n\t@if [ \"$(MACOS_GUI_HOST_OS)\" = darwin ]; then",
		},
		{
			name:            "release accepts empty artifact",
			orchestratorOld: "$item.Length -le 0",
			orchestratorNew: "$item.Length -lt 0",
		},
		{
			name:            "release accepts an architecture subset",
			orchestratorOld: "$result.Count -ne 2 -or $result[0] -cne 'amd64' -or $result[1] -cne 'arm64'",
			orchestratorNew: "$result.Count -lt 1",
		},
		{
			name:    "Make bypasses the shared orchestrator",
			makeOld: "-File make.ps1 windows-gui-build",
			makeNew: "-File scripts/build-windows-desktop-msi-artifact.ps1 windows-gui-build",
		},
		{
			name:    "Make injects a spoofable build architecture",
			makeOld: `make.ps1 windows-gui-build "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`,
			makeNew: `make.ps1 windows-gui-build "WINDOWS_GUI_ARCHES=amd64" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`,
		},
		{
			name:    "PowerShell shim loses a public target",
			shimOld: "'macos-gui-build', 'macos-gui-deploy', 'windows-gui-build', 'windows-gui-deploy'",
			shimNew: "'macos-gui-build', 'macos-gui-deploy', 'windows-gui-build'",
		},
		{
			name:    "PowerShell shim skips the shared R2 uploader",
			shimOld: "Invoke-WindowsGuiUpload -RepositoryRoot $PSScriptRoot -ResolvedSettings $settings",
			shimNew: "Write-Host 'upload skipped'",
		},
		{
			name:    "PowerShell shim bypasses the container orchestrator",
			shimOld: "scripts/run-windows-gui-container.ps1",
			shimNew: "scripts/build-windows-desktop-msi-artifact.ps1",
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutatedBuild := build
			mutatedOrchestrator := orchestrator
			mutatedMake := makefile
			mutatedShim := shim
			if mutation.buildOld != "" {
				if strings.Count(mutatedBuild, mutation.buildOld) == 0 {
					t.Fatalf("build mutation source %q is absent", mutation.buildOld)
				}
				mutatedBuild = strings.Replace(mutatedBuild, mutation.buildOld, mutation.buildNew, 1)
			}
			if mutation.orchestratorOld != "" {
				if strings.Count(mutatedOrchestrator, mutation.orchestratorOld) == 0 {
					t.Fatalf("orchestrator mutation source %q is absent", mutation.orchestratorOld)
				}
				mutatedOrchestrator = strings.Replace(mutatedOrchestrator, mutation.orchestratorOld, mutation.orchestratorNew, 1)
			}
			if mutation.makeOld != "" {
				if strings.Count(mutatedMake, mutation.makeOld) == 0 {
					t.Fatalf("Make mutation source %q is absent", mutation.makeOld)
				}
				mutatedMake = strings.Replace(mutatedMake, mutation.makeOld, mutation.makeNew, 1)
			}
			if mutation.shimOld != "" {
				if strings.Count(mutatedShim, mutation.shimOld) == 0 {
					t.Fatalf("shim mutation source %q is absent", mutation.shimOld)
				}
				mutatedShim = strings.Replace(mutatedShim, mutation.shimOld, mutation.shimNew, 1)
			}
			if err := checkWindowsMSIReleaseSource(mutatedBuild, mutatedOrchestrator, mutatedMake, mutatedShim); err == nil {
				t.Fatal("release contract mutation passed")
			}
		})
	}
}

func TestWindowsGUIUploadPreflightSourceContract(t *testing.T) {
	uploadData, err := os.ReadFile("upload-r2.sh")
	if err != nil {
		t.Fatal(err)
	}
	provenanceData, err := os.ReadFile("verify-windows-gui-upload-provenance.ps1")
	if err != nil {
		t.Fatal(err)
	}
	upload := normalizeSourceNewlines(string(uploadData))
	provenance := normalizeSourceNewlines(string(provenanceData))
	if err := checkWindowsGUIUploadPreflightSource(upload, provenance); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name          string
		uploadOld     string
		uploadNew     string
		provenanceOld string
		provenanceNew string
	}{
		{
			name:      "actual checksum comparison removed",
			uploadOld: `[ "$preflight_expected_hash" = "$preflight_actual_hash" ]`,
			uploadNew: `[ -n "$preflight_expected_hash" ]`,
		},
		{
			name:      "one architecture skips preflight",
			uploadOld: `preflight_windows_gui_arch "$arch"`,
			uploadNew: `: "$arch"`,
		},
		{
			name:      "private staging skipped",
			uploadOld: `stage_windows_gui_arch "$arch"`,
			uploadNew: `: "$arch"`,
		},
		{
			name:      "upload returns to mutable source",
			uploadOld: `arch_dir="$windows_gui_staged_root/$arch"`,
			uploadNew: `arch_dir="$windows_gui_input_root/$arch"`,
		},
		{
			name:          "JSON parser removed",
			provenanceOld: `ConvertFrom-Json`,
			provenanceNew: `ConvertTo-Json`,
		},
		{
			name:          "publishability binding removed",
			provenanceOld: `$publishable -isnot [bool] -or -not $publishable`,
			provenanceNew: `$null -eq $publishable`,
		},
		{
			name:          "source head binding removed",
			provenanceOld: `source.sourceHead`,
			provenanceNew: `source.unboundHead`,
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutatedUpload := upload
			mutatedProvenance := provenance
			if mutation.uploadOld != "" {
				if strings.Count(mutatedUpload, mutation.uploadOld) != 1 {
					t.Fatalf("upload mutation source count for %q = %d, want 1", mutation.uploadOld, strings.Count(mutatedUpload, mutation.uploadOld))
				}
				mutatedUpload = strings.Replace(mutatedUpload, mutation.uploadOld, mutation.uploadNew, 1)
			}
			if mutation.provenanceOld != "" {
				if strings.Count(mutatedProvenance, mutation.provenanceOld) != 1 {
					t.Fatalf("provenance mutation source count for %q = %d, want 1", mutation.provenanceOld, strings.Count(mutatedProvenance, mutation.provenanceOld))
				}
				mutatedProvenance = strings.Replace(mutatedProvenance, mutation.provenanceOld, mutation.provenanceNew, 1)
			}
			if err := checkWindowsGUIUploadPreflightSource(mutatedUpload, mutatedProvenance); err == nil {
				t.Fatal("Windows GUI upload preflight mutation passed")
			}
		})
	}
}

func TestBuildDeployContractIsCIGated(t *testing.T) {
	workflowData, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := normalizeSourceNewlines(string(workflowData))
	if err := checkBuildDeployContractCIGate(workflow); err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(workflow, "          make build-deploy-contract-check\n", "          : build-deploy-contract-check\n", 1)
	if err := checkBuildDeployContractCIGate(mutated); err == nil {
		t.Fatal("build/deploy CI gate removal mutation passed")
	}
}

func checkBuildDeployContractCIGate(workflow string) error {
	const gate = `      - name: Installer and release contract tests
        run: |
          make daemon-installer-check
          make daemon-uninstall-test
          make build-deploy-contract-check
`
	if got := strings.Count(workflow, gate); got != 1 {
		return fmt.Errorf("build/deploy CI gate count = %d, want 1", got)
	}
	return nil
}

func TestWindowsMSICIRunsNativeValidationAndLifecycle(t *testing.T) {
	workflowData, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycleData, err := os.ReadFile("test-windows-desktop-msi-lifecycle.ps1")
	if err != nil {
		t.Fatal(err)
	}
	workflow := normalizeSourceNewlines(string(workflowData))
	lifecycle := normalizeSourceNewlines(string(lifecycleData))
	if err := checkWindowsMSICILifecycle(workflow, lifecycle); err != nil {
		t.Fatal(err)
	}

	for _, mutation := range []struct {
		name, target, old, replacement string
	}{
		{"published builder image not pulled", "workflow", `docker pull $imageRef`, `Write-Host "builder pull skipped"`},
		{"pulled image ID not pinned", "workflow", `-BuilderImage "${{ steps.builder.outputs.image_id }}"`, `-BuilderImage "${{ needs.builder-manifest.outputs.image }}"`},
		{"container does not perform WiX deploy", "workflow", `-Target windows-gui-deploy`, `-Target windows-gui-build`},
		{"lifecycle not invoked", "workflow", `./scripts/test-windows-desktop-msi-lifecycle.ps1`, `Write-Host "MSI lifecycle skipped"`},
		{"release install removed", "lifecycle", `Invoke-Msi -Operation install -Package $release.release.MsiPath`, `Write-Host "release install skipped"`},
		{"release uninstall removed", "lifecycle", `Invoke-Msi -Operation uninstall -Package $release.release.MsiPath`, `Write-Host "release uninstall skipped"`},
		{"installed payload hash not checked", "lifecycle", `installed Codesk.exe does not match the validated payload`, `installed Codesk.exe was not checked`},
		{"checksum inventory not parsed", "lifecycle", `Get-Content -LiteralPath $checksumsPath`, `@()`},
		{"post-uninstall registry state not checked", "lifecycle", `release uninstall left component registry state`, `component registry state was not checked`},
		{"installed binary not executed", "lifecycle", `$installedAgentTool --version`, `$ExpectedVersion`},
		{"quiet install removed", "lifecycle", `'/qn'`, `'/passive'`},
		{"restart suppression removed", "lifecycle", `'/norestart'`, `'/forcerestart'`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutatedWorkflow, mutatedLifecycle := workflow, lifecycle
			var source *string
			switch mutation.target {
			case "workflow":
				source = &mutatedWorkflow
			case "lifecycle":
				source = &mutatedLifecycle
			default:
				t.Fatalf("unsupported mutation target %q", mutation.target)
			}
			if strings.Count(*source, mutation.old) != 1 {
				t.Fatalf("mutation source %q is not unique", mutation.old)
			}
			*source = strings.Replace(*source, mutation.old, mutation.replacement, 1)
			if err := checkWindowsMSICILifecycle(mutatedWorkflow, mutatedLifecycle); err == nil {
				t.Fatal("Windows MSI lifecycle mutation passed")
			}
		})
	}

	mutated := workflow + "\n      - uses: actions/upload-artifact@v4\n"
	if err := checkWindowsMSICILifecycle(mutated, lifecycle); err == nil {
		t.Fatal("artifact-quota handoff mutation passed")
	}
}

func checkWindowsMSICILifecycle(workflow, lifecycle string) error {
	job, err := windowsNativeCIJob(workflow)
	if err != nil {
		return err
	}
	for required, count := range map[string]int{
		"fetch-depth: 0": 1,
		`$imageRef = "${{ needs.builder-manifest.outputs.image }}"`: 1,
		`docker pull $imageRef`: 1,
		"- name: Build and ICE-validate Windows payloads and MSIs in the builder image": 1,
		`./scripts/run-windows-gui-container.ps1`:                                       1,
		`-Target windows-gui-deploy`:                                                    1,
		`-BuilderImage "${{ steps.builder.outputs.image_id }}"`:                         1,
		"- name: Install, verify, and uninstall the runner-native MSI":                  1,
		`./scripts/test-windows-desktop-msi-lifecycle.ps1`:                              1,
		`dist\windows-gui\msi\$architecture`:                                            1,
		`dist\windows-gui\payload\$architecture`:                                        1,
	} {
		if got := strings.Count(job, required); got != count {
			return fmt.Errorf("Windows MSI CI source count for %q = %d, want %d", required, got, count)
		}
	}
	for _, forbidden := range []string{
		"actions/upload-artifact",
		"actions/download-artifact",
		"actions/setup-dotnet",
		"Install runner-native LLVM-MinGW toolchain",
	} {
		if strings.Contains(workflow, forbidden) {
			return fmt.Errorf("Windows MSI CI reintroduced artifact-quota plumbing %q", forbidden)
		}
	}
	for required, count := range map[string]int{
		"-BuildMode 'release' -Publishable $true -Roles @('release')":                 1,
		"Get-FileHash -LiteralPath $msiPath -Algorithm SHA256":                        1,
		"Get-Content -LiteralPath $checksumsPath":                                     1,
		"SHA256SUMS does not match provenance for $name":                              1,
		"Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\\msiexec.exe')": 1,
		"$verb = if ($Operation -ceq 'install') { '/i' } else { '/x' }":               1,
		"'/qn'":        1,
		"'/norestart'": 1,
		"'/l*v'":       1,
		"installed Codesk.exe does not match the validated payload":         1,
		"installed agent tool does not match the validated payload":         1,
		"$installedAgentTool --version":                                     1,
		"Start Menu shortcut targets the wrong executable":                  1,
		"Codesk component registration is missing":                          1,
		"Invoke-Msi -Operation install -Package $release.release.MsiPath":   1,
		"Invoke-Msi -Operation uninstall -Package $release.release.MsiPath": 1,
		"release uninstall left the install directory":                      1,
		"release uninstall left component registry state":                   1,
	} {
		if got := strings.Count(lifecycle, required); got != count {
			return fmt.Errorf("Windows MSI lifecycle source count for %q = %d, want %d", required, got, count)
		}
	}
	return nil
}

func windowsNativeCIJob(workflow string) (string, error) {
	const start = "\n  windows-daemon:\n"
	const end = "\n  regression-e2e:\n"
	startAt := strings.Index(workflow, start)
	if startAt < 0 {
		return "", fmt.Errorf("Windows native workflow has no windows-daemon job")
	}
	endOffset := strings.Index(workflow[startAt+len(start):], end)
	if endOffset < 0 {
		return "", fmt.Errorf("Windows native workflow has no job boundary after windows-daemon")
	}
	return workflow[startAt : startAt+len(start)+endOffset], nil
}

func TestWindowsMSITestOnlyUpgradeFixtureCannotBecomeReleaseArtifact(t *testing.T) {
	buildData, err := os.ReadFile("build-windows-desktop-msi-artifact.ps1")
	if err != nil {
		t.Fatal(err)
	}
	fixtureData, err := os.ReadFile(filepath.Join("testdata", "windows-msi-upgrade-versions.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	workflowData, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	build := normalizeSourceNewlines(string(buildData))
	fixture := normalizeSourceNewlines(string(fixtureData))
	workflow := normalizeSourceNewlines(string(workflowData))
	if err := checkWindowsMSITestOnlyFixtureBoundary(build, fixture, workflow); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name, target, old, replacement string
	}{
		{"fixture escapes testdata", "build", `testdata\windows-msi-upgrade-versions.ps1`, `windows-msi-upgrade-versions.ps1`},
		{"fixture becomes default", "build", "[CmdletBinding(DefaultParameterSetName = 'Release')]", "[CmdletBinding(DefaultParameterSetName = 'TestOnlyUpgradeQa')]"},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutatedBuild, mutatedFixture, mutatedWorkflow := build, fixture, workflow
			var source *string
			switch mutation.target {
			case "build":
				source = &mutatedBuild
			default:
				t.Fatalf("unsupported mutation target %q", mutation.target)
			}
			if strings.Count(*source, mutation.old) != 1 {
				t.Fatalf("mutation source %q is not unique", mutation.old)
			}
			*source = strings.Replace(*source, mutation.old, mutation.replacement, 1)
			if err := checkWindowsMSITestOnlyFixtureBoundary(mutatedBuild, mutatedFixture, mutatedWorkflow); err == nil {
				t.Fatal("test-only fixture boundary mutation survived")
			}
		})
	}
}

func checkWindowsMSITestOnlyFixtureBoundary(build, fixture, workflow string) error {
	for required, count := range map[string]int{
		"[CmdletBinding(DefaultParameterSetName = 'Release')]":                   1,
		"[Parameter(Mandatory = $true, ParameterSetName = 'TestOnlyUpgradeQa')]": 1,
		`testdata\windows-msi-upgrade-versions.ps1`:                              1,
		"publishable = ($buildMode -ceq 'release')":                              1,
	} {
		if got := strings.Count(build, required); got != count {
			return fmt.Errorf("MSI builder fixture boundary count for %q = %d, want %d", required, got, count)
		}
	}
	for _, forbidden := range []string{"version = '0.0.1'", "version = '0.0.2'"} {
		if strings.Contains(build, forbidden) {
			return fmt.Errorf("production builder contains fixture version %q", forbidden)
		}
	}
	for required, count := range map[string]int{
		"production artifact path never reads this fixture": 1,
		"version = '0.0.1'": 1,
		"version = '0.0.2'": 1,
	} {
		if got := strings.Count(fixture, required); got != count {
			return fmt.Errorf("MSI test fixture count for %q = %d, want %d", required, got, count)
		}
	}

	// Native CI installs the production release package. The synthetic pair remains a local
	// builder fixture and cannot enter CI or publication accidentally.
	if strings.Contains(workflow, "actions/upload-artifact") {
		return fmt.Errorf("test-only MSI fixture is exposed to artifact upload")
	}
	return nil
}

func TestWindowsGUIContainerSourceContract(t *testing.T) {
	builderData, err := os.ReadFile("build-windows-gui-builder-image.ps1")
	if err != nil {
		t.Fatal(err)
	}
	wrapperData, err := os.ReadFile("run-windows-gui-container.ps1")
	if err != nil {
		t.Fatal(err)
	}
	dockerfileData, err := os.ReadFile("../deploy/windows-desktop/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	builder := normalizeSourceNewlines(string(builderData))
	wrapper := normalizeSourceNewlines(string(wrapperData))
	dockerfile := normalizeSourceNewlines(string(dockerfileData))

	checkWrapper := func(source string) error {
		for required, want := range map[string]int{
			"[string] $BuilderImage = 'ghcr.io/xiazy/notty-windows-builder:latest'":                             1,
			"docker info --format '{{.OSType}}|{{.Architecture}}|{{.OSVersion}}'":                               1,
			"[System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()":                    1,
			"Docker engine architecture $dockerArchitecture does not match host architecture $hostArchitecture": 1,
			"'create', '--isolation=process'":                                                                   1,
			"docker image inspect $BuilderImage":                                                                2,
			"scripts/build-windows-gui-builder-image.ps1":                                                       1,
			"is not available; building it now":                                                                 1,
			"WINDOWS_GUI_CC_AMD64=C:/toolchains/llvm-mingw/bin/x86_64-w64-mingw32-clang.exe -static":            1,
			"WINDOWS_GUI_CC_ARM64=C:/toolchains/llvm-mingw/bin/aarch64-w64-mingw32-clang.exe -static":           1,
			"third_party/y-crdt/Cargo.lock":                                                                     1,
			`("$root\.")`:                                                                                       1,
			`"${containerId}:C:\workspace"`:                                                                     1,
			"& docker start --attach $containerId":                                                              1,
			"docker inspect $containerId --format '{{.State.ExitCode}}'":                                        1,
			`"${containerId}:$containerSource"`:                                                                 1,
			"& docker rm --force $containerId":                                                                  1,
			"scripts\\run-windows-gui-target.ps1":                                                               1,
		} {
			if got := strings.Count(source, required); got != want {
				return fmt.Errorf("Windows container runner source count for %q = %d, want %d", required, got, want)
			}
		}
		for _, forbidden := range []string{"'run', '--rm'", "--mount", "type=bind"} {
			if strings.Contains(source, forbidden) {
				return fmt.Errorf("Windows container runner retains daemon-host-dependent source %q", forbidden)
			}
		}
		return nil
	}
	if err := checkWrapper(wrapper); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "source directory nested instead of contents copied", old: `("$root\.")`, new: `$root`},
		{name: "source copy omitted", old: `"${containerId}:C:\workspace"`, new: `"${containerId}:C:\missing"`},
		{name: "container start detached", old: "& docker start --attach $containerId", new: "& docker start $containerId"},
		{name: "container exit ignored", old: "docker inspect $containerId --format '{{.State.ExitCode}}'", new: "Write-Host 'container complete'"},
		{name: "output copy omitted", old: `"${containerId}:$containerSource"`, new: `"${containerId}:C:/discarded"`},
		{name: "container cleanup omitted", old: "& docker rm --force $containerId", new: "Write-Host 'container retained'"},
		{name: "daemon-host bind restored", old: "# Docker cp streams through the client API", new: "# --mount type=bind bypasses Docker cp"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(wrapper, mutation.old) != 1 {
				t.Fatalf("wrapper mutation source %q is not unique", mutation.old)
			}
			mutated := strings.Replace(wrapper, mutation.old, mutation.new, 1)
			if err := checkWrapper(mutated); err == nil {
				t.Fatal("Windows container wrapper mutation passed")
			}
		})
	}

	for source, want := range map[string]int{
		"[string] $BuilderImage = 'ghcr.io/xiazy/notty-windows-builder:latest'":                             1,
		"docker info --format '{{.OSType}}|{{.Architecture}}|{{.OSVersion}}'":                               1,
		"[System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()":                    1,
		"Docker engine architecture $dockerArchitecture does not match host architecture $hostArchitecture": 1,
		"& docker build":      1,
		"--isolation=process": 1,
		"--build-arg \"TARGETARCH=$dockerArchitecture\"": 1,
	} {
		if got := strings.Count(builder, source); got != want {
			t.Errorf("Windows builder image source count for %q = %d, want %d", source, got, want)
		}
	}
	for source, want := range map[string]int{
		"ARG TARGETARCH": 2,
		"FROM mcr.microsoft.com/windows/servercore:ltsc2025-${TARGETARCH}": 1,
		"GO_VERSION=\"1.23.12\"":                                    1,
		"ZIG_VERSION=\"0.16.0\"":                                    1,
		"RUSTUP_VERSION=\"1.29.0\"":                                 1,
		"RUST_VERSION=\"1.97.0\"":                                   1,
		"DOTNET_VERSION=\"8.0.423\"":                                1,
		"MINGIT_VERSION=\"2.55.0\"":                                 1,
		"MINGIT_RELEASE=\"3\"":                                      1,
		"LLVM_MINGW_VERSION=\"20260616\"":                           1,
		"$architecture = $env:TARGETARCH.ToLowerInvariant()":        1,
		"$env:PROCESSOR_ARCHITECTURE.ToUpperInvariant()":            1,
		"https://go.dev/dl/go{0}.windows-{1}.zip":                   1,
		"https://ziglang.org/download/{0}/":                         1,
		"https://static.rust-lang.org/rustup/archive/":              1,
		"https://builds.dotnet.microsoft.com/dotnet/Sdk/":           1,
		"https://github.com/git-for-windows/git/releases/download/": 1,
		"https://github.com/mstorsjo/llvm-mingw/releases/download/": 1,
		"Invoke-WebRequest -UseBasicParsing":                        1,
		"$rustHost = if ($env:TARGETARCH.ToLowerInvariant() -ceq 'arm64') { 'aarch64-pc-windows-gnullvm' } else { 'x86_64-pc-windows-gnullvm' }": 1,
		"--default-host $rustHost":                              1,
		"$rustToolchain = $env:RUST_VERSION + '-' + $rustHost":  1,
		"x86_64-pc-windows-gnu aarch64-pc-windows-gnullvm":      1,
		"$expectedRustHost = if ($expectedGoArch -ceq 'arm64')": 1,
		"host: ' + $expectedRustHost":                           1,
		"aarch64-w64-mingw32-clang.exe":                         1,
		"x86_64-w64-mingw32-clang.exe":                          1,
		"C:\\Windows\\System32\\msi.dll":                        1,
		"dotnet restore C:\\toolchain-restore\\Codesk.wixproj":  1,
		"wixtoolset.sdk\\4.0.5":                                 1,
		"ENTRYPOINT [\"powershell.exe\"":                        1,
	} {
		if got := strings.Count(dockerfile, source); got != want {
			t.Errorf("Windows toolchain Dockerfile source count for %q = %d, want %d", source, got, want)
		}
	}
	for _, forbidden := range []string{"--isolation=hyperv", "nanoserver:", "windows/amd64", "--default-host x86_64-pc-windows-gnu"} {
		if strings.Contains(builder, forbidden) || strings.Contains(wrapper, forbidden) || strings.Contains(dockerfile, forbidden) {
			t.Errorf("Windows container build contains forbidden %q", forbidden)
		}
	}
	for _, forbidden := range []string{"[string] $Version", "'-Version'", "GUI_VERSION"} {
		if strings.Contains(wrapper, forbidden) {
			t.Errorf("Windows container runner retains removed version input %q", forbidden)
		}
	}
	if strings.Contains(wrapper, "'build', '--isolation=process'") {
		t.Error("Windows product container runner must not build the builder image")
	}
	for _, forbidden := range []string{"go.dev", "ziglang.org", "rust-lang.org", "dotnet.microsoft.com", "git-for-windows", "llvm-mingw", "SHA256", "SHA512"} {
		if strings.Contains(builder, forbidden) {
			t.Errorf("lightweight Windows builder script contains Dockerfile dependency detail %q", forbidden)
		}
	}
	for _, forbidden := range []string{"Get-FileHash", "SHA256", "SHA512", "@sha256:"} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("Windows toolchain Dockerfile contains unwanted checksum pin %q", forbidden)
		}
	}
}

func TestWindowsDesktopPayloadSourceContract(t *testing.T) {
	data, err := os.ReadFile("build-windows-desktop-payloads.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if err := checkWindowsDesktopPayloadSource(source); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name string
		old  string
		new  string
	}{
		{name: "Windows drive path treated as relative", old: `/*|[A-Za-z]:/*)`, new: `/*)`},
		{name: "final symlink accepted", old: `[ ! -L "$path" ]`, new: `[ -L "$path" ]`},
		{name: "test root may equal safe parent", old: `case "$test_dir/" in`, new: `case "$test_dir" in`},
		{name: "equal outputs accepted", old: `[ "$payload_dir" != "$test_dir" ]`, new: `[ "$payload_dir" = "$test_dir" ]`},
		{name: "payload may contain test output", old: `case "$payload_dir" in`, new: `case "$safe_parent" in`},
		{name: "test output may contain payload", old: `case "$test_dir" in`, new: `case "$safe_parent" in`},
		{name: "stale tests retained", old: `rm -rf "$payload_dir" "$test_dir"`, new: `rm -rf "$payload_dir"`},
		{name: "Zig pin floated", old: `required_zig_version="${WINDOWS_GUI_ZIG_VERSION:-0.16.0}"`, new: `required_zig_version="$(zig version)"`},
		{name: "shared DAEMON_VERSION reader bypassed", old: `build_version="$("$root_dir/scripts/read-daemon-version.sh")"`, new: `build_version="$(cat "$root_dir/DAEMON_VERSION")"`},
		{name: "GUI subsystem dropped", old: `-ldflags="-H=windowsgui -extldflags=-Wl,--subsystem,windows -X notty/daemon/internal/buildinfo.Version=$build_version"`, new: `-ldflags="-s -w"`},
		{name: "agent payload omitted", old: `-o "$arch_payload_dir/notty-agent-tool.exe" ./daemon/cmd/agenttool`, new: `-o "$arch_payload_dir/notty-agent-tool.exe" ./daemon/cmd/codesk-desktop`},
		{name: "AMD64 native tests lose race instrumentation", old: `test_race_flag=-race`, new: `test_race_flag=`},
		{name: "native tests lose static external linking", old: `-ldflags="-linkmode external -extldflags '-static'"`, new: `-ldflags="-linkmode external"`},
		{name: "cross build not marked as touching host yffi", old: `yffi_touched=1`, new: `yffi_touched=0`},
		{name: "exit trap omitted", old: `trap cleanup EXIT`, new: `trap - EXIT`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.old) != 1 {
				t.Fatalf("payload mutation source %q is not unique", mutation.old)
			}
			mutated := strings.Replace(source, mutation.old, mutation.new, 1)
			if err := checkWindowsDesktopPayloadSource(mutated); err == nil {
				t.Fatal("payload contract mutation passed")
			}
		})
	}
}

func TestWindowsDesktopPayloadRestoresHostYffi(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	data, err := os.ReadFile("build-windows-desktop-payloads.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)

	for _, test := range []struct {
		name        string
		preexisting bool
		failGo      bool
	}{
		{name: "success restores exact preexisting host archive", preexisting: true},
		{name: "failure restores exact preexisting host archive", preexisting: true, failGo: true},
		{name: "success preserves an absent host archive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := runWindowsPayloadYffiFixture(source, test.preexisting, test.failGo); err != nil {
				t.Fatal(err)
			}
		})
	}

	mutations := []struct {
		name        string
		old         string
		new         string
		preexisting bool
		failGo      bool
	}{
		{
			name:        "pre-build snapshot omitted",
			old:         "cd \"$root_dir\"\nsnapshot_host_yffi\n",
			new:         "cd \"$root_dir\"\n:\n",
			preexisting: true,
		},
		{
			name:        "cross build not marked as touching host archive",
			old:         "\tyffi_touched=1\n",
			new:         "\tyffi_touched=0\n",
			preexisting: true,
		},
		{
			name:        "failure cleanup restoration omitted",
			old:         "\tif [ \"$yffi_touched\" -ne 0 ]; then\n",
			new:         "\tif false; then\n",
			preexisting: true,
			failGo:      true,
		},
		{
			name:        "success restoration omitted",
			old:         "\nrestore_host_yffi\nprintf 'Built Windows GUI payloads",
			new:         "\nyffi_touched=0\nprintf 'Built Windows GUI payloads",
			preexisting: true,
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.old) != 1 {
				t.Fatalf("restoration mutation source count for %q = %d, want 1", mutation.old, strings.Count(source, mutation.old))
			}
			mutated := strings.Replace(source, mutation.old, mutation.new, 1)
			if err := runWindowsPayloadYffiFixture(mutated, mutation.preexisting, mutation.failGo); err == nil {
				t.Fatal("host yffi restoration mutation passed")
			}
		})
	}
}

func runWindowsGUIDispatch(makeSource, target string) error {
	if target != "windows-gui-build" && target != "windows-gui-deploy" {
		return fmt.Errorf("unsupported Windows GUI dispatch target %q", target)
	}
	tempDir, err := os.MkdirTemp("", "notty-windows-gui-make-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	binDir := filepath.Join(tempDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	unameCalled := filepath.Join(tempDir, "uname-called")
	if err := writeExecutable(filepath.Join(binDir, "uname"), `#!/bin/sh
: "${WINDOWS_GUI_UNAME_CALLED:?}"
: >"$WINDOWS_GUI_UNAME_CALLED"
exit 97
`); err != nil {
		return err
	}
	powershellCommand := "powershell.exe"
	commandDirectory := ""
	makeOverrides := []string{
		"OS=Windows_NT", "PROCESSOR_ARCHITECTURE=ARM64", "WINDOWS_GUI_POWERSHELL=" + powershellCommand,
	}
	if runtime.GOOS == "windows" {
		resolvedPowerShell, err := exec.LookPath("powershell.exe")
		if err != nil {
			return fmt.Errorf("resolve native PowerShell fixture: %w", err)
		}
		powershellCommand = resolvedPowerShell
		fakeShim := `param()
$capture = [Environment]::GetEnvironmentVariable('WINDOWS_GUI_CAPTURE', 'Process')
[System.IO.File]::WriteAllLines($capture, [Environment]::GetCommandLineArgs())
`
		if err := os.WriteFile(filepath.Join(tempDir, "make.ps1"), []byte(fakeShim), 0o600); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(tempDir, "scripts"), 0o755); err != nil {
			return err
		}
		reader, err := os.ReadFile("read-daemon-version.ps1")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tempDir, "scripts", "read-daemon-version.ps1"), reader, 0o600); err != nil {
			return err
		}
		commandDirectory = tempDir
		makeOverrides = []string{
			"OS=Windows_NT", "PROCESSOR_ARCHITECTURE=ARM64", "WINDOWS_GUI_POWERSHELL=" + powershellCommand,
			"SHELL=" + os.Getenv("COMSPEC"),
		}
	} else {
		if err := writeExecutable(filepath.Join(binDir, "powershell.exe"), `#!/bin/sh
: "${WINDOWS_GUI_CAPTURE:?}"
case "$*" in
  *read-daemon-version.ps1*) printf '%s\n' '0.0.1'; exit 0 ;;
esac
: >"$WINDOWS_GUI_CAPTURE"
for argument in "$@"; do
  printf '%s\n' "$argument" >>"$WINDOWS_GUI_CAPTURE"
done
`); err != nil {
			return err
		}
	}
	makePath := filepath.Join(tempDir, "Makefile")
	if err := os.WriteFile(makePath, []byte(makeSource), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tempDir, "DAEMON_VERSION"), []byte("0.0.1\n"), 0o644); err != nil {
		return err
	}
	capturePath := filepath.Join(tempDir, "powershell.args")
	if commandDirectory == "" {
		commandDirectory, err = filepath.Abs("..")
		if err != nil {
			return err
		}
	}
	makeArguments := append([]string{"-s", "-f", makePath, target}, makeOverrides...)
	command := exec.Command("make", makeArguments...)
	command.Dir = commandDirectory
	command.Env = environmentWith(map[string]string{
		"PATH":                     binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"WINDOWS_GUI_CAPTURE":      capturePath,
		"WINDOWS_GUI_UNAME_CALLED": unameCalled,
	})
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("make %s: %w: %s", target, err, output)
	}
	data, err := os.ReadFile(capturePath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(unameCalled); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Windows Make route invoked uname: %v", err)
	}
	arguments := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), "\n")
	file, ok := argumentValue(arguments, "-File")
	if !ok || file != "make.ps1" {
		return fmt.Errorf("PowerShell route = %q, present %t", file, ok)
	}
	fileIndex := -1
	for index, argument := range arguments {
		if argument == "-File" {
			fileIndex = index
			break
		}
	}
	if fileIndex < 0 || fileIndex+2 >= len(arguments) || arguments[fileIndex+2] != target {
		return fmt.Errorf("PowerShell target %s missing from route: %v", target, arguments)
	}
	architectureAssignments := 0
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "WINDOWS_GUI_ARCH") {
			architectureAssignments++
			return fmt.Errorf("PowerShell public route contains spoofable architecture %q", argument)
		}
	}
	if architectureAssignments != 0 {
		return fmt.Errorf("PowerShell public route architecture assignment count = %d, want 0", architectureAssignments)
	}
	return nil
}

func runWindowsPayloadYffiFixture(source string, preexisting, failGo bool) error {
	root, err := os.MkdirTemp("", "notty-windows-yffi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	targetYffiOriginalPath := filepath.Join(root, "third_party", "y-crdt", "target", "x86_64-pc-windows-gnu", "release", "libyrs.a")
	targetYffiAbsentPath := filepath.Join(root, "third_party", "y-crdt", "target", "aarch64-pc-windows-gnullvm", "release", "libyrs.a")
	targetYffiOriginal := []byte("target-original\n")
	if err := os.MkdirAll(filepath.Dir(targetYffiOriginalPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(targetYffiOriginalPath, targetYffiOriginal, 0o640); err != nil {
		return err
	}
	targetFixturesRestored := false
	restoreTargetFixtures := func() error {
		restorePath := targetYffiOriginalPath + ".restore"
		if err := os.MkdirAll(filepath.Dir(restorePath), 0o755); err != nil {
			return err
		}
		if err := os.Remove(restorePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(restorePath, targetYffiOriginal, 0o640); err != nil {
			return err
		}
		if err := os.Rename(restorePath, targetYffiOriginalPath); err != nil {
			return err
		}
		if err := os.Remove(targetYffiAbsentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	defer func() {
		if !targetFixturesRestored {
			_ = restoreTargetFixtures()
		}
	}()

	scriptsDir := filepath.Join(root, "scripts")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "DAEMON_VERSION"), []byte("0.0.1\n"), 0o644); err != nil {
		return err
	}
	payloadScript := filepath.Join(scriptsDir, "build-windows-desktop-payloads.sh")
	if err := writeExecutable(payloadScript, source); err != nil {
		return err
	}
	reader, err := os.ReadFile("read-daemon-version.sh")
	if err != nil {
		return err
	}
	if err := writeExecutable(filepath.Join(scriptsDir, "read-daemon-version.sh"), string(reader)); err != nil {
		return err
	}
	if err := writeExecutable(filepath.Join(scriptsDir, "build-yffi.sh"), `#!/bin/sh
set -eu
root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
target="$root/third_party/y-crdt/target/$RUST_TARGET/release/libyrs.a"
link="$root/third_party/y-crdt/target/release/libyrs.a"
mkdir -p "$(dirname -- "$target")" "$(dirname -- "$link")"
printf 'target:%s\n' "$RUST_TARGET" >"$target.tmp"
mv -f "$target.tmp" "$target"
cp "$target" "$link.tmp"
mv -f "$link.tmp" "$link"
printf '%s\n' "$RUST_TARGET" >>"$QA_YFFI_LOG"
`); err != nil {
		return err
	}
	if err := writeExecutable(filepath.Join(binDir, "zig"), `#!/bin/sh
if [ "${1-}" = version ]; then
  printf '%s\n' '0.16.0'
fi
`); err != nil {
		return err
	}
	for _, name := range []string{"cargo", "rustc"} {
		if err := writeExecutable(filepath.Join(binDir, name), "#!/bin/sh\nexit 0\n"); err != nil {
			return err
		}
	}
	if err := writeExecutable(filepath.Join(binDir, "go"), `#!/bin/sh
set -eu
if [ "${QA_FAIL_GO:-0}" = 1 ]; then
  exit 73
fi
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    shift
    output="$1"
  fi
  shift
done
if [ -n "$output" ]; then
  mkdir -p "$(dirname -- "$output")"
  printf '%s\n' 'fixture executable' >"$output"
fi
`); err != nil {
		return err
	}

	hostYffi := filepath.Join(root, "third_party", "y-crdt", "target", "release", "libyrs.a")
	original := []byte("host-original\n")
	if preexisting {
		if err := os.MkdirAll(filepath.Dir(hostYffi), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(hostYffi, original, 0o640); err != nil {
			return err
		}
	}
	logPath := filepath.Join(root, "yffi-targets.log")
	command := exec.Command("sh", payloadScript)
	command.Dir = root
	failValue := "0"
	if failGo {
		failValue = "1"
	}
	command.Env = environmentWith(map[string]string{
		"PATH":                    binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"QA_FAIL_GO":              failValue,
		"QA_YFFI_LOG":             logPath,
		"WINDOWS_GUI_ARCHES":      "amd64 arm64",
		"WINDOWS_GUI_ZIG_VERSION": "0.16.0",
	})
	output, runErr := command.CombinedOutput()
	if failGo && runErr == nil {
		return fmt.Errorf("injected post-yffi Go failure unexpectedly succeeded: %s", output)
	}
	if !failGo && runErr != nil {
		return fmt.Errorf("payload fixture failed: %w: %s", runErr, output)
	}

	if preexisting {
		actual, err := os.ReadFile(hostYffi)
		if err != nil {
			return fmt.Errorf("read restored host yffi: %w", err)
		}
		if string(actual) != string(original) {
			return fmt.Errorf("restored host yffi = %q, want %q", actual, original)
		}
		info, err := os.Stat(hostYffi)
		if err != nil {
			return fmt.Errorf("stat restored host yffi: %w", err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			return fmt.Errorf("restored host yffi mode = %o, want 640", got)
		}
	} else if _, err := os.Lstat(hostYffi); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("initially absent host yffi remains after build: %v", err)
	}

	if !failGo {
		logData, err := os.ReadFile(logPath)
		if err != nil {
			return fmt.Errorf("read yffi target log: %w", err)
		}
		got := strings.Fields(string(logData))
		want := []string{"x86_64-pc-windows-gnu", "aarch64-pc-windows-gnullvm"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			return fmt.Errorf("cross yffi targets = %v, want %v", got, want)
		}
	}

	for path, want := range map[string]string{
		targetYffiOriginalPath: "target:x86_64-pc-windows-gnu\n",
		targetYffiAbsentPath:   "target:aarch64-pc-windows-gnullvm\n",
	} {
		actual, err := os.ReadFile(path)
		if failGo && path == targetYffiAbsentPath && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read mutated target yffi %s: %w", path, err)
		}
		if string(actual) != want {
			return fmt.Errorf("mutated target yffi %s = %q, want %q", path, actual, want)
		}
	}
	if err := restoreTargetFixtures(); err != nil {
		return fmt.Errorf("restore target yffi fixtures: %w", err)
	}
	targetFixturesRestored = true
	restoredTarget, err := os.ReadFile(targetYffiOriginalPath)
	if err != nil {
		return fmt.Errorf("read restored target yffi: %w", err)
	}
	if string(restoredTarget) != string(targetYffiOriginal) {
		return fmt.Errorf("restored target yffi = %q, want %q", restoredTarget, targetYffiOriginal)
	}
	restoredInfo, err := os.Stat(targetYffiOriginalPath)
	if err != nil {
		return fmt.Errorf("stat restored target yffi: %w", err)
	}
	if got := restoredInfo.Mode().Perm(); got != 0o640 {
		return fmt.Errorf("restored target yffi mode = %o, want 640", got)
	}
	if _, err := os.Lstat(targetYffiAbsentPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("initially absent target yffi remains after fixture: %v", err)
	}
	return nil
}

func writeExecutable(path, source string) error {
	if err := os.WriteFile(path, []byte(source), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func environmentWith(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		separator := strings.IndexByte(entry, '=')
		if separator >= 0 {
			if _, replaced := overrides[entry[:separator]]; replaced {
				continue
			}
		}
		environment = append(environment, entry)
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func normalizeSourceNewlines(source string) string {
	return strings.ReplaceAll(source, "\r\n", "\n")
}

func argumentValue(arguments []string, name string) (string, bool) {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1], true
		}
	}
	return "", false
}

func checkWindowsDesktopPayloadSource(source string) error {
	for required, want := range map[string]int{
		"set -eu":         1,
		"set -f":          1,
		`/*|[A-Za-z]:/*)`: 1,
		`required_zig_version="${WINDOWS_GUI_ZIG_VERSION:-0.16.0}"`:     1,
		`[ "$actual_zig_version" = "$required_zig_version" ]`:           1,
		`build_version="$("$root_dir/scripts/read-daemon-version.sh")"`: 1,
		`[ ! -L "$path" ]`:                              1,
		`case "$payload_dir/" in`:                       1,
		`case "$test_dir/" in`:                          1,
		`[ "$payload_dir" != "$test_dir" ]`:             1,
		`case "$payload_dir" in`:                        1,
		`case "$test_dir" in`:                           1,
		`payload and test directories must not overlap`: 2,
		`rm -rf "$payload_dir" "$test_dir"`:             1,
		`host_yffi_link="$root_dir/third_party/y-crdt/target/release/libyrs.a"`: 1,
		`snapshot_host_yffi()`:                        1,
		`restore_host_yffi()`:                         1,
		`trap cleanup EXIT`:                           1,
		`yffi_touched=1`:                              1,
		`cp -p "$host_yffi_link" "$host_yffi_backup"`: 1,
		`cp -p "$host_yffi_backup" "$restore_tmp"`:    1,
		`RUST_TARGET="$rust_target" RUSTFLAGS='-C panic=abort' scripts/build-yffi.sh`: 1,
		`go vet ./daemon/internal/syncer ./internal/ycrdt`:                            1,
		`test_race_flag=-race`:                                1,
		"test_race_flag=\n":                                   1,
		`go test -c $test_race_flag`:                          1,
		`-ldflags="-linkmode external -extldflags '-static'"`: 1,
		`-o "$test_dir/notty-syncer-$architecture.test.exe" ./daemon/internal/syncer`:                                              1,
		`go vet ./daemon/internal/desktopstate ./daemon/internal/desktop ./daemon/internal/desktopapp ./daemon/cmd/codesk-desktop`: 1,
		`go test -c -o "$test_dir/codesk-desktop-$architecture.test.exe" ./daemon/cmd/codesk-desktop`:                              1,
		`-ldflags="-H=windowsgui -extldflags=-Wl,--subsystem,windows -X notty/daemon/internal/buildinfo.Version=$build_version"`:   1,
		`-o "$arch_payload_dir/Codesk.exe" ./daemon/cmd/codesk-desktop`:                                                            1,
		`-o "$arch_payload_dir/notty-agent-tool.exe" ./daemon/cmd/agenttool`:                                                       1,
		`verify-windows-desktop-pe.go "$arch_payload_dir/Codesk.exe" "$architecture" gui`:                                          1,
		`verify-windows-desktop-pe.go "$arch_payload_dir/notty-agent-tool.exe" "$architecture" console`:                            1,
	} {
		if got := strings.Count(source, required); got != want {
			return fmt.Errorf("Windows payload source count for %q = %d, want %d", required, got, want)
		}
	}
	if strings.Count(source, "snapshot_host_yffi") != 2 || strings.Count(source, "restore_host_yffi") != 3 {
		return fmt.Errorf("Windows payload host yffi transaction is not wired through success and cleanup")
	}
	for _, forbidden := range []string{"-H=console", "rm -rf \"$safe_parent\""} {
		if strings.Contains(source, forbidden) {
			return fmt.Errorf("Windows payload source contains forbidden %q", forbidden)
		}
	}
	return nil
}

func checkPowerShellDaemonVersionReaderSource(source string) error {
	for required, want := range map[string]int{
		"param()": 1,
		"$versionFile = Join-Path (Split-Path -Parent $PSScriptRoot) 'DAEMON_VERSION'": 1,
		"DAEMON_VERSION must be a regular file":                                        1,
		"[System.IO.File]::ReadAllBytes":                                               1,
		"$bytes[$bytes.Length - 1] -ne 10":                                             1,
		"$lineLength = $bytes.Length - 1":                                              1,
		"$bytes[$lineLength - 1] -eq 13":                                               1,
		"for ($index = 0; $index -lt $lineLength; $index++)":                           1,
		"$bytes[$index] -eq 10 -or $bytes[$index] -eq 13":                              1,
		"[System.Text.Encoding]::ASCII.GetString($bytes, 0, $lineLength)":              1,
		"^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$":                        1,
		"[uint32]::TryParse($fields[0]":                                                1,
		"[uint32]::TryParse($fields[1]":                                                1,
		"[uint32]::TryParse($fields[2]":                                                1,
		"$major -gt 255":                                                               1,
		"$minor -gt 255":                                                               1,
		"$build -gt 65535":                                                             1,
	} {
		if got := strings.Count(source, required); got != want {
			return fmt.Errorf("PowerShell DAEMON_VERSION reader source count for %q = %d, want %d", required, got, want)
		}
	}
	for _, forbidden := range []string{"Get-Content", "ReadAllText", "TotalCount", ".Trim()", "$env:", "git "} {
		if strings.Contains(source, forbidden) {
			return fmt.Errorf("PowerShell DAEMON_VERSION reader contains forbidden fallback %q", forbidden)
		}
	}
	return nil
}

func checkWindowsGUIUploadPreflightSource(upload, provenance string) error {
	for _, required := range []string{
		`printf '%s\n' amd64 arm64 >"$tmp_dir/windows-architectures.expected"`,
		`windows_gui_input_root="$windows_gui_msi_root"`,
		`windows_gui_staged_root="$tmp_dir/msi"`,
		`assert_exact_top_level_entries "$windows_gui_input_root" "$tmp_dir/windows-architectures.expected" windows-source-architectures`,
		`cp "$stage_source_file" "$stage_arch_dir/$stage_name"`,
		`windows_gui_msi_root="$windows_gui_staged_root"`,
		`printf '%s\n' "$preflight_msi_name" SHA256SUMS provenance.json`,
		`if (bad || NR != 2) exit 1`,
		`cmp -s "$tmp_dir/$preflight_arch.expected-checksum-files" "$tmp_dir/$preflight_arch.actual-checksum-files"`,
		`preflight_actual_hash="$(sha256_file "$preflight_arch_dir/$preflight_name")"`,
		`[ "$preflight_expected_hash" = "$preflight_actual_hash" ]`,
		`git -C "$root_dir" rev-parse --verify HEAD`,
		`git -C "$root_dir" rev-parse --verify 'HEAD^1'`,
		`windows_gui_ps_script="$root_dir/scripts/verify-windows-gui-upload-provenance.ps1"`,
		`MSYS2_ARG_CONV_EXCL='*' "$windows_gui_ps"`,
		`-Repository "${WINDOWS_GUI_REPOSITORY:-XIAZY/notty}"`,
		`arch_dir="$windows_gui_staged_root/$arch"`,
		`command -v rclone >/dev/null 2>&1 || die 'rclone is required for R2 uploads'`,
		`need AWS_ACCESS_KEY_ID`,
		`need AWS_SECRET_ACCESS_KEY`,
		`RCLONE_CONFIG_NOTTYR2_PROVIDER=Cloudflare`,
		`RCLONE_CONFIG_NOTTYR2_ENV_AUTH=true`,
		`rclone lsjson`,
		`rclone copyto`,
	} {
		if !strings.Contains(upload, required) {
			return fmt.Errorf("Windows GUI upload preflight is missing %q", required)
		}
	}

	windowsBlockIndex := strings.Index(upload, `if [ "$target" = windows-gui ]; then`)
	if windowsBlockIndex < 0 {
		return fmt.Errorf("shared uploader has no Windows GUI block")
	}
	windowsBlock := upload[windowsBlockIndex:]
	stageIndex := strings.Index(windowsBlock, `stage_windows_gui_arch "$arch"`)
	preflightIndex := strings.Index(windowsBlock, `preflight_windows_gui_arch "$arch"`)
	provenanceIndex := strings.Index(windowsBlock, `"$windows_gui_ps" -NoLogo`)
	manifestIndex := strings.Index(windowsBlock, `manifest="$tmp_dir/manifest.json"`)
	uploadIndex := strings.Index(windowsBlock, `upload_file "$R2_DESKTOP_BUCKET"`)
	if stageIndex < 0 || preflightIndex < 0 || provenanceIndex < 0 || manifestIndex < 0 || uploadIndex < 0 ||
		!(stageIndex < preflightIndex && preflightIndex < provenanceIndex && provenanceIndex < manifestIndex && manifestIndex < uploadIndex) {
		return fmt.Errorf("Windows GUI staged two-phase ordering is stage=%d preflight=%d provenance=%d manifest=%d upload=%d", stageIndex, preflightIndex, provenanceIndex, manifestIndex, uploadIndex)
	}

	for _, required := range []string{
		`ConvertFrom-Json`,
		`Write-Output -NoEnumerate $property.Value`,
		`$schemaVersion -isnot [int] -and $schemaVersion -isnot [long]`,
		`source.checkoutCommit`,
		`source.sourceHead`,
		`source.sourceBase`,
		`source.workflowRef`,
		`target.goArchitecture`,
		`target.installerPlatform`,
		`target.buildMode`,
		`$publishable -isnot [bool] -or -not $publishable`,
		`$packageValue -isnot [System.Array]`,
		`$packages.Count -ne 1`,
		`package.version`,
		`package.canonicalFile`,
		`Get-FileHash -LiteralPath $msiPath -Algorithm SHA256`,
		`package.canonicalSha256`,
		`package.canonicalSize`,
		`$canonicalSize -isnot [int] -and $canonicalSize -isnot [long]`,
		`productCodeDerivation.algorithm`,
		`productCodeDerivation.name`,
	} {
		if !strings.Contains(provenance, required) {
			return fmt.Errorf("Windows GUI structured provenance preflight is missing %q", required)
		}
	}
	return nil
}

func checkWindowsMSIReleaseSource(build, orchestrator, makefile, shim string) error {
	buildRequired := map[string]int{
		"[CmdletBinding(DefaultParameterSetName = 'Release')]":                   1,
		"[Parameter(ParameterSetName = 'Release')]":                              1,
		"[Parameter(Mandatory = $true, ParameterSetName = 'TestOnlyUpgradeQa')]": 1,
		"[switch] $Release":           1,
		"[switch] $TestOnlyUpgradeQa": 1,
		"$ProductCodeNamespace = [guid] '55A27873-BF9C-5DC3-AA8B-9D6F996041EF'":                                     1,
		"$ProductVersion = & (Join-Path $scriptRoot 'read-daemon-version.ps1')":                                     1,
		"$productCodeName = \"$ProductVersion+$GoArchitecture\"":                                                    1,
		"Get-UuidV5 -Namespace $ProductCodeNamespace -Name $productCodeName":                                        1,
		"[System.Text.Encoding]::UTF8.GetBytes($Name)":                                                              1,
		"[System.Security.Cryptography.SHA1]::Create()":                                                             1,
		"$uuidBytes[6] = ($uuidBytes[6] -band 0x0f) -bor 0x50":                                                      1,
		"$uuidBytes[8] = ($uuidBytes[8] -band 0x3f) -bor 0x80":                                                      1,
		"$buildMode = if ($PSCmdlet.ParameterSetName -ceq 'Release') { 'release' } else { 'test-only-upgrade-qa' }": 1,
		"name = 'release'":                           1,
		"testdata\\windows-msi-upgrade-versions.ps1": 1,
		"test-only upgrade fixture must define exactly previous then candidate": 1,
		"buildMode = $buildMode":                    1,
		"publishable = ($buildMode -ceq 'release')": 1,
		"productCodeDerivation":                     1,
		"algorithm = 'UUIDv5-SHA1'":                 1,
		`$canonicalName = "Codesk_$($version.version)_windows_$GoArchitecture.msi"`: 1,
		`Codesk_$($_.version)_windows_$GoArchitecture.msi`:                          2,
	}
	for source, want := range buildRequired {
		if got := strings.Count(build, source); got != want {
			return fmt.Errorf("Windows MSI release source count for %q = %d, want %d", source, got, want)
		}
	}
	for _, forbidden := range []string{"version = '0.0.1'", "version = '0.0.2'", "QaPair", "PreviousProductCode", "CandidateProductCode"} {
		if strings.Contains(build, forbidden) {
			return fmt.Errorf("Windows MSI production builder contains test-only identity %q", forbidden)
		}
	}
	for source, want := range map[string]int{
		"[ValidateSet('windows-gui-build', 'windows-gui-deploy')]":                       1,
		"function Get-WindowsHostArchitecture":                                           1,
		"[System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()": 1,
		"'ARM64' { return 'arm64' }":                                                     1,
		"$hostArchitecture = Get-WindowsHostArchitecture":                                1,
		"windows-gui-build requires exactly one host architecture":                       1,
		"does not match host $hostArchitecture":                                          1,
		"windows-gui-deploy requires exactly amd64 then arm64":                           1,
		"$result.Count -ne 2 -or $result[0] -cne 'amd64' -or $result[1] -cne 'arm64'":    1,
		"[System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT":      1,
		"scripts/build-windows-desktop-payloads.sh":                                      1,
		"scripts/build-windows-desktop-msi-artifact.ps1":                                 1,
		"local/scripts/run-windows-gui-target.ps1@$head":                                 1,
		"$version = & (Join-Path $root 'scripts/read-daemon-version.ps1')":               1,
		"Release = $true":    1,
		"$item.Length -le 0": 1,
		"Assert-ExactArchitectureDirectories -Directory $PayloadDirectory -Architectures $SelectedArchitectures": 1,
		"Assert-ExactRealFiles @releaseParameters":                                                               1,
		"Assert-ExactArchitectureDirectories -Directory $MsiDirectory -Architectures $SelectedArchitectures":     1,
		"Remove-Item -LiteralPath $workingDirectory -Recurse -Force":                                             1,
		`Names = @("Codesk_${version}_windows_$architecture.msi", 'SHA256SUMS', 'provenance.json')`:              1,
		"[Array]::Sort($actual, [System.StringComparer]::Ordinal)":                                               2,
		"[Array]::Sort($expected, [System.StringComparer]::Ordinal)":                                             2,
	} {
		if got := strings.Count(orchestrator, source); got != want {
			return fmt.Errorf("Windows GUI orchestrator source count for %q = %d, want %d", source, got, want)
		}
	}
	for _, forbidden := range []string{"??", "Sort-Object", "status --porcelain", "requires a clean checkout", "[string] $Version", "Assert-CanonicalMsiVersion", "Get-Content"} {
		if strings.Contains(orchestrator, forbidden) {
			return fmt.Errorf("Windows GUI orchestrator contains unsupported or non-ordinal %q", forbidden)
		}
	}
	if regexp.MustCompile(`(?m)\\\s*$`).MatchString(orchestrator) {
		return fmt.Errorf("Windows GUI orchestrator contains a shell-style line continuation")
	}
	for source, want := range map[string]int{
		"ifeq ($(OS),Windows_NT)":                                                      2,
		"override REPOSITORY_DAEMON_VERSION = $(shell":                                 2,
		"-File scripts/read-daemon-version.ps1)":                                       1,
		"override REPOSITORY_DAEMON_VERSION = $(shell scripts/read-daemon-version.sh)": 1,
		"WINDOWS_PROCESSOR_ARCH :=":                                                    1,
		"override MACOS_GUI_HOST_OS :=":                                                2,
		`if [ "$(MACOS_GUI_HOST_OS)" != darwin ]; then`:                                2,
		"-File make.ps1":                                                               4,
		"WINDOWS_GUI_BUILDER_IMAGE ?= ghcr.io/xiazy/notty-windows-builder:latest":      1,
		`"WINDOWS_GUI_BUILDER_IMAGE=$(WINDOWS_GUI_BUILDER_IMAGE)"`:                     2,
		"-File make.ps1 windows-gui-build":                                             1,
		"-File make.ps1 windows-gui-deploy":                                            1,
	} {
		if got := strings.Count(makefile, source); got != want {
			return fmt.Errorf("Windows GUI Make source count for %q = %d, want %d", source, got, want)
		}
	}
	publicTargets := []string{"macos-gui-build", "macos-gui-deploy", "windows-gui-build", "windows-gui-deploy"}
	for _, target := range publicTargets {
		pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:\s*$`)
		if got := len(pattern.FindAllString(makefile, -1)); got != 2 {
			return fmt.Errorf("Make public GUI target %q conditional definition count = %d, want 2", target, got)
		}
	}
	if err := checkWindowsMakeBranch(makefile); err != nil {
		return err
	}
	if err := checkPowerShellShimTargetTable(shim, publicTargets); err != nil {
		return err
	}
	for source, want := range map[string]int{
		"scripts/run-windows-gui-container.ps1":                            1,
		"scripts/upload-r2.sh":                                             1,
		"'WINDOWS_GUI_BUILDER_IMAGE'":                                      2,
		"'WINDOWS_GUI_REPOSITORY'":                                         6,
		"macos-gui-build requires a real macOS host; no GUI was built":     1,
		"macos-gui-deploy requires a real macOS host; no GUI was deployed": 1,
		"scripts/read-daemon-version.ps1":                                  1,
		"Invoke-WindowsGuiUpload":                                          2,
		"$env:UPLOAD_TARGET = 'windows-gui'":                               1,
	} {
		if got := strings.Count(shim, source); got != want {
			return fmt.Errorf("Windows GUI PowerShell shim source count for %q = %d, want %d", source, got, want)
		}
	}
	for _, forbidden := range []string{
		"WINDOWS_GUI_PREVIOUS_PRODUCT_CODE",
		"WINDOWS_GUI_CANDIDATE_PRODUCT_CODE",
		"windows-gui-deploy-arch",
		"scripts/build-windows-desktop-msi-artifact.ps1",
		"WINDOWS_GUI_ARCH ?=",
		"GUI_VERSION",
		"FILE_VERSION",
		"WINDOWS_GUI_ARCHES",
	} {
		if strings.Contains(makefile, forbidden) {
			return fmt.Errorf("Windows release Make path still pins QA identity %q", forbidden)
		}
	}
	windowsBuildRecipe := regexp.MustCompile(`(?m)^windows-gui-build:\s*\n\t@.*-File make\.ps1 windows-gui-build[^\n]*$`).FindString(makefile)
	if windowsBuildRecipe == "" {
		return fmt.Errorf("Windows-native Make build route is missing")
	}
	if strings.Contains(windowsBuildRecipe, "WINDOWS_GUI_ARCH") {
		return fmt.Errorf("Windows-native Make build route injects a spoofable architecture")
	}
	for _, forbidden := range []string{
		"build-windows-desktop-payloads.sh",
		"build-windows-desktop-msi-artifact.ps1",
		"go build",
		"cargo",
		"wix",
		"Get-Content",
		"fileVersion",
		"Version =",
	} {
		if strings.Contains(shim, forbidden) {
			return fmt.Errorf("Windows GUI PowerShell shim duplicates build logic %q", forbidden)
		}
	}
	if strings.Count(build, "[Array]::Reverse($namespaceBytes") != 3 ||
		strings.Count(build, "[Array]::Reverse($uuidBytes") != 3 {
		return fmt.Errorf("UUIDv5 conversion does not preserve RFC byte order")
	}
	return nil
}

func checkWindowsMakeBranch(makefile string) error {
	marker := "ifeq ($(OS),Windows_NT)\n"
	first := strings.Index(makefile, marker)
	if first < 0 {
		return fmt.Errorf("Make has no Windows-native variable branch")
	}
	firstElse := strings.Index(makefile[first:], "\nelse\n")
	if firstElse < 0 {
		return fmt.Errorf("Make Windows-native variable branch has no else")
	}
	variableBranch := makefile[first : first+firstElse]
	for _, forbidden := range []string{"uname", "/dev/null", "printf"} {
		if strings.Contains(variableBranch, forbidden) {
			return fmt.Errorf("Make Windows-native variable branch uses POSIX dependency %q", forbidden)
		}
	}

	secondRelative := strings.Index(makefile[first+len(marker):], marker)
	if secondRelative < 0 {
		return fmt.Errorf("Make has no Windows-native GUI recipe branch")
	}
	second := first + len(marker) + secondRelative
	secondElse := strings.Index(makefile[second:], "\nelse\n")
	if secondElse < 0 {
		return fmt.Errorf("Make Windows-native GUI recipe branch has no else")
	}
	recipeBranch := makefile[second : second+secondElse]
	for _, forbidden := range []string{"$(shell", "uname", "if [", "command -v", "\\\n"} {
		if strings.Contains(recipeBranch, forbidden) {
			return fmt.Errorf("Make Windows-native GUI recipe branch uses POSIX dependency %q", forbidden)
		}
	}
	return nil
}

func checkPowerShellShimTargetTable(shim string, want []string) error {
	match := regexp.MustCompile(`(?s)\[ValidateSet\(([^)]*)\)\]`).FindStringSubmatch(shim)
	if len(match) != 2 {
		return fmt.Errorf("Windows GUI PowerShell shim has no single target table")
	}
	quoted := regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(match[1], -1)
	got := make([]string, 0, len(quoted))
	for _, entry := range quoted {
		got = append(got, entry[1])
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("Windows GUI PowerShell shim targets = %v, want %v", got, want)
	}
	return nil
}

func validateMSIProductVersion(version string) error {
	if !canonicalMSIProductVersion.MatchString(version) {
		return fmt.Errorf("not canonical X.Y.Z")
	}
	limits := []uint64{255, 255, 65535}
	for index, field := range strings.Split(version, ".") {
		value, err := strconv.ParseUint(field, 10, 32)
		if err != nil || value > limits[index] {
			return fmt.Errorf("field %d is outside the MSI range", index)
		}
	}
	return nil
}

func uuidV5(namespace, name string) (string, error) {
	rawNamespace := strings.ReplaceAll(namespace, "-", "")
	namespaceBytes, err := hex.DecodeString(rawNamespace)
	if err != nil || len(namespaceBytes) != 16 {
		return "", fmt.Errorf("invalid namespace UUID %q", namespace)
	}
	hash := sha1.New()
	_, _ = hash.Write(namespaceBytes)
	_, _ = hash.Write([]byte(name))
	uuid := hash.Sum(nil)[:16]
	uuid[6] = (uuid[6] & 0x0f) | 0x50
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	encoded := strings.ToUpper(hex.EncodeToString(uuid))
	return fmt.Sprintf("{%s-%s-%s-%s-%s}", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}
