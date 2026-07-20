[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [ValidateSet('macos-gui-build', 'macos-gui-release', 'build-windows-builder-image', 'windows-gui-build', 'windows-gui-release')]
    [string] $Target,

    [Parameter(Position = 1, ValueFromRemainingArguments = $true)]
    [string[]] $Assignments = @()
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$supportedSettings = @(
    'GUI_VERSION',
    'WINDOWS_GUI_ARCHES',
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
    'macos-gui-release' {
        throw 'macos-gui-release requires a real macOS host; no release was built'
    }
}

$parameters = @{
    Target = $Target
    RepositoryRoot = $PSScriptRoot
}
$mapping = @{
    'GUI_VERSION' = 'Version'
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

if ($Target -ceq 'build-windows-builder-image') {
    $builderParameters = @{
        RepositoryRoot = $PSScriptRoot
    }
    if ($parameters.ContainsKey('BuilderImage')) {
        $builderParameters['BuilderImage'] = $parameters['BuilderImage']
    }
    & (Join-Path $PSScriptRoot 'scripts/build-windows-gui-builder-image.ps1') @builderParameters
    return
}

if (-not $settings.ContainsKey('GUI_VERSION')) {
    $versionFile = Join-Path $PSScriptRoot 'VERSION'
    if (Test-Path -LiteralPath $versionFile -PathType Leaf) {
        $settings['GUI_VERSION'] = (Get-Content -LiteralPath $versionFile -TotalCount 1).Trim()
        $parameters['Version'] = $settings['GUI_VERSION']
    }
}

if ($Target -ceq 'windows-gui-release' -and $settings.ContainsKey('WINDOWS_GUI_ARCHES')) {
    $parameters['Architectures'] = $settings['WINDOWS_GUI_ARCHES']
}

& (Join-Path $PSScriptRoot 'scripts/run-windows-gui-container.ps1') @parameters
