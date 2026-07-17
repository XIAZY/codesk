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

func TestWindowsAcceptanceUsesOnlyTheCurrentMSILifecycleContract(t *testing.T) {
	files := []string{"adapter_windows.go", "msi_lifecycle_policy.go", "msi_lifecycle_windows.go", "windows_release.go"}
	var source strings.Builder
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source.Write(data)
	}
	combined := source.String()
	for _, obsolete := range []string{"Codesk" + "Setup_", "Uninstall Codesk" + ".exe", "setup" + "Root", "--no" + "-launch"} {
		if strings.Contains(combined, obsolete) {
			t.Fatalf("Windows acceptance reintroduced obsolete Setup contract %q", obsolete)
		}
	}
	for _, required := range []string{
		"CodeskMSI_", "msiexec.exe", "fresh-install-disabled-sentinel", "repair-preserves-disabled",
		"repair-preserves-enabled", "major-upgrade-preserves-disabled", "major-upgrade-preserves-enabled",
		"uninstall-preserves-sibling-and-shared-key", "x64-to-arm64-handoff", "lifecycle-cleanup",
		"firstCleanupErr := cleanupPass()", "secondCleanupErr := cleanupPass()",
		"cleanupOwnership := msiLifecycleCleanupOwnership{}", "runMSILifecycleCleanupPass(",
		"MSI lifecycle cleanup left residue",
	} {
		if !strings.Contains(combined, required) {
			t.Fatalf("Windows MSI acceptance contract %q is absent", required)
		}
	}
	baseline := strings.Index(combined, `row("clean-baseline"`)
	armCleanup := strings.Index(combined, "cleanupOwnership.armProducts()")
	if baseline < 0 || armCleanup <= baseline {
		t.Fatal("MSI product cleanup must remain disarmed until the clean-account baseline passes")
	}
}

func TestWindowsMSILifecycleCountsEachResidentProcessOnce(t *testing.T) {
	data, err := os.ReadFile("msi_lifecycle_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "func (a *windowsAdapter) captureMSILifecycleState(ctx context.Context)")
	end := strings.Index(source, "func (a *windowsAdapter) requireLifecycleInstall(")
	if start < 0 || end <= start {
		t.Fatal("MSI lifecycle state capture implementation is absent")
	}
	body := source[start:end]
	for _, required := range []string{"processes, err := enumerateProcesses()", "strings.EqualFold(process.Executable, a.paths.desktop)"} {
		if !strings.Contains(body, required) {
			t.Fatalf("MSI lifecycle resident-process oracle %q is absent", required)
		}
	}
	if got := strings.Count(body, "state.ResidentProcess++"); got != 1 {
		t.Fatalf("MSI lifecycle must increment the resident count exactly once per matching process; found %d increments", got)
	}
}

func TestWindowsPowerShellProbeUsesCanonicalEnvironmentOverrides(t *testing.T) {
	data, err := os.ReadFile("adapter_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "func (a *windowsAdapter) runPowerShell(")
	end := strings.Index(source, "func (a *windowsAdapter) runMSI(")
	if start < 0 || end <= start {
		t.Fatal("Windows PowerShell probe implementation is absent")
	}
	body := source[start:end]
	if !strings.Contains(body, "command.Env = environmentWithOverrides(values)") {
		t.Fatal("Windows PowerShell probes do not use canonical case-insensitive environment replacement")
	}
	if strings.Contains(source, "func mergeEnvironment(") {
		t.Fatal("Windows acceptance reintroduced duplicate-preserving environment merging")
	}
}
