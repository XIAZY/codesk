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
		{"terminating read removed", ` -ErrorAction Stop`, ``},
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
