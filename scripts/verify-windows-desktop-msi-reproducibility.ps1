param(
    [Parameter(Mandatory = $true)]
    [string] $FirstMsi,

    [Parameter(Mandatory = $true)]
    [string] $SecondMsi,

    [Parameter(Mandatory = $true)]
    [string] $ExpectedProductCode,

    [Parameter(Mandatory = $true)]
    [string] $ExpectedProductVersion,

    [Parameter(Mandatory = $true)]
    [ValidateSet('x64', 'arm64')]
    [string] $ExpectedInstallerPlatform,

    [Parameter(Mandatory = $true)]
    [string] $ExpectedCodeskSha256,

    [Parameter(Mandatory = $true)]
    [string] $ExpectedAgentSha256,

    [Parameter(Mandatory = $true)]
    [string] $ExpectedIconSha256,

    [Parameter(Mandatory = $true)]
    [string] $WorkingDirectory,

    [Parameter(Mandatory = $true)]
    [string] $SafeParentDirectory,

    [Parameter(Mandatory = $true)]
    [string] $ReportPath,

    [string] $WixSdkVersion = '4.0.5'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ExpectedUpgradeCode = '{0C8C0BBA-06EE-43BA-BC34-768B9B740A09}'
$ExpectedComponents = [ordered] @{
    AgentToolExecutable = '{4DE1EFE2-7E29-4E46-A615-4CC9A6EB7DBE}'
    CodeskExecutable = '{931D5BAC-B213-44A8-B234-E24E415613EC}'
    CodeskLoginItemCleanup = '{A11ADE55-B9B8-45E9-9DAB-60203C2A824E}'
}
$SummaryPropertyNames = [ordered] @{
    '1' = 'CodePage'
    '2' = 'Title'
    '3' = 'Subject'
    '4' = 'Author'
    '5' = 'Keywords'
    '6' = 'Comments'
    '7' = 'Template'
    '8' = 'LastSavedBy'
    '9' = 'RevisionNumber'
    '10' = 'EditTime'
    '11' = 'LastPrintTime'
    '12' = 'CreateTime'
    '13' = 'LastSaveTime'
    '14' = 'PageCount'
    '15' = 'WordCount'
    '16' = 'CharacterCount'
    '17' = 'Thumbnail'
    '18' = 'CreatingApp'
    '19' = 'Security'
}
$AllowedSummaryDifferencePids = @(9, 12, 13)
$ExpectedSummaryTemplate = switch ($ExpectedInstallerPlatform) {
    'x64' { 'x64;1033' }
    'arm64' { 'Arm64;1033' }
}

function Get-Sha256 {
    param([Parameter(Mandatory = $true)][string] $Path)

    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-StringSha256 {
    param([Parameter(Mandatory = $true)][string] $Value)

    $algorithm = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($Value)
        return ([System.BitConverter]::ToString($algorithm.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
    } finally {
        $algorithm.Dispose()
    }
}

function ConvertTo-NormalizedGuid {
    param([Parameter(Mandatory = $true)][string] $Value)

    return ([guid] $Value).ToString('B').ToUpperInvariant()
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

function Get-NuGetRoot {
    if (-not [string]::IsNullOrWhiteSpace($env:NUGET_PACKAGES)) {
        return $env:NUGET_PACKAGES
    }
    return Join-Path ([Environment]::GetFolderPath('UserProfile')) '.nuget\packages'
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
        throw "extracted path is outside its root: $pathFull"
    }
    return $pathFull.Substring($rootFull.Length).Replace('\', '/')
}

function Get-ExtractedFileRecord {
    param([Parameter(Mandatory = $true)][string] $Root)

    $recordsByPath = @{}
    foreach ($file in Get-ChildItem -LiteralPath $Root -File -Recurse) {
        $relative = Get-RelativePath -Root $Root -Path $file.FullName
        if ($recordsByPath.ContainsKey($relative)) {
            throw "duplicate extracted resource path: $relative"
        }
        $recordsByPath[$relative] = [ordered] @{
            path = $relative
            size = $file.Length
            sha256 = Get-Sha256 $file.FullName
        }
    }
    $paths = [string[]] @($recordsByPath.Keys)
    [Array]::Sort($paths, [System.StringComparer]::Ordinal)
    return @($paths | ForEach-Object { $recordsByPath[$_] })
}

function Get-NormalizedIdt {
    param(
        [Parameter(Mandatory = $true)][string] $Path,
        [Parameter(Mandatory = $true)][bool] $IsSummaryInformation
    )

    $lines = [System.IO.File]::ReadAllLines($Path)
    if ($lines.Count -lt 3) {
        throw "exported MSI table has fewer than three schema rows: $Path"
    }

    $rows = @()
    $summaryPids = @{}
    if ($lines.Count -gt 3) {
        foreach ($line in $lines[3..($lines.Count - 1)]) {
            if ($IsSummaryInformation) {
                $fields = $line.Split([char[]] @([char] 9), 2)
                $propertyId = 0
                if ($fields.Count -ne 2 -or -not [int]::TryParse($fields[0], [ref] $propertyId)) {
                    throw "malformed SummaryInformation export row: $line"
                }
                if ($summaryPids.ContainsKey($propertyId)) {
                    throw "duplicate SummaryInformation PID $propertyId"
                }
                $summaryPids[$propertyId] = $true
                if ($AllowedSummaryDifferencePids -contains $propertyId) {
                    continue
                }
            }
            $rows += $line
        }
    }
    if ($IsSummaryInformation -and -not $summaryPids.ContainsKey(9)) {
        throw 'exported SummaryInformation has no PackageCode PID 9'
    }

    $sortedRows = [string[]] $rows
    [Array]::Sort($sortedRows, [System.StringComparer]::Ordinal)
    return (@($lines[0..2]) + @($sortedRows)) -join "`n"
}

function Get-NormalizedDatabaseFileRecord {
    param([Parameter(Mandatory = $true)][string] $Root)

    $recordsByPath = @{}
    $summaryFiles = 0
    $streamFiles = 0
    foreach ($file in Get-ChildItem -LiteralPath $Root -File -Recurse) {
        $relative = Get-RelativePath -Root $Root -Path $file.FullName
        if ($relative -ceq '_SummaryInformation.idt') {
            $summaryFiles++
        }
        if ($relative.StartsWith('_Streams/', [System.StringComparison]::Ordinal)) {
            $streamFiles++
        }

        if ($file.Extension -ieq '.idt') {
            $normalized = Get-NormalizedIdt `
                -Path $file.FullName `
                -IsSummaryInformation ($relative -ceq '_SummaryInformation.idt')
            $record = [ordered] @{
                path = $relative
                kind = 'table'
                size = [System.Text.Encoding]::UTF8.GetByteCount($normalized)
                sha256 = Get-StringSha256 $normalized
            }
        } else {
            $record = [ordered] @{
                path = $relative
                kind = 'stream'
                size = $file.Length
                sha256 = Get-Sha256 $file.FullName
            }
        }
        if ($recordsByPath.ContainsKey($relative)) {
            throw "duplicate MSI database export path: $relative"
        }
        $recordsByPath[$relative] = $record
    }
    if ($summaryFiles -ne 1) {
        throw "MSI database export has $summaryFiles SummaryInformation tables, want 1"
    }
    if ($streamFiles -lt 1) {
        throw 'MSI database export has no embedded streams'
    }
    $paths = [string[]] @($recordsByPath.Keys)
    [Array]::Sort($paths, [System.StringComparer]::Ordinal)
    return @($paths | ForEach-Object { $recordsByPath[$_] })
}

function Export-MsiDatabase {
    param(
        [Parameter(Mandatory = $true)][string] $MsiPath,
        [Parameter(Mandatory = $true)][string] $Destination
    )

    $database = [WixToolset.Dtf.WindowsInstaller.Database]::new(
        $MsiPath,
        [WixToolset.Dtf.WindowsInstaller.DatabaseOpenMode]::ReadOnly
    )
    try {
        $database.ExportAll($Destination)
    } finally {
        $database.Close()
    }
}

function Get-PackageIdentity {
    param(
        [Parameter(Mandatory = $true)][string] $SourcePath,
        [Parameter(Mandatory = $true)][string] $ExtractRoot
    )

    [xml] $document = [System.IO.File]::ReadAllText($SourcePath)
    $namespaces = [System.Xml.XmlNamespaceManager]::new($document.NameTable)
    $namespaces.AddNamespace('w', 'http://wixtoolset.org/schemas/v4/wxs')
    $package = $document.SelectSingleNode('/w:Wix/w:Package', $namespaces)
    if ($null -eq $package) {
        throw "decompiled source has no Package element: $SourcePath"
    }

    $components = [ordered] @{}
    foreach ($component in @($document.SelectNodes('//w:Component', $namespaces) | Sort-Object { $_.Id })) {
        $componentId = [string] $component.Id
        if ([string]::IsNullOrEmpty($componentId) -or $components.Contains($componentId)) {
            throw "decompiled source has an empty or duplicate Component Id: $componentId"
        }
        $components[$componentId] = ConvertTo-NormalizedGuid $component.Guid
    }

    $payloads = [ordered] @{}
    foreach ($fileId in @('CodeskExe', 'AgentToolExe')) {
        $matchingFiles = @($document.SelectNodes("//w:File[@Id='$fileId']", $namespaces))
        if ($matchingFiles.Count -ne 1) {
            throw "decompiled source has $($matchingFiles.Count) $fileId File rows, want 1"
        }
        $file = $matchingFiles[0]
        $relative = $file.Source -replace '^SourceDir[\\/]', ''
        $path = Join-Path $ExtractRoot $relative
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "decompiled $fileId payload is missing: $path"
        }
        $payloads[$fileId] = [ordered] @{
            path = $relative.Replace('\', '/')
            size = (Get-Item -LiteralPath $path).Length
            sha256 = Get-Sha256 $path
        }
    }

    $matchingIcons = @($document.SelectNodes("//w:Icon[@Id='Codesk.ico']", $namespaces))
    if ($matchingIcons.Count -ne 1) {
        throw "decompiled source has $($matchingIcons.Count) Codesk.ico Icon rows, want 1"
    }
    $icon = $matchingIcons[0]
    $iconRelative = $icon.SourceFile -replace '^SourceDir[\\/]', ''
    $iconPath = Join-Path $ExtractRoot $iconRelative
    if (-not (Test-Path -LiteralPath $iconPath -PathType Leaf)) {
        throw "decompiled icon is missing: $iconPath"
    }
    $payloads['CodeskIcon'] = [ordered] @{
        path = $iconRelative.Replace('\', '/')
        size = (Get-Item -LiteralPath $iconPath).Length
        sha256 = Get-Sha256 $iconPath
    }

    return [ordered] @{
        productCode = ConvertTo-NormalizedGuid $package.ProductCode
        upgradeCode = ConvertTo-NormalizedGuid $package.UpgradeCode
        productVersion = [string] $package.Version
        components = $components
        payloads = $payloads
    }
}

function Get-SummaryInformation {
    param([Parameter(Mandatory = $true)][string] $Path)

    $lines = [System.IO.File]::ReadAllLines($Path)
    if ($lines.Count -lt 3) {
        throw "exported SummaryInformation has fewer than three schema rows: $Path"
    }
    $result = [ordered] @{}
    foreach ($propertyId in $SummaryPropertyNames.Keys) {
        $result["PID$propertyId-$($SummaryPropertyNames[$propertyId])"] = $null
    }
    $seen = @{}
    if ($lines.Count -gt 3) {
        foreach ($line in $lines[3..($lines.Count - 1)]) {
            $fields = $line.Split([char[]] @([char] 9), 2)
            $propertyId = 0
            if ($fields.Count -ne 2 -or -not [int]::TryParse($fields[0], [ref] $propertyId)) {
                throw "malformed SummaryInformation export row: $line"
            }
            $propertyKey = [string] $propertyId
            if (-not $SummaryPropertyNames.Contains($propertyKey)) {
                throw "unsupported SummaryInformation PID $propertyId"
            }
            if ($seen.ContainsKey($propertyId)) {
                throw "duplicate SummaryInformation PID $propertyId"
            }
            $seen[$propertyId] = $true
            $result["PID$propertyId-$($SummaryPropertyNames[$propertyKey])"] = $fields[1]
        }
    }
    foreach ($propertyId in $AllowedSummaryDifferencePids) {
        if (-not $seen.ContainsKey($propertyId)) {
            throw "exported SummaryInformation has no required allowed PID $propertyId"
        }
        $propertyKey = [string] $propertyId
        $valueKey = "PID$propertyId-$($SummaryPropertyNames[$propertyKey])"
        if ([string]::IsNullOrWhiteSpace([string] $result[$valueKey])) {
            throw "exported SummaryInformation PID $propertyId has no value"
        }
    }
    return $result
}

function Get-ValidatedAllowedSummaryInformation {
    param([Parameter(Mandatory = $true)][string] $MsiPath)

    $database = [WixToolset.Dtf.WindowsInstaller.Database]::new(
        $MsiPath,
        [WixToolset.Dtf.WindowsInstaller.DatabaseOpenMode]::ReadOnly
    )
    $summaryInfo = $null
    try {
        $summaryInfo = $database.SummaryInfo
        $packageCodeValue = [string] $summaryInfo.RevisionNumber
        try {
            $packageCode = ConvertTo-NormalizedGuid $packageCodeValue
        } catch {
            throw "SummaryInformation PID 9 is not a PackageCode GUID: $packageCodeValue"
        }
        if ($packageCode -ceq ([guid]::Empty).ToString('B').ToUpperInvariant()) {
            throw 'SummaryInformation PID 9 PackageCode must not be empty'
        }

        $createTime = [datetime] $summaryInfo.CreateTime
        if ($createTime -eq [datetime]::MinValue) {
            throw 'SummaryInformation PID 12 is not a parseable MSI timestamp'
        }
        $lastSaveTime = [datetime] $summaryInfo.LastSaveTime
        if ($lastSaveTime -eq [datetime]::MinValue) {
            throw 'SummaryInformation PID 13 is not a parseable MSI timestamp'
        }

        return [ordered] @{
            packageCode = $packageCode
            createTime = $createTime.ToString('o', [System.Globalization.CultureInfo]::InvariantCulture)
            lastSaveTime = $lastSaveTime.ToString('o', [System.Globalization.CultureInfo]::InvariantCulture)
        }
    } finally {
        if ($null -ne $summaryInfo) {
            $summaryInfo.Close()
        }
        $database.Close()
    }
}

function Get-NormalizedSnapshot {
    param(
        [Parameter(Mandatory = $true)][string] $MsiPath,
        [Parameter(Mandatory = $true)][string] $Name
    )

    $root = Join-Path $WorkingDirectory $Name
    $root = Reset-EmptyDirectory `
        -Path $root `
        -ExpectedParent $WorkingDirectory `
        -Label "$Name snapshot root"
    $extract = Join-Path $root 'extract'
    $extract = Reset-EmptyDirectory `
        -Path $extract `
        -ExpectedParent $root `
        -Label "$Name extracted resources"
    $databaseExport = Join-Path $root 'database'
    $databaseExport = Reset-EmptyDirectory `
        -Path $databaseExport `
        -ExpectedParent $root `
        -Label "$Name database export"
    $intermediate = Join-Path $root 'intermediate'
    $intermediate = Reset-EmptyDirectory `
        -Path $intermediate `
        -ExpectedParent $root `
        -Label "$Name decompiler intermediate"
    $source = Join-Path $root 'package.wxs'

    & dotnet $script:WixDll msi decompile $MsiPath `
        -intermediateFolder $intermediate `
        -o $source `
        -x $extract | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) {
        throw "WiX decompile failed for $Name with exit $LASTEXITCODE"
    }
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "WiX decompile produced no source for $Name"
    }

    $sourceText = [System.IO.File]::ReadAllText($source).Replace("`r`n", "`n")
    $files = Get-ExtractedFileRecord $extract
    if ($files.Count -eq 0) {
        throw "WiX decompile extracted no resources for $Name"
    }
    $filesJson = ConvertTo-Json $files -Depth 5 -Compress
    Export-MsiDatabase -MsiPath $MsiPath -Destination $databaseExport
    $databaseFiles = Get-NormalizedDatabaseFileRecord $databaseExport
    $databaseFilesJson = ConvertTo-Json $databaseFiles -Depth 5 -Compress
    $summary = Get-SummaryInformation (Join-Path $databaseExport '_SummaryInformation.idt')
    $validatedAllowedSummary = Get-ValidatedAllowedSummaryInformation -MsiPath $MsiPath

    return [pscustomobject] @{
        Name = $Name
        MsiPath = $MsiPath
        SourcePath = $source
        SourceSha256 = Get-StringSha256 $sourceText
        SourceText = $sourceText
        ExtractRoot = $extract
        ExtractedFiles = $files
        ExtractedFilesJson = $filesJson
        ExtractedTreeSha256 = Get-StringSha256 $filesJson
        DatabaseFiles = $databaseFiles
        DatabaseFilesJson = $databaseFilesJson
        DatabaseTreeSha256 = Get-StringSha256 $databaseFilesJson
        Identity = Get-PackageIdentity -SourcePath $source -ExtractRoot $extract
        Summary = $summary
        ValidatedAllowedSummary = $validatedAllowedSummary
    }
}

function Assert-JsonEqual {
    param(
        [Parameter(Mandatory = $true)][string] $Label,
        [Parameter(Mandatory = $true)] $First,
        [Parameter(Mandatory = $true)] $Second
    )

    $firstJson = ConvertTo-Json $First -Depth 20 -Compress
    $secondJson = ConvertTo-Json $Second -Depth 20 -Compress
    if ($firstJson -cne $secondJson) {
        throw "$Label differs between clean links"
    }
}

function Assert-SnapshotsEquivalent {
    param(
        [Parameter(Mandatory = $true)] $First,
        [Parameter(Mandatory = $true)] $Second
    )

    if ($First.SourceText -cne $Second.SourceText) {
        throw 'normalized MSI tables differ between clean links'
    }
    if ($First.DatabaseFilesJson -cne $Second.DatabaseFilesJson) {
        throw 'normalized MSI database schema, rows, or streams differ between clean links'
    }
    if ($First.ExtractedFilesJson -cne $Second.ExtractedFilesJson) {
        throw 'normalized MSI resources or embedded payloads differ between clean links'
    }
    Assert-JsonEqual -Label 'package identity' -First $First.Identity -Second $Second.Identity

    $firstStableSummary = [ordered] @{}
    $secondStableSummary = [ordered] @{}
    foreach ($propertyId in $SummaryPropertyNames.Keys) {
        if ($AllowedSummaryDifferencePids -contains [int] $propertyId) {
            continue
        }
        $key = "PID$propertyId-$($SummaryPropertyNames[$propertyId])"
        $firstStableSummary[$key] = $First.Summary[$key]
        $secondStableSummary[$key] = $Second.Summary[$key]
    }
    Assert-JsonEqual -Label 'nonvolatile summary information' -First $firstStableSummary -Second $secondStableSummary
}

function Assert-ExpectedIdentity {
    param([Parameter(Mandatory = $true)] $Snapshot)

    $identity = $Snapshot.Identity
    if ($identity.productCode -cne (ConvertTo-NormalizedGuid $ExpectedProductCode)) {
        throw "ProductCode $($identity.productCode) does not match expected $ExpectedProductCode"
    }
    if ($identity.upgradeCode -cne $ExpectedUpgradeCode) {
        throw "UpgradeCode $($identity.upgradeCode) does not match expected $ExpectedUpgradeCode"
    }
    if ($identity.productVersion -cne $ExpectedProductVersion) {
        throw "ProductVersion $($identity.productVersion) does not match expected $ExpectedProductVersion"
    }
    if ($Snapshot.Summary['PID7-Template'] -cne $ExpectedSummaryTemplate) {
        throw "MSI SummaryInformation Template $($Snapshot.Summary['PID7-Template']) does not match expected $ExpectedSummaryTemplate"
    }
    Assert-JsonEqual -Label 'component identities' -First $ExpectedComponents -Second $identity.components

    $expectedPayloads = [ordered] @{
        AgentToolExe = $ExpectedAgentSha256.ToLowerInvariant()
        CodeskExe = $ExpectedCodeskSha256.ToLowerInvariant()
        CodeskIcon = $ExpectedIconSha256.ToLowerInvariant()
    }
    foreach ($id in $expectedPayloads.Keys) {
        if ($identity.payloads[$id].sha256 -cne $expectedPayloads[$id]) {
            throw "$id embedded SHA256 $($identity.payloads[$id].sha256) does not match expected $($expectedPayloads[$id])"
        }
    }
}

$first = (Resolve-Path -LiteralPath $FirstMsi).Path
$second = (Resolve-Path -LiteralPath $SecondMsi).Path
if ($first -ceq $second) {
    throw 'reproducibility comparison requires two distinct MSI paths'
}
if (-not (Test-Path -LiteralPath $SafeParentDirectory -PathType Container)) {
    throw "safe parent directory is missing: $SafeParentDirectory"
}
$SafeParentDirectory = (Resolve-Path -LiteralPath $SafeParentDirectory).Path
$WorkingDirectory = Reset-EmptyDirectory `
    -Path $WorkingDirectory `
    -ExpectedParent $SafeParentDirectory `
    -Label 'reproducibility working directory'

$nugetRoot = Get-NuGetRoot
$toolRoot = Join-Path $nugetRoot "wixtoolset.sdk\$WixSdkVersion\tools\net6.0"
$script:WixDll = Join-Path $toolRoot 'wix.dll'
$dtfAssembly = Join-Path $nugetRoot "wixtoolset.sdk\$WixSdkVersion\tools\net472\WixToolset.Dtf.WindowsInstaller.dll"
foreach ($tool in @($script:WixDll, $dtfAssembly)) {
    if (-not (Test-Path -LiteralPath $tool -PathType Leaf)) {
        throw "required WiX SDK tool is missing: $tool"
    }
}
if (-not ([AppDomain]::CurrentDomain.GetAssemblies() | Where-Object { $_.GetName().Name -eq 'WixToolset.Dtf.WindowsInstaller' })) {
    Add-Type -Path $dtfAssembly
}

$firstSnapshot = Get-NormalizedSnapshot -MsiPath $first -Name 'first'
$secondSnapshot = Get-NormalizedSnapshot -MsiPath $second -Name 'second'
Assert-ExpectedIdentity $firstSnapshot
Assert-ExpectedIdentity $secondSnapshot
Assert-SnapshotsEquivalent $firstSnapshot $secondSnapshot

$observedAllowedDifferences = @()
foreach ($propertyId in $AllowedSummaryDifferencePids) {
    $name = $SummaryPropertyNames[[string] $propertyId]
    $key = "PID$propertyId-$name"
    if ($firstSnapshot.Summary[$key] -cne $secondSnapshot.Summary[$key]) {
        $observedAllowedDifferences += [ordered] @{
            pid = $propertyId
            name = $name
            first = $firstSnapshot.Summary[$key]
            second = $secondSnapshot.Summary[$key]
        }
    }
}
if (@($observedAllowedDifferences | Where-Object { $_.pid -eq 9 }).Count -eq 0) {
    throw 'independent clean links reused the same PackageCode (SummaryInformation PID 9)'
}

$mutationTarget = Join-Path $WorkingDirectory 'causal-mismatch.msi'
Copy-Item -LiteralPath $second -Destination $mutationTarget
$mutationDatabase = [WixToolset.Dtf.WindowsInstaller.Database]::new(
    $mutationTarget,
    [WixToolset.Dtf.WindowsInstaller.DatabaseOpenMode]::Transact
)
try {
    $mutationSql = "UPDATE ``Property`` SET ``Value`` = 'CodeskCausalMismatch' WHERE ``Property`` = 'ProductName'"
    $mutationDatabase.Execute($mutationSql, [object[]] @())
    $mutationDatabase.Commit()
} finally {
    $mutationDatabase.Close()
}
$mutatedSnapshot = Get-NormalizedSnapshot -MsiPath $mutationTarget -Name 'causal'
$causalMismatchRejected = $false
try {
    Assert-SnapshotsEquivalent $firstSnapshot $mutatedSnapshot
} catch {
    if ($_.Exception.Message -notlike '*normalized MSI tables differ*') {
        throw
    }
    $causalMismatchRejected = $true
}
if (-not $causalMismatchRejected) {
    throw 'MSI database causal mismatch was not rejected'
}

$report = [ordered] @{
    schemaVersion = 1
    identity = $firstSnapshot.Identity
    allowedRawContainerDifferences = [ordered] @{
        summaryInformation = @(
            [ordered] @{ pid = 9; name = 'RevisionNumber'; meaning = 'PackageCode' },
            [ordered] @{ pid = 12; name = 'CreateTime' },
            [ordered] @{ pid = 13; name = 'LastSaveTime' }
        )
        explanation = 'Only observed differences in SummaryInformation PID 9, 12, and 13 are allowed; all other summary fields, normalized tables, and extracted resources must be identical.'
    }
    observedAllowedDifferences = $observedAllowedDifferences
    normalized = [ordered] @{
        sourceSha256 = $firstSnapshot.SourceSha256
        databaseTreeSha256 = $firstSnapshot.DatabaseTreeSha256
        databaseFiles = $firstSnapshot.DatabaseFiles
        extractedTreeSha256 = $firstSnapshot.ExtractedTreeSha256
        extractedFiles = $firstSnapshot.ExtractedFiles
    }
    builds = @(
        [ordered] @{
            name = 'first'
            msiSha256 = Get-Sha256 $first
            msiSize = (Get-Item -LiteralPath $first).Length
            summaryInformation = $firstSnapshot.Summary
            validatedAllowedSummaryInformation = $firstSnapshot.ValidatedAllowedSummary
        },
        [ordered] @{
            name = 'second'
            msiSha256 = Get-Sha256 $second
            msiSize = (Get-Item -LiteralPath $second).Length
            summaryInformation = $secondSnapshot.Summary
            validatedAllowedSummaryInformation = $secondSnapshot.ValidatedAllowedSummary
        }
    )
    causalMismatch = [ordered] @{
        kind = 'msi-property-row'
        target = 'Property.ProductName'
        rejected = $causalMismatchRejected
    }
}

$reportDirectory = Split-Path -Parent $ReportPath
if ([string]::IsNullOrEmpty($reportDirectory) -or
    -not (Test-Path -LiteralPath $reportDirectory -PathType Container)) {
    throw "reproducibility report directory is missing: $reportDirectory"
}
$json = ConvertTo-Json $report -Depth 20
[System.IO.File]::WriteAllText($ReportPath, $json + "`n", [System.Text.UTF8Encoding]::new($false))
Write-Host "reproducibility verified: $ExpectedProductVersion $ExpectedProductCode"
