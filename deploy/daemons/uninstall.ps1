[CmdletBinding()]
param(
    [switch]$All,
    [string]$InstallDir = "",
    [string]$DataDir = "",
    [switch]$KeepBinaries
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"

function Get-UserHome {
    if (-not [string]::IsNullOrWhiteSpace($env:USERPROFILE)) {
        return $env:USERPROFILE
    }
    return [Environment]::GetFolderPath([Environment+SpecialFolder]::UserProfile)
}

function Write-UninstallWarning {
    param([string]$Message)
    Write-Warning "Codesk uninstall: $Message"
}

function Get-StartupDirectory {
    $startup = [Environment]::GetFolderPath([Environment+SpecialFolder]::Startup)
    if (-not [string]::IsNullOrWhiteSpace($startup)) {
        return $startup
    }
    if (-not [string]::IsNullOrWhiteSpace($env:APPDATA)) {
        return Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\Startup"
    }
    return ""
}

function Remove-EmptyDirectory {
    param([string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        return
    }
    $child = Get-ChildItem -LiteralPath $Path -Force -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $child) {
        Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
    }
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
            Write-UninstallWarning "Ignored stale launcher PID $launcherPid because it does not belong to $RunScript."
            return
        }
        & taskkill.exe /PID $launcherPid /T /F *> $null
    } catch {
        # A missing process is already stopped.
    } finally {
        Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
    }
}

function Stop-ManagedDaemon {
    param([string]$DaemonDir)

    $daemonName = Split-Path -Leaf $DaemonDir
    $runScript = Join-Path $DaemonDir "run.ps1"
    $taskName = "Codesk daemon $daemonName"
    $startupDirectory = Get-StartupDirectory
    $derivedStartupLink = if ([string]::IsNullOrWhiteSpace($startupDirectory)) {
        ""
    } else {
        Join-Path $startupDirectory "$taskName.lnk"
    }
    $startupLink = $derivedStartupLink
    $servicePath = Join-Path $DaemonDir "service.json"

    if (Test-Path -LiteralPath $servicePath -PathType Leaf) {
        try {
            $service = Get-Content -LiteralPath $servicePath -Raw | ConvertFrom-Json
            if ($service.PSObject.Properties.Name -contains "TaskName" -and -not [string]::IsNullOrWhiteSpace([string]$service.TaskName)) {
                $taskName = [string]$service.TaskName
            }
            if ($service.PSObject.Properties.Name -contains "StartupLink") {
                $startupLink = [string]$service.StartupLink
            }
        } catch {
            Write-UninstallWarning "Could not read $servicePath; falling back to the derived registration name."
        }
    }

    if ($null -ne (Get-Command -Name Get-ScheduledTask -ErrorAction SilentlyContinue)) {
        try {
            $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
            if ($null -ne $task) {
                Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
                Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
            }
        } catch {
            Write-UninstallWarning "Could not remove Scheduled Task '$taskName': $($_.Exception.Message)"
        }
    }

    if (-not [string]::IsNullOrWhiteSpace($startupLink)) {
        Remove-Item -LiteralPath $startupLink -Force -ErrorAction SilentlyContinue
    }
    if (-not [string]::IsNullOrWhiteSpace($derivedStartupLink) -and $derivedStartupLink -ne $startupLink) {
        Remove-Item -LiteralPath $derivedStartupLink -Force -ErrorAction SilentlyContinue
    }
    Stop-LauncherProcess -DaemonDir $DaemonDir -RunScript $runScript
}

function Remove-ManagedBinaries {
    param([string]$Root)

    if (-not (Test-Path -LiteralPath $Root -PathType Container)) {
        return
    }

    foreach ($versionDir in (Get-ChildItem -LiteralPath $Root -Directory -Force -ErrorAction SilentlyContinue)) {
        $daemonBinary = Join-Path $versionDir.FullName "notty-daemon.exe"
        $agentToolBinary = Join-Path $versionDir.FullName "notty-agent-tool.exe"
        if ((Test-Path -LiteralPath $daemonBinary -PathType Leaf) -or (Test-Path -LiteralPath $agentToolBinary -PathType Leaf)) {
            Remove-Item -LiteralPath $daemonBinary, $agentToolBinary -Force -ErrorAction SilentlyContinue
            Remove-EmptyDirectory $versionDir.FullName
        }
    }

    Remove-Item `
        -LiteralPath (Join-Path $Root "notty-daemon.exe"), (Join-Path $Root "notty-agent-tool.exe") `
        -Force `
        -ErrorAction SilentlyContinue
    Remove-EmptyDirectory $Root
}

if ($env:OS -ne "Windows_NT") {
    throw "Codesk uninstall: uninstall.ps1 supports Windows only"
}
if (-not $All) {
    throw "Codesk uninstall: -All is required"
}

$userHome = Get-UserHome
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

$daemonsDir = Join-Path $DataDir "daemons"
if (Test-Path -LiteralPath $daemonsDir -PathType Container) {
    foreach ($daemonDir in (Get-ChildItem -LiteralPath $daemonsDir -Directory -Force -ErrorAction SilentlyContinue)) {
        Stop-ManagedDaemon $daemonDir.FullName
    }
    Start-Sleep -Milliseconds 250
    Remove-Item -LiteralPath $daemonsDir -Recurse -Force -ErrorAction SilentlyContinue
}

Remove-Item -LiteralPath (Join-Path $DataDir "runtime") -Recurse -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath (Join-Path $userHome "Notty\workspaces") -Recurse -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath (Join-Path $userHome "Notty\agents") -Recurse -Force -ErrorAction SilentlyContinue

if (-not $KeepBinaries) {
    Remove-ManagedBinaries $InstallDir
}

Remove-EmptyDirectory $DataDir
Remove-EmptyDirectory (Join-Path $userHome "Notty")

Write-Host "Codesk daemon uninstall complete."
