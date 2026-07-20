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

	for _, target := range []string{"windows-gui-build", "windows-gui-release"} {
		if err := runWindowsGUIDispatch(makeSource, target); err != nil {
			t.Fatalf("Windows-native Make %s dispatch failed: %v", target, err)
		}
	}
	old := `"GUI_VERSION=$(GUI_VERSION)" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`
	if strings.Count(makeSource, old) != 1 {
		t.Fatalf("Windows build route source count = %d, want 1", strings.Count(makeSource, old))
	}
	mutated := strings.Replace(makeSource, old, `"GUI_VERSION=$(GUI_VERSION)" "WINDOWS_GUI_ARCHES=amd64" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`, 1)
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

func TestWindowsGUIPowerShellDefaultsArchitectureAndRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell defaults require Windows")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}

	command := exec.Command(
		powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", "scripts/run-windows-gui-target.ps1", "-Target", "windows-gui-release", "-Version", "invalid",
	)
	command.Dir = ".."
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid release version unexpectedly succeeded: %s", output)
	}
	text := string(output)
	if !strings.Contains(text, "GUI_VERSION must be canonical MSI X.Y.Z") {
		t.Fatalf("release default failure = %q, want canonical version rejection", output)
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
			name:     "release mode merged into QA parameter set",
			buildOld: "[Parameter(Mandatory = $true, ParameterSetName = 'Release')]",
			buildNew: "[Parameter(Mandatory = $true, ParameterSetName = 'QaPair')]",
		},
		{
			name:     "leading-zero versions accepted",
			buildOld: "^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$",
			buildNew: "^[0-9]+\\.[0-9]+\\.[0-9]+$",
		},
		{
			name:     "MSI major range widened",
			buildOld: "$major -gt 255",
			buildNew: "$major -gt 256",
		},
		{
			name:     "MSI build range widened",
			buildOld: "$build -gt 65535",
			buildNew: "$build -gt 65536",
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
			buildOld: "Codesk_${ProductVersion}_windows_$GoArchitecture.msi",
			buildNew: "Codesk_release_windows_$GoArchitecture.msi",
		},
		{
			name:            "release invokes QA mode",
			orchestratorOld: "ProductVersion = $Version",
			orchestratorNew: "PreviousProductCode = $Version",
		},
		{
			name:            "release accepts out-of-range build",
			orchestratorOld: "$fields[2] -gt 65535",
			orchestratorNew: "$fields[2] -gt 65536",
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
			name:    "Make bypasses the shared orchestrator",
			makeOld: "-File make.ps1 windows-gui-build",
			makeNew: "-File scripts/build-windows-desktop-msi-artifact.ps1 windows-gui-build",
		},
		{
			name:    "Make injects a spoofable build architecture",
			makeOld: `"GUI_VERSION=$(GUI_VERSION)" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`,
			makeNew: `"GUI_VERSION=$(GUI_VERSION)" "WINDOWS_GUI_ARCHES=amd64" "WINDOWS_GUI_ROOT=$(WINDOWS_GUI_ROOT)"`,
		},
		{
			name:    "PowerShell shim loses a public target",
			shimOld: "'macos-gui-build', 'macos-gui-release', 'build-windows-builder-image', 'windows-gui-build', 'windows-gui-release'",
			shimNew: "'macos-gui-build', 'macos-gui-release', 'windows-gui-build'",
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

	for source, want := range map[string]int{
		"[string] $BuilderImage = 'alphatoad/notty:windows-builder'":                                        1,
		"docker info --format '{{.OSType}}|{{.Architecture}}|{{.OSVersion}}'":                               1,
		"[System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()":                    1,
		"Docker engine architecture $dockerArchitecture does not match host architecture $hostArchitecture": 1,
		"'run', '--rm', '--isolation=process'":                                                              1,
		"docker image inspect $BuilderImage":                                                                1,
		"run make build-windows-builder-image":                                                              1,
		"WINDOWS_GUI_CC_AMD64=C:/toolchains/llvm-mingw/bin/x86_64-w64-mingw32-clang.exe -static":            1,
		"WINDOWS_GUI_CC_ARM64=C:/toolchains/llvm-mingw/bin/aarch64-w64-mingw32-clang.exe -static":           1,
		"third_party/y-crdt/Cargo.lock":                                                                     1,
		"target=C:\\workspace":                                                                              1,
		"scripts\\run-windows-gui-target.ps1":                                                               1,
	} {
		if got := strings.Count(wrapper, source); got != want {
			t.Errorf("Windows container runner source count for %q = %d, want %d", source, got, want)
		}
	}
	for source, want := range map[string]int{
		"[string] $BuilderImage = 'alphatoad/notty:windows-builder'":                                        1,
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
		"x86_64-pc-windows-gnu aarch64-pc-windows-gnullvm":          1,
		"aarch64-w64-mingw32-clang.exe":                             1,
		"x86_64-w64-mingw32-clang.exe":                              1,
		"C:\\Windows\\System32\\msi.dll":                            1,
		"dotnet restore C:\\toolchain-restore\\Codesk.wixproj":      1,
		"wixtoolset.sdk\\4.0.5":                                     1,
		"ENTRYPOINT [\"powershell.exe\"":                            1,
	} {
		if got := strings.Count(dockerfile, source); got != want {
			t.Errorf("Windows toolchain Dockerfile source count for %q = %d, want %d", source, got, want)
		}
	}
	for _, forbidden := range []string{"--isolation=hyperv", "nanoserver:", "windows/amd64"} {
		if strings.Contains(builder, forbidden) || strings.Contains(wrapper, forbidden) || strings.Contains(dockerfile, forbidden) {
			t.Errorf("Windows container build contains forbidden %q", forbidden)
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
		{name: "GUI subsystem dropped", old: `-ldflags="-H=windowsgui -extldflags=-Wl,--subsystem,windows -X main.desktopVersion=$build_version"`, new: `-ldflags="-s -w"`},
		{name: "agent payload omitted", old: `-o "$arch_payload_dir/notty-agent-tool.exe" ./daemon/cmd/agenttool`, new: `-o "$arch_payload_dir/notty-agent-tool.exe" ./daemon/cmd/codesk-desktop`},
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
	if target != "windows-gui-build" && target != "windows-gui-release" {
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
		commandDirectory = tempDir
		makeOverrides = []string{
			"OS=Windows_NT", "PROCESSOR_ARCHITECTURE=ARM64", "WINDOWS_GUI_POWERSHELL=" + powershellCommand,
			"SHELL=" + os.Getenv("COMSPEC"),
		}
	} else {
		if err := writeExecutable(filepath.Join(binDir, "powershell.exe"), `#!/bin/sh
: "${WINDOWS_GUI_CAPTURE:?}"
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
			if target == "windows-gui-build" {
				return fmt.Errorf("PowerShell build route contains spoofable architecture %q", argument)
			}
			if argument != "WINDOWS_GUI_ARCHES=amd64 arm64" {
				return fmt.Errorf("PowerShell release route architecture assignment = %q", argument)
			}
		}
	}
	if target == "windows-gui-release" && architectureAssignments != 1 {
		return fmt.Errorf("PowerShell release route architecture assignment count = %d, want 1", architectureAssignments)
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
	payloadScript := filepath.Join(scriptsDir, "build-windows-desktop-payloads.sh")
	if err := writeExecutable(payloadScript, source); err != nil {
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
		`required_zig_version="${WINDOWS_GUI_ZIG_VERSION:-0.16.0}"`: 1,
		`[ "$actual_zig_version" = "$required_zig_version" ]`:       1,
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
		`RUST_TARGET="$rust_target" RUSTFLAGS='-C panic=abort' scripts/build-yffi.sh`:                                              1,
		`go vet ./daemon/internal/syncer ./internal/ycrdt`:                                                                         1,
		`go test -c -o "$test_dir/notty-syncer-$architecture.test.exe" ./daemon/internal/syncer`:                                   1,
		`go vet ./daemon/internal/desktopstate ./daemon/internal/desktop ./daemon/internal/desktopapp ./daemon/cmd/codesk-desktop`: 1,
		`go test -c -o "$test_dir/codesk-desktop-$architecture.test.exe" ./daemon/cmd/codesk-desktop`:                              1,
		`-ldflags="-H=windowsgui -extldflags=-Wl,--subsystem,windows -X main.desktopVersion=$build_version"`:                       1,
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

func checkWindowsMSIReleaseSource(build, orchestrator, makefile, shim string) error {
	buildRequired := map[string]int{
		"[CmdletBinding(DefaultParameterSetName = 'QaPair')]":                   1,
		"[Parameter(Mandatory = $true, ParameterSetName = 'QaPair')]":           2,
		"[Parameter(Mandatory = $true, ParameterSetName = 'Release')]":          1,
		"$ProductCodeNamespace = [guid] '55A27873-BF9C-5DC3-AA8B-9D6F996041EF'": 1,
		"^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$":                 1,
		"$major -gt 255":   1,
		"$minor -gt 255":   1,
		"$build -gt 65535": 1,
		"$productCodeName = \"$ProductVersion+$GoArchitecture\"":                                       1,
		"Get-UuidV5 -Namespace $ProductCodeNamespace -Name $productCodeName":                           1,
		"[System.Text.Encoding]::UTF8.GetBytes($Name)":                                                 1,
		"[System.Security.Cryptography.SHA1]::Create()":                                                1,
		"$uuidBytes[6] = ($uuidBytes[6] -band 0x0f) -bor 0x50":                                         1,
		"$uuidBytes[8] = ($uuidBytes[8] -band 0x3f) -bor 0x80":                                         1,
		"$buildMode = if ($PSCmdlet.ParameterSetName -ceq 'Release') { 'release' } else { 'qa-pair' }": 1,
		"name = 'release'":  1,
		"version = '0.0.1'": 1,
		"version = '0.0.2'": 1,
		"previous and candidate ProductCodes must be distinct": 1,
		"buildMode = $buildMode":                               1,
		"productCodeDerivation":                                1,
		"algorithm = 'UUIDv5-SHA1'":                            1,
		"Codesk_${ProductVersion}_windows_$GoArchitecture.msi": 2,
		"Codesk_0.0.1_windows_$GoArchitecture.msi":             2,
		"Codesk_0.0.2_windows_$GoArchitecture.msi":             2,
	}
	for source, want := range buildRequired {
		if got := strings.Count(build, source); got != want {
			return fmt.Errorf("Windows MSI release source count for %q = %d, want %d", source, got, want)
		}
	}
	for source, want := range map[string]int{
		"[ValidateSet('windows-gui-build', 'windows-gui-release')]":                      1,
		"function Get-WindowsHostArchitecture":                                           1,
		"[System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()": 1,
		"'ARM64' { return 'arm64' }":                                                     1,
		"$hostArchitecture = Get-WindowsHostArchitecture":                                1,
		"windows-gui-build requires exactly one host architecture":                       1,
		"does not match host $hostArchitecture":                                          1,
		"[System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT":      1,
		"scripts/build-windows-desktop-payloads.sh":                                      1,
		"scripts/build-windows-desktop-msi-artifact.ps1":                                 1,
		"local/scripts/run-windows-gui-target.ps1@$head":                                 1,
		"^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$":                          1,
		"$fields[0] -gt 255":                                                             1,
		"$fields[1] -gt 255":                                                             1,
		"$fields[2] -gt 65535":                                                           1,
		"ProductVersion = $Version":                                                      1,
		"$item.Length -le 0":                                                             1,
		"Assert-ExactArchitectureDirectories -Directory $PayloadDirectory -Architectures $SelectedArchitectures": 1,
		"Assert-ExactRealFiles @releaseParameters":                                                               1,
		`Names = @("Codesk_${Version}_windows_$architecture.msi", 'SHA256SUMS', 'provenance.json')`:              1,
		"[Array]::Sort($actual, [System.StringComparer]::Ordinal)":                                               2,
		"[Array]::Sort($expected, [System.StringComparer]::Ordinal)":                                             2,
	} {
		if got := strings.Count(orchestrator, source); got != want {
			return fmt.Errorf("Windows GUI orchestrator source count for %q = %d, want %d", source, got, want)
		}
	}
	for _, forbidden := range []string{"??", "Sort-Object", "status --porcelain", "requires a clean checkout"} {
		if strings.Contains(orchestrator, forbidden) {
			return fmt.Errorf("Windows GUI orchestrator contains unsupported or non-ordinal %q", forbidden)
		}
	}
	if regexp.MustCompile(`(?m)\\\s*$`).MatchString(orchestrator) {
		return fmt.Errorf("Windows GUI orchestrator contains a shell-style line continuation")
	}
	for source, want := range map[string]int{
		"ifeq ($(OS),Windows_NT)":                                      2,
		"\nVERSION ?= $(FILE_VERSION)\n":                               2,
		"WINDOWS_PROCESSOR_ARCH :=":                                    1,
		"override MACOS_GUI_HOST_OS :=":                                2,
		`if [ "$(MACOS_GUI_HOST_OS)" != darwin ]; then`:                2,
		"-File make.ps1":                                               5,
		"WINDOWS_GUI_BUILDER_IMAGE ?= alphatoad/notty:windows-builder": 1,
		"-File make.ps1 build-windows-builder-image":                   1,
		`"WINDOWS_GUI_BUILDER_IMAGE=$(WINDOWS_GUI_BUILDER_IMAGE)"`:     3,
		"-File make.ps1 windows-gui-build":                             1,
		"-File make.ps1 windows-gui-release":                           1,
		`"WINDOWS_GUI_ARCHES=$(WINDOWS_GUI_ARCHES)"`:                   1,
		"scripts/build-windows-desktop-payloads.sh":                    1,
	} {
		if got := strings.Count(makefile, source); got != want {
			return fmt.Errorf("Windows GUI Make source count for %q = %d, want %d", source, got, want)
		}
	}
	publicTargets := []string{"macos-gui-build", "macos-gui-release", "build-windows-builder-image", "windows-gui-build", "windows-gui-release"}
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
		"scripts/build-windows-gui-builder-image.ps1":                        1,
		"scripts/run-windows-gui-container.ps1":                              1,
		"'WINDOWS_GUI_BUILDER_IMAGE'":                                        2,
		"'WINDOWS_GUI_ARCHES'":                                               3,
		"'Architectures'":                                                    1,
		"macos-gui-build requires a real macOS host; no GUI was built":       1,
		"macos-gui-release requires a real macOS host; no release was built": 1,
	} {
		if got := strings.Count(shim, source); got != want {
			return fmt.Errorf("Windows GUI PowerShell shim source count for %q = %d, want %d", source, got, want)
		}
	}
	for _, forbidden := range []string{
		"WINDOWS_GUI_PREVIOUS_PRODUCT_CODE",
		"WINDOWS_GUI_CANDIDATE_PRODUCT_CODE",
		"windows-gui-release-arch",
		"scripts/build-windows-desktop-msi-artifact.ps1",
		"WINDOWS_GUI_ARCH ?=",
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
	for _, forbidden := range []string{"$(shell", "uname", "/dev/null", "printf"} {
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
