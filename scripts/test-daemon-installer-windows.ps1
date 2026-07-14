[CmdletBinding()]
param(
    [string]$Amd64Fixture = ""
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"

$repoDir = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))
$installer = Join-Path $repoDir "deploy\daemons\install.ps1"
$runner = Join-Path $repoDir "deploy\daemons\run-windows.ps1"
$uninstaller = Join-Path $repoDir "deploy\daemons\uninstall.ps1"

function Assert-True {
    param(
        [bool]$Condition,
        [string]$Message
    )
    if (-not $Condition) {
        throw "Windows installer test failed: $Message"
    }
}

function Assert-File {
    param([string]$Path)
    Assert-True (Test-Path -LiteralPath $Path -PathType Leaf) "expected file $Path"
}

function Assert-Missing {
    param([string]$Path)
    Assert-True (-not (Test-Path -LiteralPath $Path)) "expected $Path to be absent"
}

function Get-PEMachine {
    param([string]$Path)

    $stream = [IO.File]::OpenRead($Path)
    try {
        $reader = [IO.BinaryReader]::new($stream)
        if ($reader.ReadUInt16() -ne 0x5A4D) {
            throw "fixture is not a PE executable: $Path"
        }
        $stream.Position = 0x3C
        $peOffset = $reader.ReadInt32()
        if ($peOffset -lt 0x40 -or $peOffset -gt ($stream.Length - 6)) {
            throw "fixture has an invalid PE header offset: $Path"
        }
        $stream.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x00004550) {
            throw "fixture has an invalid PE signature: $Path"
        }
        return $reader.ReadUInt16()
    } finally {
        $stream.Dispose()
    }
}

$installerAst = $null
foreach ($scriptPath in @($installer, $runner, $uninstaller, $PSCommandPath)) {
    $tokens = $null
    $parseErrors = $null
    $scriptAst = [Management.Automation.Language.Parser]::ParseFile($scriptPath, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0) {
        $messages = ($parseErrors | ForEach-Object { $_.Message }) -join "; "
        throw "PowerShell parse failed for ${scriptPath}: $messages"
    }
    if ($scriptPath -eq $installer) {
        $installerAst = $scriptAst
    }
}

Assert-True ($null -ne $installerAst) "installer AST was not captured"
$hostGuard = $installerAst.Find({
        param($node)
        $node -is [Management.Automation.Language.FunctionDefinitionAst] -and
            $node.Name -eq "Assert-SupportedWindowsHost"
    }, $true)
Assert-True ($null -ne $hostGuard) "installer host support guard was not found"
Invoke-Expression $hostGuard.Extent.Text

$hostGuardCall = $installerAst.Find({
        param($node)
        $node -is [Management.Automation.Language.CommandAst] -and
            $node.GetCommandName() -eq "Assert-SupportedWindowsHost"
    }, $true)
$userHomeCall = $installerAst.Find({
        param($node)
        $node -is [Management.Automation.Language.CommandAst] -and
            $node.GetCommandName() -eq "Get-UserHome"
    }, $true)
Assert-True ($null -ne $hostGuardCall -and $null -ne $userHomeCall) "installer host guard wiring was not found"
Assert-True ($hostGuardCall.Extent.StartOffset -lt $userHomeCall.Extent.StartOffset) "installer host guard runs after installation path resolution"

foreach ($architecture in @("ARM64", "aarch64")) {
    $windows10ArmRejected = $false
    try {
        Assert-SupportedWindowsHost -MachineArchitecture $architecture -WindowsBuildNumber 19045
    } catch {
        $windows10ArmRejected = $_.Exception.Message -like "*Windows 11 or newer*ARM64*19045*"
    }
    Assert-True $windows10ArmRejected "Windows 10 $architecture was not rejected"
    Assert-SupportedWindowsHost -MachineArchitecture $architecture -WindowsBuildNumber 22000
}
foreach ($architecture in @("AMD64", "x86_64")) {
    Assert-SupportedWindowsHost -MachineArchitecture $architecture -WindowsBuildNumber 19045
}
$unsupportedArchitectureRejected = $false
try {
    Assert-SupportedWindowsHost -MachineArchitecture "x86" -WindowsBuildNumber 22631
} catch {
    $unsupportedArchitectureRejected = $_.Exception.Message -like "*AMD64 or ARM64 is required*x86*"
}
Assert-True $unsupportedArchitectureRejected "unsupported Windows architecture was not rejected"

if ($env:OS -ne "Windows_NT") {
    throw "Windows installer lifecycle tests require Windows"
}

$tempDir = Join-Path ([IO.Path]::GetTempPath()) ("codesk-windows-installer-test-" + [Guid]::NewGuid().ToString("N"))
$staticRoot = Join-Path $tempDir "static"
$version = "test"
$packageName = "notty-daemon_${version}_windows_amd64"
$releaseDir = Join-Path $staticRoot $version
$packageDir = Join-Path $releaseDir $packageName
$archive = Join-Path $releaseDir "$packageName.zip"
$testHome = Join-Path $tempDir "home"
$installDir = Join-Path $tempDir "install"
$dataDir = Join-Path $tempDir "data"
$workspaceId = "ws-installer-$([Guid]::NewGuid().ToString('N'))"

$savedEnvironment = @{}
foreach ($name in @("USERPROFILE", "PROCESSOR_ARCHITECTURE", "PROCESSOR_ARCHITEW6432", "NOTTY_CODEX_COMMAND", "NOTTY_CLAUDE_COMMAND", "NOTTY_INSTALL_DIR", "NOTTY_DATA_DIR")) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
}

New-Item -ItemType Directory -Path (Join-Path $packageDir "bin"), (Join-Path $staticRoot "latest"), $testHome -Force | Out-Null
$testExecutable = if ([string]::IsNullOrWhiteSpace($Amd64Fixture)) {
    Join-Path $env:SystemRoot "System32\where.exe"
} else {
    [IO.Path]::GetFullPath($Amd64Fixture)
}
Assert-File $testExecutable
$fixtureMachine = Get-PEMachine $testExecutable
Assert-True ($fixtureMachine -eq 0x8664) "fixture must be an AMD64 PE executable; got 0x$($fixtureMachine.ToString('X4')). Pass -Amd64Fixture on ARM64 hosts."
Copy-Item -LiteralPath $testExecutable -Destination (Join-Path $packageDir "bin\notty-daemon.exe")
Copy-Item -LiteralPath $testExecutable -Destination (Join-Path $packageDir "bin\notty-agent-tool.exe")
Copy-Item -LiteralPath $runner -Destination (Join-Path $packageDir "run-windows.ps1")
Compress-Archive -LiteralPath $packageDir -DestinationPath $archive -Force
$archiveHash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath (Join-Path $releaseDir "SHA256SUMS") -Value "$archiveHash  $packageName.zip" -Encoding ASCII
Set-Content -LiteralPath (Join-Path $staticRoot "latest\manifest.json") -Value '{"version":"test"}' -Encoding ASCII
$staticBase = ([Uri]::new($staticRoot.TrimEnd("\") + "\")).AbsoluteUri.TrimEnd("/")

try {
    $env:USERPROFILE = $testHome
    $env:NOTTY_CODEX_COMMAND = Join-Path $tempDir "missing-codex.exe"
    $env:NOTTY_CLAUDE_COMMAND = Join-Path $tempDir "missing-claude.exe"
    $env:NOTTY_INSTALL_DIR = $installDir
    $env:NOTTY_DATA_DIR = $dataDir

    & $installer `
        -BackendUrl "https://api.example.test/" `
        -WorkspaceId $workspaceId `
        -DaemonToken "nottyd_test_secret" `
        -StaticBase $staticBase `
        -NoService

    $daemonName = $workspaceId -replace "[^A-Za-z0-9_.-]", "-"
    $binaryDir = Join-Path $installDir $version
    $daemonDir = Join-Path (Join-Path $dataDir "daemons") $daemonName
    $configPath = Join-Path $daemonDir "daemon.env.json"
    Assert-File (Join-Path $binaryDir "notty-daemon.exe")
    Assert-File (Join-Path $binaryDir "notty-agent-tool.exe")
    Assert-File (Join-Path $daemonDir "run.ps1")
    Assert-File $configPath
    Assert-File (Join-Path $daemonDir "service.json")

    $config = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json
    Assert-True ($config.NOTTY_BACKEND_URL -eq "https://api.example.test") "backend URL was not normalized"
    Assert-True ($config.NOTTY_WORKSPACE_ID -eq $workspaceId) "workspace ID was not preserved"
    Assert-True ($config.NOTTY_DAEMON_TOKEN -eq "nottyd_test_secret") "daemon token was not preserved"
    Assert-True ($config.NOTTY_DAEMON_VERSION -eq $version) "daemon version was not preserved"
    Assert-True ($config.NOTTY_BINARY_DIR -eq $binaryDir) "versioned binary directory was not persisted"
    Assert-True ($config.NOTTY_CODEX_COMMAND -eq $env:NOTTY_CODEX_COMMAND) "Codex command was not preserved"
    Assert-True ((Get-Acl -LiteralPath $configPath).AreAccessRulesProtected) "daemon config still inherits ACL entries"

    $service = Get-Content -LiteralPath (Join-Path $daemonDir "service.json") -Raw | ConvertFrom-Json
    Assert-True ($service.Mode -eq "none") "-NoService unexpectedly registered a launcher"

    $originalChecksums = Get-Content -LiteralPath (Join-Path $releaseDir "SHA256SUMS") -Raw
    Set-Content `
        -LiteralPath (Join-Path $releaseDir "SHA256SUMS") `
        -Value "0000000000000000000000000000000000000000000000000000000000000000  $packageName.zip" `
        -Encoding ASCII
    $checksumRejected = $false
    try {
        & $installer `
            -BackendUrl "https://api.example.test" `
            -WorkspaceId "ws-bad-checksum" `
            -DaemonToken "nottyd_bad" `
            -StaticBase $staticBase `
            -InstallDir (Join-Path $tempDir "bad-install") `
            -DataDir (Join-Path $tempDir "bad-data") `
            -NoService
    } catch {
        $checksumRejected = $_.Exception.Message -like "*checksum mismatch*"
    }
    Assert-True $checksumRejected "checksum mismatch was not rejected"
    Set-Content -LiteralPath (Join-Path $releaseDir "SHA256SUMS") -Value $originalChecksums.TrimEnd() -Encoding ASCII

    & $installer `
        -BackendUrl "https://api.example.test" `
        -WorkspaceId $workspaceId `
        -DaemonToken "nottyd_test_secret" `
        -StaticBase $staticBase

    $service = Get-Content -LiteralPath (Join-Path $daemonDir "service.json") -Raw | ConvertFrom-Json
    Assert-True ($service.Mode -in @("scheduled-task", "startup-shortcut")) "installer did not register a user launcher"
    if ($service.Mode -eq "scheduled-task") {
        $registeredTask = Get-ScheduledTask -TaskName $service.TaskName -ErrorAction SilentlyContinue
        Assert-True ($null -ne $registeredTask) "Scheduled Task was not registered"
        $taskArguments = [string]$registeredTask.Actions[0].Arguments
        Assert-True ($taskArguments -like "*-WindowStyle Hidden*") "Scheduled Task launcher is not hidden"
    } else {
        Assert-File $service.StartupLink
        $shell = New-Object -ComObject WScript.Shell
        $shortcut = $shell.CreateShortcut($service.StartupLink)
        Assert-True ([string]$shortcut.Arguments -like "*-WindowStyle Hidden*") "Startup shortcut launcher is not hidden"
    }
    $launcherPidPath = Join-Path $daemonDir "launcher.pid"
    for ($attempt = 0; $attempt -lt 50 -and -not (Test-Path -LiteralPath $launcherPidPath -PathType Leaf); $attempt++) {
        Start-Sleep -Milliseconds 100
    }
    Assert-File $launcherPidPath

    $daemonLogPath = Join-Path $daemonDir "daemon.log"
    $daemonLog = ""
    for ($attempt = 0; $attempt -lt 70; $attempt++) {
        if (Test-Path -LiteralPath $daemonLogPath -PathType Leaf) {
            $daemonLog = Get-Content -LiteralPath $daemonLogPath -Raw
            if ($daemonLog -like "*Codesk daemon exited with code*") {
                break
            }
        }
        Start-Sleep -Milliseconds 100
    }
    Assert-True ($daemonLog -like "*Codesk daemon exited with code*") "launcher did not record the native daemon exit"
    Assert-True ($daemonLog -notlike "*Codesk daemon launch failed:*") "native stderr triggered the launcher catch path"

    $missingAllRejected = $false
    try {
        & $uninstaller -InstallDir $installDir -DataDir $dataDir
    } catch {
        $missingAllRejected = $_.Exception.Message -like "*-All is required*"
    }
    Assert-True $missingAllRejected "uninstall without -All was not rejected"

    & $uninstaller -All -InstallDir $installDir -DataDir $dataDir
    if ($service.Mode -eq "scheduled-task") {
        $registeredTask = Get-ScheduledTask -TaskName $service.TaskName -ErrorAction SilentlyContinue
        Assert-True ($null -eq $registeredTask) "Scheduled Task was not removed"
    } else {
        Assert-Missing $service.StartupLink
    }
    Assert-Missing (Join-Path $dataDir "daemons")
    Assert-Missing (Join-Path $testHome "Notty\workspaces")
    Assert-Missing (Join-Path $testHome "Notty\agents")
    Assert-Missing (Join-Path $binaryDir "notty-daemon.exe")
    Assert-Missing (Join-Path $binaryDir "notty-agent-tool.exe")

    $keepDataDir = Join-Path $tempDir "keep-data"
    $keepWorkspaceId = "ws-keep-$([Guid]::NewGuid().ToString('N'))"
    & $installer `
        -BackendUrl "https://api.example.test" `
        -WorkspaceId $keepWorkspaceId `
        -DaemonToken "nottyd_keep" `
        -StaticBase $staticBase `
        -InstallDir $installDir `
        -DataDir $keepDataDir `
        -NoService
    & $uninstaller -All -InstallDir $installDir -DataDir $keepDataDir -KeepBinaries
    Assert-File (Join-Path $binaryDir "notty-daemon.exe")
    Assert-File (Join-Path $binaryDir "notty-agent-tool.exe")
    Assert-Missing (Join-Path $keepDataDir "daemons")

    Write-Host "Windows daemon installer tests passed"
} finally {
    foreach ($entry in $savedEnvironment.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable($entry.Key, $entry.Value, "Process")
    }
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
