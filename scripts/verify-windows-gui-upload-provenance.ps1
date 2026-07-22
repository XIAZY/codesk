[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string] $MsiRoot,

    [Parameter(Mandatory = $true)]
    [string] $Version,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-f]{40}$')]
    [string] $SourceHead,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-f]{40}$')]
    [string] $SourceBase,

    [Parameter(Mandatory = $true)]
    [string] $Repository
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-RequiredProperty {
    param(
        [Parameter(Mandatory = $true)] [object] $Object,
        [Parameter(Mandatory = $true)] [string] $Name,
        [Parameter(Mandatory = $true)] [string] $Label
    )

    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        throw "$Label is missing property $Name"
    }
    Write-Output -NoEnumerate $property.Value
}

function Assert-ExactString {
    param(
        [AllowNull()] [object] $Actual,
        [Parameter(Mandatory = $true)] [string] $Expected,
        [Parameter(Mandatory = $true)] [string] $Label
    )

    if ($Actual -isnot [string] -or [string] $Actual -cne $Expected) {
        throw "$Label mismatch: expected $Expected, got $Actual"
    }
}

function Assert-ExactNames {
    param(
        [Parameter(Mandatory = $true)] [string[]] $Actual,
        [Parameter(Mandatory = $true)] [string[]] $Expected,
        [Parameter(Mandatory = $true)] [string] $Label
    )

    [Array]::Sort($Actual, [System.StringComparer]::Ordinal)
    [Array]::Sort($Expected, [System.StringComparer]::Ordinal)
    if ($Actual.Count -ne $Expected.Count) {
        throw "$Label inventory mismatch: $($Actual -join ', ')"
    }
    for ($index = 0; $index -lt $Expected.Count; $index++) {
        if ($Actual[$index] -cne $Expected[$index]) {
            throw "$Label inventory mismatch: $($Actual -join ', ')"
        }
    }
}

if (-not (Test-Path -LiteralPath $MsiRoot -PathType Container)) {
    throw "Windows GUI MSI root is missing: $MsiRoot"
}
$MsiRoot = (Resolve-Path -LiteralPath $MsiRoot).Path
$rootItem = Get-Item -LiteralPath $MsiRoot -Force
if (($rootItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw "Windows GUI MSI root must be a real directory: $MsiRoot"
}

[string[]] $architectures = @('amd64', 'arm64')
$architectureItems = @(Get-ChildItem -LiteralPath $MsiRoot -Force)
[string[]] $architectureNames = @($architectureItems | ForEach-Object { $_.Name })
Assert-ExactNames -Actual $architectureNames -Expected ($architectures.Clone()) -Label 'Windows GUI architecture'
foreach ($item in $architectureItems) {
    if (-not $item.PSIsContainer -or
        (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) {
        throw "Windows GUI architecture entry must be a real directory: $($item.FullName)"
    }
}

foreach ($architecture in $architectures) {
    $architectureDirectory = Join-Path $MsiRoot $architecture
    $msiName = "Codesk_${Version}_windows_${architecture}.msi"
    [string[]] $expectedFiles = @($msiName, 'SHA256SUMS', 'provenance.json')
    $fileItems = @(Get-ChildItem -LiteralPath $architectureDirectory -Force)
    [string[]] $fileNames = @($fileItems | ForEach-Object { $_.Name })
    Assert-ExactNames -Actual $fileNames -Expected $expectedFiles -Label "$architecture release"
    foreach ($item in $fileItems) {
        if ($item.PSIsContainer -or
            (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) -or
            $item.Length -le 0) {
            throw "$architecture release entry must be a nonempty real file: $($item.FullName)"
        }
    }

    $provenancePath = Join-Path $architectureDirectory 'provenance.json'
    $provenance = Get-Content -LiteralPath $provenancePath -Raw | ConvertFrom-Json
    $schemaVersion = Get-RequiredProperty -Object $provenance -Name 'schemaVersion' -Label "$architecture provenance"
    if (($schemaVersion -isnot [int] -and $schemaVersion -isnot [long]) -or [long] $schemaVersion -ne 2) {
        throw "$architecture provenance schemaVersion must be numeric 2"
    }

    $source = Get-RequiredProperty -Object $provenance -Name 'source' -Label "$architecture provenance"
    Assert-ExactString (Get-RequiredProperty $source 'repository' "$architecture source") $Repository "$architecture source.repository"
    Assert-ExactString (Get-RequiredProperty $source 'event' "$architecture source") 'push' "$architecture source.event"
    Assert-ExactString (Get-RequiredProperty $source 'checkoutCommit' "$architecture source") $SourceHead "$architecture source.checkoutCommit"
    Assert-ExactString (Get-RequiredProperty $source 'sourceHead' "$architecture source") $SourceHead "$architecture source.sourceHead"
    Assert-ExactString (Get-RequiredProperty $source 'sourceBase' "$architecture source") $SourceBase "$architecture source.sourceBase"
    Assert-ExactString (Get-RequiredProperty $source 'sourceBaseResolution' "$architecture source") 'event' "$architecture source.sourceBaseResolution"
    Assert-ExactString (Get-RequiredProperty $source 'workflowRef' "$architecture source") "local/scripts/run-windows-gui-target.ps1@$SourceHead" "$architecture source.workflowRef"
    Assert-ExactString (Get-RequiredProperty $source 'runId' "$architecture source") 'local' "$architecture source.runId"
    Assert-ExactString (Get-RequiredProperty $source 'runAttempt' "$architecture source") '1' "$architecture source.runAttempt"

    $expectedNativeArchitecture = if ($architecture -ceq 'amd64') { 'AMD64' } else { 'ARM64' }
    $expectedInstallerPlatform = if ($architecture -ceq 'amd64') { 'x64' } else { 'arm64' }
    $runner = Get-RequiredProperty -Object $provenance -Name 'runner' -Label "$architecture provenance"
    Assert-ExactString (Get-RequiredProperty $runner 'os' "$architecture runner") 'Windows' "$architecture runner.os"
    Assert-ExactString (Get-RequiredProperty $runner 'architecture' "$architecture runner") $expectedNativeArchitecture "$architecture runner.architecture"

    $target = Get-RequiredProperty -Object $provenance -Name 'target' -Label "$architecture provenance"
    Assert-ExactString (Get-RequiredProperty $target 'architecture' "$architecture target") $expectedNativeArchitecture "$architecture target.architecture"
    Assert-ExactString (Get-RequiredProperty $target 'goArchitecture' "$architecture target") $architecture "$architecture target.goArchitecture"
    Assert-ExactString (Get-RequiredProperty $target 'installerPlatform' "$architecture target") $expectedInstallerPlatform "$architecture target.installerPlatform"
    Assert-ExactString (Get-RequiredProperty $target 'buildMode' "$architecture target") 'release' "$architecture target.buildMode"
    $publishable = Get-RequiredProperty -Object $target -Name 'publishable' -Label "$architecture target"
    if ($publishable -isnot [bool] -or -not $publishable) {
        throw "$architecture target.publishable must be boolean true"
    }

    $msiPath = Join-Path $architectureDirectory $msiName
    $msiItem = Get-Item -LiteralPath $msiPath
    $msiSha256 = (Get-FileHash -LiteralPath $msiPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $packageValue = Get-RequiredProperty -Object $provenance -Name 'packages' -Label "$architecture provenance"
    if ($packageValue -isnot [System.Array]) {
        throw "$architecture provenance packages must be an array"
    }
    $packages = @($packageValue)
    if ($packages.Count -ne 1) {
        throw "$architecture provenance must describe exactly one package"
    }
    $package = $packages[0]
    Assert-ExactString (Get-RequiredProperty $package 'role' "$architecture package") 'release' "$architecture package.role"
    Assert-ExactString (Get-RequiredProperty $package 'version' "$architecture package") $Version "$architecture package.version"
    Assert-ExactString (Get-RequiredProperty $package 'canonicalFile' "$architecture package") $msiName "$architecture package.canonicalFile"
    Assert-ExactString (Get-RequiredProperty $package 'canonicalSha256' "$architecture package") $msiSha256 "$architecture package.canonicalSha256"
    $canonicalSize = Get-RequiredProperty -Object $package -Name 'canonicalSize' -Label "$architecture package"
    if (($canonicalSize -isnot [int] -and $canonicalSize -isnot [long]) -or
        [long] $canonicalSize -ne $msiItem.Length) {
        throw "$architecture package.canonicalSize mismatch"
    }

    $derivation = Get-RequiredProperty -Object $provenance -Name 'productCodeDerivation' -Label "$architecture provenance"
    Assert-ExactString (Get-RequiredProperty $derivation 'algorithm' "$architecture ProductCode derivation") 'UUIDv5-SHA1' "$architecture productCodeDerivation.algorithm"
    Assert-ExactString (Get-RequiredProperty $derivation 'name' "$architecture ProductCode derivation") "$Version+$architecture" "$architecture productCodeDerivation.name"
}

Write-Host "Windows GUI upload provenance is bound to $SourceHead for amd64 and arm64"
