[CmdletBinding()]
param(
    [string] $RepositoryRoot = '',
    [string] $BuilderImage = 'alphatoad/notty:windows-builder'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

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

if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw 'Windows GUI builder image construction requires a Windows host running Windows containers'
}
if ([string]::IsNullOrWhiteSpace($BuilderImage)) {
    throw 'WINDOWS_GUI_BUILDER_IMAGE must not be empty'
}
if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $RepositoryRoot = Split-Path -Parent $PSScriptRoot
}
$root = [System.IO.Path]::GetFullPath($RepositoryRoot)
$dockerfile = Join-Path $root 'deploy/windows-desktop/Dockerfile'
if (-not (Test-Path -LiteralPath $dockerfile -PathType Leaf)) {
    throw "Windows builder Dockerfile is missing: $dockerfile"
}
if ($null -eq (Get-Command 'docker' -ErrorAction SilentlyContinue)) {
    throw 'Windows GUI builder image creation requires Docker'
}

$infoOutput = @(& docker info --format '{{.OSType}}|{{.Architecture}}|{{.OSVersion}}')
if ($LASTEXITCODE -ne 0 -or $infoOutput.Count -ne 1) {
    throw 'could not query the Docker engine; ensure Docker is running and this process can access it'
}
$infoFields = @(([string] $infoOutput[0]).Trim().Split('|'))
if ($infoFields.Count -ne 3 -or $infoFields[0] -cne 'windows') {
    throw "Windows GUI builder image creation requires a Windows Docker engine (got $($infoOutput -join ' '))"
}
$dockerArchitecture = Get-NormalizedWindowsArchitecture -Value $infoFields[1] -Label 'Docker engine'
$hostArchitecture = Get-NormalizedWindowsArchitecture `
    -Value ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) `
    -Label 'host'
if ($dockerArchitecture -cne $hostArchitecture) {
    throw "Docker engine architecture $dockerArchitecture does not match host architecture $hostArchitecture"
}

$context = Split-Path -Parent $dockerfile
Write-Host "Building reusable Windows GUI builder image $BuilderImage for native $dockerArchitecture with process isolation"
& docker build `
    --isolation=process `
    --file $dockerfile `
    --tag $BuilderImage `
    --build-arg "TARGETARCH=$dockerArchitecture" `
    $context
if ($LASTEXITCODE -ne 0) {
    throw "Windows GUI builder image build failed with exit $LASTEXITCODE"
}

$imageOutput = @(& docker image inspect $BuilderImage --format '{{.Os}}|{{.Architecture}}|{{.OsVersion}}')
if ($LASTEXITCODE -ne 0 -or $imageOutput.Count -ne 1) {
    throw "could not inspect the completed builder image $BuilderImage"
}
$imageFields = @(([string] $imageOutput[0]).Trim().Split('|'))
if ($imageFields.Count -ne 3 -or $imageFields[0] -cne 'windows') {
    throw "builder image $BuilderImage is not a Windows image (got $($imageOutput -join ' '))"
}
$imageArchitecture = Get-NormalizedWindowsArchitecture -Value $imageFields[1] -Label 'builder image'
if ($imageArchitecture -cne $dockerArchitecture) {
    throw "builder image $BuilderImage does not match windows/$dockerArchitecture (got $($imageOutput -join ' '))"
}
Write-Host "Built reusable Windows GUI builder image $BuilderImage ($($imageFields[0])/$imageArchitecture, Windows $($imageFields[2]))"
