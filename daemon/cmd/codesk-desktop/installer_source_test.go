package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	wixNamespace                     = "http://wixtoolset.org/schemas/v4/wxs"
	codeskExecutableComponentGUID    = "931D5BAC-B213-44A8-B234-E24E415613EC"
	agentToolExecutableComponentGUID = "4DE1EFE2-7E29-4E46-A615-4CC9A6EB7DBE"
)

type wixElement struct {
	XMLName xml.Name
	Attrs   []xml.Attr   `xml:",any,attr"`
	Nodes   []wixElement `xml:",any"`
	Text    string       `xml:",chardata"`
}

func TestWindowsInstallerCIUsesArchitectureBoundProductPayloads(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	const msiShell = `powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command ". '{0}'"`
	for source, count := range map[string]int{
		"uses: actions/upload-artifact@v4":                                                                  3,
		"uses: actions/download-artifact@v4":                                                                1,
		"name: windows-desktop-payload-amd64":                                                               1,
		"name: windows-desktop-payload-arm64":                                                               1,
		"name: windows-desktop-payload-${{ matrix.go_arch }}":                                               1,
		"name: windows-desktop-msi-${{ matrix.go_arch }}":                                                   1,
		"path: ${{ runner.temp }}/windows-desktop-msi-${{ matrix.go_arch }}/":                               1,
		"needs: windows-daemon-build":                                                                       1,
		`-o "$payload_dir/Codesk.exe" ./daemon/cmd/codesk-desktop`:                                          1,
		`-o "$payload_dir/notty-agent-tool.exe" ./daemon/cmd/agenttool`:                                     1,
		`go run ./scripts/verify-windows-desktop-pe.go "$payload_dir/Codesk.exe" "$arch" gui`:               1,
		`go run ./scripts/verify-windows-desktop-pe.go "$payload_dir/notty-agent-tool.exe" "$arch" console`: 1,
		"path: ${{ runner.temp }}/windows-desktop-payload/amd64/":                                           1,
		"path: ${{ runner.temp }}/windows-desktop-payload/arm64/":                                           1,
		"runs-on: [self-hosted, Windows, ARM64]":                                                            1,
		"shell: " + msiShell:                                                                                3,
		`(Join-Path $payload "Codesk.exe")`:                                                                 3,
		`(Join-Path $payload "notty-agent-tool.exe")`:                                                       3,
		`./scripts/build-windows-desktop-msi-artifact.ps1`:                                                  2,
		`-Release`:           1,
		`-TestOnlyUpgradeQa`: 1,
		`-SourceEvent "${{ github.event_name }}"`:   2,
		`-SourceCheckoutCommit "${{ github.sha }}"`: 2,
		`-SourceHead "${{ github.event_name == 'pull_request' && github.event.pull_request.head.sha || github.sha }}"`:          2,
		`-SourceBase "${{ github.event_name == 'pull_request' && github.event.pull_request.base.sha || github.event.before }}"`: 2,
		`-SafeParentDirectory $env:RUNNER_TEMP`: 2,
		`-DotnetSdkVersion "8.0.423"`:           2,
		"dotnet-version: 8.0.423":               1,
		"path: ${{ runner.temp }}/wix-payload-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.architecture }}": 1,
		`$output = Join-Path $env:RUNNER_TEMP "windows-desktop-msi-upgrade-qa-${{ matrix.go_arch }}"`:                    1,
		`$provenance.target.publishable -ne $false`:                                                                      1,
		`"-p:ProductVersion=not-a-valid-msi-version"`:                                                                    1,
		`throw "WiX accepted the compiler-only invalid ProductVersion mutation"`:                                         1,
	} {
		if got := strings.Count(workflow, source); got != count {
			t.Errorf("CI source count for %q = %d, want %d", source, got, count)
		}
	}
	for _, placeholder := range []string{"WriteAllBytes", "[byte[]](0x4d, 0x5a)"} {
		if strings.Contains(workflow, placeholder) {
			t.Errorf("CI installer payload must not use placeholder construction %q", placeholder)
		}
	}
	if err := checkWindowsInstallerPowerShellContract(workflow); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		old  string
		new  string
	}{
		{name: "missing explicit shell", old: "\n        shell: " + msiShell, new: ""},
		{name: "bare PowerShell regression", old: "\n        shell: " + msiShell, new: "\n        shell: powershell"},
		{name: "pwsh regression", old: "\n        shell: " + msiShell, new: "\n        shell: pwsh"},
		{
			name: "execution policy bypass removed",
			old:  "\n        shell: " + msiShell,
			new:  "\n        shell: powershell -NoProfile -NonInteractive -Command \". '{0}'\"",
		},
		{name: "floating dotnet SDK", old: "dotnet-version: 8.0.423", new: "dotnet-version: 8.0.x"},
		{
			name: "shallow provenance checkout",
			old:  "      - uses: actions/checkout@v4\n        with:\n          fetch-depth: 0\n      - uses: actions/setup-go@v5",
			new:  "      - uses: actions/checkout@v4\n        with:\n          fetch-depth: 1\n      - uses: actions/setup-go@v5",
		},
		{name: "missing checkout identity", old: `-SourceCheckoutCommit "${{ github.sha }}"`, new: ""},
		{
			name: "swapped reviewed head identity",
			old:  `-SourceHead "${{ github.event_name == 'pull_request' && github.event.pull_request.head.sha || github.sha }}"`,
			new:  `-SourceHead "${{ github.event_name == 'pull_request' && github.event.pull_request.base.sha || github.sha }}"`,
		},
		{name: "missing source base identity", old: `-SourceBase "${{ github.event_name == 'pull_request' && github.event.pull_request.base.sha || github.event.before }}"`, new: ""},
		{
			name: "payload path reuses prior run",
			old:  "path: ${{ runner.temp }}/wix-payload-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.architecture }}",
			new:  "path: ${{ runner.temp }}/wix-payload-${{ matrix.architecture }}",
		},
		{
			name: "payload inventory is not recursive",
			old:  "$payloadFiles = @(Get-ChildItem -LiteralPath $payload -File -Recurse -Force)",
			new:  "$payloadFiles = @(Get-ChildItem -LiteralPath $payload -File -Force)",
		},
		{
			name: "payload directory rejection removed",
			old:  "if ($payloadDirectories.Count -ne 0)",
			new:  "if ($payloadDirectories.Count -lt 0)",
		},
		{
			name: "payload inventory drops agent tool",
			old:  `$expectedPayloadNames = @("Codesk.exe", "notty-agent-tool.exe")`,
			new:  `$expectedPayloadNames = @("Codesk.exe")`,
		},
		{name: "production build invokes QA fixture", old: "            -Release `", new: "            -TestOnlyUpgradeQa `"},
		{
			name: "QA fixture overwrites production output",
			old:  `$output = Join-Path $env:RUNNER_TEMP "windows-desktop-msi-upgrade-qa-${{ matrix.go_arch }}"`,
			new:  `$output = Join-Path $env:RUNNER_TEMP "windows-desktop-msi-${{ matrix.go_arch }}"`,
		},
		{
			name: "QA fixture can be published",
			old:  `$provenance.target.publishable -ne $false`,
			new:  `$provenance.target.publishable -eq $false`,
		},
		{
			name: "upload promotes QA fixture",
			old:  `path: ${{ runner.temp }}/windows-desktop-msi-${{ matrix.go_arch }}/`,
			new:  `path: ${{ runner.temp }}/windows-desktop-msi-upgrade-qa-${{ matrix.go_arch }}/`,
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			wantCount := 1
			if mutation.old == "\n        shell: "+msiShell {
				wantCount = 3
			}
			for _, duplicated := range []string{
				`-SourceEvent "${{ github.event_name }}"`,
				`-SourceCheckoutCommit "${{ github.sha }}"`,
				`-SourceHead "${{ github.event_name == 'pull_request' && github.event.pull_request.head.sha || github.sha }}"`,
				`-SourceBase "${{ github.event_name == 'pull_request' && github.event.pull_request.base.sha || github.event.before }}"`,
			} {
				if mutation.old == duplicated {
					wantCount = 2
				}
			}
			if strings.Count(workflow, mutation.old) != wantCount {
				t.Fatalf("workflow mutation source %q count changed", mutation.old)
			}
			mutated := strings.Replace(workflow, mutation.old, mutation.new, 1)
			if err := checkWindowsInstallerPowerShellContract(mutated); err == nil {
				t.Fatal("MSI workflow contract mutation passed")
			}
		})
	}
}

func checkWindowsInstallerPowerShellContract(workflow string) error {
	const msiShell = `powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command ". '{0}'"`
	const (
		start = "  windows-desktop-msi:\n"
		end   = "\n  windows-daemon-installer:\n"
	)
	startAt := strings.Index(workflow, start)
	if startAt < 0 {
		return fmt.Errorf("CI has no windows-desktop-msi job")
	}
	endAt := strings.Index(workflow[startAt:], end)
	if endAt < 0 {
		return fmt.Errorf("CI has no boundary after windows-desktop-msi job")
	}
	job := workflow[startAt : startAt+endAt]
	if strings.Contains(job, "shell: pwsh") {
		return fmt.Errorf("MSI job requires unavailable pwsh")
	}
	if strings.Contains(job, "\n        shell: powershell\n") {
		return fmt.Errorf("MSI job uses execution-policy-sensitive bare PowerShell")
	}
	for required, count := range map[string]int{
		"fetch-depth: 0":                            1,
		"dotnet-version: 8.0.423":                   1,
		`-SourceEvent "${{ github.event_name }}"`:   2,
		`-SourceCheckoutCommit "${{ github.sha }}"`: 2,
		`-SourceHead "${{ github.event_name == 'pull_request' && github.event.pull_request.head.sha || github.sha }}"`:          2,
		`-SourceBase "${{ github.event_name == 'pull_request' && github.event.pull_request.base.sha || github.event.before }}"`: 2,
		`-SafeParentDirectory $env:RUNNER_TEMP`: 2,
		`-DotnetSdkVersion "8.0.423"`:           2,
		"path: ${{ runner.temp }}/wix-payload-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.architecture }}":                1,
		`$payload = Join-Path $env:RUNNER_TEMP "wix-payload-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.architecture }}"`: 3,
		`$payloadDirectories = @(Get-ChildItem -LiteralPath $payload -Directory -Recurse -Force)`:                                       1,
		`if ($payloadDirectories.Count -ne 0)`:                                         1,
		`$payloadFiles = @(Get-ChildItem -LiteralPath $payload -File -Recurse -Force)`: 1,
		`$expectedPayloadNames = @("Codesk.exe", "notty-agent-tool.exe")`:              1,
	} {
		if got := strings.Count(job, required); got != count {
			return fmt.Errorf("MSI job source count for %q = %d, want %d", required, got, count)
		}
	}
	if strings.Contains(job, "dotnet-version: 8.0.x") {
		return fmt.Errorf("MSI job uses a floating .NET SDK")
	}
	productionStart := strings.Index(job, "      - name: Build reproducible root-version WiX release package\n")
	qaStart := strings.Index(job, "      - name: Build non-publishable MSI upgrade QA fixture\n")
	uploadStart := strings.Index(job, "      - name: Upload reproducible root-version MSI and provenance\n")
	if productionStart < 0 || qaStart <= productionStart || uploadStart <= qaStart {
		return fmt.Errorf("MSI production, QA fixture, and upload steps are not distinctly ordered")
	}
	productionStep := job[productionStart:qaStart]
	qaStep := job[qaStart:uploadStart]
	uploadStep := job[uploadStart:]
	if !strings.Contains(productionStep, "            -Release `") || strings.Contains(productionStep, "-TestOnlyUpgradeQa") {
		return fmt.Errorf("production MSI step does not explicitly use release mode")
	}
	for _, required := range []string{
		"            -TestOnlyUpgradeQa `",
		`windows-desktop-msi-upgrade-qa-${{ matrix.go_arch }}`,
		`$provenance.target.buildMode -cne 'test-only-upgrade-qa'`,
		`$provenance.target.publishable -ne $false`,
	} {
		if !strings.Contains(qaStep, required) {
			return fmt.Errorf("MSI QA fixture step is missing %q", required)
		}
	}
	if !strings.Contains(uploadStep, `path: ${{ runner.temp }}/windows-desktop-msi-${{ matrix.go_arch }}/`) ||
		strings.Contains(uploadStep, "upgrade-qa") {
		return fmt.Errorf("MSI upload does not exclusively publish the root-version release path")
	}

	runSteps := 0
	for _, step := range strings.Split(job, "\n      - ") {
		if !strings.Contains(step, "\n        run:") {
			continue
		}
		runSteps++
		if !strings.Contains(step, "\n        shell: "+msiShell+"\n") {
			return fmt.Errorf("MSI run step does not explicitly use Windows PowerShell: %q", strings.SplitN(step, "\n", 2)[0])
		}
	}
	if runSteps != 3 {
		return fmt.Errorf("MSI job has %d run steps, want 3", runSteps)
	}
	return nil
}

func TestWindowsInstallerReproducibilityScriptsAreFailClosed(t *testing.T) {
	root := filepath.Join("..", "..", "..", "scripts")
	buildData, err := os.ReadFile(filepath.Join(root, "build-windows-desktop-msi-artifact.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	verifyData, err := os.ReadFile(filepath.Join(root, "verify-windows-desktop-msi-reproducibility.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkWindowsInstallerReproducibilityScripts(string(buildData), string(verifyData)); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name      string
		buildOld  string
		buildNew  string
		verifyOld string
		verifyNew string
	}{
		{
			name:     "suppressed ICE",
			buildOld: "-p:SuppressValidation=false",
			buildNew: "-p:SuppressValidation=true",
		},
		{
			name:     "no independent-link clock boundary",
			buildOld: "Start-Sleep -Seconds 2",
			buildNew: "Start-Sleep -Seconds 0",
		},
		{
			name:     "one clean link only",
			buildOld: "$secondMsi = Invoke-CleanLink -Version $version -BuildNumber 2",
			buildNew: "$secondMsi = $firstMsi",
		},
		{
			name:     "architecture tuple not enforced",
			buildOld: "$GoArchitecture -cne $expectedTarget.GoArchitecture",
			buildNew: "$GoArchitecture -ceq $GoArchitecture",
		},
		{
			name:     "actual dotnet SDK drift accepted",
			buildOld: "$dotnetVersion -cne $DotnetSdkVersion",
			buildNew: "$dotnetVersion -ceq $DotnetSdkVersion",
		},
		{
			name:     "pull request parents swapped",
			buildOld: "$mergeBase -cne $SourceBase -or $mergeHead -cne $SourceHead",
			buildNew: "$mergeBase -cne $SourceHead -or $mergeHead -cne $SourceBase",
		},
		{
			name:     "reviewed head omitted from provenance",
			buildOld: "sourceHead = $SourceHead",
			buildNew: "omittedSourceHead = $SourceHead",
		},
		{
			name:     "builder reset ignores deletion failure",
			buildOld: "Remove-Item -LiteralPath $pathFull -Recurse -Force -ErrorAction Stop",
			buildNew: "Remove-Item -LiteralPath $pathFull -Recurse -Force -ErrorAction SilentlyContinue",
		},
		{
			name:      "verifier reset ignores deletion failure",
			verifyOld: "Remove-Item -LiteralPath $pathFull -Recurse -Force -ErrorAction Stop",
			verifyNew: "Remove-Item -LiteralPath $pathFull -Recurse -Force -ErrorAction SilentlyContinue",
		},
		{
			name:     "builder reset skips empty assertion",
			buildOld: "$remaining.Count -ne 0",
			buildNew: "$remaining.Count -lt 0",
		},
		{
			name:      "verifier reset skips empty assertion",
			verifyOld: "$remaining.Count -ne 0",
			verifyNew: "$remaining.Count -lt 0",
		},
		{
			name:     "canonical artifact inventory not recursive",
			buildOld: "Get-ChildItem -LiteralPath $Root -File -Recurse -Force -ErrorAction Stop",
			buildNew: "Get-ChildItem -LiteralPath $Root -File -Force -ErrorAction Stop",
		},
		{
			name:     "canonical artifact directories accepted",
			buildOld: "$directories.Count -ne 0",
			buildNew: "$directories.Count -lt 0",
		},
		{
			name:     "canonical expected inventory uses case-insensitive sort",
			buildOld: "[Array]::Sort($expectedNames, [System.StringComparer]::Ordinal)",
			buildNew: "[Array]::Sort($expectedNames, [System.StringComparer]::OrdinalIgnoreCase)",
		},
		{
			name:      "required allowed summary PID omitted",
			verifyOld: "if (-not $seen.ContainsKey($propertyId))",
			verifyNew: "if ($seen.ContainsKey($propertyId))",
		},
		{
			name:      "PackageCode GUID domain check removed",
			verifyOld: "$packageCode = ConvertTo-NormalizedGuid $packageCodeValue",
			verifyNew: "$packageCode = $packageCodeValue",
		},
		{
			name:      "CreateTime domain check removed",
			verifyOld: "$createTime -eq [datetime]::MinValue",
			verifyNew: "$createTime -ne $createTime",
		},
		{
			name:      "LastSaveTime domain check removed",
			verifyOld: "$lastSaveTime -eq [datetime]::MinValue",
			verifyNew: "$lastSaveTime -ne $lastSaveTime",
		},
		{
			name:      "linked MSI platform not enforced",
			verifyOld: "$Snapshot.Summary['PID7-Template'] -cne $ExpectedSummaryTemplate",
			verifyNew: "$Snapshot.Summary['PID7-Template'] -cne $Snapshot.Summary['PID7-Template']",
		},
		{
			name:      "decompiled source comparison disabled",
			verifyOld: "Assert-TextEqual -Label 'normalized MSI tables' -First $First.SourceText -Second $Second.SourceText",
			verifyNew: "Assert-TextEqual -Label 'normalized MSI tables' -First $First.SourceText -Second $First.SourceText",
		},
		{
			name:      "database comparison disabled",
			verifyOld: "Assert-TextEqual -Label 'normalized MSI database schema, rows, or streams' -First $First.DatabaseFilesJson -Second $Second.DatabaseFilesJson",
			verifyNew: "Assert-TextEqual -Label 'normalized MSI database schema, rows, or streams' -First $First.DatabaseFilesJson -Second $First.DatabaseFilesJson",
		},
		{
			name:      "extracted payload comparison disabled",
			verifyOld: "Assert-TextEqual -Label 'normalized MSI resources or embedded payloads' -First $First.ExtractedFilesJson -Second $Second.ExtractedFilesJson",
			verifyNew: "Assert-TextEqual -Label 'normalized MSI resources or embedded payloads' -First $First.ExtractedFilesJson -Second $First.ExtractedFilesJson",
		},
		{
			name:      "package identity comparison disabled",
			verifyOld: "Assert-JsonEqual -Label 'package identity' -First $First.Identity -Second $Second.Identity",
			verifyNew: "Assert-JsonEqual -Label 'package identity' -First $First.Identity -Second $First.Identity",
		},
		{
			name:      "nonvolatile summary comparison disabled",
			verifyOld: "Assert-JsonEqual -Label 'nonvolatile summary information' -First $firstStableSummary -Second $secondStableSummary",
			verifyNew: "Assert-JsonEqual -Label 'nonvolatile summary information' -First $firstStableSummary -Second $firstStableSummary",
		},
		{
			name:      "component identity comparison disabled",
			verifyOld: "Assert-JsonEqual -Label 'component identities' -First $ExpectedComponents -Second $identity.components",
			verifyNew: "Assert-JsonEqual -Label 'component identities' -First $ExpectedComponents -Second $ExpectedComponents",
		},
		{
			name:      "embedded payload hash comparison disabled",
			verifyOld: "$identity.payloads[$id].sha256 -cne $expectedPayloads[$id]",
			verifyNew: "$expectedPayloads[$id] -cne $expectedPayloads[$id]",
		},
		{
			name:      "rooted decompiler source path unsupported",
			verifyOld: "if ([System.IO.Path]::IsPathRooted($Source)) {",
			verifyNew: "if ($false) {",
		},
		{
			name:      "decompiler source path escape accepted",
			verifyOld: "$relative = Get-RelativePath -Root $Root -Path $pathFull",
			verifyNew: "$relative = [System.IO.Path]::GetFileName($pathFull)",
		},
		{
			name:      "snapshot extract root not normalized",
			verifyOld: "$normalized = $normalized -replace $extractRootPattern, $extractRootToken",
			verifyNew: "$normalized = $normalized",
		},
		{
			name:      "reserved extract-root token collision accepted",
			verifyOld: "if ($normalized.Contains($extractRootToken)) {",
			verifyNew: "if ($false) {",
		},
		{
			name:      "extract-root prefix overlap normalized",
			verifyOld: "$extractRootPattern = [regex]::Escape($extractRootForm) + '(?=[\\\\/])'",
			verifyNew: "$extractRootPattern = [regex]::Escape($extractRootForm)",
		},
		{
			name:      "non-root source differences normalized away",
			verifyOld: "$normalized = $normalized -replace $extractRootPattern, $extractRootToken",
			verifyNew: "$normalized = $extractRootToken",
		},
		{
			name:      "first-difference evidence omitted",
			verifyOld: `Write-Host "$Label first differing excerpt: $firstExcerpt"`,
			verifyNew: `$null = $firstExcerpt`,
		},
		{
			name:      "DTF summary handle retained",
			verifyOld: "$summaryInfo.Close()",
			verifyNew: "$null = $summaryInfo",
		},
		{
			name:      "no causal database mutation",
			verifyOld: "'CodeskCausalMismatch'",
			verifyNew: "'Codesk'",
		},
		{
			name:      "PackageCode no longer allowed",
			verifyOld: "$AllowedSummaryDifferencePids = @(9, 12, 13)",
			verifyNew: "$AllowedSummaryDifferencePids = @(12, 13)",
		},
		{
			name:      "LastPrintTime volatility broadened",
			verifyOld: "$AllowedSummaryDifferencePids = @(9, 12, 13)",
			verifyNew: "$AllowedSummaryDifferencePids = @(9, 11, 12, 13)",
		},
		{
			name:      "component identity unpinned",
			verifyOld: "CodeskExecutable = '{931D5BAC-B213-44A8-B234-E24E415613EC}'",
			verifyNew: "CodeskExecutable = '*'",
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutatedBuild := string(buildData)
			mutatedVerify := string(verifyData)
			if mutation.buildOld != "" {
				if strings.Count(mutatedBuild, mutation.buildOld) != 1 {
					t.Fatalf("build mutation source %q is not unique", mutation.buildOld)
				}
				mutatedBuild = strings.Replace(mutatedBuild, mutation.buildOld, mutation.buildNew, 1)
			}
			if mutation.verifyOld != "" {
				if strings.Count(mutatedVerify, mutation.verifyOld) != 1 {
					t.Fatalf("verifier mutation source %q is not unique", mutation.verifyOld)
				}
				mutatedVerify = strings.Replace(mutatedVerify, mutation.verifyOld, mutation.verifyNew, 1)
			}
			if err := checkWindowsInstallerReproducibilityScripts(mutatedBuild, mutatedVerify); err == nil {
				t.Fatal("reproducibility contract mutation passed")
			}
		})
	}
}

func checkWindowsInstallerReproducibilityScripts(build, verify string) error {
	for _, required := range []string{
		"test-only-upgrade-qa",
		"testdata\\windows-msi-upgrade-versions.ps1",
		"publishable = ($buildMode -ceq 'release')",
		"Start-Sleep -Seconds 2",
		"$firstMsi = Invoke-CleanLink -Version $version -BuildNumber 1",
		"$secondMsi = Invoke-CleanLink -Version $version -BuildNumber 2",
		"-p:SuppressValidation=false",
		"verify-windows-desktop-msi-reproducibility.ps1",
		`Codesk_$($_.version)_windows_$GoArchitecture.msi`,
		"cleanLinksPerVersion = 2",
		"provenance.json",
		"SHA256SUMS",
		"source checkout commit mismatch",
		"pull request checkout parents do not map to source base then source head",
		"$mergeBase -cne $SourceBase -or $mergeHead -cne $SourceHead",
		"checkout-first-parent-fallback",
		"git merge-base --is-ancestor",
		"checkoutCommit = $SourceCheckoutCommit",
		"sourceHead = $SourceHead",
		"sourceBase = $SourceBase",
		"sourceBaseResolution = $sourceBaseResolution",
		"schemaVersion = 2",
		"DotnetSdkVersion = '8.0.423'",
		"$dotnetVersion -cne $DotnetSdkVersion",
		"dotnet SDK mismatch before the first WiX link",
		"Reset-EmptyDirectory",
		"Remove-Item -LiteralPath $pathFull -Recurse -Force -ErrorAction Stop",
		"$remaining.Count -ne 0",
		"still exists after reset removal",
		"is not empty after reset",
		"-SafeParentDirectory $WorkingDirectory",
		"-ExpectedInstallerPlatform $InstallerPlatform",
		"Get-CanonicalArtifactFiles",
		"Get-ChildItem -LiteralPath $Root -Directory -Recurse -Force -ErrorAction Stop",
		"Get-ChildItem -LiteralPath $Root -File -Recurse -Force -ErrorAction Stop",
		"$directories.Count -ne 0",
		"canonical artifact set contains directories",
		"[Array]::Sort($paths, [System.StringComparer]::Ordinal)",
		"[Array]::Sort($expectedChecksumNames, [System.StringComparer]::Ordinal)",
		"[Array]::Sort($expectedNames, [System.StringComparer]::Ordinal)",
		"(ConvertTo-Json $expectedChecksumNames -Compress)",
		"(ConvertTo-Json $expectedNames -Compress)",
		"unexpected checksummed artifact set",
		"unexpected canonical artifact set",
		"inconsistent target tuple",
		"$GoArchitecture -cne $expectedTarget.GoArchitecture",
		"$InstallerPlatform -cne $expectedTarget.InstallerPlatform",
	} {
		if !strings.Contains(build, required) {
			return fmt.Errorf("MSI artifact builder is missing %q", required)
		}
	}
	if strings.Contains(build, "-p:SuppressValidation=true") {
		return fmt.Errorf("MSI artifact builder suppresses ICE validation")
	}
	if strings.Contains(build, "Sort-Object") {
		return fmt.Errorf("MSI artifact builder mixes canonical artifact comparers")
	}
	if strings.Contains(build, "-ErrorAction SilentlyContinue") || strings.Contains(verify, "-ErrorAction SilentlyContinue") {
		return fmt.Errorf("MSI reproducibility cleanup ignores deletion failures")
	}

	for _, required := range []string{
		"msi decompile",
		"WixToolset.Dtf.WindowsInstaller.Database",
		"$database.ExportAll($Destination)",
		"tools\\net472\\WixToolset.Dtf.WindowsInstaller.dll",
		"$AllowedSummaryDifferencePids = @(9, 12, 13)",
		"'9' = 'RevisionNumber'",
		"'11' = 'LastPrintTime'",
		"'12' = 'CreateTime'",
		"'13' = 'LastSaveTime'",
		"function Get-NormalizedDecompiledSourceText",
		"__WIX_EXTRACT_ROOT__",
		"if ($normalized.Contains($extractRootToken)) {",
		"decompiled source contains reserved extract-root token",
		"$extractRootFull.Replace('\\', '/')",
		"$extractRootPattern = [regex]::Escape($extractRootForm) + '(?=[\\\\/])'",
		"$normalized = $normalized -replace $extractRootPattern, $extractRootToken",
		"$sourceText = Get-NormalizedDecompiledSourceText -SourcePath $source -ExtractRoot $extract",
		"function Assert-TextEqual",
		"first difference at character",
		`Write-Host "$Label first differing excerpt: $firstExcerpt"`,
		`Write-Host "$Label second differing excerpt: $secondExcerpt"`,
		"Assert-TextEqual -Label 'normalized MSI tables' -First $First.SourceText -Second $Second.SourceText",
		"Assert-TextEqual -Label 'normalized MSI database schema, rows, or streams' -First $First.DatabaseFilesJson -Second $Second.DatabaseFilesJson",
		"Assert-TextEqual -Label 'normalized MSI resources or embedded payloads' -First $First.ExtractedFilesJson -Second $Second.ExtractedFilesJson",
		"Assert-JsonEqual -Label 'package identity' -First $First.Identity -Second $Second.Identity",
		"Assert-JsonEqual -Label 'nonvolatile summary information' -First $firstStableSummary -Second $secondStableSummary",
		"function Resolve-ExtractedSourceFile",
		"if ([System.IO.Path]::IsPathRooted($Source)) {",
		"$relativeSource = $Source -replace '^SourceDir[\\\\/]', ''",
		"$relative = Get-RelativePath -Root $Root -Path $pathFull",
		"$resolvedFile = Resolve-ExtractedSourceFile",
		"-Source ([string] $file.Source)",
		"$resolvedIcon = Resolve-ExtractedSourceFile",
		"-Source ([string] $icon.SourceFile)",
		"independent clean links reused the same PackageCode (SummaryInformation PID 9)",
		"reproducibility comparison requires two distinct MSI paths",
		"CodeskExecutable = '{931D5BAC-B213-44A8-B234-E24E415613EC}'",
		"AgentToolExecutable = '{4DE1EFE2-7E29-4E46-A615-4CC9A6EB7DBE}'",
		"Assert-JsonEqual -Label 'component identities' -First $ExpectedComponents -Second $identity.components",
		"$identity.payloads[$id].sha256 -cne $expectedPayloads[$id]",
		"'CodeskCausalMismatch'",
		"MSI database causal mismatch was not rejected",
		"'x64' { 'x64;1033' }",
		"'arm64' { 'Arm64;1033' }",
		"if (-not $seen.ContainsKey($propertyId))",
		"exported SummaryInformation has no required allowed PID",
		"$packageCode = ConvertTo-NormalizedGuid $packageCodeValue",
		"SummaryInformation PID 9 is not a PackageCode GUID",
		"$createTime -eq [datetime]::MinValue",
		"SummaryInformation PID 12 is not a parseable MSI timestamp",
		"$lastSaveTime -eq [datetime]::MinValue",
		"SummaryInformation PID 13 is not a parseable MSI timestamp",
		"$Snapshot.Summary['PID7-Template'] -cne $ExpectedSummaryTemplate",
		"does not match expected $ExpectedSummaryTemplate",
		"$summaryInfo.Close()",
		"Reset-EmptyDirectory",
		"Remove-Item -LiteralPath $pathFull -Recurse -Force -ErrorAction Stop",
		"$remaining.Count -ne 0",
		"still exists after reset removal",
		"is not empty after reset",
	} {
		if !strings.Contains(verify, required) {
			return fmt.Errorf("MSI reproducibility verifier is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"[System.IO.Path]::GetRelativePath",
		"RuntimeInformation",
		"ConvertFrom-Json -AsHashtable",
		"ForEach-Object -Parallel",
		"Join-String",
		"??",
		"?.",
		" && ",
		" || ",
	} {
		if strings.Contains(build, forbidden) || strings.Contains(verify, forbidden) {
			return fmt.Errorf("MSI PowerShell 5.1 scripts use unsupported source %q", forbidden)
		}
	}
	return nil
}

func TestWindowsInstallerProjectPinsWiXBuildInputs(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Codesk.wixproj")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root wixElement
	if err := xml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse WiX project: %v", err)
	}
	if root.XMLName.Local != "Project" || root.XMLName.Space != "" {
		t.Fatalf("unexpected WiX project root: {%s}%s", root.XMLName.Space, root.XMLName.Local)
	}
	assertWixAttrs(t, root, map[string]string{"Sdk": "WixToolset.Sdk/4.0.5"})
	assertWixElementCounts(t, root, map[string]int{
		"Project": 1, "PropertyGroup": 1, "OutputType": 1, "Platform": 1, "DefineConstants": 1,
	})
	if len(root.Nodes) != 1 {
		t.Fatalf("WiX project has %d root elements, want exactly PropertyGroup", len(root.Nodes))
	}
	propertyGroup, err := wixDirectChild(root, "PropertyGroup")
	if err != nil {
		t.Fatal(err)
	}
	assertWixAttrs(t, propertyGroup, map[string]string{})
	if len(propertyGroup.Nodes) != 3 {
		t.Fatalf("WiX PropertyGroup has %d elements, want exactly 3", len(propertyGroup.Nodes))
	}
	for name, value := range map[string]string{
		"OutputType": "Package",
		"Platform":   "$(InstallerPlatform)",
	} {
		element, err := wixDirectChild(propertyGroup, name)
		if err != nil {
			t.Fatal(err)
		}
		assertWixAttrs(t, element, map[string]string{})
		if len(element.Nodes) != 0 {
			t.Fatalf("%s has %d nested elements, want none", name, len(element.Nodes))
		}
		if got := strings.TrimSpace(element.Text); got != value {
			t.Errorf("%s text = %q, want %q", name, got, value)
		}
	}
	constants, err := wixDirectChild(propertyGroup, "DefineConstants")
	if err != nil {
		t.Fatal(err)
	}
	assertWixAttrs(t, constants, map[string]string{})
	if len(constants.Nodes) != 0 {
		t.Fatalf("DefineConstants has %d nested elements, want none", len(constants.Nodes))
	}
	assertWixDefinitions(t, constants.Text, map[string]string{
		"ProductVersion":     "$(ProductVersion)",
		"VersionProductCode": "$(VersionProductCode)",
		"CodeskExe":          "$(CodeskExe)",
		"AgentToolExe":       "$(AgentToolExe)",
		"CodeskIcon":         "$(CodeskIcon)",
	})
}

func TestWindowsInstallerIsThinPerUserWiXPackage(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertNoInstallerDirectives(t, data)
	var root wixElement
	if err := xml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse WiX package: %v", err)
	}
	if root.XMLName.Local != "Wix" || root.XMLName.Space != wixNamespace {
		t.Fatalf("unexpected WiX root: {%s}%s", root.XMLName.Space, root.XMLName.Local)
	}
	assertWixAttrs(t, root, map[string]string{"RequiredVersion": "4.0.0"})
	assertWixVocabulary(t, root, map[string]struct{}{
		"Wix": {}, "Package": {}, "MajorUpgrade": {}, "MediaTemplate": {},
		"Icon": {}, "Property": {}, "StandardDirectory": {}, "Directory": {},
		"Component": {}, "File": {}, "Shortcut": {}, "RemoveFolder": {},
		"RegistryValue": {}, "Feature": {}, "ComponentRef": {},
	})
	assertWixElementCounts(t, root, map[string]int{
		"Wix": 1, "Package": 1, "MajorUpgrade": 1, "MediaTemplate": 1,
		"Icon": 1, "Property": 1, "StandardDirectory": 2, "Directory": 3,
		"Component": 3, "File": 2, "Shortcut": 1, "RemoveFolder": 3,
		"RegistryValue": 3, "Feature": 1, "ComponentRef": 3,
	})
	assertWixTopology(t, root)

	packages := wixElements(root, "Package")
	if len(packages) != 1 {
		t.Fatalf("got %d Package elements, want 1", len(packages))
	}
	packageElement := packages[0]
	assertWixAttrs(t, packageElement, map[string]string{
		"Name":             "Codesk",
		"Manufacturer":     "Codesk",
		"Version":          "$(ProductVersion)",
		"ProductCode":      "$(VersionProductCode)",
		"Language":         "1033",
		"UpgradeCode":      "0C8C0BBA-06EE-43BA-BC34-768B9B740A09",
		"Scope":            "perUser",
		"InstallerVersion": "500",
		"Compressed":       "yes",
	})

	if got := len(wixElements(root, "CustomAction")); got != 0 {
		t.Fatalf("custom installer actions are not allowed, got %d", got)
	}
	assertWixAttrs(t, wixElements(root, "MajorUpgrade")[0], map[string]string{
		"DowngradeErrorMessage": "A newer version of Codesk is already installed.",
		"Schedule":              "afterInstallExecute",
	})
	assertWixAttrs(t, wixElements(root, "MediaTemplate")[0], map[string]string{
		"EmbedCab":         "yes",
		"CompressionLevel": "high",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "StandardDirectory", "LocalAppDataFolder"), map[string]string{
		"Id": "LocalAppDataFolder",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "StandardDirectory", "ProgramMenuFolder"), map[string]string{
		"Id": "ProgramMenuFolder",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Directory", "PerUserProgramsFolder"), map[string]string{
		"Id":   "PerUserProgramsFolder",
		"Name": "Programs",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Directory", "INSTALLFOLDER"), map[string]string{
		"Id":   "INSTALLFOLDER",
		"Name": "Codesk",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Directory", "CodeskProgramsFolder"), map[string]string{
		"Id":   "CodeskProgramsFolder",
		"Name": "Codesk",
	})
	codeskComponent := assertWixElementByID(t, root, "Component", "CodeskExecutable")
	assertWixAttrs(t, codeskComponent, map[string]string{
		"Id":   "CodeskExecutable",
		"Guid": codeskExecutableComponentGUID,
	})
	agentComponent := assertWixElementByID(t, root, "Component", "AgentToolExecutable")
	assertWixAttrs(t, agentComponent, map[string]string{
		"Id":   "AgentToolExecutable",
		"Guid": agentToolExecutableComponentGUID,
	})
	if codeskExecutableComponentGUID == agentToolExecutableComponentGUID {
		t.Fatal("payload components must have distinct fixed GUIDs")
	}
	loginItemComponent := assertWixElementByID(t, root, "Component", "CodeskLoginItemCleanup")
	assertWixAttrs(t, loginItemComponent, map[string]string{
		"Id":             "CodeskLoginItemCleanup",
		"Guid":           "A11ADE55-B9B8-45E9-9DAB-60203C2A824E",
		"NeverOverwrite": "yes",
	})

	codeskFile := assertWixElementByID(t, root, "File", "CodeskExe")
	assertWixAttrs(t, codeskFile, map[string]string{
		"Id":     "CodeskExe",
		"Source": "$(CodeskExe)",
	})
	shortcut := assertWixElementByID(t, codeskFile, "Shortcut", "CodeskStartMenuShortcut")
	assertWixAttrs(t, shortcut, map[string]string{
		"Id":               "CodeskStartMenuShortcut",
		"Directory":        "CodeskProgramsFolder",
		"Name":             "Codesk",
		"Description":      "Codesk desktop app",
		"WorkingDirectory": "INSTALLFOLDER",
		"Icon":             "Codesk.ico",
	})
	agentFile := assertWixElementByID(t, root, "File", "AgentToolExe")
	assertWixAttrs(t, agentFile, map[string]string{
		"Id":     "AgentToolExe",
		"Name":   "notty-agent-tool.exe",
		"Source": "$(AgentToolExe)",
	})
	codeskMarker, err := wixDirectChild(codeskComponent, "RegistryValue")
	if err != nil {
		t.Fatal(err)
	}
	assertWixAttrs(t, codeskMarker, map[string]string{
		"Root":    "HKCU",
		"Key":     `Software\Codesk\Installer\Components`,
		"Name":    "CodeskExecutable",
		"Type":    "integer",
		"Value":   "1",
		"KeyPath": "yes",
	})
	agentMarker, err := wixDirectChild(agentComponent, "RegistryValue")
	if err != nil {
		t.Fatal(err)
	}
	assertWixAttrs(t, agentMarker, map[string]string{
		"Root":    "HKCU",
		"Key":     `Software\Codesk\Installer\Components`,
		"Name":    "AgentToolExecutable",
		"Type":    "integer",
		"Value":   "1",
		"KeyPath": "yes",
	})
	loginItem, err := wixDirectChild(loginItemComponent, "RegistryValue")
	if err != nil {
		t.Fatal(err)
	}
	assertWixAttrs(t, loginItem, map[string]string{
		"Root":    "HKCU",
		"Key":     `Software\Microsoft\Windows\CurrentVersion\Run`,
		"Name":    "Codesk",
		"Type":    "string",
		"Value":   "",
		"KeyPath": "yes",
	})
	removeFolder := assertWixElementByID(t, root, "RemoveFolder", "RemoveCodeskProgramsFolder")
	assertWixAttrs(t, removeFolder, map[string]string{
		"Id":        "RemoveCodeskProgramsFolder",
		"Directory": "CodeskProgramsFolder",
		"On":        "uninstall",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "RemoveFolder", "RemoveInstallFolder"), map[string]string{
		"Id":        "RemoveInstallFolder",
		"Directory": "INSTALLFOLDER",
		"On":        "uninstall",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "RemoveFolder", "RemovePerUserProgramsFolder"), map[string]string{
		"Id":        "RemovePerUserProgramsFolder",
		"Directory": "PerUserProgramsFolder",
		"On":        "uninstall",
	})

	icon := assertWixElementByID(t, root, "Icon", "Codesk.ico")
	assertWixAttrs(t, icon, map[string]string{
		"Id":         "Codesk.ico",
		"SourceFile": "$(CodeskIcon)",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Property", "ARPPRODUCTICON"), map[string]string{
		"Id":    "ARPPRODUCTICON",
		"Value": "Codesk.ico",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Feature", "CodeskFeature"), map[string]string{
		"Id":    "CodeskFeature",
		"Title": "Codesk",
		"Level": "1",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "ComponentRef", "CodeskExecutable"), map[string]string{
		"Id": "CodeskExecutable",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "ComponentRef", "AgentToolExecutable"), map[string]string{
		"Id": "AgentToolExecutable",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "ComponentRef", "CodeskLoginItemCleanup"), map[string]string{
		"Id": "CodeskLoginItemCleanup",
	})
}

func TestWindowsInstallerRejectsUnexpectedElementAttributes(t *testing.T) {
	expected := map[string]string{
		"Id":   "CodeskExecutable",
		"Guid": codeskExecutableComponentGUID,
	}
	var baseline wixElement
	if err := xml.Unmarshal([]byte(`<Component xmlns="`+wixNamespace+`" Id="CodeskExecutable" Guid="`+codeskExecutableComponentGUID+`" />`), &baseline); err != nil {
		t.Fatal(err)
	}
	if err := checkWixAttrs(baseline, expected); err != nil {
		t.Fatalf("valid component failed the exact WiX attribute contract: %v", err)
	}

	var mutated wixElement
	if err := xml.Unmarshal([]byte(`<Component xmlns="`+wixNamespace+`" Id="CodeskExecutable" Guid="`+codeskExecutableComponentGUID+`" Permanent="yes" />`), &mutated); err != nil {
		t.Fatal(err)
	}
	if err := checkWixAttrs(mutated, expected); err == nil {
		t.Fatal("unexpected Permanent attribute passed the exact WiX attribute contract")
	} else if !strings.Contains(err.Error(), "Permanent") {
		t.Fatalf("unexpected-attribute mutation failed for the wrong reason: %v", err)
	}
}

func TestWindowsInstallerRejectsInvalidPayloadComponentGUIDs(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		id        string
		canonical string
		mutated   string
		expected  string
	}{
		{
			name:      "Codesk wildcard",
			id:        "CodeskExecutable",
			canonical: `<Component Id="CodeskExecutable" Guid="` + codeskExecutableComponentGUID + `">`,
			mutated:   `<Component Id="CodeskExecutable" Guid="*">`,
			expected:  codeskExecutableComponentGUID,
		},
		{
			name:      "agent wildcard",
			id:        "AgentToolExecutable",
			canonical: `<Component Id="AgentToolExecutable" Guid="` + agentToolExecutableComponentGUID + `">`,
			mutated:   `<Component Id="AgentToolExecutable" Guid="*">`,
			expected:  agentToolExecutableComponentGUID,
		},
		{
			name:      "Codesk empty",
			id:        "CodeskExecutable",
			canonical: `<Component Id="CodeskExecutable" Guid="` + codeskExecutableComponentGUID + `">`,
			mutated:   `<Component Id="CodeskExecutable" Guid="">`,
			expected:  codeskExecutableComponentGUID,
		},
		{
			name:      "agent empty",
			id:        "AgentToolExecutable",
			canonical: `<Component Id="AgentToolExecutable" Guid="` + agentToolExecutableComponentGUID + `">`,
			mutated:   `<Component Id="AgentToolExecutable" Guid="">`,
			expected:  agentToolExecutableComponentGUID,
		},
		{
			name:      "duplicate",
			id:        "AgentToolExecutable",
			canonical: `<Component Id="AgentToolExecutable" Guid="` + agentToolExecutableComponentGUID + `">`,
			mutated:   `<Component Id="AgentToolExecutable" Guid="` + codeskExecutableComponentGUID + `">`,
			expected:  agentToolExecutableComponentGUID,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := bytes.Count(data, []byte(test.canonical)); got != 1 {
				t.Fatalf("got %d canonical %s component starts, want 1", got, test.id)
			}
			mutated := bytes.Replace(data, []byte(test.canonical), []byte(test.mutated), 1)
			var root wixElement
			if err := xml.Unmarshal(mutated, &root); err != nil {
				t.Fatal(err)
			}
			component, err := wixElementByID(root, "Component", test.id)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkWixAttrs(component, map[string]string{"Id": test.id, "Guid": test.expected}); err == nil {
				t.Fatal("invalid component GUID mutation passed the exact WiX attribute contract")
			} else if !strings.Contains(err.Error(), "Guid") {
				t.Fatalf("component GUID mutation failed for the wrong reason: %v", err)
			}
		})
	}
}

func TestWindowsInstallerRejectsArchitectureConditionedComponentGUIDs(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	canonical := []byte(`<Component Id="CodeskExecutable" Guid="` + codeskExecutableComponentGUID + `">`)
	if got := bytes.Count(data, canonical); got != 1 {
		t.Fatalf("got %d canonical Codesk component starts, want 1", got)
	}
	conditioned := append([]byte(`<?if $(InstallerPlatform) = arm64 ?>`), canonical...)
	mutated := bytes.Replace(data, canonical, conditioned, 1)
	if err := checkInstallerDirectives(mutated); err == nil {
		t.Fatal("architecture-conditioned component GUID mutation passed the fixed shared-identity contract")
	} else if !strings.Contains(err.Error(), "if") {
		t.Fatalf("architecture-conditioned GUID mutation failed for the wrong reason: %v", err)
	}
}

func TestWindowsInstallerRejectsPerUserFileKeyPathMutations(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		id        string
		canonical string
		mutated   string
		expected  map[string]string
	}{
		{
			name:      "Codesk executable",
			id:        "CodeskExe",
			canonical: `<File Id="CodeskExe" Source="$(CodeskExe)">`,
			mutated:   `<File Id="CodeskExe" Source="$(CodeskExe)" KeyPath="yes">`,
			expected:  map[string]string{"Id": "CodeskExe", "Source": "$(CodeskExe)"},
		},
		{
			name:      "agent tool",
			id:        "AgentToolExe",
			canonical: `<File Id="AgentToolExe" Name="notty-agent-tool.exe" Source="$(AgentToolExe)" />`,
			mutated:   `<File Id="AgentToolExe" Name="notty-agent-tool.exe" Source="$(AgentToolExe)" KeyPath="yes" />`,
			expected: map[string]string{
				"Id": "AgentToolExe", "Name": "notty-agent-tool.exe", "Source": "$(AgentToolExe)",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := bytes.Count(data, []byte(test.canonical)); got != 1 {
				t.Fatalf("got %d canonical %s file elements, want 1", got, test.id)
			}
			mutated := bytes.Replace(data, []byte(test.canonical), []byte(test.mutated), 1)
			var root wixElement
			if err := xml.Unmarshal(mutated, &root); err != nil {
				t.Fatal(err)
			}
			file, err := wixElementByID(root, "File", test.id)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkWixAttrs(file, test.expected); err == nil {
				t.Fatal("per-user file KeyPath mutation passed the exact WiX attribute contract")
			} else if !strings.Contains(err.Error(), "KeyPath") {
				t.Fatalf("file KeyPath mutation failed for the wrong reason: %v", err)
			}
		})
	}
}

func TestWindowsInstallerRejectsMissingProfileDirectoryCleanup(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range []struct {
		id        string
		directory string
	}{
		{id: "RemoveInstallFolder", directory: "INSTALLFOLDER"},
		{id: "RemovePerUserProgramsFolder", directory: "PerUserProgramsFolder"},
	} {
		t.Run(cleanup.id, func(t *testing.T) {
			row := []byte(fmt.Sprintf("            <RemoveFolder Id=\"%s\" Directory=\"%s\" On=\"uninstall\" />\n", cleanup.id, cleanup.directory))
			if got := bytes.Count(data, row); got != 1 {
				t.Fatalf("got %d %s rows, want 1", got, cleanup.id)
			}
			mutated := bytes.Replace(data, row, nil, 1)
			var root wixElement
			if err := xml.Unmarshal(mutated, &root); err != nil {
				t.Fatal(err)
			}
			if err := checkWixTopology(root); err == nil {
				t.Fatalf("deleting %s passed the exact WiX topology contract", cleanup.id)
			} else if !strings.Contains(err.Error(), cleanup.id) {
				t.Fatalf("deleting %s failed for the wrong reason: %v", cleanup.id, err)
			}
		})
	}
}

func TestWindowsInstallerRejectsRelocatedAgentComponent(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	agentComponent := []byte(`          <Component Id="AgentToolExecutable" Guid="` + agentToolExecutableComponentGUID + `">
            <File Id="AgentToolExe" Name="notty-agent-tool.exe" Source="$(AgentToolExe)" />
            <RegistryValue
              Root="HKCU"
              Key="Software\Codesk\Installer\Components"
              Name="AgentToolExecutable"
              Type="integer"
              Value="1"
              KeyPath="yes" />
          </Component>
`)
	if got := bytes.Count(data, agentComponent); got != 1 {
		t.Fatalf("got %d canonical agent component blocks, want 1", got)
	}
	mutated := bytes.Replace(data, agentComponent, nil, 1)
	menuDirectory := []byte("      <Directory Id=\"CodeskProgramsFolder\" Name=\"Codesk\" />\n")
	if got := bytes.Count(mutated, menuDirectory); got != 1 {
		t.Fatalf("got %d canonical menu directory elements, want 1", got)
	}
	relocatedMenuDirectory := append([]byte("      <Directory Id=\"CodeskProgramsFolder\" Name=\"Codesk\">\n"), agentComponent...)
	relocatedMenuDirectory = append(relocatedMenuDirectory, []byte("      </Directory>\n")...)
	mutated = bytes.Replace(mutated, menuDirectory, relocatedMenuDirectory, 1)

	var root wixElement
	if err := xml.Unmarshal(mutated, &root); err != nil {
		t.Fatal(err)
	}
	if err := checkWixTopology(root); err == nil {
		t.Fatal("relocated AgentToolExecutable component passed the exact WiX topology contract")
	} else if !strings.Contains(err.Error(), "AgentToolExecutable") {
		t.Fatalf("component-relocation mutation failed for the wrong reason: %v", err)
	}
}

func assertNoInstallerDirectives(t *testing.T, data []byte) {
	t.Helper()
	if err := checkInstallerDirectives(data); err != nil {
		t.Error(err)
	}
}

func checkInstallerDirectives(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scan WiX package tokens: %w", err)
		}
		switch token := token.(type) {
		case xml.Directive:
			return fmt.Errorf("installer directives are not allowed: %q", token)
		case xml.ProcInst:
			if token.Target != "xml" {
				return fmt.Errorf("installer processing instruction %q is not allowed", token.Target)
			}
		}
	}
}

func wixElements(root wixElement, name string) []wixElement {
	var result []wixElement
	if root.XMLName.Local == name {
		result = append(result, root)
	}
	for _, child := range root.Nodes {
		result = append(result, wixElements(child, name)...)
	}
	return result
}

func assertWixVocabulary(t *testing.T, root wixElement, allowed map[string]struct{}) {
	t.Helper()
	if root.XMLName.Space != wixNamespace {
		t.Errorf("element %s uses namespace %q", root.XMLName.Local, root.XMLName.Space)
	}
	if _, ok := allowed[root.XMLName.Local]; !ok {
		t.Errorf("installer element %s is outside the thin MSI vocabulary", root.XMLName.Local)
	}
	for _, child := range root.Nodes {
		assertWixVocabulary(t, child, allowed)
	}
}

func assertWixElementCounts(t *testing.T, root wixElement, expected map[string]int) {
	t.Helper()
	for name, count := range expected {
		if got := len(wixElements(root, name)); got != count {
			t.Fatalf("got %d %s elements, want %d", got, name, count)
		}
	}
}

func assertWixTopology(t *testing.T, root wixElement) {
	t.Helper()
	if err := checkWixTopology(root); err != nil {
		t.Error(err)
	}
}

func checkWixTopology(root wixElement) error {
	packageElement, err := wixDirectChild(root, "Package")
	if err != nil {
		return fmt.Errorf("Wix: %w", err)
	}
	for _, name := range []string{"MajorUpgrade", "MediaTemplate"} {
		if _, err := wixDirectChild(packageElement, name); err != nil {
			return fmt.Errorf("Package: %w", err)
		}
	}
	for _, child := range []struct {
		name string
		id   string
	}{
		{name: "Icon", id: "Codesk.ico"},
		{name: "Property", id: "ARPPRODUCTICON"},
		{name: "Feature", id: "CodeskFeature"},
	} {
		if _, err := wixDirectChildByID(packageElement, child.name, child.id); err != nil {
			return fmt.Errorf("Package: %w", err)
		}
	}

	localAppData, err := wixDirectChildByID(packageElement, "StandardDirectory", "LocalAppDataFolder")
	if err != nil {
		return fmt.Errorf("Package: %w", err)
	}
	perUserPrograms, err := wixDirectChildByID(localAppData, "Directory", "PerUserProgramsFolder")
	if err != nil {
		return fmt.Errorf("StandardDirectory[LocalAppDataFolder]: %w", err)
	}
	installFolder, err := wixDirectChildByID(perUserPrograms, "Directory", "INSTALLFOLDER")
	if err != nil {
		return fmt.Errorf("Directory[PerUserProgramsFolder]: %w", err)
	}
	codeskComponent, err := wixDirectChildByID(installFolder, "Component", "CodeskExecutable")
	if err != nil {
		return fmt.Errorf("Directory[INSTALLFOLDER]: %w", err)
	}
	agentComponent, err := wixDirectChildByID(installFolder, "Component", "AgentToolExecutable")
	if err != nil {
		return fmt.Errorf("Directory[INSTALLFOLDER]: %w", err)
	}
	codeskFile, err := wixDirectChildByID(codeskComponent, "File", "CodeskExe")
	if err != nil {
		return fmt.Errorf("Component[CodeskExecutable]: %w", err)
	}
	if _, err := wixDirectChildByID(codeskFile, "Shortcut", "CodeskStartMenuShortcut"); err != nil {
		return fmt.Errorf("File[CodeskExe]: %w", err)
	}
	if _, err := wixDirectChild(codeskComponent, "RegistryValue"); err != nil {
		return fmt.Errorf("Component[CodeskExecutable]: %w", err)
	}
	for _, id := range []string{"RemoveCodeskProgramsFolder", "RemoveInstallFolder", "RemovePerUserProgramsFolder"} {
		if _, err := wixDirectChildByID(codeskComponent, "RemoveFolder", id); err != nil {
			return fmt.Errorf("Component[CodeskExecutable]: %w", err)
		}
	}
	if _, err := wixDirectChildByID(agentComponent, "File", "AgentToolExe"); err != nil {
		return fmt.Errorf("Component[AgentToolExecutable]: %w", err)
	}
	if _, err := wixDirectChild(agentComponent, "RegistryValue"); err != nil {
		return fmt.Errorf("Component[AgentToolExecutable]: %w", err)
	}
	loginItemComponent, err := wixDirectChildByID(installFolder, "Component", "CodeskLoginItemCleanup")
	if err != nil {
		return fmt.Errorf("Directory[INSTALLFOLDER]: %w", err)
	}
	if _, err := wixDirectChild(loginItemComponent, "RegistryValue"); err != nil {
		return fmt.Errorf("Component[CodeskLoginItemCleanup]: %w", err)
	}

	programMenu, err := wixDirectChildByID(packageElement, "StandardDirectory", "ProgramMenuFolder")
	if err != nil {
		return fmt.Errorf("Package: %w", err)
	}
	if _, err := wixDirectChildByID(programMenu, "Directory", "CodeskProgramsFolder"); err != nil {
		return fmt.Errorf("StandardDirectory[ProgramMenuFolder]: %w", err)
	}
	feature, err := wixDirectChildByID(packageElement, "Feature", "CodeskFeature")
	if err != nil {
		return fmt.Errorf("Package: %w", err)
	}
	for _, id := range []string{"CodeskExecutable", "AgentToolExecutable", "CodeskLoginItemCleanup"} {
		if _, err := wixDirectChildByID(feature, "ComponentRef", id); err != nil {
			return fmt.Errorf("Feature[CodeskFeature]: %w", err)
		}
	}
	return nil
}

func wixDirectChild(parent wixElement, name string) (wixElement, error) {
	var matches []wixElement
	for _, child := range parent.Nodes {
		if child.XMLName.Local == name {
			matches = append(matches, child)
		}
	}
	if len(matches) != 1 {
		return wixElement{}, fmt.Errorf("got %d direct %s children, want 1", len(matches), name)
	}
	return matches[0], nil
}

func wixDirectChildByID(parent wixElement, name, id string) (wixElement, error) {
	var matches []wixElement
	for _, child := range parent.Nodes {
		if child.XMLName.Local == name && wixAttr(child, "Id") == id {
			matches = append(matches, child)
		}
	}
	if len(matches) != 1 {
		return wixElement{}, fmt.Errorf("got %d direct %s children with Id=%q, want 1", len(matches), name, id)
	}
	return matches[0], nil
}

func assertWixElementByID(t *testing.T, root wixElement, name, id string) wixElement {
	t.Helper()
	element, err := wixElementByID(root, name, id)
	if err != nil {
		t.Fatal(err)
	}
	return element
}

func wixElementByID(root wixElement, name, id string) (wixElement, error) {
	for _, candidate := range wixElements(root, name) {
		if wixAttr(candidate, "Id") == id {
			return candidate, nil
		}
	}
	return wixElement{}, fmt.Errorf("missing %s element with Id=%q", name, id)
}

func assertWixAttrs(t *testing.T, element wixElement, expected map[string]string) {
	t.Helper()
	if err := checkWixAttrs(element, expected); err != nil {
		t.Error(err)
	}
}

func checkWixAttrs(element wixElement, expected map[string]string) error {
	actual := make(map[string]string, len(element.Attrs))
	for _, attr := range element.Attrs {
		if isDefaultWixNamespaceDeclaration(element, attr) {
			continue
		}
		if attr.Name.Space != "" {
			return fmt.Errorf("%s has namespaced attribute {%s}%s", element.XMLName.Local, attr.Name.Space, attr.Name.Local)
		}
		if _, exists := actual[attr.Name.Local]; exists {
			return fmt.Errorf("%s has duplicate attribute %s", element.XMLName.Local, attr.Name.Local)
		}
		actual[attr.Name.Local] = attr.Value
	}
	for name, value := range actual {
		want, ok := expected[name]
		if !ok {
			return fmt.Errorf("%s has unexpected attribute %s=%q", element.XMLName.Local, name, value)
		}
		if value != want {
			return fmt.Errorf("%s[%s]: got %q, want %q", element.XMLName.Local, name, value, want)
		}
	}
	for name, value := range expected {
		if got, ok := actual[name]; !ok {
			return fmt.Errorf("%s is missing required attribute %s=%q", element.XMLName.Local, name, value)
		} else if got != value {
			return fmt.Errorf("%s[%s]: got %q, want %q", element.XMLName.Local, name, got, value)
		}
	}
	return nil
}

func isDefaultWixNamespaceDeclaration(element wixElement, attr xml.Attr) bool {
	if element.XMLName.Space != wixNamespace || attr.Value != wixNamespace {
		return false
	}
	return (attr.Name.Space == "" && attr.Name.Local == "xmlns") ||
		(attr.Name.Space == "xmlns" && (attr.Name.Local == "" || attr.Name.Local == "xmlns"))
}

func wixAttr(element wixElement, name string) string {
	for _, attr := range element.Attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func assertWixDefinitions(t *testing.T, source string, expected map[string]string) {
	t.Helper()
	actual := map[string]string{}
	for _, definition := range strings.Split(source, ";") {
		definition = strings.TrimSpace(definition)
		if definition == "" {
			continue
		}
		name, value, ok := strings.Cut(definition, "=")
		if !ok || strings.TrimSpace(name) == "" {
			t.Fatalf("invalid WiX definition %q", definition)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if _, exists := actual[name]; exists {
			t.Fatalf("duplicate WiX definition %q", name)
		}
		actual[name] = value
	}
	if len(actual) != len(expected) {
		t.Fatalf("WiX definitions = %#v, want %#v", actual, expected)
	}
	for name, value := range expected {
		if got, ok := actual[name]; !ok || got != value {
			t.Errorf("WiX definition %s = %q (present=%t), want %q", name, got, ok, value)
		}
	}
}
