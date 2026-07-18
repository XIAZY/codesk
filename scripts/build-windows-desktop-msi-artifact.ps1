param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('AMD64', 'ARM64')]
    [string] $Architecture,

    [Parameter(Mandatory = $true)]
    [ValidateSet('amd64', 'arm64')]
    [string] $GoArchitecture,

    [Parameter(Mandatory = $true)]
    [ValidateSet('x64', 'arm64')]
    [string] $InstallerPlatform,

    [Parameter(Mandatory = $true)]
    [string] $PreviousProductCode,

    [Parameter(Mandatory = $true)]
    [string] $CandidateProductCode,

    [Parameter(Mandatory = $true)]
    [string] $CodeskExe,

    [Parameter(Mandatory = $true)]
    [string] $AgentToolExe,

    [Parameter(Mandatory = $true)]
    [string] $CodeskIcon,

    [Parameter(Mandatory = $true)]
    [string] $SourceHead,

    [Parameter(Mandatory = $true)]
    [string] $OutputDirectory,

    [Parameter(Mandatory = $true)]
    [string] $WorkingDirectory,

    [string] $WixSdkVersion = '4.0.5'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-Sha256 {
    param([Parameter(Mandatory = $true)][string] $Path)

    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function ConvertTo-NormalizedGuid {
    param([Parameter(Mandatory = $true)][string] $Value)

    return ([guid] $Value).ToString('B').ToUpperInvariant()
}

function Invoke-CleanLink {
    param(
        [Parameter(Mandatory = $true)] $Version,
        [Parameter(Mandatory = $true)][int] $BuildNumber
    )

    $buildRoot = Join-Path $WorkingDirectory "$($Version.name)-build-$BuildNumber"
    $sourceRoot = Join-Path $buildRoot 'source'
    $outputRoot = Join-Path $buildRoot 'output'
    Remove-Item -LiteralPath $buildRoot -Recurse -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path $sourceRoot, $outputRoot | Out-Null
    Copy-Item -LiteralPath (Join-Path $script:ProjectRoot 'Codesk.wixproj') -Destination $sourceRoot
    Copy-Item -LiteralPath (Join-Path $script:ProjectRoot 'Package.wxs') -Destination $sourceRoot

    $project = Join-Path $sourceRoot 'Codesk.wixproj'
    $arguments = @(
        'build',
        $project,
        '--configuration', 'Release',
        '--no-incremental',
        "-p:InstallerPlatform=$InstallerPlatform",
        "-p:ProductVersion=$($Version.version)",
        "-p:VersionProductCode=$($Version.productCode)",
        "-p:CodeskExe=$CodeskExe",
        "-p:AgentToolExe=$AgentToolExe",
        "-p:CodeskIcon=$CodeskIcon",
        "-p:OutputPath=$outputRoot",
        '-p:SuppressValidation=false'
    )
    & dotnet @arguments | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) {
        throw "WiX link failed for $($Version.name) clean build $BuildNumber with exit $LASTEXITCODE"
    }

    $packages = @(Get-ChildItem -LiteralPath $outputRoot -Filter *.msi -File -Recurse)
    if ($packages.Count -ne 1) {
        throw "expected exactly one $($Version.name) MSI from clean build $BuildNumber, found $($packages.Count)"
    }
    if ($packages[0].Length -le 0) {
        throw "linked MSI is empty: $($packages[0].FullName)"
    }
    return $packages[0].FullName
}

foreach ($path in @($CodeskExe, $AgentToolExe, $CodeskIcon)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "required MSI input is missing: $path"
    }
}

$expectedTarget = switch ($Architecture) {
    'AMD64' {
        [pscustomobject] @{ GoArchitecture = 'amd64'; InstallerPlatform = 'x64' }
    }
    'ARM64' {
        [pscustomobject] @{ GoArchitecture = 'arm64'; InstallerPlatform = 'arm64' }
    }
}
if ($GoArchitecture -cne $expectedTarget.GoArchitecture -or
    $InstallerPlatform -cne $expectedTarget.InstallerPlatform) {
    throw "inconsistent target tuple: $Architecture/$GoArchitecture/$InstallerPlatform"
}

$CodeskExe = (Resolve-Path -LiteralPath $CodeskExe).Path
$AgentToolExe = (Resolve-Path -LiteralPath $AgentToolExe).Path
$CodeskIcon = (Resolve-Path -LiteralPath $CodeskIcon).Path

$resolvedHead = (& git rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $resolvedHead -cne $SourceHead) {
    throw "source head mismatch: expected $SourceHead, got $resolvedHead"
}

$scriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = Split-Path -Parent $scriptRoot
$script:ProjectRoot = Join-Path $repositoryRoot 'deploy\windows-desktop'
$verifier = Join-Path $scriptRoot 'verify-windows-desktop-msi-reproducibility.ps1'
foreach ($path in @(
    (Join-Path $script:ProjectRoot 'Codesk.wixproj'),
    (Join-Path $script:ProjectRoot 'Package.wxs'),
    $verifier
)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "required MSI source is missing: $path"
    }
}

$versions = @(
    [pscustomobject] @{
        name = 'previous'
        version = '0.0.1'
        productCode = ConvertTo-NormalizedGuid $PreviousProductCode
    },
    [pscustomobject] @{
        name = 'candidate'
        version = '0.0.2'
        productCode = ConvertTo-NormalizedGuid $CandidateProductCode
    }
)
if ($versions[0].productCode -ceq $versions[1].productCode) {
    throw 'previous and candidate ProductCodes must be distinct'
}

Remove-Item -LiteralPath $OutputDirectory -Recurse -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath $WorkingDirectory -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $OutputDirectory, $WorkingDirectory | Out-Null

$inputHashes = [ordered] @{
    CodeskExe = [ordered] @{
        sha256 = Get-Sha256 $CodeskExe
        size = (Get-Item -LiteralPath $CodeskExe).Length
    }
    AgentToolExe = [ordered] @{
        sha256 = Get-Sha256 $AgentToolExe
        size = (Get-Item -LiteralPath $AgentToolExe).Length
    }
    CodeskIcon = [ordered] @{
        sha256 = Get-Sha256 $CodeskIcon
        size = (Get-Item -LiteralPath $CodeskIcon).Length
    }
    WixProject = [ordered] @{
        sha256 = Get-Sha256 (Join-Path $script:ProjectRoot 'Codesk.wixproj')
    }
    WixPackage = [ordered] @{
        sha256 = Get-Sha256 (Join-Path $script:ProjectRoot 'Package.wxs')
    }
}

$packageReports = @()
foreach ($version in $versions) {
    $firstMsi = Invoke-CleanLink -Version $version -BuildNumber 1
    Start-Sleep -Seconds 2
    $secondMsi = Invoke-CleanLink -Version $version -BuildNumber 2

    $reportPath = Join-Path $WorkingDirectory "$($version.name)-reproducibility.json"
    $compareRoot = Join-Path $WorkingDirectory "$($version.name)-compare"
    & $verifier `
        -FirstMsi $firstMsi `
        -SecondMsi $secondMsi `
        -ExpectedProductCode $version.productCode `
        -ExpectedProductVersion $version.version `
        -ExpectedCodeskSha256 $inputHashes.CodeskExe.sha256 `
        -ExpectedAgentSha256 $inputHashes.AgentToolExe.sha256 `
        -ExpectedIconSha256 $inputHashes.CodeskIcon.sha256 `
        -WorkingDirectory $compareRoot `
        -ReportPath $reportPath `
        -WixSdkVersion $WixSdkVersion

    $canonicalName = "Codesk_$($version.version)_windows_$GoArchitecture.msi"
    $canonicalPath = Join-Path $OutputDirectory $canonicalName
    Copy-Item -LiteralPath $firstMsi -Destination $canonicalPath
    $report = Get-Content -LiteralPath $reportPath -Raw | ConvertFrom-Json
    $packageReports += [ordered] @{
        role = $version.name
        version = $version.version
        productCode = $version.productCode
        canonicalFile = $canonicalName
        canonicalSha256 = Get-Sha256 $canonicalPath
        canonicalSize = (Get-Item -LiteralPath $canonicalPath).Length
        reproducibility = $report
    }
}

$dotnetVersion = (& dotnet --version).Trim()
if ($LASTEXITCODE -ne 0) {
    throw 'dotnet --version failed'
}
$provenance = [ordered] @{
    schemaVersion = 1
    source = [ordered] @{
        repository = $env:GITHUB_REPOSITORY
        head = $SourceHead
        workflowRef = $env:GITHUB_WORKFLOW_REF
        runId = $env:GITHUB_RUN_ID
        runAttempt = $env:GITHUB_RUN_ATTEMPT
    }
    runner = [ordered] @{
        name = $env:RUNNER_NAME
        os = $env:RUNNER_OS
        architecture = $env:RUNNER_ARCH
    }
    target = [ordered] @{
        architecture = $Architecture
        goArchitecture = $GoArchitecture
        installerPlatform = $InstallerPlatform
    }
    toolchain = [ordered] @{
        dotnetSdk = $dotnetVersion
        wixSdk = $WixSdkVersion
        powershell = $PSVersionTable.PSVersion.ToString()
    }
    inputs = $inputHashes
    validation = [ordered] @{
        cleanLinksPerVersion = 2
        wixIceSuppressed = $false
        normalizedComparator = 'Full MSI table/stream export plus WiX v4 decompile/extracted resources; only SummaryInformation PID 9, 12, and 13 may differ'
        causalMismatch = 'msi-property-row'
    }
    packages = $packageReports
}

$provenancePath = Join-Path $OutputDirectory 'provenance.json'
$provenanceJson = ConvertTo-Json $provenance -Depth 30
[System.IO.File]::WriteAllText($provenancePath, $provenanceJson + "`n", [System.Text.UTF8Encoding]::new($false))

$checksumFiles = @(
    Get-ChildItem -LiteralPath $OutputDirectory -File |
        Where-Object { $_.Name -ne 'SHA256SUMS' } |
        Sort-Object Name
)
$checksumLines = @($checksumFiles | ForEach-Object { "$(Get-Sha256 $_.FullName)  $($_.Name)" })
$checksumsPath = Join-Path $OutputDirectory 'SHA256SUMS'
[System.IO.File]::WriteAllLines($checksumsPath, $checksumLines, [System.Text.UTF8Encoding]::new($false))

$expectedNames = @(
    "Codesk_0.0.1_windows_$GoArchitecture.msi",
    "Codesk_0.0.2_windows_$GoArchitecture.msi",
    'provenance.json',
    'SHA256SUMS'
)
$actualNames = @(Get-ChildItem -LiteralPath $OutputDirectory -File | ForEach-Object Name | Sort-Object)
if ((ConvertTo-Json $actualNames -Compress) -cne (ConvertTo-Json ($expectedNames | Sort-Object) -Compress)) {
    throw "unexpected canonical artifact set: $($actualNames -join ', ')"
}
Write-Host "canonical $Architecture MSI artifact ready: $OutputDirectory"
