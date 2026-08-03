[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string] $ReleaseDirectory,

    [Parameter(Mandatory = $true)]
    [string] $PayloadDirectory,

    [Parameter(Mandatory = $true)]
    [ValidateSet('amd64', 'arm64')]
    [string] $GoArchitecture,

    [Parameter(Mandatory = $true)]
    [string] $ExpectedVersion
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$MsiErrorSuccess = [uint32] 0
$MsiErrorUnknownProduct = [uint32] 1605
$MsiInstallContextUserUnmanaged = [uint32] 2

if (-not ('CodeskWindowsInstallerNative' -as [type])) {
    Add-Type -TypeDefinition @'
using System.Runtime.InteropServices;
using System.Text;

public static class CodeskWindowsInstallerNative
{
    [DllImport("msi.dll", CharSet = CharSet.Unicode, ExactSpelling = true)]
    public static extern uint MsiGetProductInfoExW(
        string productCode,
        string userSid,
        uint context,
        string property,
        StringBuilder value,
        ref uint valueLength);
}
'@
}

function Assert-True {
    param(
        [Parameter(Mandatory = $true)] [bool] $Condition,
        [Parameter(Mandatory = $true)] [string] $Message
    )

    if (-not $Condition) {
        throw $Message
    }
}

function Get-PackageEvidence {
    param(
        [Parameter(Mandatory = $true)] [string] $Directory,
        [Parameter(Mandatory = $true)] [string] $BuildMode,
        [Parameter(Mandatory = $true)] [bool] $Publishable,
        [Parameter(Mandatory = $true)] [string[]] $Roles
    )

    $resolvedDirectory = (Resolve-Path -LiteralPath $Directory).Path
    $provenancePath = Join-Path $resolvedDirectory 'provenance.json'
    $checksumsPath = Join-Path $resolvedDirectory 'SHA256SUMS'
    Assert-True (Test-Path -LiteralPath $provenancePath -PathType Leaf) "MSI provenance is missing: $provenancePath"
    Assert-True (Test-Path -LiteralPath $checksumsPath -PathType Leaf) "MSI checksums are missing: $checksumsPath"

    $provenance = Get-Content -LiteralPath $provenancePath -Raw | ConvertFrom-Json
    Assert-True ($provenance.schemaVersion -eq 2) "unexpected MSI provenance schema: $($provenance.schemaVersion)"
    Assert-True ($provenance.target.goArchitecture -ceq $GoArchitecture) "MSI architecture does not match the runner"
    Assert-True ($provenance.target.buildMode -ceq $BuildMode) "MSI build mode is $($provenance.target.buildMode), want $BuildMode"
    Assert-True ($provenance.target.publishable -eq $Publishable) "MSI publishability does not match $BuildMode"

    $packages = @($provenance.packages)
    Assert-True ($packages.Count -eq $Roles.Count) "$BuildMode package count is $($packages.Count), want $($Roles.Count)"
    $evidence = @{}
    for ($index = 0; $index -lt $Roles.Count; $index++) {
        $package = $packages[$index]
        $role = $Roles[$index]
        Assert-True ($package.role -ceq $role) "$BuildMode package role $index is $($package.role), want $role"
        Assert-True ($package.productCode -cmatch '^\{[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}\}$') "invalid $role ProductCode"
        Assert-True ($package.version -cmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') "invalid $role ProductVersion"
        Assert-True ([System.IO.Path]::GetFileName([string] $package.canonicalFile) -ceq [string] $package.canonicalFile) "$role MSI filename is not a single path component"
        Assert-True ($package.canonicalSha256 -cmatch '^[0-9a-f]{64}$') "$role MSI provenance hash is invalid"
        $msiPath = Join-Path $resolvedDirectory $package.canonicalFile
        Assert-True (Test-Path -LiteralPath $msiPath -PathType Leaf) "$role MSI is missing: $msiPath"
        $actualHash = (Get-FileHash -LiteralPath $msiPath -Algorithm SHA256).Hash.ToLowerInvariant()
        Assert-True ($actualHash -ceq $package.canonicalSha256) "$role MSI hash does not match provenance"
        $evidence[$role] = [pscustomobject] @{
            Role = $role
            Version = [string] $package.version
            ProductCode = [string] $package.productCode
            MsiPath = $msiPath
        }
    }

    $expectedChecksums = @{}
    foreach ($package in $packages) {
        $expectedChecksums[[string] $package.canonicalFile] = [string] $package.canonicalSha256
    }
    $expectedChecksums['provenance.json'] = (Get-FileHash -LiteralPath $provenancePath -Algorithm SHA256).Hash.ToLowerInvariant()
    $checksumLines = @(Get-Content -LiteralPath $checksumsPath)
    Assert-True ($checksumLines.Count -eq $expectedChecksums.Count) 'SHA256SUMS entry count does not match provenance'
    $seenChecksums = @{}
    foreach ($line in $checksumLines) {
        Assert-True ($line -cmatch '^(?<hash>[0-9a-f]{64})  (?<name>[^/\\\r\n]+)$') "invalid SHA256SUMS line: $line"
        $name = [string] $Matches.name
        $hash = [string] $Matches.hash
        Assert-True ($expectedChecksums.ContainsKey($name)) "SHA256SUMS contains unexpected file: $name"
        Assert-True (-not $seenChecksums.ContainsKey($name)) "SHA256SUMS contains duplicate file: $name"
        Assert-True ($hash -ceq $expectedChecksums[$name]) "SHA256SUMS does not match provenance for $name"
        $seenChecksums[$name] = $true
    }
    return $evidence
}

function Invoke-Msi {
    param(
        [Parameter(Mandatory = $true)] [ValidateSet('install', 'uninstall')] [string] $Operation,
        [Parameter(Mandatory = $true)] [string] $Package,
        [Parameter(Mandatory = $true)] [string] $LogPath,
        [int[]] $AllowedExitCodes = @(0)
    )

    Assert-True (-not $Package.Contains('"')) "MSI package argument contains a quote"
    Assert-True (-not $LogPath.Contains('"')) "MSI log argument contains a quote"
    $verb = if ($Operation -ceq 'install') { '/i' } else { '/x' }
    $arguments = @($verb, "`"$Package`"", '/qn', '/norestart', '/l*v', "`"$LogPath`"")
    $process = Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\msiexec.exe') `
        -ArgumentList $arguments -NoNewWindow -PassThru -Wait
    if ($AllowedExitCodes -notcontains $process.ExitCode) {
        $tail = if (Test-Path -LiteralPath $LogPath -PathType Leaf) {
            (Get-Content -LiteralPath $LogPath -Tail 80) -join "`n"
        } else {
            '<MSI log missing>'
        }
        throw "msiexec $Operation failed with exit $($process.ExitCode):`n$tail"
    }
}

function Get-MsiProductProperty {
    param(
        [Parameter(Mandatory = $true)] [string] $ProductCode,
        [Parameter(Mandatory = $true)] [string] $Property
    )

    [uint32] $length = 0
    $result = [CodeskWindowsInstallerNative]::MsiGetProductInfoExW(
        $ProductCode,
        $null,
        $MsiInstallContextUserUnmanaged,
        $Property,
        $null,
        [ref] $length)
    if ($result -eq $MsiErrorUnknownProduct) {
        return $null
    }
    Assert-True ($result -eq $MsiErrorSuccess) `
        "MsiGetProductInfoExW size query for $Property failed with exit $result"
    if ($length -eq 0) {
        return ''
    }

    $value = [System.Text.StringBuilder]::new([int] $length + 1)
    [uint32] $capacity = $value.Capacity
    $result = [CodeskWindowsInstallerNative]::MsiGetProductInfoExW(
        $ProductCode,
        $null,
        $MsiInstallContextUserUnmanaged,
        $Property,
        $value,
        [ref] $capacity)
    Assert-True ($result -eq $MsiErrorSuccess) "MsiGetProductInfoExW read for $Property failed with exit $result"
    return $value.ToString()
}

function Get-MsiProductRegistration {
    param([Parameter(Mandatory = $true)] [string] $ProductCode)

    $state = Get-MsiProductProperty -ProductCode $ProductCode -Property 'ProductState'
    if ($null -eq $state) {
        return $null
    }
    return [pscustomobject] @{
        ProductState = $state
        DisplayName = Get-MsiProductProperty -ProductCode $ProductCode -Property 'InstalledProductName'
        DisplayVersion = Get-MsiProductProperty -ProductCode $ProductCode -Property 'VersionString'
    }
}

function Assert-ProductAbsent {
    param([Parameter(Mandatory = $true)] $Package)

    Assert-True ($null -eq (Get-MsiProductRegistration -ProductCode $Package.ProductCode)) "$($Package.Role) product remains registered"
}

function Assert-InstalledProduct {
    param(
        [Parameter(Mandatory = $true)] $Package,
        [Parameter(Mandatory = $true)] [string] $InstallDirectory,
        [Parameter(Mandatory = $true)] [string] $ExpectedCodesk,
        [Parameter(Mandatory = $true)] [string] $ExpectedAgentTool,
        [Parameter(Mandatory = $true)] [string] $ShortcutPath
    )

    $registration = Get-MsiProductRegistration -ProductCode $Package.ProductCode
    Assert-True ($null -ne $registration) "$($Package.Role) product is not registered"
    Assert-True ($registration.ProductState -ceq '5') "$($Package.Role) product state is $($registration.ProductState), want installed"
    Assert-True ($registration.DisplayName -ceq 'Codesk') "$($Package.Role) DisplayName is not Codesk"
    Assert-True ($registration.DisplayVersion -ceq $Package.Version) "$($Package.Role) DisplayVersion is $($registration.DisplayVersion), want $($Package.Version)"

    $installedCodesk = Join-Path $InstallDirectory 'Codesk.exe'
    $installedAgentTool = Join-Path $InstallDirectory 'notty-agent-tool.exe'
    foreach ($path in @($installedCodesk, $installedAgentTool, $ShortcutPath)) {
        Assert-True (Test-Path -LiteralPath $path -PathType Leaf) "installed MSI payload is missing: $path"
    }
    Assert-True ((Get-FileHash -LiteralPath $installedCodesk -Algorithm SHA256).Hash -ceq (Get-FileHash -LiteralPath $ExpectedCodesk -Algorithm SHA256).Hash) 'installed Codesk.exe does not match the validated payload'
    Assert-True ((Get-FileHash -LiteralPath $installedAgentTool -Algorithm SHA256).Hash -ceq (Get-FileHash -LiteralPath $ExpectedAgentTool -Algorithm SHA256).Hash) 'installed agent tool does not match the validated payload'

    $shell = New-Object -ComObject WScript.Shell
    try {
        $shortcut = $shell.CreateShortcut($ShortcutPath)
        Assert-True ([System.IO.Path]::GetFullPath($shortcut.TargetPath) -ceq [System.IO.Path]::GetFullPath($installedCodesk)) 'Start Menu shortcut targets the wrong executable'
    } finally {
        [System.Runtime.InteropServices.Marshal]::FinalReleaseComObject($shell) | Out-Null
    }

    $componentKey = 'HKCU:\Software\Codesk\Installer\Components'
    Assert-True (Test-Path -LiteralPath $componentKey) 'MSI component registry key is missing'
    $components = Get-ItemProperty -LiteralPath $componentKey
    Assert-True ($components.CodeskExecutable -eq 1) 'Codesk component registration is missing'
    Assert-True ($components.AgentToolExecutable -eq 1) 'agent tool component registration is missing'

    $actualVersion = (& $installedAgentTool --version).Trim()
    Assert-True ($LASTEXITCODE -eq 0) 'installed agent tool --version failed'
    Assert-True ($actualVersion -ceq $ExpectedVersion) "installed agent tool version is $actualVersion, want $ExpectedVersion"
}

if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw 'Windows desktop MSI lifecycle requires a real Windows host'
}

$release = Get-PackageEvidence -Directory $ReleaseDirectory -BuildMode 'release' -Publishable $true -Roles @('release')
Assert-True ($release.release.Version -ceq $ExpectedVersion) "release MSI version is $($release.release.Version), want $ExpectedVersion"

$payloadRoot = (Resolve-Path -LiteralPath $PayloadDirectory).Path
$expectedCodesk = Join-Path $payloadRoot 'Codesk.exe'
$expectedAgentTool = Join-Path $payloadRoot 'notty-agent-tool.exe'
foreach ($path in @($expectedCodesk, $expectedAgentTool)) {
    Assert-True (Test-Path -LiteralPath $path -PathType Leaf) "expected MSI payload is missing: $path"
}

$installDirectory = Join-Path $env:LOCALAPPDATA 'Programs\Codesk'
$shortcutPath = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\Codesk\Codesk.lnk'
$logDirectory = Join-Path $env:RUNNER_TEMP "codesk-msi-lifecycle-$GoArchitecture"
New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null

# Remove only the exact deterministic ProductCode this test owns. Any other Codesk installation
# remains a hard contamination failure rather than being deleted from a shared runner.
foreach ($package in @($release.release)) {
    if ($null -ne (Get-MsiProductRegistration -ProductCode $package.ProductCode)) {
        Invoke-Msi -Operation uninstall -Package $package.ProductCode `
            -LogPath (Join-Path $logDirectory "baseline-$($package.Role)-uninstall.log")
    }
    Assert-ProductAbsent -Package $package
}
Assert-True (-not (Test-Path -LiteralPath $installDirectory)) "pre-existing Codesk install directory contaminates the runner: $installDirectory"
Assert-True (-not (Test-Path -LiteralPath $shortcutPath)) "pre-existing Codesk shortcut contaminates the runner: $shortcutPath"

try {
    Invoke-Msi -Operation install -Package $release.release.MsiPath -LogPath (Join-Path $logDirectory 'release-install.log')
    Assert-InstalledProduct -Package $release.release -InstallDirectory $installDirectory `
        -ExpectedCodesk $expectedCodesk -ExpectedAgentTool $expectedAgentTool -ShortcutPath $shortcutPath
    Invoke-Msi -Operation uninstall -Package $release.release.MsiPath -LogPath (Join-Path $logDirectory 'release-uninstall.log')
    Assert-ProductAbsent -Package $release.release
    Assert-True (-not (Test-Path -LiteralPath $installDirectory)) 'release uninstall left the install directory'
    Assert-True (-not (Test-Path -LiteralPath $shortcutPath)) 'release uninstall left the Start Menu shortcut'
    Assert-True (-not (Test-Path -LiteralPath 'HKCU:\Software\Codesk\Installer\Components')) 'release uninstall left component registry state'
} finally {
    foreach ($package in @($release.release)) {
        try {
            if ($null -ne (Get-MsiProductRegistration -ProductCode $package.ProductCode)) {
                Invoke-Msi -Operation uninstall -Package $package.ProductCode `
                    -LogPath (Join-Path $logDirectory "cleanup-$($package.Role)-uninstall.log")
            }
        } catch {
            Write-Warning $_
        }
    }
}

Write-Host "Windows desktop MSI validation/install/uninstall lifecycle passed for $GoArchitecture"
