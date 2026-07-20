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
    [string] $ZigVersion = '0.16.0'
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
if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $RepositoryRoot = Split-Path -Parent $PSScriptRoot
}
$root = [System.IO.Path]::GetFullPath($RepositoryRoot)
foreach ($path in @(
    (Join-Path $root 'deploy/windows-desktop/Dockerfile'),
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

$toolchains = @{
    amd64 = [ordered] @{
        BaseImage = 'mcr.microsoft.com/windows/servercore:ltsc2025-amd64@sha256:bf9f85729e61bf27cad3844c506a41140c88b95d1388947e7a5e051a4672cebf'
        GoUrl = 'https://go.dev/dl/go1.23.12.windows-amd64.zip'
        GoSha256 = '07c35866cdd864b81bb6f1cfbf25ac7f87ddc3a976ede1bf5112acbb12dfe6dc'
        ZigUrl = 'https://ziglang.org/download/0.16.0/zig-x86_64-windows-0.16.0.zip'
        ZigSha256 = '68659eb5f1e4eb1437a722f1dd889c5a322c9954607f5edcf337bc3684a75a7e'
        RustupUrl = 'https://static.rust-lang.org/rustup/archive/1.29.0/x86_64-pc-windows-msvc/rustup-init.exe'
        RustupSha256 = '86478e53f769379d7f0ebfa7c9aa97cb76ca92233f79aa2cc0dbee2efaac73c7'
        DotnetUrl = 'https://builds.dotnet.microsoft.com/dotnet/Sdk/8.0.423/dotnet-sdk-8.0.423-win-x64.zip'
        DotnetSha512 = '063fcc35c136277e6fd767c66579f3b92db22a078a7f0c7177b6af1edb2c9afae1613f6cfdc01acf7421773d9ac77f0ef73a7fd8b37f469e7e3505e5c1361ba0'
        MinGitUrl = 'https://github.com/git-for-windows/git/releases/download/v2.55.0.windows.3/MinGit-2.55.0.3-64-bit.zip'
        MinGitSha256 = 'f48e2d2dc74a24454adc6d8fd0ac25bf9c2386f19cfb06202b9465aaad4f9f05'
        LlvmMingwUrl = 'https://github.com/mstorsjo/llvm-mingw/releases/download/20260616/llvm-mingw-20260616-ucrt-x86_64.zip'
        LlvmMingwSha256 = 'b9b68a4d276e16fa25802aaba458e4638f64b3884c290aaccdc2d87083b6ca35'
    }
    arm64 = [ordered] @{
        BaseImage = 'mcr.microsoft.com/windows/servercore:ltsc2025-arm64@sha256:cc1af59d5d6a39f38d6df0e35df71a1e83d2c9e88a12041a84d3b6fe6f4a8cfd'
        GoUrl = 'https://go.dev/dl/go1.23.12.windows-arm64.zip'
        GoSha256 = '22a5da4989e57ee4b0fb106429ffadc3bc2357b268885720025be5b0877d6fe9'
        ZigUrl = 'https://ziglang.org/download/0.16.0/zig-aarch64-windows-0.16.0.zip'
        ZigSha256 = 'aee38316ee4111717900f45dd3130145c39289e105541d737eb8c5ed653c78ef'
        RustupUrl = 'https://static.rust-lang.org/rustup/archive/1.29.0/aarch64-pc-windows-msvc/rustup-init.exe'
        RustupSha256 = '3af309e6c3062aa11df0e932954f69d13b734d8a431e593812f3ecd9ff9e6ef6'
        DotnetUrl = 'https://builds.dotnet.microsoft.com/dotnet/Sdk/8.0.423/dotnet-sdk-8.0.423-win-arm64.zip'
        DotnetSha512 = '106d85d70323eec5fd35b6d23d52877aff2480892ab9f5f53df205a0cbe7b8063029b17e5190c2fe0d01fafd61578defa8c1dd6708c2c2239e046ba4f38f6f16'
        MinGitUrl = 'https://github.com/git-for-windows/git/releases/download/v2.55.0.windows.3/MinGit-2.55.0.3-arm64.zip'
        MinGitSha256 = 'f7748965d5068e81ad93ca1923650db6742d6e22332b1ae7567a841c59f6bde5'
        LlvmMingwUrl = 'https://github.com/mstorsjo/llvm-mingw/releases/download/20260616/llvm-mingw-20260616-ucrt-aarch64.zip'
        LlvmMingwSha256 = '312593669435bd0bfc1a43ac3fba23c8b27e0610bade88b2738e5a01702a99ba'
    }
}
$toolchain = $toolchains[$dockerArchitecture]
$image = "codesk-windows-gui-build:ltsc2025-$dockerArchitecture"
$dockerfile = Join-Path $root 'deploy/windows-desktop/Dockerfile'
$context = Split-Path -Parent $dockerfile
$buildArguments = @(
    'build', '--isolation=process', '--file', $dockerfile, '--tag', $image,
    '--build-arg', "BASE_IMAGE=$($toolchain.BaseImage)",
    '--build-arg', "TOOLCHAIN_ARCH=$dockerArchitecture",
    '--build-arg', "GO_URL=$($toolchain.GoUrl)",
    '--build-arg', "GO_SHA256=$($toolchain.GoSha256)",
    '--build-arg', "ZIG_URL=$($toolchain.ZigUrl)",
    '--build-arg', "ZIG_SHA256=$($toolchain.ZigSha256)",
    '--build-arg', "RUSTUP_URL=$($toolchain.RustupUrl)",
    '--build-arg', "RUSTUP_SHA256=$($toolchain.RustupSha256)",
    '--build-arg', "DOTNET_URL=$($toolchain.DotnetUrl)",
    '--build-arg', "DOTNET_SHA512=$($toolchain.DotnetSha512)",
    '--build-arg', "MINGIT_URL=$($toolchain.MinGitUrl)",
    '--build-arg', "MINGIT_SHA256=$($toolchain.MinGitSha256)",
    '--build-arg', "LLVM_MINGW_URL=$($toolchain.LlvmMingwUrl)",
    '--build-arg', "LLVM_MINGW_SHA256=$($toolchain.LlvmMingwSha256)",
    $context
)
Write-Host "Building $image for native $dockerArchitecture with process isolation"
Invoke-Docker -Arguments $buildArguments -Description 'Windows GUI toolchain image build'

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
    $image,
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
Write-Host "Running $Target in native $dockerArchitecture Windows container with process isolation"
Invoke-Docker -Arguments $runArguments -Description $Target
