[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidateSet('Prepare', 'Resume')][string]$Phase,
    [Parameter(Mandatory = $true)][string]$SourceRevision,
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
