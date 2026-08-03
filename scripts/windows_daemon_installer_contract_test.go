package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestWindowsInstallerLifecycleRetriesTransientLogLocks(t *testing.T) {
	data, err := os.ReadFile("test-daemon-installer-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if err := checkWindowsInstallerLogPollContract(source); err != nil {
		t.Fatal(err)
	}

	for _, mutation := range []struct {
		name        string
		old         string
		replacement string
	}{
		{"bounded retry removed", `$attempt -lt 70`, `$true`},
		{"terminating read removed", `Get-Content -LiteralPath $daemonLogPath -Raw -ErrorAction Stop`, `Get-Content -LiteralPath $daemonLogPath -Raw`},
		{"file-sharing retry removed", `} catch [IO.IOException] {`, `} catch [UnauthorizedAccessException] {`},
		{"retry continuation removed", `                continue`, ``},
		{"timeout diagnostic removed", `; last log read error: $daemonLogReadError`, ``},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.old) != 1 {
				t.Fatalf("mutation source %q is not unique", mutation.old)
			}
			mutated := strings.Replace(source, mutation.old, mutation.replacement, 1)
			if err := checkWindowsInstallerLogPollContract(mutated); err == nil {
				t.Fatal("Windows installer log-poll mutation passed")
			}
		})
	}
}

func TestWindowsInstallerLifecycleWaitsForLauncherReadiness(t *testing.T) {
	data, err := os.ReadFile("test-daemon-installer-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if err := checkWindowsInstallerLauncherPollContract(source); err != nil {
		t.Fatal(err)
	}

	for _, mutation := range []struct {
		name        string
		old         string
		replacement string
	}{
		{"readiness deadline shortened", `$attempt -lt 300`, `$attempt -lt 50`},
		{"readiness polling removed", `-not (Test-Path -LiteralPath $launcherPidPath -PathType Leaf)`, `$false`},
		{"poll interval removed", `$attempt++) {
        Start-Sleep -Milliseconds 100
    }
    $launcherDiagnostic`, `$attempt++) {
    }
    $launcherDiagnostic`},
		{"task state diagnostic removed", `Get-ScheduledTaskInfo -TaskName $service.TaskName -ErrorAction Stop`, `Get-ScheduledTask -TaskName $service.TaskName -ErrorAction Stop`},
		{"terminal readiness assertion bypassed", `Assert-True (Test-Path -LiteralPath $launcherPidPath -PathType Leaf)`, `Assert-True $true`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.old) != 1 {
				t.Fatalf("mutation source %q is not unique", mutation.old)
			}
			mutated := strings.Replace(source, mutation.old, mutation.replacement, 1)
			if err := checkWindowsInstallerLauncherPollContract(mutated); err == nil {
				t.Fatal("Windows installer launcher-readiness mutation passed")
			}
		})
	}
}

func TestWindowsInstallerLifecycleReturnsSuccessAfterBestEffortCleanup(t *testing.T) {
	data, err := os.ReadFile("test-daemon-installer-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.TrimSpace(string(data))
	if err := checkWindowsInstallerSuccessExitContract(source); err != nil {
		t.Fatal(err)
	}

	mutated := strings.TrimSuffix(source, "exit 0")
	if err := checkWindowsInstallerSuccessExitContract(mutated); err == nil {
		t.Fatal("successful-exit mutation passed")
	}
}

func checkWindowsInstallerSuccessExitContract(source string) error {
	const successfulExit = "exit 0"
	source = strings.TrimSpace(source)
	if strings.Count(source, successfulExit) != 1 || !strings.HasSuffix(source, successfulExit) {
		return fmt.Errorf("Windows installer lifecycle must end with one explicit %q after cleanup", successfulExit)
	}
	return nil
}

func checkWindowsInstallerLauncherPollContract(source string) error {
	const (
		start = `    $launcherPidPath = Join-Path $daemonDir "launcher.pid"`
		end   = `    $daemonLogPath = Join-Path $daemonDir "daemon.log"`
	)
	startAt := strings.Index(source, start)
	if startAt < 0 {
		return fmt.Errorf("Windows installer lifecycle has no launcher readiness poll")
	}
	endAt := strings.Index(source[startAt:], end)
	if endAt < 0 {
		return fmt.Errorf("Windows installer lifecycle has no launcher readiness boundary")
	}
	poll := source[startAt : startAt+endAt]

	for required, count := range map[string]int{
		`$attempt -lt 300`: 1,
		`-not (Test-Path -LiteralPath $launcherPidPath -PathType Leaf)`:        1,
		`Start-Sleep -Milliseconds 100`:                                        1,
		`Get-ScheduledTaskInfo -TaskName $service.TaskName -ErrorAction Stop`:  1,
		`$taskState = [string]$registeredTask.State`:                           1,
		`$lastTaskResult = [string]$taskInfo.LastTaskResult`:                   1,
		`Assert-True (Test-Path -LiteralPath $launcherPidPath -PathType Leaf)`: 1,
		`launcher did not publish pid within 30 seconds; $launcherDiagnostic`:  1,
	} {
		if got := strings.Count(poll, required); got != count {
			return fmt.Errorf("Windows installer launcher-poll source count for %q = %d, want %d", required, got, count)
		}
	}
	return nil
}

func checkWindowsInstallerLogPollContract(source string) error {
	const (
		start = `    $daemonLogPath = Join-Path $daemonDir "daemon.log"`
		end   = `    Assert-True ($daemonLog -notlike "*Codesk daemon launch failed:*")`
	)
	startAt := strings.Index(source, start)
	if startAt < 0 {
		return fmt.Errorf("Windows installer lifecycle has no daemon log poll")
	}
	endAt := strings.Index(source[startAt:], end)
	if endAt < 0 {
		return fmt.Errorf("Windows installer lifecycle has no daemon log assertion boundary")
	}
	poll := source[startAt : startAt+endAt]

	for required, count := range map[string]int{
		`$attempt -lt 70`: 1,
		`Get-Content -LiteralPath $daemonLogPath -Raw -ErrorAction Stop`: 1,
		`} catch [IO.IOException] {`:                                     1,
		`$daemonLogReadError = $_.Exception.Message`:                     1,
		`continue`:                                            1,
		`Start-Sleep -Milliseconds 100`:                       2,
		`; last log read error: $daemonLogReadError`:          1,
		`$daemonLog -like "*Codesk daemon exited with code*"`: 2,
	} {
		if got := strings.Count(poll, required); got != count {
			return fmt.Errorf("Windows installer log-poll source count for %q = %d, want %d", required, got, count)
		}
	}
	return nil
}
