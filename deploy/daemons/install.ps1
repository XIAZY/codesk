[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$BackendUrl,

    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$WorkspaceId,

    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$DaemonToken,

    [string]$StaticBase = "",
    [string]$Version = "",
    [string]$InstallDir = "",
    [string]$DataDir = "",
    [switch]$NoService
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"

function Write-InstallWarning {
    param([string]$Message)
    Write-Warning "Codesk install: $Message"
}

function Get-UserHome {
    if (-not [string]::IsNullOrWhiteSpace($env:USERPROFILE)) {
        return $env:USERPROFILE
    }
    return [Environment]::GetFolderPath([Environment+SpecialFolder]::UserProfile)
}

function Join-RemotePath {
    param(
        [string]$Base,
        [string]$Relative
    )
    return "$($Base.TrimEnd('/'))/$($Relative.TrimStart('/'))"
}

function Add-CacheBust {
    param([string]$Url)

    if ($Url -notmatch "^https?://") {
        return $Url
    }
    $separator = if ($Url.Contains("?")) { "&" } else { "?" }
    return "${Url}${separator}codesk_cache_bust=$([DateTime]::UtcNow.Ticks).$PID"
}

function Copy-Download {
    param(
        [string]$Url,
        [string]$Destination
    )

    if ($Url -match "^file://") {
        $source = ([Uri]$Url).LocalPath
        Copy-Item -LiteralPath $source -Destination $Destination -Force
        return
    }

    Invoke-WebRequest -UseBasicParsing -Uri (Add-CacheBust $Url) -OutFile $Destination
}

function Get-RuntimeInfo {
    param([string]$Command)

    if ([string]::IsNullOrWhiteSpace($Command)) {
        return $null
    }

    $resolved = Get-Command -Name $Command -CommandType Application, ExternalScript -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($null -eq $resolved) {
        return $null
    }

    $path = if (-not [string]::IsNullOrWhiteSpace($resolved.Source)) {
        $resolved.Source
    } else {
        $resolved.Definition
    }
    if ([string]::IsNullOrWhiteSpace($path)) {
        return $null
    }

    return [pscustomobject]@{
        Path = $path
        Directory = Split-Path -Parent $path
    }
}

function Test-CodexRuntime {
    param(
        [string]$Command,
        $Runtime
    )

    $degraded = "Codex agents will be unavailable until Codex is configured; other runtimes such as Claude Code are unaffected."
    if ($null -eq $Runtime) {
        Write-InstallWarning "Codex runtime unavailable: '$Command' was not found. $degraded Install Codex or set NOTTY_CODEX_COMMAND to the executable path."
        return
    }

    try {
        & $Runtime.Path --version *> $null
        if (-not $?) {
            throw "version command failed"
        }
    } catch {
        Write-InstallWarning "Codex runtime unavailable: '$Command --version' did not run successfully. $degraded Fix Codex to enable Codex agents."
        return
    }

    try {
        & $Runtime.Path app-server --help *> $null
        if (-not $?) {
            throw "app-server command failed"
        }
    } catch {
        Write-InstallWarning "Codex runtime unavailable: '$Command' does not support 'app-server'. $degraded Upgrade Codex to enable Codex agents."
    }
}

function Test-ClaudeRuntime {
    param(
        [string]$Command,
        $Runtime
    )

    if ($null -eq $Runtime) {
        Write-InstallWarning "Claude Code runtime unavailable: '$Command' was not found. Claude Code agents are unavailable until it is installed. Install Claude Code or set NOTTY_CLAUDE_COMMAND to the executable path."
        return
    }

    try {
        & $Runtime.Path --version *> $null
        if (-not $?) {
            throw "version command failed"
        }
    } catch {
        Write-InstallWarning "Claude Code runtime unavailable: '$Command --version' did not run successfully. Fix Claude Code to enable Claude Code agents."
    }
}

function Protect-CurrentUserFile {
    param([string]$Path)

    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $acl = [Security.AccessControl.FileSecurity]::new()
    $acl.SetOwner($identity.User)
    $acl.SetAccessRuleProtection($true, $false)
    $rule = [Security.AccessControl.FileSystemAccessRule]::new(
        $identity.User,
        [Security.AccessControl.FileSystemRights]::FullControl,
        [Security.AccessControl.InheritanceFlags]::None,
        [Security.AccessControl.PropagationFlags]::None,
        [Security.AccessControl.AccessControlType]::Allow
    )
    $acl.AddAccessRule($rule)
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function Get-StartupDirectory {
    $startup = [Environment]::GetFolderPath([Environment+SpecialFolder]::Startup)
    if (-not [string]::IsNullOrWhiteSpace($startup)) {
        return $startup
    }
    if (-not [string]::IsNullOrWhiteSpace($env:APPDATA)) {
        return Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\Startup"
    }
    throw "Could not resolve the current user's Startup directory"
}

function Get-PowerShellExecutable {
    $windowsPowerShell = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
    if (Test-Path -LiteralPath $windowsPowerShell -PathType Leaf) {
        return $windowsPowerShell
    }
    $resolved = Get-Command -Name powershell.exe -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($null -ne $resolved) {
        return $resolved.Source
    }
    throw "Windows PowerShell is required to launch the Codesk daemon"
}

function Stop-LauncherProcess {
    param(
        [string]$DaemonDir,
        [string]$RunScript
    )

    $pidFile = Join-Path $DaemonDir "launcher.pid"
    if (-not (Test-Path -LiteralPath $pidFile -PathType Leaf)) {
        return
    }

    $launcherPid = 0
    $pidText = Get-Content -LiteralPath $pidFile -Raw -ErrorAction SilentlyContinue
    if ([string]::IsNullOrWhiteSpace($pidText)) {
        Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
        return
    }
    $pidText = $pidText.Trim()
    if (-not [int]::TryParse($pidText, [ref]$launcherPid) -or $launcherPid -le 0 -or $launcherPid -eq $PID) {
        Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
        return
    }

    try {
        $process = Get-CimInstance Win32_Process -Filter ("ProcessId = {0}" -f $launcherPid) -ErrorAction Stop
        if ($null -eq $process -or [string]::IsNullOrWhiteSpace($process.CommandLine)) {
            return
        }
        if ($process.CommandLine.IndexOf($RunScript, [StringComparison]::OrdinalIgnoreCase) -lt 0) {
            Write-InstallWarning "Ignored stale launcher PID $launcherPid because it does not belong to $RunScript."
            return
        }
        & taskkill.exe /PID $launcherPid /T /F *> $null
    } catch {
        # A stopped task or stale PID is already in the desired state.
    } finally {
        Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
    }
}

function Remove-ExistingRegistration {
    param(
        [string]$TaskName,
        [string]$StartupLink,
        [string]$DaemonDir,
        [string]$RunScript
    )

    if ($null -ne (Get-Command -Name Get-ScheduledTask -ErrorAction SilentlyContinue)) {
        try {
            $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
            if ($null -ne $task) {
                Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
                Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
            }
        } catch {
            Write-InstallWarning "Could not remove the previous Scheduled Task '$TaskName': $($_.Exception.Message)"
        }
    }

    Remove-Item -LiteralPath $StartupLink -Force -ErrorAction SilentlyContinue
    Stop-LauncherProcess -DaemonDir $DaemonDir -RunScript $RunScript
}

function Install-FileAtomically {
    param(
        [string]$Source,
        [string]$Destination
    )

    if (Test-Path -LiteralPath $Destination -PathType Leaf) {
        $sourceHash = (Get-FileHash -LiteralPath $Source -Algorithm SHA256).Hash
        $destinationHash = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash
        if ($sourceHash -eq $destinationHash) {
            return
        }
    }

    $staged = "$Destination.new.$PID"
    try {
        Copy-Item -LiteralPath $Source -Destination $staged -Force
        Move-Item -LiteralPath $staged -Destination $Destination -Force
    } finally {
        Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
    }
}

function Install-StartupShortcut {
    param(
        [string]$ShortcutPath,
        [string]$PowerShellPath,
        [string]$RunScript
    )

    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut($ShortcutPath)
    $shortcut.TargetPath = $PowerShellPath
    $shortcut.Arguments = "-NoLogo -NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$RunScript`""
    $shortcut.WorkingDirectory = Split-Path -Parent $RunScript
    $shortcut.WindowStyle = 7
    $shortcut.Description = "Codesk local environment daemon"
    $shortcut.Save()
}

function Register-Launcher {
    param(
        [string]$TaskName,
        [string]$StartupLink,
        [string]$RunScript
    )

    $powerShellPath = Get-PowerShellExecutable
    $arguments = "-NoLogo -NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$RunScript`""
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()

    try {
        Import-Module ScheduledTasks -ErrorAction Stop
        $action = New-ScheduledTaskAction -Execute $powerShellPath -Argument $arguments
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $identity.Name
        $principal = New-ScheduledTaskPrincipal -UserId $identity.Name -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet `
            -AllowStartIfOnBatteries `
            -DontStopIfGoingOnBatteries `
            -StartWhenAvailable `
            -MultipleInstances IgnoreNew `
            -RestartCount 3 `
            -RestartInterval (New-TimeSpan -Minutes 1) `
            -ExecutionTimeLimit ([TimeSpan]::Zero)
        Register-ScheduledTask `
            -TaskName $TaskName `
            -Action $action `
            -Trigger $trigger `
            -Principal $principal `
            -Settings $settings `
            -Description "Codesk local environment daemon" `
            -Force | Out-Null
        Remove-Item -LiteralPath $StartupLink -Force -ErrorAction SilentlyContinue
        Start-ScheduledTask -TaskName $TaskName
        return [pscustomobject]@{
            Mode = "scheduled-task"
            TaskName = $TaskName
            StartupLink = ""
        }
    } catch {
        $registrationError = $_.Exception.Message
        try {
            Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
        } catch {
            # Registration may have failed before a task existed.
        }
        Write-InstallWarning "Scheduled Task registration was unavailable; using the current-user Startup folder instead. $registrationError"
    }

    $startupDirectory = Split-Path -Parent $StartupLink
    New-Item -ItemType Directory -Path $startupDirectory -Force | Out-Null
    Install-StartupShortcut -ShortcutPath $StartupLink -PowerShellPath $powerShellPath -RunScript $RunScript
    Start-Process -FilePath $powerShellPath -ArgumentList $arguments -WindowStyle Hidden | Out-Null
    return [pscustomobject]@{
        Mode = "startup-shortcut"
        TaskName = ""
        StartupLink = $StartupLink
    }
}

if ($env:OS -ne "Windows_NT") {
    throw "Codesk install: install.ps1 supports Windows only"
}

$machineArchitecture = if (-not [string]::IsNullOrWhiteSpace($env:PROCESSOR_ARCHITEW6432)) {
    $env:PROCESSOR_ARCHITEW6432
} else {
    $env:PROCESSOR_ARCHITECTURE
}
if ($machineArchitecture -notin @("AMD64", "x86_64", "ARM64", "aarch64")) {
    throw "Codesk install: Windows AMD64 or ARM64 is required; detected $machineArchitecture"
}
if ($machineArchitecture -in @("ARM64", "aarch64")) {
    Write-InstallWarning "Windows ARM64 detected; installing the AMD64 release through Windows x64 emulation."
}

$userHome = Get-UserHome
if ([string]::IsNullOrWhiteSpace($StaticBase)) {
    $StaticBase = $env:NOTTY_DAEMON_STATIC_BASE
}
if ([string]::IsNullOrWhiteSpace($StaticBase)) {
    throw "Codesk install: -StaticBase is required"
}
if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = if ([string]::IsNullOrWhiteSpace($env:NOTTY_DAEMON_VERSION)) { "latest" } else { $env:NOTTY_DAEMON_VERSION }
}
if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = if ([string]::IsNullOrWhiteSpace($env:NOTTY_INSTALL_DIR)) {
        Join-Path $userHome ".notty\bin"
    } else {
        $env:NOTTY_INSTALL_DIR
    }
}
if ([string]::IsNullOrWhiteSpace($DataDir)) {
    $DataDir = if ([string]::IsNullOrWhiteSpace($env:NOTTY_DATA_DIR)) {
        Join-Path $userHome ".notty"
    } else {
        $env:NOTTY_DATA_DIR
    }
}

$StaticBase = $StaticBase.TrimEnd("/")
$BackendUrl = $BackendUrl.TrimEnd("/")
$codexCommand = if ([string]::IsNullOrWhiteSpace($env:NOTTY_CODEX_COMMAND)) { "codex" } else { $env:NOTTY_CODEX_COMMAND }
$claudeCommand = if ([string]::IsNullOrWhiteSpace($env:NOTTY_CLAUDE_COMMAND)) { "claude" } else { $env:NOTTY_CLAUDE_COMMAND }
$codexRuntime = Get-RuntimeInfo $codexCommand
$claudeRuntime = Get-RuntimeInfo $claudeCommand
Test-CodexRuntime -Command $codexCommand -Runtime $codexRuntime
Test-ClaudeRuntime -Command $claudeCommand -Runtime $claudeRuntime

$tempDir = Join-Path ([IO.Path]::GetTempPath()) ("codesk-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

try {
    if ($Version -eq "latest") {
        $manifestPath = Join-Path $tempDir "manifest.json"
        Copy-Download -Url (Join-RemotePath $StaticBase "latest/manifest.json") -Destination $manifestPath
        $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
        $Version = [string]$manifest.version
        if ([string]::IsNullOrWhiteSpace($Version)) {
            throw "Codesk install: could not determine the latest daemon version"
        }
    }
    if ($Version -notmatch "^[A-Za-z0-9._-]+$") {
        throw "Codesk install: invalid daemon version '$Version'"
    }

    $artifact = "notty-daemon_${Version}_windows_amd64.zip"
    $versionBase = Join-RemotePath $StaticBase $Version
    $archivePath = Join-Path $tempDir $artifact
    $checksumsPath = Join-Path $tempDir "SHA256SUMS"

    Write-Host "Installing Codesk daemon $Version for windows/amd64"
    Copy-Download -Url (Join-RemotePath $versionBase "SHA256SUMS") -Destination $checksumsPath
    $checksumText = Get-Content -LiteralPath $checksumsPath -Raw
    $checksumPattern = "(?mi)^([0-9a-f]{64})\s+" + [Regex]::Escape($artifact) + "\s*$"
    $checksumMatch = [Regex]::Match($checksumText, $checksumPattern)
    if (-not $checksumMatch.Success) {
        throw "Codesk install: release $Version does not contain $artifact"
    }

    Copy-Download -Url (Join-RemotePath $versionBase $artifact) -Destination $archivePath
    $expectedHash = $checksumMatch.Groups[1].Value.ToLowerInvariant()
    $actualHash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        throw "Codesk install: checksum mismatch for $artifact"
    }

    Expand-Archive -LiteralPath $archivePath -DestinationPath $tempDir -Force
    $packageDir = Join-Path $tempDir "notty-daemon_${Version}_windows_amd64"
    $daemonSource = Join-Path $packageDir "bin\notty-daemon.exe"
    $agentToolSource = Join-Path $packageDir "bin\notty-agent-tool.exe"
    $runnerSource = Join-Path $packageDir "run-windows.ps1"
    foreach ($requiredFile in @($daemonSource, $agentToolSource, $runnerSource)) {
        if (-not (Test-Path -LiteralPath $requiredFile -PathType Leaf)) {
            throw "Codesk install: archive is missing $requiredFile"
        }
    }

    $daemonName = $WorkspaceId -replace "[^A-Za-z0-9_.-]", "-"
    if ([string]::IsNullOrWhiteSpace($daemonName)) {
        $daemonName = "workspace"
    }
    $binaryDir = Join-Path $InstallDir $Version
    $daemonDir = Join-Path (Join-Path $DataDir "daemons") $daemonName
    $workspaceDir = Join-Path (Join-Path $userHome "Notty\workspaces") $daemonName
    $agentWorkspaceRoot = Join-Path (Join-Path $userHome "Notty\agents") $daemonName
    $runScript = Join-Path $daemonDir "run.ps1"
    $taskName = "Codesk daemon $daemonName"
    $startupLink = Join-Path (Get-StartupDirectory) "$taskName.lnk"

    New-Item -ItemType Directory -Path $binaryDir, $daemonDir, $workspaceDir, $agentWorkspaceRoot -Force | Out-Null
    Remove-ExistingRegistration `
        -TaskName $taskName `
        -StartupLink $startupLink `
        -DaemonDir $daemonDir `
        -RunScript $runScript

    Install-FileAtomically -Source $daemonSource -Destination (Join-Path $binaryDir "notty-daemon.exe")
    Install-FileAtomically -Source $agentToolSource -Destination (Join-Path $binaryDir "notty-agent-tool.exe")
    Copy-Item -LiteralPath $runnerSource -Destination $runScript -Force

    $codexToolDir = if ($null -eq $codexRuntime) { "" } else { [string]$codexRuntime.Directory }
    $claudeToolDir = if ($null -eq $claudeRuntime) { "" } else { [string]$claudeRuntime.Directory }
    $configPath = Join-Path $daemonDir "daemon.env.json"
    $config = [ordered]@{
        NOTTY_BACKEND_URL = $BackendUrl
        NOTTY_WORKSPACE_ID = $WorkspaceId
        NOTTY_DAEMON_TOKEN = $DaemonToken
        NOTTY_DAEMON_VERSION = $Version
        NOTTY_DATA_DIR = $DataDir
        NOTTY_WORKSPACE_DIR = $workspaceDir
        NOTTY_AGENT_WORKSPACE_ROOT = $agentWorkspaceRoot
        NOTTY_CODEX_COMMAND = $codexCommand
        NOTTY_CLAUDE_COMMAND = $claudeCommand
        NOTTY_TOOL_DIR_CODEX = $codexToolDir
        NOTTY_TOOL_DIR_CLAUDE = $claudeToolDir
        NOTTY_BINARY_DIR = $binaryDir
    }
    $config | ConvertTo-Json -Depth 3 | Set-Content -LiteralPath $configPath -Encoding UTF8
    Protect-CurrentUserFile $configPath

    $service = if ($NoService) {
        [pscustomobject]@{
            Mode = "none"
            TaskName = ""
            StartupLink = ""
        }
    } else {
        Register-Launcher -TaskName $taskName -StartupLink $startupLink -RunScript $runScript
    }
    $servicePath = Join-Path $daemonDir "service.json"
    $service | ConvertTo-Json -Depth 3 | Set-Content -LiteralPath $servicePath -Encoding UTF8

    $servicePathEntries = @($binaryDir, $codexToolDir, $claudeToolDir) |
        Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
    Write-Host "Daemon service PATH starts with: $($servicePathEntries -join ';')"
    if ($null -ne $codexRuntime) {
        Write-Host "Codex resolved in: $codexToolDir"
    }
    if ($null -ne $claudeRuntime) {
        Write-Host "Claude Code resolved in: $claudeToolDir"
    }

    if ($NoService) {
        $powerShellPath = Get-PowerShellExecutable
        Write-Host "Installed daemon binaries and config."
        Write-Host "Start the daemon in the foreground with:"
        Write-Host "  & '$powerShellPath' -NoLogo -NoProfile -ExecutionPolicy Bypass -File '$runScript'"
    } elseif ($service.Mode -eq "scheduled-task") {
        Write-Host "Installed Scheduled Task '$($service.TaskName)'. Log: $(Join-Path $daemonDir 'daemon.log')"
    } else {
        Write-Host "Installed Startup shortcut '$($service.StartupLink)'. Log: $(Join-Path $daemonDir 'daemon.log')"
    }

    Write-Host "Codesk daemon install complete."
} finally {
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
