& {
    $ErrorActionPreference = 'Stop'

    $ProductHead = '7bd7172b2c9ff7c04ff2d98827dc36644058d276'
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
    $RunKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
    $MarkerKey = 'HKCU:\Software\Codesk\Installer\Components'
    $SentinelName = 'CodeskQASibling_7bd7172'
    $Log = Join-Path $Root 'native-arm64-7bd7172-part2.log'
    $StablePrevious = Join-Path $Root 'Codesk_0.0.1_arm64_7bd7172.msi'
    $StableCandidate = Join-Path $Root 'Codesk_0.0.2_arm64_7bd7172.msi'
    $App = Join-Path $env:LOCALAPPDATA 'Programs\Codesk\Codesk.exe'
    $Agent = Join-Path $env:LOCALAPPDATA 'Programs\Codesk\notty-agent-tool.exe'
    $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\Codesk'
    $StartMenuDir = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\Codesk'
    $Shortcut = Join-Path $StartMenuDir 'Codesk.lnk'
    $ExpectedRun = '"' + $App + '"'
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
        for ($Index = 0; $Index -lt $ArpRoots.Count; $Index++) {
            $LiteralKey = Join-Path $ArpRoots[$Index] $ProductCode
            $Exists = Test-Path -LiteralPath $LiteralKey
            if (($Index -eq 0 -and -not $Exists) -or ($Index -ne 0 -and $Exists)) {
                throw "wrong literal ProductCode placement: $LiteralKey exists=$Exists"
            }
        }
    }

    function Assert-ProductCodeAbsent([string] $ProductCode, [string] $Step) {
        foreach ($ArpRoot in $ArpRoots) {
            $LiteralKey = Join-Path $ArpRoot $ProductCode
            if (Test-Path -LiteralPath $LiteralKey) {
                throw "$Step left a literal ProductCode key: $LiteralKey"
            }
        }
    }

    function Wait-CodeskTreeStopped([string] $Step) {
        $Deadline = [DateTime]::UtcNow.AddSeconds(30)
        do {
            $Remaining = @(Get-Process -Name 'Codesk', 'notty-agent-tool' -ErrorAction SilentlyContinue)
            if ($Remaining.Count -eq 0) {
                return
            }
            Start-Sleep -Milliseconds 250
        } while ([DateTime]::UtcNow -lt $Deadline)
        throw "$Step did not stop the Codesk process tree: $($Remaining.Name -join ', ')"
    }

    $TranscriptStarted = $false
    try {
        Start-Transcript -LiteralPath $Log -Force | Out-Null
        $TranscriptStarted = $true

        $ArchSignals = @($env:PROCESSOR_ARCHITECTURE, $env:PROCESSOR_ARCHITEW6432) |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
        if ($ArchSignals -notcontains 'ARM64') {
            throw "native ARM64 Windows host required, got: $($ArchSignals -join ', ')"
        }
        Assert-FileHash $StablePrevious $PreviousHash 'staged previous MSI'
        Assert-FileHash $StableCandidate $CandidateHash 'staged candidate MSI'
        Assert-FileHash (Join-Path $Bundle 'SHA256SUMS') $ChecksumsHash 'canonical SHA256SUMS'
        Assert-FileHash (Join-Path $Bundle 'provenance.json') $ProvenanceHash 'canonical provenance'
        $Provenance = Get-Content -LiteralPath (Join-Path $Bundle 'provenance.json') -Raw | ConvertFrom-Json
        $Packages = @($Provenance.packages)
        $PreviousPackage = @($Packages | Where-Object { $_.role -eq 'previous' })
        $CandidatePackage = @($Packages | Where-Object { $_.role -eq 'candidate' })
        if ([string] $Provenance.source.sourceHead -cne $ProductHead -or
            [string] $Provenance.target.architecture -cne 'ARM64' -or
            $Packages.Count -ne 2 -or
            $PreviousPackage.Count -ne 1 -or
            $CandidatePackage.Count -ne 1 -or
            [string] $PreviousPackage[0].canonicalSha256 -cne $PreviousHash -or
            [string] $CandidatePackage[0].canonicalSha256 -cne $CandidateHash) {
            throw 'canonical artifact identity changed between Part 1 and Part 2'
        }
        'PART 2 INPUT OK: exact v6 ARM64 artifact remains hash-bound'

        $PreviousObservation = Read-Host 'Type PREVIOUS-PASS only if the 0.0.1 app launch, connect, real turn, sign-out/sign-in, and autostart all succeeded with no terminal flash'
        if ($PreviousObservation -cne 'PREVIOUS-PASS') {
            throw 'previous-version native turn was not accepted'
        }

        $Deadline = [DateTime]::UtcNow.AddSeconds(30)
        do {
            $CodeskProcess = @(Get-Process -Name 'Codesk' -ErrorAction SilentlyContinue)
            if ($CodeskProcess.Count -eq 1) {
                break
            }
            Start-Sleep -Milliseconds 500
        } while ([DateTime]::UtcNow -lt $Deadline)
        if ($CodeskProcess.Count -ne 1) {
            throw "expected exactly one auto-started Codesk process, found $($CodeskProcess.Count)"
        }
        if ($CodeskProcess[0].Path -ne $App) {
            throw "wrong auto-started Codesk path: $($CodeskProcess[0].Path)"
        }
        if ((Get-PeMachine $App) -ne 0xAA64 -or (Get-PeMachine $Agent) -ne 0xAA64) {
            throw 'auto-started installed payload PE machine is not ARM64 (0xAA64)'
        }
        $ActualRun = [string] (Get-RegistryValue $RunKey 'Codesk')
        if ($ActualRun -cne $ExpectedRun) {
            throw "wrong Codesk Run command; expected '$ExpectedRun', got '$ActualRun'"
        }
        if ([string] (Get-RegistryValue $RunKey $SentinelName) -ne '') {
            throw 'sibling Run sentinel changed before repair'
        }
        Assert-SingleArp $PreviousCode '0.0.1'
        Assert-ProductCodeAbsent $CandidateCode 'pre-upgrade state'
        Assert-FileHash $App $CodeskHash 'auto-started Codesk'
        Assert-FileHash $Agent $AgentHash 'installed agent before repair'
        if ([string] (Get-RegistryValue $MarkerKey 'CodeskExecutable') -ne '1' -or
            [string] (Get-RegistryValue $MarkerKey 'AgentToolExecutable') -ne '1') {
            throw 'private installer marker values are wrong before repair'
        }
        'AUTOSTART 0.0.1 OK: exact process, Run command, ARP, ARM64 payloads, markers, and sibling sentinel'

        'Select Quit from the auto-started Codesk tray menu now; do not use Task Manager or Stop-Process.'
        $PreviousQuit = Read-Host 'Type PREVIOUS-QUIT only after selecting Quit from the tray menu'
        if ($PreviousQuit -cne 'PREVIOUS-QUIT') {
            throw 'previous-version graceful tray Quit was not attested'
        }
        Wait-CodeskTreeStopped 'previous-version graceful tray Quit'
        $RepairLog = Join-Path $Root 'native-arm64-7bd7172-repair-prev.log'
        $Process = Start-Process msiexec.exe -ArgumentList @(
            '/famus', $PreviousCode, '/qn', '/norestart', '/l*v', $RepairLog
        ) -Wait -PassThru
        Assert-Exit $Process.ExitCode 'repair previous MSI'
        Assert-FileHash $App $CodeskHash 'Codesk after repair'
        Assert-FileHash $Agent $AgentHash 'agent after repair'
        Assert-SingleArp $PreviousCode '0.0.1'
        Assert-ProductCodeAbsent $CandidateCode 'repair'
        if (-not (Test-Path -LiteralPath $Shortcut -PathType Leaf)) {
            throw 'Start Menu shortcut missing after repair'
        }
        if ([string] (Get-RegistryValue $RunKey 'Codesk') -cne $ExpectedRun) {
            throw 'repair clobbered the enabled Codesk Run command'
        }
        if ([string] (Get-RegistryValue $RunKey $SentinelName) -ne '') {
            throw 'repair clobbered the sibling Run sentinel'
        }
        if ([string] (Get-RegistryValue $MarkerKey 'CodeskExecutable') -ne '1' -or
            [string] (Get-RegistryValue $MarkerKey 'AgentToolExecutable') -ne '1') {
            throw 'private installer markers are wrong after repair'
        }
        'REPAIR 0.0.1 OK'

        $UpgradeLog = Join-Path $Root 'native-arm64-7bd7172-upgrade.log'
        $Process = Start-Process msiexec.exe -ArgumentList @(
            '/i', $StableCandidate, '/qn', '/norestart', '/l*v', $UpgradeLog
        ) -Wait -PassThru
        Assert-Exit $Process.ExitCode 'upgrade to candidate MSI'
        Assert-SingleArp $CandidateCode '0.0.2'
        Assert-ProductCodeAbsent $PreviousCode 'upgrade'
        Assert-FileHash $App $CodeskHash 'Codesk after upgrade'
        Assert-FileHash $Agent $AgentHash 'agent after upgrade'
        if (-not (Test-Path -LiteralPath $Shortcut -PathType Leaf)) {
            throw 'Start Menu shortcut missing after upgrade'
        }
        if ([string] (Get-RegistryValue $RunKey 'Codesk') -cne $ExpectedRun) {
            throw 'upgrade clobbered the enabled Codesk Run command'
        }
        if ([string] (Get-RegistryValue $RunKey $SentinelName) -ne '') {
            throw 'upgrade clobbered the sibling Run sentinel'
        }
        if ([string] (Get-RegistryValue $MarkerKey 'CodeskExecutable') -ne '1' -or
            [string] (Get-RegistryValue $MarkerKey 'AgentToolExecutable') -ne '1') {
            throw 'private installer markers are wrong after upgrade'
        }
        'UPGRADE 0.0.2 OK: exact candidate ARP, payloads, markers, shortcut, Run command, and sibling sentinel'

        Start-Process -FilePath $App
        Start-Sleep -Seconds 3
        if (@(Get-Process -Name 'Codesk' -ErrorAction SilentlyContinue).Count -ne 1) {
            throw 'candidate Codesk did not remain running after launch'
        }
        'Candidate is running. Connect it and run one real Codex turn now.'
        $CandidateObservation = Read-Host 'Type CANDIDATE-PASS only if the 0.0.2 app launch, connect, and real turn all succeed with no terminal flash'
        if ($CandidateObservation -cne 'CANDIDATE-PASS') {
            throw 'candidate native turn was not accepted; preserving installation for diagnosis'
        }

        'Select Quit from the candidate Codesk tray menu now; do not use Task Manager or Stop-Process.'
        $CandidateQuit = Read-Host 'Type CANDIDATE-QUIT only after selecting Quit from the tray menu'
        if ($CandidateQuit -cne 'CANDIDATE-QUIT') {
            throw 'candidate graceful tray Quit was not attested; preserving installation for diagnosis'
        }
        Wait-CodeskTreeStopped 'candidate graceful tray Quit'
        $UninstallLog = Join-Path $Root 'native-arm64-7bd7172-uninstall.log'
        $Process = Start-Process msiexec.exe -ArgumentList @(
            '/x', $CandidateCode, '/qn', '/norestart', '/l*v', $UninstallLog
        ) -Wait -PassThru
        Assert-Exit $Process.ExitCode 'uninstall candidate MSI'
        if (@(Get-Process -Name 'Codesk', 'notty-agent-tool' -ErrorAction SilentlyContinue).Count -ne 0) {
            throw 'Codesk process survived uninstall'
        }
        if (Test-Path -LiteralPath $InstallDir) {
            throw "install directory survived uninstall: $InstallDir"
        }
        if (Test-Path -LiteralPath $StartMenuDir) {
            throw "Start Menu directory survived uninstall: $StartMenuDir"
        }
        if (@(Get-CodeskArpEntries).Count -ne 0) {
            throw "Codesk ARP entry survived uninstall: $(@(Get-CodeskArpEntries) | ConvertTo-Json -Compress)"
        }
        Assert-ProductCodeAbsent $PreviousCode 'uninstall'
        Assert-ProductCodeAbsent $CandidateCode 'uninstall'
        Assert-RegistryValueAbsent $RunKey 'Codesk'
        if (Test-Path -LiteralPath $MarkerKey) {
            throw "private installer marker path survived uninstall: $MarkerKey"
        }
        if ([string] (Get-RegistryValue $RunKey $SentinelName) -ne '') {
            throw 'uninstall removed or changed the sibling Run sentinel'
        }
        'UNINSTALL OK: no payload, directory, shortcut, ARP, Run, process, or marker orphan; sibling value preserved'

        Remove-ItemProperty -LiteralPath $RunKey -Name $SentinelName -Force
        Assert-RegistryValueAbsent $RunKey $SentinelName
        'PART 2 PASS - sibling QA sentinel removed after preservation proof'
        "POST THESE LOGS: $Log, native-arm64-7bd7172-install-prev.log, native-arm64-7bd7172-repair-prev.log, native-arm64-7bd7172-upgrade.log, native-arm64-7bd7172-uninstall.log"
    } catch {
        "PART 2 FAILED: $($_.Exception.Message)"
        throw
    } finally {
        if ($TranscriptStarted) {
            try { Stop-Transcript | Out-Null } catch { }
        }
    }
}
