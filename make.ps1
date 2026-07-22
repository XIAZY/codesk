[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [ValidateSet('macos-gui-build', 'macos-gui-deploy', 'windows-gui-build', 'windows-gui-deploy')]
    [string] $Target,

    [Parameter(Position = 1, ValueFromRemainingArguments = $true)]
    [string[]] $Assignments = @()
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-GitBashExecutable {
    $gitCommand = Get-Command 'git.exe' -ErrorAction SilentlyContinue
    if ($null -eq $gitCommand) {
        $gitCommand = Get-Command 'git' -ErrorAction SilentlyContinue
    }
    if ($null -eq $gitCommand) {
        throw 'windows-gui-deploy requires Git for Windows'
    }

    $directory = Split-Path -Parent $gitCommand.Source
    for ($depth = 0; $depth -lt 4 -and -not [string]::IsNullOrWhiteSpace($directory); $depth++) {
        foreach ($relative in @('bin/bash.exe', 'usr/bin/bash.exe')) {
            $candidate = Join-Path $directory $relative
            if (Test-Path -LiteralPath $candidate -PathType Leaf) {
                return $candidate
            }
        }
        $parent = Split-Path -Parent $directory
        if ($parent -ceq $directory) {
            break
        }
        $directory = $parent
    }
    throw 'windows-gui-deploy requires a Git for Windows Bash executable'
}

function Invoke-WindowsGuiUpload {
    param(
        [Parameter(Mandatory = $true)] [string] $RepositoryRoot,
        [Parameter(Mandatory = $true)] [hashtable] $ResolvedSettings
    )

    $bash = Resolve-GitBashExecutable
    $script = (Join-Path $RepositoryRoot 'scripts/upload-r2.sh').Replace('\', '/')
    $previousTarget = [Environment]::GetEnvironmentVariable('UPLOAD_TARGET', 'Process')
    $previousMsiRoot = [Environment]::GetEnvironmentVariable('WINDOWS_GUI_MSI_ROOT', 'Process')
    $previousRepository = [Environment]::GetEnvironmentVariable('WINDOWS_GUI_REPOSITORY', 'Process')
    try {
        $env:UPLOAD_TARGET = 'windows-gui'
        if ($ResolvedSettings.ContainsKey('WINDOWS_GUI_MSI_ROOT')) {
            $env:WINDOWS_GUI_MSI_ROOT = $ResolvedSettings['WINDOWS_GUI_MSI_ROOT']
        }
        if ($ResolvedSettings.ContainsKey('WINDOWS_GUI_REPOSITORY')) {
            $env:WINDOWS_GUI_REPOSITORY = $ResolvedSettings['WINDOWS_GUI_REPOSITORY']
        }
        & $bash $script
        if ($LASTEXITCODE -ne 0) {
            throw "Windows GUI R2 upload failed with exit $LASTEXITCODE"
        }
    } finally {
        [Environment]::SetEnvironmentVariable('UPLOAD_TARGET', $previousTarget, 'Process')
        [Environment]::SetEnvironmentVariable('WINDOWS_GUI_MSI_ROOT', $previousMsiRoot, 'Process')
        [Environment]::SetEnvironmentVariable('WINDOWS_GUI_REPOSITORY', $previousRepository, 'Process')
    }
}

$supportedSettings = @(
    'WINDOWS_GUI_ROOT',
    'WINDOWS_GUI_PAYLOAD_ROOT',
    'WINDOWS_GUI_TEST_DIR',
    'WINDOWS_GUI_MSI_ROOT',
    'WINDOWS_GUI_REPOSITORY',
    'WINDOWS_GUI_BUILDER_IMAGE',
    'WINDOWS_GUI_ZIG_VERSION'
)
$settings = @{}
$assigned = @{}
foreach ($name in $supportedSettings) {
    $value = [Environment]::GetEnvironmentVariable($name, 'Process')
    if ($null -ne $value) {
        $settings[$name] = $value
    }
}

foreach ($assignment in $Assignments) {
    if ($assignment -cnotmatch '^([A-Z][A-Z0-9_]*)=(.*)$') {
        throw "invalid assignment: $assignment"
    }
    $name = $Matches[1]
    $value = $Matches[2]
    if ($supportedSettings -cnotcontains $name) {
        throw "unsupported GUI setting: $name"
    }
    if ($assigned.ContainsKey($name)) {
        throw "duplicate GUI setting: $name"
    }
    $assigned[$name] = $true
    $settings[$name] = $value
}

switch ($Target) {
    'macos-gui-build' {
        throw 'macos-gui-build requires a real macOS host; no GUI was built'
    }
    'macos-gui-deploy' {
        throw 'macos-gui-deploy requires a real macOS host; no GUI was deployed'
    }
}

$null = & (Join-Path $PSScriptRoot 'scripts/read-version.ps1')

$parameters = @{
    Target = $Target
    RepositoryRoot = $PSScriptRoot
}
$mapping = @{
    'WINDOWS_GUI_ROOT' = 'WindowsRoot'
    'WINDOWS_GUI_PAYLOAD_ROOT' = 'PayloadRoot'
    'WINDOWS_GUI_TEST_DIR' = 'TestRoot'
    'WINDOWS_GUI_MSI_ROOT' = 'MsiRoot'
    'WINDOWS_GUI_REPOSITORY' = 'Repository'
    'WINDOWS_GUI_BUILDER_IMAGE' = 'BuilderImage'
    'WINDOWS_GUI_ZIG_VERSION' = 'ZigVersion'
}
foreach ($name in $mapping.Keys) {
    if ($settings.ContainsKey($name)) {
        $parameters[$mapping[$name]] = $settings[$name]
    }
}

& (Join-Path $PSScriptRoot 'scripts/run-windows-gui-container.ps1') @parameters
if ($Target -ceq 'windows-gui-deploy') {
    Invoke-WindowsGuiUpload -RepositoryRoot $PSScriptRoot -ResolvedSettings $settings
}
