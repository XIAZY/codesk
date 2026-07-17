package main

import (
	"os"
	"strings"
	"testing"
)

func TestWindowsObserverRetainsEventTimeShowWithoutVisibilityOrTitleReads(t *testing.T) {
	sourceBytes, err := os.ReadFile("windows_observer.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, forbidden := range []string{"IsWindowVisible", "GetWindowText"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("windows observer reintroduced a lossy or sensitive %s query", forbidden)
		}
	}
	if !strings.Contains(source, "o.capture(hwnd, time.Now().UTC(), windowClass(hwnd))") {
		t.Fatal("windows observer no longer captures SHOW timestamp and class in the callback")
	}
	captureStart := strings.Index(source, "func (o *windowsObserver) capture(")
	if captureStart < 0 {
		t.Fatal("windows observer capture implementation is absent")
	}
	captureEnd := strings.Index(source[captureStart:], "\nfunc (o *windowsObserver) recordUnboundConsoleShow")
	if captureEnd < 0 {
		t.Fatal("windows observer capture implementation is absent")
	}
	captureBody := source[captureStart : captureStart+captureEnd]
	openIndex := strings.Index(captureBody, "windows.OpenProcess")
	enumerateIndex := strings.Index(captureBody, "enumerateProcesses()")
	if openIndex < 0 || enumerateIndex < 0 || openIndex > enumerateIndex {
		t.Fatal("windows observer must bind the exact process handle before later enumeration")
	}
	if !strings.Contains(captureBody, "recordUnboundConsoleShow") {
		t.Fatal("windows observer may silently discard an unbound transient console SHOW")
	}
}

func TestWindowsFixtureCleanupArmsBeforeCreationAndVerifiesTaskAbsence(t *testing.T) {
	sourceBytes, err := os.ReadFile("adapter_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	seedStart := strings.Index(source, "func (a *windowsAdapter) SeedLegacyLaunchers(")
	if seedStart < 0 {
		t.Fatal("Windows fixture seeding implementation is absent")
	}
	seedEnd := strings.Index(source[seedStart:], "\nfunc (a *windowsAdapter) Install(")
	if seedEnd < 0 {
		t.Fatal("Windows fixture seeding implementation is absent")
	}
	seedBody := source[seedStart : seedStart+seedEnd]
	armIndex := strings.Index(seedBody, "a.fixtureSeeded = true")
	linkIndex := strings.Index(seedBody, "os.WriteFile(a.fixtureLink")
	registerIndex := strings.Index(seedBody, "Register-ScheduledTask")
	if armIndex < 0 || linkIndex < 0 || registerIndex < 0 || armIndex > linkIndex || armIndex > registerIndex {
		t.Fatal("Windows fixture cleanup must be armed before either native fixture can be created")
	}
	cleanupStart := strings.Index(source, "func (a *windowsAdapter) CleanupFixtures(")
	if cleanupStart < 0 {
		t.Fatal("Windows fixture cleanup implementation is absent")
	}
	cleanupEnd := strings.Index(source[cleanupStart:], "\nfunc (a *windowsAdapter) legacyLaunchers(")
	if cleanupEnd < 0 {
		t.Fatal("Windows fixture cleanup implementation is absent")
	}
	cleanupBody := source[cleanupStart : cleanupStart+cleanupEnd]
	if !strings.Contains(cleanupBody, "$remaining = @(Get-ScheduledTask") ||
		!strings.Contains(cleanupBody, "if ($remaining.Count -ne 0)") {
		t.Fatal("Windows fixture cleanup does not re-prove the exact Scheduled Task absent")
	}
}
