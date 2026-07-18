& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $ProductHead = '7bd7172b2c9ff7c04ff2d98827dc36644058d276'
    $SourceBase = '1a2a467ff4e1507ae55262a928dec1ba9bc69aff'
    $CheckoutCommit = '986219a0a187e17f4a9f9497f2e85277f88d0ab6'
    $RunId = '29649667401'
    $ArtifactId = '8431239739'
    $ArtifactName = 'windows-desktop-msi-arm64'
    $ArtifactSize = 17327270
    $ArchiveHash = '6f6b0371d50698fcb5758888a4735b44cf9d95efe9f9300bc25f112a6c6b2c3d'
    $PreviousHash = 'eb3384b8b805e21c017ba5705d6587362875584733175a7be4bbb1a07ada2344'
    $CandidateHash = 'd417db25c3cd2b56ebae385bc37c454cf35793d2a71dcca58b72a104acc6c06c'
    $ChecksumsHash = '2fc83ee2e6b3eb539ef150147c0700c808bcfcaa8e47fa965bc31e1b071b0959'
    $ProvenanceHash = 'ec51a3085d7ef01eb0364ca02618012c7b3cfb1a91e14eb73bc1342de1d0fcd0'
    $CodeskHash = 'cfae60d740a1a6a01953cdebf7b9cd8739ca76ad546aff3cd53c80252d74c242'
    $AgentHash = '8e4c18e441661435084883079d730c5f76abdcadae568168eea9bc49e8f9e3bb'
    $PreviousCode = '{83D25A98-8C7D-4DB0-98F7-95BA31732600}'
    $CandidateCode = '{3E947E2D-775C-4580-827D-4DC7368186F4}'
    $Root = 'C:\CodeskQA'
    $Bundle = Join-Path $Root 'artifact-arm64-7bd7172'
    $Archive = Join-Path $Root 'windows-desktop-msi-arm64-8431239739.zip'
    $StablePrevious = Join-Path $Root 'Codesk_0.0.1_arm64_7bd7172.msi'
    $StableCandidate = Join-Path $Root 'Codesk_0.0.2_arm64_7bd7172.msi'
    $RunKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
    $MarkerKey = 'HKCU:\Software\Codesk\Installer\Components'
    $SentinelName = 'CodeskQASibling_7bd7172'
    $Log = Join-Path $Root 'native-arm64-7bd7172-part1.log'
    $ArpRoots = @(
        'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall',
        'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall',
        'HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall'
    )

    function Assert-Exit([int] $Code, [string] $Step) {
        if ($Code -ne 0) {
            throw "$Step failed: exit $Code"
        }
    }

    function Assert-Equal([object] $Actual, [object] $Expected, [string] $Step) {
        if ([string] $Actual -cne [string] $Expected) {
            throw "$Step mismatch: expected '$Expected', got '$Actual'"
        }
    }

    function Get-SHA256([string] $Path) {
        (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
    }

    function Assert-FileHash([string] $Path, [string] $Expected, [string] $Step) {
        if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
            throw "$Step missing: $Path"
        }
        $Actual = Get-SHA256 $Path
        if ($Actual -cne $Expected) {
            throw "$Step hash mismatch: expected $Expected, got $Actual"
        }
    }

    function Reset-Directory([string] $Path) {
        $RootPrefix = $Root.TrimEnd('\') + '\'
        if (-not $Path.StartsWith($RootPrefix, [StringComparison]::OrdinalIgnoreCase) -or
            $Path.TrimEnd('\') -ieq $Root.TrimEnd('\')) {
            throw "refusing unsafe reset path: $Path"
        }
        if (Test-Path -LiteralPath $Path) {
            Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction Stop
        }
        if (Test-Path -LiteralPath $Path) {
            throw "reset did not remove: $Path"
        }
        New-Item -ItemType Directory -Path $Path -ErrorAction Stop | Out-Null
        if (@(Get-ChildItem -LiteralPath $Path -Force).Count -ne 0) {
            throw "reset did not create an empty directory: $Path"
        }
    }

    function Get-PeMachine([string] $Path) {
        $Stream = [System.IO.File]::OpenRead($Path)
        $Reader = New-Object System.IO.BinaryReader($Stream)
        try {
            if ($Reader.ReadUInt16() -ne 0x5A4D) {
                throw "not a PE file: $Path"
            }
            $Stream.Position = 0x3C
            $PeOffset = $Reader.ReadInt32()
            $Stream.Position = $PeOffset
            if ($Reader.ReadUInt32() -ne 0x00004550) {
                throw "invalid PE signature: $Path"
            }
            return $Reader.ReadUInt16()
        } finally {
            $Reader.Dispose()
            $Stream.Dispose()
        }
    }

    function Get-CodeskArpEntries {
        $Rows = @()
        foreach ($ArpRoot in $ArpRoots) {
            foreach ($Item in @(Get-ChildItem -LiteralPath $ArpRoot -ErrorAction SilentlyContinue)) {
                $Props = Get-ItemProperty -LiteralPath $Item.PSPath -ErrorAction SilentlyContinue
                if ($null -ne $Props -and $Props.DisplayName -like '*Codesk*') {
                    $Rows += [PSCustomObject]@{
                        Root = $ArpRoot
                        ProductCode = $Item.PSChildName
                        DisplayName = [string] $Props.DisplayName
                        DisplayVersion = [string] $Props.DisplayVersion
                    }
                }
            }
        }
        $Rows
    }

    function Get-RegistryValue([string] $KeyPath, [string] $Name) {
        $Key = Get-Item -LiteralPath $KeyPath -ErrorAction SilentlyContinue
        if ($null -eq $Key -or -not ($Key.GetValueNames() -contains $Name)) {
            throw "registry value missing: $KeyPath\$Name"
        }
        $Key.GetValue($Name, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    }

    function Assert-RegistryValueAbsent([string] $KeyPath, [string] $Name) {
        $Key = Get-Item -LiteralPath $KeyPath -ErrorAction SilentlyContinue
        if ($null -ne $Key -and $Key.GetValueNames() -contains $Name) {
            throw "registry value must be absent: $KeyPath\$Name"
        }
    }

    function Assert-SingleArp([string] $ProductCode, [string] $Version) {
        $Entries = @(Get-CodeskArpEntries)
        if ($Entries.Count -ne 1) {
            throw "expected exactly one Codesk ARP entry, found $($Entries.Count): $($Entries | ConvertTo-Json -Compress)"
        }
        $Entry = $Entries[0]
        if ($Entry.Root -ne $ArpRoots[0] -or
            $Entry.ProductCode -ne $ProductCode -or
            $Entry.DisplayName -ne 'Codesk' -or
            $Entry.DisplayVersion -ne $Version) {
            throw "wrong Codesk ARP identity: $($Entry | ConvertTo-Json -Compress)"
        }
    }

    New-Item -ItemType Directory -Path $Root -Force | Out-Null
    $ArchSignals = @($env:PROCESSOR_ARCHITECTURE, $env:PROCESSOR_ARCHITEW6432) |
        Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
    if ($ArchSignals -notcontains 'ARM64') {
        throw "native ARM64 Windows host required, got: $($ArchSignals -join ', ')"
    }

    $GhCommand = Get-Command gh.exe -CommandType Application -ErrorAction SilentlyContinue
    if ($null -eq $GhCommand) {
        $Winget = Get-Command winget.exe -CommandType Application -ErrorAction SilentlyContinue
        if ($null -eq $Winget) {
            throw 'GitHub CLI is required. Install GitHub CLI, then rerun this script.'
        }
        & $Winget.Source install --id GitHub.cli --exact --source winget --accept-package-agreements --accept-source-agreements
        Assert-Exit $LASTEXITCODE 'install GitHub CLI'
        $InstalledGh = Join-Path $env:ProgramFiles 'GitHub CLI\gh.exe'
        if (-not (Test-Path -LiteralPath $InstalledGh -PathType Leaf)) {
            throw "GitHub CLI installed but is not visible at $InstalledGh; open a new PowerShell window and rerun this script"
        }
        $Gh = $InstalledGh
    } else {
        $Gh = $GhCommand.Source
    }

    & $Gh auth status --hostname github.com *> $null
    if ($LASTEXITCODE -ne 0) {
        & $Gh auth login --hostname github.com --web --git-protocol https
        Assert-Exit $LASTEXITCODE 'authenticate GitHub CLI'
    }
    $Token = [string] (& $Gh auth token --hostname github.com)
    Assert-Exit $LASTEXITCODE 'read GitHub CLI session'
    if ([string]::IsNullOrWhiteSpace($Token)) {
        throw 'GitHub CLI returned an empty session token'
    }

    $MetadataText = [string] (& $Gh api "/repos/XIAZY/notty/actions/artifacts/$ArtifactId")
    Assert-Exit $LASTEXITCODE 'read exact Actions artifact metadata'
    $Metadata = $MetadataText | ConvertFrom-Json
    Assert-Equal $Metadata.id $ArtifactId 'artifact id'
    Assert-Equal $Metadata.name $ArtifactName 'artifact name'
    Assert-Equal $Metadata.size_in_bytes $ArtifactSize 'artifact archive size'
    Assert-Equal $Metadata.expired $false 'artifact expiration'
    Assert-Equal $Metadata.workflow_run.id $RunId 'artifact run id'
    Assert-Equal $Metadata.workflow_run.head_sha $ProductHead 'artifact source head'

    if (Test-Path -LiteralPath $Archive) {
        Remove-Item -LiteralPath $Archive -Force -ErrorAction Stop
    }
    Reset-Directory $Bundle
    Add-Type -AssemblyName System.Net.Http
    $DownloadApi = "https://api.github.com/repos/XIAZY/notty/actions/artifacts/$ArtifactId/zip"
    $Handler = $null
    $Client = $null
    $Response = $null
    $DownloadUri = $null
    try {
        $Handler = New-Object System.Net.Http.HttpClientHandler
        $Handler.AllowAutoRedirect = $false
        $Client = New-Object System.Net.Http.HttpClient -ArgumentList $Handler
        $Client.DefaultRequestHeaders.Authorization =
            New-Object System.Net.Http.Headers.AuthenticationHeaderValue -ArgumentList 'Bearer', $Token
        $AcceptHeader = New-Object System.Net.Http.Headers.MediaTypeWithQualityHeaderValue -ArgumentList 'application/vnd.github+json'
        $Client.DefaultRequestHeaders.Accept.Add($AcceptHeader)
        $Client.DefaultRequestHeaders.UserAgent.ParseAdd('Codesk-Native-Acceptance/1.0')
        [void] $Client.DefaultRequestHeaders.TryAddWithoutValidation('X-GitHub-Api-Version', '2022-11-28')

        $Response = $Client.GetAsync(
            $DownloadApi,
            [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead
        ).GetAwaiter().GetResult()
        $StatusCode = [int] $Response.StatusCode
        if ($StatusCode -ne 302 -and $StatusCode -ne 307) {
            throw "artifact download API returned HTTP $StatusCode instead of a redirect"
        }
        $Location = $Response.Headers.Location
        if ($null -eq $Location -or
            -not $Location.IsAbsoluteUri -or
            $Location.Scheme -cne 'https') {
            throw 'artifact download API returned an unsafe redirect'
        }
        $DownloadUri = $Location.AbsoluteUri
    } finally {
        if ($null -ne $Response) {
            $Response.Dispose()
        }
        if ($null -ne $Client) {
            $Client.DefaultRequestHeaders.Authorization = $null
            $Client.Dispose()
        }
        if ($null -ne $Handler) {
            $Handler.Dispose()
        }
        $Token = $null
    }
    if ([string]::IsNullOrWhiteSpace($DownloadUri)) {
        throw 'artifact download API did not return a usable signed URL'
    }
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $DownloadUri -OutFile $Archive
    } finally {
        $DownloadUri = $null
    }

    $TranscriptStarted = $false
    try {
        Start-Transcript -LiteralPath $Log -Force | Out-Null
        $TranscriptStarted = $true

        Assert-FileHash $Archive $ArchiveHash 'canonical artifact archive'
        if ((Get-Item -LiteralPath $Archive).Length -ne $ArtifactSize) {
            throw "artifact archive size mismatch: expected $ArtifactSize, got $((Get-Item -LiteralPath $Archive).Length)"
        }
        Expand-Archive -LiteralPath $Archive -DestinationPath $Bundle -Force

        $Children = @(Get-ChildItem -LiteralPath $Bundle -Force)
        if (@($Children | Where-Object { $_.PSIsContainer }).Count -ne 0) {
            throw 'canonical bundle must contain root files only'
        }
        $ActualInventory = [string[]] @($Children | ForEach-Object { $_.Name })
        $ExpectedInventory = [string[]] @(
            'Codesk_0.0.1_windows_arm64.msi',
            'Codesk_0.0.2_windows_arm64.msi',
            'SHA256SUMS',
            'provenance.json'
        )
        [Array]::Sort($ActualInventory, [System.StringComparer]::Ordinal)
        [Array]::Sort($ExpectedInventory, [System.StringComparer]::Ordinal)
        if (($ActualInventory | ConvertTo-Json -Compress) -cne ($ExpectedInventory | ConvertTo-Json -Compress)) {
            throw "wrong canonical bundle inventory: $($ActualInventory -join ', ')"
        }

        $PreviousMsi = Join-Path $Bundle 'Codesk_0.0.1_windows_arm64.msi'
        $CandidateMsi = Join-Path $Bundle 'Codesk_0.0.2_windows_arm64.msi'
        $Checksums = Join-Path $Bundle 'SHA256SUMS'
        $ProvenancePath = Join-Path $Bundle 'provenance.json'
        Assert-FileHash $PreviousMsi $PreviousHash 'previous MSI'
        Assert-FileHash $CandidateMsi $CandidateHash 'candidate MSI'
        Assert-FileHash $Checksums $ChecksumsHash 'SHA256SUMS'
        Assert-FileHash $ProvenancePath $ProvenanceHash 'provenance'

        $ExpectedSums = @(
            "$PreviousHash  Codesk_0.0.1_windows_arm64.msi",
            "$CandidateHash  Codesk_0.0.2_windows_arm64.msi",
            "$ProvenanceHash  provenance.json"
        )
        $ActualSums = @([IO.File]::ReadAllLines($Checksums))
        if (($ActualSums | ConvertTo-Json -Compress) -cne ($ExpectedSums | ConvertTo-Json -Compress)) {
            throw 'SHA256SUMS content does not match the exact canonical files'
        }

        $Provenance = Get-Content -LiteralPath $ProvenancePath -Raw | ConvertFrom-Json
        Assert-Equal $Provenance.schemaVersion 2 'provenance schema'
        Assert-Equal $Provenance.source.repository 'XIAZY/notty' 'provenance repository'
        Assert-Equal $Provenance.source.event 'pull_request' 'provenance event'
        Assert-Equal $Provenance.source.checkoutCommit $CheckoutCommit 'provenance checkout commit'
        Assert-Equal $Provenance.source.sourceHead $ProductHead 'provenance source head'
        Assert-Equal $Provenance.source.sourceBase $SourceBase 'provenance source base'
        Assert-Equal $Provenance.source.runId $RunId 'provenance run id'
        Assert-Equal $Provenance.source.runAttempt 1 'provenance run attempt'
        Assert-Equal $Provenance.runner.name 'win' 'provenance runner name'
        Assert-Equal $Provenance.runner.os 'Windows' 'provenance runner OS'
        Assert-Equal $Provenance.runner.architecture 'ARM64' 'provenance runner architecture'
        Assert-Equal $Provenance.target.architecture 'ARM64' 'target architecture'
        Assert-Equal $Provenance.target.goArchitecture 'arm64' 'target Go architecture'
        Assert-Equal $Provenance.target.installerPlatform 'arm64' 'target installer platform'
        Assert-Equal $Provenance.toolchain.dotnetSdk '8.0.423' 'dotnet SDK'
        Assert-Equal $Provenance.toolchain.wixSdk '4.0.5' 'WiX SDK'
        Assert-Equal $Provenance.inputs.CodeskExe.sha256 $CodeskHash 'Codesk input hash'
        Assert-Equal $Provenance.inputs.CodeskExe.size 27081216 'Codesk input size'
        Assert-Equal $Provenance.inputs.AgentToolExe.sha256 $AgentHash 'agent input hash'
        Assert-Equal $Provenance.inputs.AgentToolExe.size 5270528 'agent input size'
        Assert-Equal $Provenance.validation.cleanLinksPerVersion 2 'clean-link count'
        Assert-Equal $Provenance.validation.wixIceSuppressed $false 'ICE suppression'
        Assert-Equal $Provenance.validation.causalMismatch 'msi-property-row' 'causal mismatch'

        $Packages = @($Provenance.packages)
        if ($Packages.Count -ne 2) {
            throw "expected exactly two provenance packages, got $($Packages.Count)"
        }
        $PreviousPackage = @($Packages | Where-Object { $_.role -eq 'previous' })
        $CandidatePackage = @($Packages | Where-Object { $_.role -eq 'candidate' })
        if ($PreviousPackage.Count -ne 1 -or $CandidatePackage.Count -ne 1) {
            throw 'provenance must contain one previous and one candidate package'
        }
        Assert-Equal $PreviousPackage[0].version '0.0.1' 'previous package version'
        Assert-Equal $PreviousPackage[0].productCode $PreviousCode 'previous ProductCode'
        Assert-Equal $PreviousPackage[0].canonicalFile 'Codesk_0.0.1_windows_arm64.msi' 'previous canonical file'
        Assert-Equal $PreviousPackage[0].canonicalSha256 $PreviousHash 'previous canonical hash'
        Assert-Equal $PreviousPackage[0].canonicalSize 8626176 'previous canonical size'
        Assert-Equal $CandidatePackage[0].version '0.0.2' 'candidate package version'
        Assert-Equal $CandidatePackage[0].productCode $CandidateCode 'candidate ProductCode'
        Assert-Equal $CandidatePackage[0].canonicalFile 'Codesk_0.0.2_windows_arm64.msi' 'candidate canonical file'
        Assert-Equal $CandidatePackage[0].canonicalSha256 $CandidateHash 'candidate canonical hash'
        Assert-Equal $CandidatePackage[0].canonicalSize 8626176 'candidate canonical size'
        'CANONICAL ARTIFACT OK: exact run/head/base, four files, checksums, provenance, toolchain, ARM64 identity, and two-link validation'

        Copy-Item -LiteralPath $PreviousMsi -Destination $StablePrevious -Force
        Copy-Item -LiteralPath $CandidateMsi -Destination $StableCandidate -Force
        Assert-FileHash $StablePrevious $PreviousHash 'staged previous MSI'
        Assert-FileHash $StableCandidate $CandidateHash 'staged candidate MSI'

        $App = Join-Path $env:LOCALAPPDATA 'Programs\Codesk\Codesk.exe'
        $Agent = Join-Path $env:LOCALAPPDATA 'Programs\Codesk\notty-agent-tool.exe'
        $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\Codesk'
        $StartMenuDir = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\Codesk'
        if (@(Get-Process -Name 'Codesk', 'notty-agent-tool' -ErrorAction SilentlyContinue).Count -ne 0) {
            throw 'Codesk or notty-agent-tool is already running'
        }
        if (Test-Path -LiteralPath $InstallDir) {
            throw "stale install directory exists: $InstallDir"
        }
        if (Test-Path -LiteralPath $StartMenuDir) {
            throw "stale Start Menu directory exists: $StartMenuDir"
        }
        $Existing = @(Get-CodeskArpEntries)
        if ($Existing.Count -ne 0) {
            throw "pre-existing Codesk ARP entries found; uninstall Codesk through Installed Apps, then rerun: $($Existing | ConvertTo-Json -Compress)"
        }
        Assert-RegistryValueAbsent $RunKey 'Codesk'
        Assert-RegistryValueAbsent $RunKey $SentinelName
        if (Test-Path -LiteralPath $MarkerKey) {
            throw "stale installer marker key exists: $MarkerKey"
        }
        'CLEAN PRESTATE OK'

        $InstallLog = Join-Path $Root 'native-arm64-7bd7172-install-prev.log'
        $Process = Start-Process msiexec.exe -ArgumentList @(
            '/i', $StablePrevious, '/qn', '/norestart', '/l*v', $InstallLog
        ) -Wait -PassThru
        Assert-Exit $Process.ExitCode 'install previous MSI'
        Assert-FileHash $App $CodeskHash 'installed Codesk'
        Assert-FileHash $Agent $AgentHash 'installed agent'
        if ((Get-PeMachine $App) -ne 0xAA64 -or (Get-PeMachine $Agent) -ne 0xAA64) {
            throw 'installed payload PE machine is not ARM64 (0xAA64)'
        }
        Assert-SingleArp $PreviousCode '0.0.1'
        if ([string] (Get-RegistryValue $MarkerKey 'CodeskExecutable') -ne '1' -or
            [string] (Get-RegistryValue $MarkerKey 'AgentToolExecutable') -ne '1') {
            throw 'private installer marker values are wrong after install'
        }
        if ([string] (Get-RegistryValue $RunKey 'Codesk') -ne '') {
            throw 'MSI did not seed the exact empty Codesk Run value'
        }
        $Shortcut = Join-Path $StartMenuDir 'Codesk.lnk'
        if (-not (Test-Path -LiteralPath $Shortcut -PathType Leaf)) {
            throw "Start Menu shortcut missing: $Shortcut"
        }
        New-ItemProperty -LiteralPath $RunKey -Name $SentinelName -PropertyType String -Value '' -Force | Out-Null
        if ([string] (Get-RegistryValue $RunKey $SentinelName) -ne '') {
            throw 'sibling Run sentinel was not created exactly'
        }
        'INSTALL 0.0.1 OK: exact ARP, ARM64 payloads, markers, shortcut, empty Run value, and sibling sentinel'

        Start-Process -FilePath $App
        Start-Sleep -Seconds 3
        if (@(Get-Process -Name 'Codesk' -ErrorAction SilentlyContinue).Count -ne 1) {
            throw 'Codesk did not remain running after launch'
        }
        'PART 1 PASS'
        '1. Connect Codesk and run one real Codex turn.'
        '2. Treat any terminal flash during Codesk use or the turn as RED; note its exact timing.'
        '3. In the tray menu, enable Launch at login.'
        '4. Sign out, sign back in, do not launch Codesk yourself, then run Part 2.'
    } catch {
        "PART 1 FAILED: $($_.Exception.Message)"
        throw
    } finally {
        if ($TranscriptStarted) {
            try { Stop-Transcript | Out-Null } catch { }
        }
    }
}
