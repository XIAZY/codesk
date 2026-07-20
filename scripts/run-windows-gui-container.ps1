[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('windows-gui-build', 'windows-gui-release')]
    [string] $Target,

    [string] $Version = 'dev',
    [AllowNull()]
    [AllowEmptyString()]
    [object] $Architectures = $null,
    [string] $RepositoryRoot = '',
    [string] $WindowsRoot = 'dist/windows-gui',
    [string] $PayloadRoot = 'dist/windows-gui/payload',
    [string] $TestRoot = 'dist/windows-gui/tests',
    [string] $MsiRoot = 'dist/windows-gui/msi',
    [string] $Repository = 'XIAZY/notty',
    [string] $ZigVersion = '0.16.0',
    [string] $BuilderImage = 'alphatoad/notty:windows-builder'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Invoke-Docker {
    param(
        [Parameter(Mandatory = $true)] [string[]] $Arguments,
        [Parameter(Mandatory = $true)] [string] $Description
    )

    & docker @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Description failed with exit $LASTEXITCODE"
    }
}

function Get-NormalizedWindowsArchitecture {
    param(
        [Parameter(Mandatory = $true)] [string] $Value,
        [Parameter(Mandatory = $true)] [string] $Label
    )

    switch ($Value.ToLowerInvariant()) {
        'amd64' { return 'amd64' }
        'x64' { return 'amd64' }
        'x86_64' { return 'amd64' }
        'arm64' { return 'arm64' }
        'aarch64' { return 'arm64' }
        default { throw "unsupported $Label architecture: $Value" }
    }
}

function ConvertTo-ContainerRepositoryPath {
    param(
        [Parameter(Mandatory = $true)] [string] $Path,
        [Parameter(Mandatory = $true)] [string] $Root,
        [Parameter(Mandatory = $true)] [string] $Label
    )

    $fullPath = if ([System.IO.Path]::IsPathRooted($Path)) {
        [System.IO.Path]::GetFullPath($Path)
    } else {
        [System.IO.Path]::GetFullPath((Join-Path $Root $Path))
    }
    $separator = [string] [System.IO.Path]::DirectorySeparatorChar
    $rootPrefix = $Root
    if (-not $rootPrefix.EndsWith($separator)) {
        $rootPrefix += $separator
    }
    if (-not $fullPath.StartsWith($rootPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label must resolve below the repository root for the container bind mount: $fullPath"
    }
    $relative = $fullPath.Substring($rootPrefix.Length)
    return [System.IO.Path]::GetFullPath((Join-Path 'C:\workspace' $relative))
}

if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw "$Target requires a Windows host running Windows containers"
}
if ($ZigVersion -cne '0.16.0') {
    throw "the container toolchain pins Zig 0.16.0 (requested $ZigVersion)"
}
if ([string]::IsNullOrWhiteSpace($BuilderImage)) {
    throw 'WINDOWS_GUI_BUILDER_IMAGE must not be empty'
}
if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $RepositoryRoot = Split-Path -Parent $PSScriptRoot
}
$root = [System.IO.Path]::GetFullPath($RepositoryRoot)
foreach ($path in @(
    (Join-Path $root 'scripts/run-windows-gui-target.ps1'),
    (Join-Path $root 'third_party/y-crdt/Cargo.lock')
)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        if ($path.EndsWith('Cargo.lock', [System.StringComparison]::OrdinalIgnoreCase)) {
            throw 'the y-crdt submodule is not initialized; run git submodule update --init --recursive'
        }
        throw "required container build source is missing: $path"
    }
}

$dockerCommand = Get-Command 'docker.exe' -ErrorAction SilentlyContinue
if ($null -eq $dockerCommand) {
    $dockerCommand = Get-Command 'docker' -ErrorAction SilentlyContinue
}
if ($null -eq $dockerCommand) {
    throw 'Windows GUI container builds require Docker'
}

$infoOutput = @(& docker info --format '{{.OSType}}|{{.Architecture}}|{{.OSVersion}}')
if ($LASTEXITCODE -ne 0 -or $infoOutput.Count -ne 1) {
    throw 'could not query the Docker engine; ensure Docker is running and this process can access it'
}
$infoFields = @(([string] $infoOutput[0]).Trim().Split('|'))
if ($infoFields.Count -ne 3 -or $infoFields[0] -cne 'windows') {
    throw "Windows GUI container builds require a Windows Docker engine (got $($infoOutput -join ' '))"
}
$dockerArchitecture = Get-NormalizedWindowsArchitecture -Value $infoFields[1] -Label 'Docker engine'
$hostArchitecture = Get-NormalizedWindowsArchitecture `
    -Value ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) `
    -Label 'host'
if ($dockerArchitecture -cne $hostArchitecture) {
    throw "Docker engine architecture $dockerArchitecture does not match host architecture $hostArchitecture"
}

$savedErrorActionPreference = $ErrorActionPreference
$ErrorActionPreference = 'SilentlyContinue'
$imageOutput = @(& docker image inspect $BuilderImage --format '{{.Os}}|{{.Architecture}}|{{.OsVersion}}' 2>$null)
$imageInspectExitCode = $LASTEXITCODE
$ErrorActionPreference = $savedErrorActionPreference
if ($imageInspectExitCode -ne 0 -or $imageOutput.Count -ne 1) {
    throw "Windows GUI builder image $BuilderImage is not available; run make build-windows-builder-image"
}
$imageFields = @(([string] $imageOutput[0]).Trim().Split('|'))
if ($imageFields.Count -ne 3 -or $imageFields[0] -cne 'windows') {
    throw "Windows GUI builder image $BuilderImage is not a Windows image (got $($imageOutput -join ' '))"
}
$imageArchitecture = Get-NormalizedWindowsArchitecture -Value $imageFields[1] -Label 'builder image'
if ($imageArchitecture -cne $dockerArchitecture) {
    throw "Windows GUI builder image $BuilderImage architecture $imageArchitecture does not match Docker engine architecture $dockerArchitecture"
}

$containerWindowsRoot = ConvertTo-ContainerRepositoryPath -Path $WindowsRoot -Root $root -Label 'WINDOWS_GUI_ROOT'
$containerPayloadRoot = ConvertTo-ContainerRepositoryPath -Path $PayloadRoot -Root $root -Label 'WINDOWS_GUI_PAYLOAD_ROOT'
$containerTestRoot = ConvertTo-ContainerRepositoryPath -Path $TestRoot -Root $root -Label 'WINDOWS_GUI_TEST_DIR'
$containerMsiRoot = ConvertTo-ContainerRepositoryPath -Path $MsiRoot -Root $root -Label 'WINDOWS_GUI_MSI_ROOT'
$runArguments = @(
    'run', '--rm', '--isolation=process',
    '--mount', "type=bind,source=$root,target=C:\workspace",
    '--workdir', 'C:\workspace',
    '--env', 'WINDOWS_GUI_CC_AMD64=C:/toolchains/llvm-mingw/bin/x86_64-w64-mingw32-clang.exe -static',
    '--env', 'WINDOWS_GUI_CC_ARM64=C:/toolchains/llvm-mingw/bin/aarch64-w64-mingw32-clang.exe -static',
    $BuilderImage,
    '-File', 'C:\workspace\scripts\run-windows-gui-target.ps1',
    '-Target', $Target,
    '-Version', $Version,
    '-RepositoryRoot', 'C:\workspace',
    '-WindowsRoot', $containerWindowsRoot,
    '-PayloadRoot', $containerPayloadRoot,
    '-TestRoot', $containerTestRoot,
    '-MsiRoot', $containerMsiRoot,
    '-Repository', $Repository,
    '-ZigVersion', $ZigVersion
)
if ($null -ne $Architectures) {
    $runArguments += @('-Architectures', [string] $Architectures)
}
Write-Host "Running $Target with reusable builder image $BuilderImage ($($imageFields[0])/$imageArchitecture) and process isolation"
Invoke-Docker -Arguments $runArguments -Description $Target
