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
    [ValidateSet('pull_request', 'push')]
    [string] $SourceEvent,

    [Parameter(Mandatory = $true)]
    [string] $SourceCheckoutCommit,

    [Parameter(Mandatory = $true)]
    [string] $SourceHead,

    [Parameter(Mandatory = $true)]
    [string] $SourceBase,

    [Parameter(Mandatory = $true)]
    [string] $SafeParentDirectory,

    [Parameter(Mandatory = $true)]
    [string] $OutputDirectory,

    [Parameter(Mandatory = $true)]
    [string] $WorkingDirectory,

    [string] $DotnetSdkVersion = '8.0.423',

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

function ConvertTo-NormalizedCommit {
    param(
        [Parameter(Mandatory = $true)][string] $Value,
        [Parameter(Mandatory = $true)][string] $Label
    )

    if ($Value -cnotmatch '^[0-9a-fA-F]{40}$') {
        throw "$Label is not a full 40-character commit ID: $Value"
    }
    return $Value.ToLowerInvariant()
}

function Get-GitCommit {
    param(
        [Parameter(Mandatory = $true)][string] $Revision,
        [Parameter(Mandatory = $true)][string] $Label
    )

    $output = @(& git rev-parse --verify "$Revision^{commit}")
    if ($LASTEXITCODE -ne 0 -or $output.Count -ne 1) {
        throw "failed to resolve $Label commit: $Revision"
    }
    $commit = ConvertTo-NormalizedCommit `
        -Value (([string] $output[0]).Trim()) `
        -Label $Label
    return $commit
}

function Get-GitCommitAndParents {
    param(
        [Parameter(Mandatory = $true)][string] $Commit,
        [Parameter(Mandatory = $true)][string] $Label
    )

    $output = @(& git rev-list --parents -n 1 $Commit)
    if ($LASTEXITCODE -ne 0 -or $output.Count -ne 1) {
        throw "failed to read $Label parents: $Commit"
    }
    $fields = @(([string] $output[0]).Trim() -split '\s+')
    if ($fields.Count -eq 0 -or
        (ConvertTo-NormalizedCommit -Value $fields[0] -Label "$Label checkout") -cne $Commit) {
        throw "$Label parent record does not start with checkout commit $Commit"
    }
    return $fields
}

function Reset-EmptyDirectory {
    param(
        [Parameter(Mandatory = $true)][string] $Path,
        [Parameter(Mandatory = $true)][string] $ExpectedParent,
        [Parameter(Mandatory = $true)][string] $Label
    )

    if (-not (Test-Path -LiteralPath $ExpectedParent -PathType Container)) {
        throw "$Label safe parent is missing: $ExpectedParent"
    }
    $parentFull = [System.IO.Path]::GetFullPath($ExpectedParent)
    $pathFull = [System.IO.Path]::GetFullPath($Path)
    $separator = [string] [System.IO.Path]::DirectorySeparatorChar
    $parentPrefix = $parentFull
    if (-not $parentPrefix.EndsWith($separator)) {
        $parentPrefix += $separator
    }
    if (-not $pathFull.StartsWith($parentPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label is not a safe child of $parentFull`: $pathFull"
    }

    if (Test-Path -LiteralPath $pathFull) {
        Remove-Item -LiteralPath $pathFull -Recurse -Force -ErrorAction Stop
    }
    if (Test-Path -LiteralPath $pathFull) {
        throw "$Label still exists after reset removal: $pathFull"
    }
    New-Item -ItemType Directory -Path $pathFull -ErrorAction Stop | Out-Null
    if (-not (Test-Path -LiteralPath $pathFull -PathType Container)) {
        throw "$Label was not recreated as a directory: $pathFull"
    }
    $remaining = @(Get-ChildItem -LiteralPath $pathFull -Force -ErrorAction Stop)
    if ($remaining.Count -ne 0) {
        throw "$Label is not empty after reset: $pathFull"
    }
    return $pathFull
}

function Get-RelativePath {
    param(
        [Parameter(Mandatory = $true)][string] $Root,
        [Parameter(Mandatory = $true)][string] $Path
    )

    $rootFull = [System.IO.Path]::GetFullPath($Root)
    $pathFull = [System.IO.Path]::GetFullPath($Path)
    $separator = [string] [System.IO.Path]::DirectorySeparatorChar
    if (-not $rootFull.EndsWith($separator)) {
        $rootFull += $separator
    }
    if (-not $pathFull.StartsWith($rootFull, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "artifact path is outside its root: $pathFull"
    }
    return $pathFull.Substring($rootFull.Length).Replace('\', '/')
}

function Get-CanonicalArtifactFiles {
    param([Parameter(Mandatory = $true)][string] $Root)

    $directories = @(Get-ChildItem -LiteralPath $Root -Directory -Recurse -Force -ErrorAction Stop)
    if ($directories.Count -ne 0) {
        $relativeDirectories = @($directories | ForEach-Object {
            Get-RelativePath -Root $Root -Path $_.FullName
        })
        throw "canonical artifact set contains directories: $($relativeDirectories -join ', ')"
    }

    $filesByPath = @{}
    foreach ($file in Get-ChildItem -LiteralPath $Root -File -Recurse -Force -ErrorAction Stop) {
        $relative = Get-RelativePath -Root $Root -Path $file.FullName
        if ($filesByPath.ContainsKey($relative)) {
            throw "duplicate canonical artifact path: $relative"
        }
        $filesByPath[$relative] = $file
    }
    $paths = [string[]] @($filesByPath.Keys)
    [Array]::Sort($paths, [System.StringComparer]::Ordinal)
    return @($paths | ForEach-Object { $filesByPath[$_] })
}

function Invoke-CleanLink {
    param(
        [Parameter(Mandatory = $true)] $Version,
        [Parameter(Mandatory = $true)][int] $BuildNumber
    )

    $buildRoot = Join-Path $WorkingDirectory "$($Version.name)-build-$BuildNumber"
    $buildRoot = Reset-EmptyDirectory `
        -Path $buildRoot `
        -ExpectedParent $WorkingDirectory `
        -Label "$($Version.name) clean build $BuildNumber root"
    $sourceRoot = Join-Path $buildRoot 'source'
    $sourceRoot = Reset-EmptyDirectory `
        -Path $sourceRoot `
        -ExpectedParent $buildRoot `
        -Label "$($Version.name) clean build $BuildNumber source"
    $outputRoot = Join-Path $buildRoot 'output'
    $outputRoot = Reset-EmptyDirectory `
        -Path $outputRoot `
        -ExpectedParent $buildRoot `
        -Label "$($Version.name) clean build $BuildNumber output"
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

$SourceCheckoutCommit = ConvertTo-NormalizedCommit `
    -Value $SourceCheckoutCommit `
    -Label 'source checkout commit'
$SourceHead = ConvertTo-NormalizedCommit -Value $SourceHead -Label 'source head'
$SourceBase = ConvertTo-NormalizedCommit -Value $SourceBase -Label 'source base'
$resolvedCheckout = Get-GitCommit -Revision 'HEAD' -Label 'checked-out source'
if ($resolvedCheckout -cne $SourceCheckoutCommit) {
    throw "source checkout commit mismatch: expected $SourceCheckoutCommit, got $resolvedCheckout"
}

$sourceBaseResolution = 'event'
$checkoutAndParents = Get-GitCommitAndParents `
    -Commit $SourceCheckoutCommit `
    -Label $SourceEvent
if ($SourceEvent -ceq 'pull_request') {
    if ($checkoutAndParents.Count -ne 3) {
        throw "pull request checkout must be a two-parent merge commit: $SourceCheckoutCommit"
    }
    $mergeBase = ConvertTo-NormalizedCommit `
        -Value $checkoutAndParents[1] `
        -Label 'pull request merge base parent'
    $mergeHead = ConvertTo-NormalizedCommit `
        -Value $checkoutAndParents[2] `
        -Label 'pull request merge head parent'
    if ($mergeBase -cne $SourceBase -or $mergeHead -cne $SourceHead) {
        throw "pull request checkout parents do not map to source base then source head"
    }
} else {
    if ($SourceCheckoutCommit -cne $SourceHead) {
        throw "push checkout commit does not equal source head"
    }
    if ($SourceBase -ceq '0000000000000000000000000000000000000000') {
        if ($checkoutAndParents.Count -lt 2) {
            throw "push source base fallback requires a checkout parent"
        }
        $SourceBase = ConvertTo-NormalizedCommit `
            -Value $checkoutAndParents[1] `
            -Label 'push source base fallback'
        $sourceBaseResolution = 'checkout-first-parent-fallback'
    } else {
        $resolvedBase = Get-GitCommit -Revision $SourceBase -Label 'push source base'
        if ($resolvedBase -cne $SourceBase) {
            throw "push source base mismatch: expected $SourceBase, got $resolvedBase"
        }
        & git merge-base --is-ancestor $SourceBase $SourceCheckoutCommit
        if ($LASTEXITCODE -ne 0) {
            throw "push source base is not an ancestor of source head"
        }
    }
    if ($SourceBase -ceq $SourceHead) {
        throw "source base and source head must be distinct"
    }
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

if (-not (Test-Path -LiteralPath $SafeParentDirectory -PathType Container)) {
    throw "safe parent directory is missing: $SafeParentDirectory"
}
$SafeParentDirectory = (Resolve-Path -LiteralPath $SafeParentDirectory).Path
$OutputDirectory = Reset-EmptyDirectory `
    -Path $OutputDirectory `
    -ExpectedParent $SafeParentDirectory `
    -Label 'canonical output directory'
$WorkingDirectory = Reset-EmptyDirectory `
    -Path $WorkingDirectory `
    -ExpectedParent $SafeParentDirectory `
    -Label 'MSI working directory'

$dotnetVersionOutput = @(& dotnet --version)
if ($LASTEXITCODE -ne 0 -or $dotnetVersionOutput.Count -ne 1) {
    throw 'dotnet --version failed before the first WiX link'
}
$dotnetVersion = ([string] $dotnetVersionOutput[0]).Trim()
if ($dotnetVersion -cne $DotnetSdkVersion) {
    throw "dotnet SDK mismatch before the first WiX link: expected $DotnetSdkVersion, got $dotnetVersion"
}

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
        -ExpectedInstallerPlatform $InstallerPlatform `
        -ExpectedCodeskSha256 $inputHashes.CodeskExe.sha256 `
        -ExpectedAgentSha256 $inputHashes.AgentToolExe.sha256 `
        -ExpectedIconSha256 $inputHashes.CodeskIcon.sha256 `
        -WorkingDirectory $compareRoot `
        -SafeParentDirectory $WorkingDirectory `
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

$provenance = [ordered] @{
    schemaVersion = 2
    source = [ordered] @{
        repository = $env:GITHUB_REPOSITORY
        event = $SourceEvent
        checkoutCommit = $SourceCheckoutCommit
        sourceHead = $SourceHead
        sourceBase = $SourceBase
        sourceBaseResolution = $sourceBaseResolution
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

$checksumFiles = @(Get-CanonicalArtifactFiles -Root $OutputDirectory)
$expectedChecksumNames = @(
    "Codesk_0.0.1_windows_$GoArchitecture.msi",
    "Codesk_0.0.2_windows_$GoArchitecture.msi",
    'provenance.json'
)
$actualChecksumNames = @($checksumFiles | ForEach-Object Name)
if ((ConvertTo-Json $actualChecksumNames -Compress) -cne
    (ConvertTo-Json ($expectedChecksumNames | Sort-Object) -Compress)) {
    throw "unexpected checksummed artifact set: $($actualChecksumNames -join ', ')"
}
$checksumLines = @($checksumFiles | ForEach-Object { "$(Get-Sha256 $_.FullName)  $($_.Name)" })
$checksumsPath = Join-Path $OutputDirectory 'SHA256SUMS'
[System.IO.File]::WriteAllLines($checksumsPath, $checksumLines, [System.Text.UTF8Encoding]::new($false))

$expectedNames = @(
    "Codesk_0.0.1_windows_$GoArchitecture.msi",
    "Codesk_0.0.2_windows_$GoArchitecture.msi",
    'provenance.json',
    'SHA256SUMS'
)
$canonicalFiles = @(Get-CanonicalArtifactFiles -Root $OutputDirectory)
$actualNames = @($canonicalFiles | ForEach-Object Name)
if ((ConvertTo-Json $actualNames -Compress) -cne (ConvertTo-Json ($expectedNames | Sort-Object) -Compress)) {
    throw "unexpected canonical artifact set: $($actualNames -join ', ')"
}
Write-Host "canonical $Architecture MSI artifact ready: $OutputDirectory"
