[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('windows-gui-build', 'windows-gui-release')]
    [string] $Target,

    [AllowNull()]
    [AllowEmptyString()]
    [object] $Architectures = $null,
    [string] $RepositoryRoot = '',
    [string] $WindowsRoot = 'dist/windows-gui',
    [string] $PayloadRoot = 'dist/windows-gui/payload',
    [string] $TestRoot = 'dist/windows-gui/tests',
    [string] $MsiRoot = 'dist/windows-gui/msi',
    [string] $Repository = 'XIAZY/notty',
    [string] $ZigVersion = '0.16.0'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-RepositoryPath {
    param(
        [Parameter(Mandatory = $true)] [string] $Path,
        [Parameter(Mandatory = $true)] [string] $Root
    )

    if ([System.IO.Path]::IsPathRooted($Path)) {
        return [System.IO.Path]::GetFullPath($Path)
    }
    return [System.IO.Path]::GetFullPath((Join-Path $Root $Path))
}

function Resolve-GitBashExecutable {
    $gitCommand = Get-Command 'git.exe' -ErrorAction SilentlyContinue
    if ($null -eq $gitCommand) {
        $gitCommand = Get-Command 'git' -ErrorAction SilentlyContinue
    }
    if ($null -eq $gitCommand) {
        throw 'windows-gui targets require Git for Windows'
    }

    $directory = Split-Path -Parent $gitCommand.Source
    for ($depth = 0; $depth -lt 4 -and -not [string]::IsNullOrWhiteSpace($directory); $depth++) {
        foreach ($relative in @('bin/bash.exe', 'usr/bin/bash.exe', 'usr/bin/sh.exe')) {
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
    throw 'windows-gui targets require a Git for Windows POSIX shell'
}

function Get-WindowsHostArchitecture {
    $hostArchitecture = $null
    try {
        $hostArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    } catch {
        $hostArchitecture = $null
    }
    if ([string]::IsNullOrWhiteSpace($hostArchitecture)) {
        $hostArchitecture = [Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITEW6432', 'Process')
    }
    if ([string]::IsNullOrWhiteSpace($hostArchitecture)) {
        $hostArchitecture = [Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITECTURE', 'Process')
    }
    if ([string]::IsNullOrWhiteSpace($hostArchitecture)) {
        throw 'could not resolve the Windows host architecture'
    }

    switch ($hostArchitecture.ToUpperInvariant()) {
        'AMD64' { return 'amd64' }
        'X64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { throw "unsupported Windows host architecture: $hostArchitecture" }
    }
}

function Get-NormalizedArchitectures {
    param(
        [AllowNull()]
        [AllowEmptyString()]
        [object] $Value
    )

    $hostArchitecture = $null
    if ($Target -ceq 'windows-gui-build') {
        $hostArchitecture = Get-WindowsHostArchitecture
    }
    if ($null -eq $Value) {
        if ($Target -ceq 'windows-gui-build') {
            $Value = $hostArchitecture
        } else {
            $Value = 'amd64 arm64'
        }
    } elseif ([string]::IsNullOrWhiteSpace($Value)) {
        throw 'Windows GUI architecture list must not be empty'
    }

    [string[]] $result = @()
    foreach ($architecture in [regex]::Split($Value.Trim(), '\s+')) {
        if ($architecture -cne 'amd64' -and $architecture -cne 'arm64') {
            throw "unsupported Windows GUI architecture: $architecture"
        }
        if ($result -ccontains $architecture) {
            throw "duplicate Windows GUI architecture: $architecture"
        }
        $result += $architecture
    }
    if ($Target -ceq 'windows-gui-build' -and $result.Count -ne 1) {
        throw 'windows-gui-build requires exactly one host architecture'
    }
    if ($Target -ceq 'windows-gui-build' -and $result[0] -cne $hostArchitecture) {
        throw "windows-gui-build architecture $($result[0]) does not match host $hostArchitecture"
    }
    return $result
}

function Assert-ExactRealFiles {
    param(
        [Parameter(Mandatory = $true)] [string] $Directory,
        [Parameter(Mandatory = $true)] [string[]] $Names,
        [Parameter(Mandatory = $true)] [string] $Label
    )

    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
        throw "$Label directory is missing: $Directory"
    }
    $items = @(Get-ChildItem -LiteralPath $Directory -Force)
    [string[]] $actual = @($items | ForEach-Object { $_.Name })
    [string[]] $expected = $Names.Clone()
    [Array]::Sort($actual, [System.StringComparer]::Ordinal)
    [Array]::Sort($expected, [System.StringComparer]::Ordinal)
    if ($actual.Count -ne $expected.Count) {
        throw "$Label inventory mismatch: $($actual -join ', ')"
    }
    for ($index = 0; $index -lt $expected.Count; $index++) {
        if ($actual[$index] -cne $expected[$index]) {
            throw "$Label inventory mismatch: $($actual -join ', ')"
        }
    }
    foreach ($item in $items) {
        if ($item.PSIsContainer -or
            (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) -or
            $item.Length -le 0) {
            throw "$Label artifact is not a nonempty real file: $($item.FullName)"
        }
    }
}

function Assert-ExactArchitectureDirectories {
    param(
        [Parameter(Mandatory = $true)] [string] $Directory,
        [Parameter(Mandatory = $true)] [string[]] $Architectures
    )

    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
        throw "payload root is missing: $Directory"
    }
    $items = @(Get-ChildItem -LiteralPath $Directory -Force)
    [string[]] $actual = @($items | ForEach-Object { $_.Name })
    [string[]] $expected = $Architectures.Clone()
    [Array]::Sort($actual, [System.StringComparer]::Ordinal)
    [Array]::Sort($expected, [System.StringComparer]::Ordinal)
    if ($actual.Count -ne $expected.Count) {
        throw "payload architecture inventory mismatch: $($actual -join ', ')"
    }
    for ($index = 0; $index -lt $expected.Count; $index++) {
        if ($actual[$index] -cne $expected[$index]) {
            throw "payload architecture inventory mismatch: $($actual -join ', ')"
        }
    }
    foreach ($item in $items) {
        if (-not $item.PSIsContainer -or
            (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) {
            throw "payload architecture entry is not a real directory: $($item.FullName)"
        }
    }
}

function Invoke-WindowsPayloadBuild {
    param(
        [Parameter(Mandatory = $true)] [string[]] $SelectedArchitectures,
        [Parameter(Mandatory = $true)] [string] $Root,
        [Parameter(Mandatory = $true)] [string] $SafeParent,
        [Parameter(Mandatory = $true)] [string] $PayloadDirectory,
        [Parameter(Mandatory = $true)] [string] $TestDirectory
    )

    $bash = Resolve-GitBashExecutable
    $script = (Join-Path $Root 'scripts/build-windows-desktop-payloads.sh').Replace('\', '/')
    $payloadArgument = $PayloadDirectory.Replace('\', '/')
    $testArgument = $TestDirectory.Replace('\', '/')
    $previous = @{}
    foreach ($name in @('WINDOWS_GUI_ARCHES', 'WINDOWS_GUI_ZIG_VERSION', 'WINDOWS_GUI_SAFE_PARENT_DIRECTORY')) {
        $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    try {
        $env:WINDOWS_GUI_ARCHES = $SelectedArchitectures -join ' '
        $env:WINDOWS_GUI_ZIG_VERSION = $ZigVersion
        $env:WINDOWS_GUI_SAFE_PARENT_DIRECTORY = $SafeParent.Replace('\', '/')
        & $bash $script $payloadArgument $testArgument
        if ($LASTEXITCODE -ne 0) {
            throw "Windows GUI payload builder failed with exit $LASTEXITCODE"
        }
    } finally {
        foreach ($name in $previous.Keys) {
            [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
        }
    }

    Assert-ExactArchitectureDirectories -Directory $PayloadDirectory -Architectures $SelectedArchitectures
    [string[]] $testNames = @()
    foreach ($architecture in $SelectedArchitectures) {
        $payloadParameters = @{
            Directory = Join-Path $PayloadDirectory $architecture
            Names = @('Codesk.exe', 'notty-agent-tool.exe')
            Label = "$architecture payload"
        }
        Assert-ExactRealFiles @payloadParameters
        $testNames += "notty-syncer-$architecture.test.exe"
        $testNames += "codesk-desktop-$architecture.test.exe"
    }
    Assert-ExactRealFiles -Directory $TestDirectory -Names $testNames -Label 'compiled test'
}

function Invoke-WindowsMsiRelease {
    param(
        [Parameter(Mandatory = $true)] [string[]] $SelectedArchitectures,
        [Parameter(Mandatory = $true)] [string] $Root,
        [Parameter(Mandatory = $true)] [string] $PayloadDirectory,
        [Parameter(Mandatory = $true)] [string] $MsiDirectory
    )

    $head = (& git -C $Root rev-parse --verify HEAD).Trim()
    if ($LASTEXITCODE -ne 0 -or $head -cnotmatch '^[0-9a-f]{40}$') {
        throw 'windows-gui-release could not resolve the source checkout HEAD'
    }
    $base = (& git -C $Root rev-parse --verify 'HEAD^1').Trim()
    if ($LASTEXITCODE -ne 0 -or $base -cnotmatch '^[0-9a-f]{40}$') {
        throw 'windows-gui-release could not resolve the source checkout parent'
    }

    [System.IO.Directory]::CreateDirectory($MsiDirectory) | Out-Null
    $builder = Join-Path $Root 'scripts/build-windows-desktop-msi-artifact.ps1'
    $icon = Join-Path $Root 'daemon/cmd/codesk-desktop/assets/codesk.ico'
    $provenanceVariables = @(
        'GITHUB_REPOSITORY', 'GITHUB_WORKFLOW_REF', 'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT',
        'RUNNER_NAME', 'RUNNER_OS', 'RUNNER_ARCH'
    )
    $previous = @{}
    foreach ($name in $provenanceVariables) {
        $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    try {
        $env:GITHUB_REPOSITORY = $Repository
        $env:GITHUB_WORKFLOW_REF = "local/scripts/run-windows-gui-target.ps1@$head"
        $env:GITHUB_RUN_ID = 'local'
        $env:GITHUB_RUN_ATTEMPT = '1'
        $env:RUNNER_NAME = 'local'
        $env:RUNNER_OS = 'Windows'
        foreach ($architecture in $SelectedArchitectures) {
            switch ($architecture) {
                'amd64' {
                    $runnerArchitecture = 'AMD64'
                    $installerPlatform = 'x64'
                }
                'arm64' {
                    $runnerArchitecture = 'ARM64'
                    $installerPlatform = 'arm64'
                }
            }
            $env:RUNNER_ARCH = $runnerArchitecture
            $outputDirectory = Join-Path $MsiDirectory $architecture
            $workingDirectory = Join-Path $MsiDirectory ".work-$architecture"
            $parameters = @{
                Architecture = $runnerArchitecture
                GoArchitecture = $architecture
                InstallerPlatform = $installerPlatform
                Release = $true
                CodeskExe = Join-Path (Join-Path $PayloadDirectory $architecture) 'Codesk.exe'
                AgentToolExe = Join-Path (Join-Path $PayloadDirectory $architecture) 'notty-agent-tool.exe'
                CodeskIcon = $icon
                SourceEvent = 'push'
                SourceCheckoutCommit = $head
                SourceHead = $head
                SourceBase = $base
                SafeParentDirectory = $MsiDirectory
                OutputDirectory = $outputDirectory
                WorkingDirectory = $workingDirectory
            }
            & $builder @parameters
            $releaseParameters = @{
                Directory = $outputDirectory
                Names = @("Codesk_${version}_windows_$architecture.msi", 'SHA256SUMS', 'provenance.json')
                Label = "$architecture release"
            }
            Assert-ExactRealFiles @releaseParameters
        }
    } finally {
        foreach ($name in $previous.Keys) {
            [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
        }
    }
}

if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw "$Target requires a real Windows host; no Windows GUI output was built"
}

if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $RepositoryRoot = Split-Path -Parent $PSScriptRoot
}
$root = Resolve-RepositoryPath -Path $RepositoryRoot -Root (Get-Location).Path
$version = & (Join-Path $root 'scripts/read-version.ps1')
$windowsDirectory = Resolve-RepositoryPath -Path $WindowsRoot -Root $root
$payloadDirectory = Resolve-RepositoryPath -Path $PayloadRoot -Root $root
$testDirectory = Resolve-RepositoryPath -Path $TestRoot -Root $root
$msiDirectory = Resolve-RepositoryPath -Path $MsiRoot -Root $root
[string[]] $selectedArchitectures = @(Get-NormalizedArchitectures -Value $Architectures)

$payloadParameters = @{
    SelectedArchitectures = $selectedArchitectures
    Root = $root
    SafeParent = $windowsDirectory
    PayloadDirectory = $payloadDirectory
    TestDirectory = $testDirectory
}
Invoke-WindowsPayloadBuild @payloadParameters

if ($Target -ceq 'windows-gui-release') {
    $releaseParameters = @{
        SelectedArchitectures = $selectedArchitectures
        Root = $root
        PayloadDirectory = $payloadDirectory
        MsiDirectory = $msiDirectory
    }
    Invoke-WindowsMsiRelease @releaseParameters
}
