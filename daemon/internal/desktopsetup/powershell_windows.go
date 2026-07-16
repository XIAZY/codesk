//go:build windows

package desktopsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const (
	powerShellTimeout   = 30 * time.Second
	powerShellOutputMax = 64 << 10
)

const cleanupLegacyLaunchersScript = `$ErrorActionPreference = 'Stop'
$prefix = 'Codesk daemon '
$tasks = @(Get-ScheduledTask -ErrorAction Stop | Where-Object {
    ([string]$_.TaskName).StartsWith($prefix, [StringComparison]::Ordinal)
})
foreach ($task in $tasks) {
    Unregister-ScheduledTask -TaskName $task.TaskName -TaskPath $task.TaskPath -Confirm:$false -ErrorAction Stop | Out-Null
}
$remainingTasks = @(Get-ScheduledTask -ErrorAction Stop | Where-Object {
    ([string]$_.TaskName).StartsWith($prefix, [StringComparison]::Ordinal)
})
if ($remainingTasks.Count -ne 0) {
    throw 'legacy Codesk scheduled tasks remain'
}
$startup = $env:CODESK_STARTUP_PATH
$links = @(Get-ChildItem -LiteralPath $startup -File -ErrorAction Stop | Where-Object {
    $_.Name.StartsWith($prefix, [StringComparison]::Ordinal) -and
    $_.Name.EndsWith('.lnk', [StringComparison]::OrdinalIgnoreCase)
})
foreach ($link in $links) {
    Remove-Item -LiteralPath $link.FullName -Force -ErrorAction Stop
}
$remainingLinks = @(Get-ChildItem -LiteralPath $startup -File -ErrorAction Stop | Where-Object {
    $_.Name.StartsWith($prefix, [StringComparison]::Ordinal) -and
    $_.Name.EndsWith('.lnk', [StringComparison]::OrdinalIgnoreCase)
})
if ($remainingLinks.Count -ne 0) {
    throw 'legacy Codesk Startup links remain'
}
`

const createShortcutScript = `$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$shortcut = $shell.CreateShortcut($env:CODESK_SHORTCUT_PATH)
$shortcut.TargetPath = $env:CODESK_DESKTOP_PATH
$shortcut.WorkingDirectory = $env:CODESK_INSTALL_PATH
$shortcut.IconLocation = $env:CODESK_ICON_PATH + ',0'
$shortcut.Description = 'Codesk'
$shortcut.Save()
if (-not (Test-Path -LiteralPath $env:CODESK_SHORTCUT_PATH -PathType Leaf)) {
    throw 'Codesk shortcut was not created'
}
`

const selfDeleteScript = `$ErrorActionPreference = 'SilentlyContinue'
$parentHandleValue = [Int64]::Parse($env:CODESK_DELETE_PARENT_HANDLE, [Globalization.CultureInfo]::InvariantCulture)
if ($parentHandleValue -le 0) {
    exit 1
}
$parentHandle = [IntPtr]::new($parentHandleValue)
$waitHandle = [System.Threading.EventWaitHandle]::new($false, [System.Threading.EventResetMode]::AutoReset)
$waitHandle.SafeWaitHandle = [Microsoft.Win32.SafeHandles.SafeWaitHandle]::new($parentHandle, $false)
if (-not $waitHandle.WaitOne()) {
    exit 1
}
for ($attempt = 0; $attempt -lt 60; $attempt++) {
    Remove-Item -LiteralPath $env:CODESK_DELETE_PATH -Recurse -Force -ErrorAction SilentlyContinue
    if (-not (Test-Path -LiteralPath $env:CODESK_DELETE_PATH)) {
        exit 0
    }
    Start-Sleep -Milliseconds 500
}
exit 1
`

func cleanupLegacyLaunchers(ctx context.Context, paths windowsPaths) error {
	return runHiddenPowerShell(ctx, paths.SystemPowerShell, cleanupLegacyLaunchersScript, map[string]string{
		"CODESK_STARTUP_PATH": paths.Startup,
	})
}

func createStartMenuShortcut(ctx context.Context, paths windowsPaths) error {
	if err := runHiddenPowerShell(ctx, paths.SystemPowerShell, createShortcutScript, map[string]string{
		"CODESK_SHORTCUT_PATH": paths.Shortcut,
		"CODESK_DESKTOP_PATH":  paths.Desktop,
		"CODESK_INSTALL_PATH":  paths.InstallRoot,
		"CODESK_ICON_PATH":     paths.Icon,
	}); err != nil {
		return err
	}
	file, err := os.OpenFile(paths.Shortcut, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("desktop setup: open Start Menu shortcut for flush: %w", err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("desktop setup: flush Start Menu shortcut: %w", err)
	}
	return nil
}

func scheduleSelfDelete(powerShellPath, executablePath string) error {
	if err := validateWindowsPath(powerShellPath); err != nil {
		return err
	}
	if err := validateWindowsPath(executablePath); err != nil {
		return err
	}
	parentHandle, err := windows.OpenProcess(windows.SYNCHRONIZE, true, windows.GetCurrentProcessId())
	if err != nil {
		return fmt.Errorf("desktop setup: open exact parent process for self-delete: %w", err)
	}
	defer windows.CloseHandle(parentHandle)
	environment, err := mergedEnvironment(map[string]string{
		"CODESK_DELETE_PARENT_HANDLE": strconv.FormatUint(uint64(parentHandle), 10),
		"CODESK_DELETE_PATH":          executablePath,
	})
	if err != nil {
		return err
	}
	return startDetachedWithInheritedHandles(
		powerShellPath,
		environment,
		[]syscall.Handle{syscall.Handle(parentHandle)},
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", selfDeleteScript,
	)
}

func runHiddenPowerShell(ctx context.Context, executable, script string, values map[string]string) error {
	if err := validateWindowsPath(executable); err != nil {
		return err
	}
	if strings.TrimSpace(script) == "" {
		return errors.New("desktop setup: empty PowerShell operation")
	}
	environment, err := mergedEnvironment(values)
	if err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, powerShellTimeout)
	defer cancel()

	command := exec.CommandContext(
		operationContext,
		executable,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script,
	)
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:    windows.CREATE_NO_WINDOW,
		HideWindow:       true,
		NoInheritHandles: true,
	}
	output := &limitedOutput{remaining: powerShellOutputMax}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if operationContext.Err() != nil {
			return fmt.Errorf("desktop setup: PowerShell operation timed out: %w", operationContext.Err())
		}
		message := strings.TrimSpace(output.String())
		if message == "" {
			return fmt.Errorf("desktop setup: PowerShell operation failed: %w", err)
		}
		return fmt.Errorf("desktop setup: PowerShell operation failed: %w: %s", err, message)
	}
	return nil
}

func mergedEnvironment(values map[string]string) ([]string, error) {
	for key, value := range values {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("desktop setup: invalid child environment")
		}
	}
	environment := make([]string, 0, len(os.Environ())+len(values))
	for _, item := range os.Environ() {
		separator := strings.IndexByte(item, '=')
		if separator <= 0 {
			continue
		}
		name := item[:separator]
		overridden := false
		for key := range values {
			if strings.EqualFold(name, key) {
				overridden = true
				break
			}
		}
		if !overridden {
			environment = append(environment, item)
		}
	}
	for key, value := range values {
		environment = append(environment, key+"="+value)
	}
	return environment, nil
}

type limitedOutput struct {
	builder   strings.Builder
	remaining int
}

func (w *limitedOutput) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > w.remaining {
		data = data[:w.remaining]
	}
	if len(data) > 0 {
		_, _ = w.builder.Write(data)
		w.remaining -= len(data)
	}
	return original, nil
}

func (w *limitedOutput) String() string {
	return w.builder.String()
}
