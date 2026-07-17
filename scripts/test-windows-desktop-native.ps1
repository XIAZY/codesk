[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidateSet('Prepare', 'Resume')][string]$Phase,
    [Parameter(Mandatory = $true)][string]$SourceRevision,
    [Parameter(Mandatory = $true)][string]$RunnerSourceRevision,
    [Parameter(Mandatory = $true)][string]$PreviousReleaseDir,
    [Parameter(Mandatory = $true)][string]$PreviousVersion,
    [Parameter(Mandatory = $true)][string]$CandidateReleaseDir,
    [Parameter(Mandatory = $true)][string]$CandidateVersion,
    [Parameter(Mandatory = $true)][string]$EvidenceDir,
    [string]$RunnerPath,
    [ValidateRange(30, 3600)][int]$TimeoutSeconds = 300,
    [switch]$AllowUnsignedFunctional,
    [switch]$Destructive
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not $Destructive) {
    throw 'Use -Destructive only from a dedicated Windows test account after reviewing the install, crash, and uninstall actions.'
}
if ($SourceRevision -cnotmatch '^[0-9a-f]{40}$' -or $SourceRevision -eq '0000000000000000000000000000000000000000') {
    throw 'SourceRevision must be the exact nonzero lowercase 40-character product Git revision.'
}
if ($RunnerSourceRevision -cnotmatch '^[0-9a-f]{40}$' -or $RunnerSourceRevision -eq '0000000000000000000000000000000000000000') {
    throw 'RunnerSourceRevision must be the exact nonzero lowercase 40-character suite Git revision.'
}

$nativeArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
switch ($nativeArchitecture) {
    'x64' { $releaseArchitecture = 'amd64' }
    'arm64' { $releaseArchitecture = 'arm64' }
    default { throw "Native Windows AMD64 or ARM64 is required; found $nativeArchitecture." }
}

$candidateRoot = [IO.Path]::GetFullPath($CandidateReleaseDir)
$evidenceRoot = [IO.Path]::GetFullPath($EvidenceDir)
if ([string]::IsNullOrWhiteSpace($RunnerPath)) {
    $RunnerPath = Join-Path $PSScriptRoot "CodeskAcceptance_${CandidateVersion}_windows_${releaseArchitecture}.exe"
}
$RunnerPath = [IO.Path]::GetFullPath($RunnerPath)
if (-not (Test-Path -LiteralPath $RunnerPath -PathType Leaf)) {
    throw "The native acceptance runner is absent: $RunnerPath"
}
$runnerSHA256 = (Get-FileHash -LiteralPath $RunnerPath -Algorithm SHA256).Hash.ToLowerInvariant()

$arguments = @(
    '--phase', $Phase.ToLowerInvariant(),
    '--source-revision', $SourceRevision,
    '--previous-dir', [IO.Path]::GetFullPath($PreviousReleaseDir),
    '--previous-version', $PreviousVersion,
    '--candidate-dir', $candidateRoot,
    '--candidate-version', $CandidateVersion,
    '--evidence-dir', $evidenceRoot,
    '--timeout', "${TimeoutSeconds}s",
    '--destructive'
)
if ($AllowUnsignedFunctional) {
    $arguments += '--allow-unsigned-functional'
}

Write-Host "Codesk native acceptance: architecture=$releaseArchitecture runner=$RunnerPath"
& $RunnerPath @arguments
if ($LASTEXITCODE -ne 0) {
    throw "Codesk native acceptance exited $LASTEXITCODE. Inspect the redacted evidence bundle at $evidenceRoot."
}

$reportPath = Join-Path $evidenceRoot 'report.json'
if (-not (Test-Path -LiteralPath $reportPath -PathType Leaf)) {
    throw "Codesk native acceptance did not create $reportPath."
}
$report = Get-Content -LiteralPath $reportPath -Raw | ConvertFrom-Json
if ($report.schema_version -ne 1 -or $report.source_revision -ne $SourceRevision -or
    $report.runner_source_revision -ne $RunnerSourceRevision -or
    $report.runner_sha256 -ne $runnerSHA256 -or $report.host.platform -ne 'windows' -or
    $report.host.architecture -ne $releaseArchitecture -or
    $report.candidate.version -ne $CandidateVersion -or
    $report.candidate.source_revision -ne $SourceRevision -or
    $report.previous.version -ne $PreviousVersion) {
    throw "Acceptance report identity mismatch: schema=$($report.schema_version) product=$($report.source_revision) runner=$($report.runner_source_revision)/$($report.runner_sha256) host=$($report.host.platform)/$($report.host.architecture) releases=$($report.previous.version)->$($report.candidate.version)."
}
if (-not $AllowUnsignedFunctional -and -not $report.publishable) {
    throw 'Signed native acceptance completed without a publishable report.'
}
$lifecyclePath = Join-Path $evidenceRoot 'msi-lifecycle.json'
if (-not (Test-Path -LiteralPath $lifecyclePath -PathType Leaf)) {
    throw "Codesk native acceptance did not create $lifecyclePath."
}
$lifecycle = Get-Content -LiteralPath $lifecyclePath -Raw | ConvertFrom-Json
if ($lifecycle.schema_version -ne 1 -or $lifecycle.status -ne 'PASS' -or
    $lifecycle.source_revision -ne $SourceRevision -or
    $lifecycle.runner_source_revision -ne $RunnerSourceRevision -or
    $lifecycle.previous.version -ne $PreviousVersion -or
    $lifecycle.candidate.version -ne $CandidateVersion -or
    $lifecycle.candidate.source_revision -ne $SourceRevision -or
    $lifecycle.host_architecture -ne $releaseArchitecture) {
    throw "MSI lifecycle identity/status mismatch: schema=$($lifecycle.schema_version) status=$($lifecycle.status) product=$($lifecycle.source_revision) runner=$($lifecycle.runner_source_revision) host=$($lifecycle.host_architecture) releases=$($lifecycle.previous.version)->$($lifecycle.candidate.version)."
}
$requiredRows = @(
    'clean-baseline',
    'shared-run-key-sibling-fixture',
    'fresh-install-disabled-sentinel',
    'repair-preserves-disabled',
    'repair-preserves-enabled',
    'uninstall-preserves-sibling-and-shared-key',
    'major-upgrade-preserves-disabled',
    'major-upgrade-preserves-enabled',
    'x64-to-arm64-handoff',
    'lifecycle-cleanup'
)
if (@($lifecycle.rows).Count -ne $requiredRows.Count) {
    throw "MSI lifecycle report contains $(@($lifecycle.rows).Count) rows; expected exactly $($requiredRows.Count)."
}
foreach ($rowName in $requiredRows) {
    $rows = @($lifecycle.rows | Where-Object { $_.name -eq $rowName })
    if ($rows.Count -ne 1) {
        throw "MSI lifecycle row $rowName appears $($rows.Count) times; expected exactly one."
    }
    $expectedStatus = 'PASS'
    if ($rowName -eq 'x64-to-arm64-handoff' -and $releaseArchitecture -eq 'amd64') {
        $expectedStatus = 'WAIVED'
    }
    if ($rows[0].status -ne $expectedStatus) {
        throw "MSI lifecycle row $rowName is $($rows[0].status); expected $expectedStatus."
    }
}
if ($Phase -eq 'Prepare') {
    if ($report.phase -ne 'prepare' -or $report.verdict -ne 'PENDING') {
        throw "Prepare phase returned unexpected phase/verdict $($report.phase)/$($report.verdict)."
    }
    Write-Host 'ACTION REQUIRED: sign out of Windows and sign back in without manually launching Codesk.'
    Write-Host 'Then rerun this command with -Phase Resume and every other input unchanged.'
    return
}
if ($report.phase -ne 'resume' -or $report.verdict -ne 'PASS') {
    throw "Resume phase returned unexpected phase/verdict $($report.phase)/$($report.verdict)."
}
Write-Host "Codesk native acceptance PASS: report=$reportPath MSI-lifecycle=$lifecyclePath"
