[CmdletBinding()]
param()

Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"

$daemonDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$configPath = Join-Path $daemonDir "daemon.env.json"
$logPath = Join-Path $daemonDir "daemon.log"
$pidPath = Join-Path $daemonDir "launcher.pid"

if (-not (Test-Path -LiteralPath $configPath -PathType Leaf)) {
    throw "Codesk daemon config is missing: $configPath"
}

$config = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json
$requiredProperties = @(
    "NOTTY_BACKEND_URL",
    "NOTTY_WORKSPACE_ID",
    "NOTTY_DAEMON_TOKEN",
    "NOTTY_DAEMON_VERSION",
    "NOTTY_DATA_DIR",
    "NOTTY_WORKSPACE_DIR",
    "NOTTY_AGENT_WORKSPACE_ROOT",
    "NOTTY_CODEX_COMMAND",
    "NOTTY_CLAUDE_COMMAND",
    "NOTTY_TOOL_DIR_CODEX",
    "NOTTY_TOOL_DIR_CLAUDE",
    "NOTTY_BINARY_DIR"
)

foreach ($property in $requiredProperties) {
    if ($config.PSObject.Properties.Name -notcontains $property) {
        throw "Codesk daemon config is missing $property"
    }
    [Environment]::SetEnvironmentVariable($property, [string]$config.$property, "Process")
}

function Join-PathIfPresent {
    param(
        [string]$Base,
        [string]$Child
    )

    if (-not [string]::IsNullOrWhiteSpace($Base)) {
        Join-Path $Base $Child
    }
}

function Resolve-CommandPath {
    param([string]$Command)

    if ([string]::IsNullOrWhiteSpace($Command)) {
        return "not-found"
    }

    $resolved = Get-Command -Name $Command -CommandType Application, ExternalScript -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($null -eq $resolved) {
        return "not-found"
    }
    if (-not [string]::IsNullOrWhiteSpace($resolved.Source)) {
        return $resolved.Source
    }
    return $resolved.Definition
}

$userProfile = $env:USERPROFILE
$appData = [Environment]::GetFolderPath([Environment+SpecialFolder]::ApplicationData)
$localAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
$programFiles = $env:ProgramFiles
$programFilesX86 = ${env:ProgramFiles(x86)}
$systemRoot = $env:SystemRoot

$pathCandidates = @(
    [string]$config.NOTTY_BINARY_DIR,
    [string]$config.NOTTY_TOOL_DIR_CODEX,
    [string]$config.NOTTY_TOOL_DIR_CLAUDE,
    (Join-PathIfPresent $userProfile ".local\bin"),
    (Join-PathIfPresent $appData "npm"),
    (Join-PathIfPresent $localAppData "Microsoft\WindowsApps"),
    (Join-PathIfPresent $programFiles "nodejs"),
    (Join-PathIfPresent $programFilesX86 "nodejs"),
    (Join-PathIfPresent $systemRoot "System32"),
    $systemRoot,
    (Join-PathIfPresent $systemRoot "System32\Wbem"),
    (Join-PathIfPresent $systemRoot "System32\WindowsPowerShell\v1.0")
)

$seenPaths = @{}
$pathEntries = New-Object System.Collections.Generic.List[string]
foreach ($candidate in $pathCandidates) {
    if ([string]::IsNullOrWhiteSpace($candidate)) {
        continue
    }
    if (-not (Test-Path -LiteralPath $candidate -PathType Container)) {
        continue
    }
    $fullPath = [IO.Path]::GetFullPath($candidate).TrimEnd("\")
    $key = $fullPath.ToLowerInvariant()
    if (-not $seenPaths.ContainsKey($key)) {
        $seenPaths[$key] = $true
        $pathEntries.Add($fullPath)
    }
}
$env:Path = $pathEntries -join ";"

$daemonPath = Join-Path ([string]$config.NOTTY_BINARY_DIR) "notty-daemon.exe"
if (-not (Test-Path -LiteralPath $daemonPath -PathType Leaf)) {
    throw "Codesk daemon executable is missing: $daemonPath"
}

$daemonName = ([string]$config.NOTTY_WORKSPACE_ID -replace "[^A-Za-z0-9_.-]", "-")
if ([string]::IsNullOrWhiteSpace($daemonName)) {
    $daemonName = "workspace"
}
$mutexName = "Local\CodeskDaemon_$daemonName"
$createdNew = $false
$mutex = [Threading.Mutex]::new($true, $mutexName, [ref]$createdNew)
if (-not $createdNew) {
    $mutex.Dispose()
    exit 0
}

try {
    Set-Content -LiteralPath $pidPath -Value $PID -Encoding ASCII
    $codexPath = Resolve-CommandPath ([string]$config.NOTTY_CODEX_COMMAND)
    $claudePath = Resolve-CommandPath ([string]$config.NOTTY_CLAUDE_COMMAND)
    Add-Content -LiteralPath $logPath -Encoding UTF8 -Value (
        "{0:o} Codesk daemon launcher start: PATH={1}" -f [DateTime]::UtcNow, $env:Path
    )
    Add-Content -LiteralPath $logPath -Encoding UTF8 -Value (
        "{0:o} Codesk daemon launcher runtimes: codex={1} claude={2}" -f [DateTime]::UtcNow, $codexPath, $claudePath
    )

    while ($true) {
        $exitCode = 1
        try {
            & $daemonPath *>> $logPath
            $exitCode = $LASTEXITCODE
        } catch {
            Add-Content -LiteralPath $logPath -Encoding UTF8 -Value (
                "{0:o} Codesk daemon launch failed: {1}" -f [DateTime]::UtcNow, $_.Exception.Message
            )
        }
        Add-Content -LiteralPath $logPath -Encoding UTF8 -Value (
            "{0:o} Codesk daemon exited with code {1}; restarting in 5 seconds" -f [DateTime]::UtcNow, $exitCode
        )
        Start-Sleep -Seconds 5
    }
} finally {
    Remove-Item -LiteralPath $pidPath -Force -ErrorAction SilentlyContinue
    try {
        $mutex.ReleaseMutex()
    } catch {
        # The process may be terminating after Task Scheduler ended it.
    }
    $mutex.Dispose()
}
