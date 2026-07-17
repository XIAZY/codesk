//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"notty/daemon/internal/desktopacceptance"
)

const (
	msiLifecycleSchema    = 1
	msiLifecycleFile      = "msi-lifecycle.json"
	msiSiblingValuePrefix = "CodeskAcceptanceSibling"
	msiSiblingValue       = "preserve-this-sibling-value"
)

type msiLifecycleReport struct {
	SchemaVersion        int                     `json:"schema_version"`
	StartedAt            time.Time               `json:"started_at"`
	FinishedAt           time.Time               `json:"finished_at"`
	Status               string                  `json:"status"`
	SourceRevision       string                  `json:"source_revision"`
	RunnerSourceRevision string                  `json:"runner_source_revision"`
	HostArchitecture     string                  `json:"host_architecture"`
	Previous             msiLifecycleRelease     `json:"previous"`
	Candidate            msiLifecycleRelease     `json:"candidate"`
	Rows                 []msiLifecycleReportRow `json:"rows"`
}

type msiLifecycleRelease struct {
	Version                 string                 `json:"version"`
	SourceRevision          string                 `json:"source_revision"`
	UpgradeCode             string                 `json:"upgrade_code"`
	CrossArchitecturePolicy string                 `json:"cross_architecture_policy"`
	Artifacts               []msiLifecycleArtifact `json:"artifacts"`
}

type msiLifecycleArtifact struct {
	Architecture    string `json:"architecture"`
	PackageSHA256   string `json:"package_sha256"`
	ProductCode     string `json:"product_code"`
	CodeskSHA256    string `json:"codesk_sha256"`
	AgentToolSHA256 string `json:"agent_tool_sha256"`
}

type msiLifecycleReportRow struct {
	Name       string        `json:"name"`
	Status     string        `json:"status"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration_ns"`
	Detail     string        `json:"detail"`
}

type msiShortcutInspection struct {
	Target           string `json:"Target"`
	WorkingDirectory string `json:"WorkingDirectory"`
	Arguments        string `json:"Arguments"`
}

func (a *windowsAdapter) ensureMSILifecycle(ctx context.Context) error {
	if a.config.Phase == desktopacceptance.PhaseResume {
		return nil
	}
	a.mu.Lock()
	if a.lifecycleFinished {
		err := a.lifecycleErr
		a.mu.Unlock()
		return err
	}
	if a.lifecycleStarted {
		a.mu.Unlock()
		return errors.New("MSI lifecycle preflight was invoked concurrently")
	}
	a.lifecycleStarted = true
	a.mu.Unlock()

	err := a.runMSILifecycle(ctx)
	a.mu.Lock()
	a.lifecycleErr = err
	a.lifecycleFinished = true
	a.mu.Unlock()
	return err
}

func (a *windowsAdapter) runMSILifecycle(ctx context.Context) (runErr error) {
	previous, candidate, err := a.boundLifecycleReleases()
	if err != nil {
		return err
	}
	hostArchitecture, err := nativeArchitecture()
	if err != nil {
		return err
	}
	if compareMSIVersions(candidate.Version, previous.Version) <= 0 {
		return fmt.Errorf("candidate MSI version %s must be greater than previous %s", candidate.Version, previous.Version)
	}
	if candidate.CrossArchitecturePolicy != previous.CrossArchitecturePolicy {
		return errors.New("previous and candidate manifests disagree on cross-architecture policy")
	}
	if err := distinctLifecycleProductCodes(previous, candidate); err != nil {
		return err
	}

	report := msiLifecycleReport{
		SchemaVersion:        msiLifecycleSchema,
		StartedAt:            time.Now().UTC(),
		Status:               "FAIL",
		SourceRevision:       a.config.SourceRevision,
		RunnerSourceRevision: a.config.RunnerSourceRevision,
		HostArchitecture:     hostArchitecture,
		Previous:             lifecycleRelease(previous),
		Candidate:            lifecycleRelease(candidate),
	}
	cleanupOwnership := msiLifecycleCleanupOwnership{}
	defer func() {
		cleanupStarted := time.Now().UTC()
		cleanupPass := func() error {
			return runMSILifecycleCleanupPass(
				a.config.Timeout,
				&cleanupOwnership,
				func(cleanupCtx context.Context) error {
					return a.cleanupLifecycleProducts(cleanupCtx, previous, candidate)
				},
				func(cleanupCtx context.Context) error { return a.cleanupMSIFixtures(cleanupCtx) },
			)
		}
		firstCleanupErr := cleanupPass()
		secondCleanupErr := cleanupPass()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), a.config.Timeout)
		cleanupState, stateErr := a.captureMSILifecycleState(cleanupCtx)
		cancel()
		var residueErr error
		if stateErr != nil {
			residueErr = stateErr
		} else if cleanupState.SiblingExists {
			residueErr = fmt.Errorf("MSI lifecycle cleanup left its sibling registry fixture: %+v", cleanupState)
		} else if cleanupOwnership.productsOwned && (cleanupState.ProductCode != "" || cleanupState.PayloadExists ||
			len(cleanupState.PayloadHashes) != 0 || len(cleanupState.PayloadMachines) != 0 || cleanupState.ShortcutRoot ||
			cleanupState.ShortcutExists || cleanupState.ShortcutTarget != "" || cleanupState.ShortcutWorkingDirectory != "" ||
			cleanupState.ShortcutArguments != "" || cleanupState.RunValueExists || cleanupState.ResidentProcess != 0) {
			residueErr = fmt.Errorf("MSI lifecycle cleanup left residue: %+v", cleanupState)
		}
		cleanupErr := errors.Join(firstCleanupErr, secondCleanupErr, residueErr)
		cleanupFinished := time.Now().UTC()
		cleanupStatus := "PASS"
		cleanupDetail := "two idempotent cleanup passes removed every acceptance-owned product and registry fixture"
		if cleanupErr != nil {
			cleanupStatus = "FAIL"
			cleanupDetail = cleanupErr.Error()
			runErr = errors.Join(runErr, fmt.Errorf("MSI lifecycle cleanup: %w", cleanupErr))
		}
		report.Rows = append(report.Rows, msiLifecycleReportRow{
			Name: "lifecycle-cleanup", Status: cleanupStatus, StartedAt: cleanupStarted, FinishedAt: cleanupFinished,
			Duration: cleanupFinished.Sub(cleanupStarted), Detail: cleanupDetail,
		})
		if runErr == nil {
			report.Status = "PASS"
		}
		report.FinishedAt = time.Now().UTC()
		if err := writeMSILifecycleReport(a.evidenceDir, report); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()

	row := func(name string, operation func() (string, error)) error {
		started := time.Now().UTC()
		detail, err := operation()
		status := "PASS"
		if err != nil {
			status = "FAIL"
			detail = err.Error()
		}
		finished := time.Now().UTC()
		report.Rows = append(report.Rows, msiLifecycleReportRow{
			Name: name, Status: status, StartedAt: started, FinishedAt: finished,
			Duration: finished.Sub(started), Detail: detail,
		})
		return err
	}

	if err := row("clean-baseline", func() (string, error) {
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := requireCleanMSILifecycleBaseline(state); err != nil {
			return "", err
		}
		return "no product, payload, shortcut, Run value, or resident process", nil
	}); err != nil {
		return err
	}
	if err := row("shared-run-key-sibling-fixture", func() (string, error) {
		if err := a.seedMSISibling(); err != nil {
			return "", err
		}
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if !state.RunKeyExists || !state.SiblingExists || state.SiblingValue != msiSiblingValue {
			return "", errors.New("shared Run key sibling fixture was not persisted exactly")
		}
		return "shared Run key and exact sibling value are present", nil
	}); err != nil {
		return err
	}

	candidateNative, _ := candidate.Artifact(hostArchitecture)
	previousNative, _ := previous.Artifact(hostArchitecture)
	// The clean-account gate has passed, so every recognized product created
	// from this point belongs to this preflight and is safe to remove on failure.
	cleanupOwnership.armProducts()
	if err := row("fresh-install-disabled-sentinel", func() (string, error) {
		if err := a.runMSI(ctx, "lifecycle-fresh-install", "/i", candidateNative.Path); err != nil {
			return "", err
		}
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := a.requireLifecycleInstall(state, candidate, candidateNative, "disabled"); err != nil {
			return "", err
		}
		return "one exact current product/payload, nested shortcut, empty Run sentinel, no launch", nil
	}); err != nil {
		return err
	}
	if err := row("repair-preserves-disabled", func() (string, error) {
		if err := a.runMSI(ctx, "lifecycle-repair-disabled", "/famus", candidateNative.ProductCode); err != nil {
			return "", err
		}
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := a.requireLifecycleInstall(state, candidate, candidateNative, "disabled"); err != nil {
			return "", err
		}
		return "repair retained the empty disabled sentinel and exact payload", nil
	}); err != nil {
		return err
	}
	if err := row("repair-preserves-enabled", func() (string, error) {
		if err := a.setCodeskRunValue(quote(a.paths.desktop)); err != nil {
			return "", err
		}
		if err := a.runMSI(ctx, "lifecycle-repair-enabled", "/famus", candidateNative.ProductCode); err != nil {
			return "", err
		}
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := a.requireLifecycleInstall(state, candidate, candidateNative, "enabled"); err != nil {
			return "", err
		}
		return "repair retained the exact quoted installed command", nil
	}); err != nil {
		return err
	}
	if err := row("uninstall-preserves-sibling-and-shared-key", func() (string, error) {
		if err := a.runMSI(ctx, "lifecycle-fresh-uninstall", "/x", candidateNative.ProductCode); err != nil {
			return "", err
		}
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := requireLifecycleUninstalled(state); err != nil {
			return "", err
		}
		return "owned Codesk value/product/payload/shortcut removed; sibling value and shared Run key preserved", nil
	}); err != nil {
		return err
	}

	if err := a.sameArchitectureUpgradeRows(ctx, row, previous, candidate, previousNative, candidateNative); err != nil {
		return err
	}
	if hostArchitecture == "arm64" {
		if err := a.crossArchitectureUpgradeRow(ctx, row, previous, candidate); err != nil {
			return err
		}
	} else {
		started := time.Now().UTC()
		report.Rows = append(report.Rows, msiLifecycleReportRow{
			Name: "x64-to-arm64-handoff", Status: "WAIVED", StartedAt: started, FinishedAt: started,
			Detail: "requires the native ARM64 runner; AMD64 row does not claim cross-architecture execution",
		})
	}
	return nil
}

func (a *windowsAdapter) sameArchitectureUpgradeRows(
	ctx context.Context,
	row func(string, func() (string, error)) error,
	previous, candidate desktopacceptance.Release,
	previousArtifact, candidateArtifact desktopacceptance.ReleaseArtifact,
) error {
	if err := row("major-upgrade-preserves-disabled", func() (string, error) {
		if err := a.runMSI(ctx, "lifecycle-previous-disabled", "/i", previousArtifact.Path); err != nil {
			return "", err
		}
		previousState, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		started, err := runAfterValidatedMSILifecycleState(
			previousState,
			a.lifecycleInstallExpectation(previous.Version, previousArtifact, "disabled"),
			func() error { return a.runMSI(ctx, "lifecycle-upgrade-disabled", "/i", candidateArtifact.Path) },
		)
		if !started || err != nil {
			return "", err
		}
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := a.requireLifecycleInstall(state, candidate, candidateArtifact, "disabled"); err != nil {
			return "", err
		}
		if err := a.runMSI(ctx, "lifecycle-upgrade-disabled-cleanup", "/x", candidateArtifact.ProductCode); err != nil {
			return "", err
		}
		reset, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := requireLifecycleUninstalled(reset); err != nil {
			return "", err
		}
		return "previous state sealed; candidate replaced it with the disabled sentinel, then uninstall restored the sibling-preserving baseline", nil
	}); err != nil {
		return err
	}
	if err := row("major-upgrade-preserves-enabled", func() (string, error) {
		if err := a.runMSI(ctx, "lifecycle-previous-enabled", "/i", previousArtifact.Path); err != nil {
			return "", err
		}
		if err := a.setCodeskRunValue(quote(a.paths.desktop)); err != nil {
			return "", err
		}
		previousState, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		started, err := runAfterValidatedMSILifecycleState(
			previousState,
			a.lifecycleInstallExpectation(previous.Version, previousArtifact, "enabled"),
			func() error { return a.runMSI(ctx, "lifecycle-upgrade-enabled", "/i", candidateArtifact.Path) },
		)
		if !started || err != nil {
			return "", err
		}
		state, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := a.requireLifecycleInstall(state, candidate, candidateArtifact, "enabled"); err != nil {
			return "", err
		}
		if err := a.runMSI(ctx, "lifecycle-upgrade-enabled-cleanup", "/x", candidateArtifact.ProductCode); err != nil {
			return "", err
		}
		reset, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		if err := requireLifecycleUninstalled(reset); err != nil {
			return "", err
		}
		return "previous state sealed; candidate replaced it with the exact enabled command, then uninstall restored the sibling-preserving baseline", nil
	}); err != nil {
		return err
	}
	return nil
}

func (a *windowsAdapter) crossArchitectureUpgradeRow(
	ctx context.Context,
	row func(string, func() (string, error)) error,
	previous, candidate desktopacceptance.Release,
) error {
	previousX64, _ := previous.Artifact("amd64")
	candidateARM64, _ := candidate.Artifact("arm64")
	return row("x64-to-arm64-handoff", func() (string, error) {
		if err := a.runMSI(ctx, "lifecycle-previous-x64", "/i", previousX64.Path); err != nil {
			return "", err
		}
		if err := a.setCodeskRunValue(quote(a.paths.desktop)); err != nil {
			return "", err
		}
		before, err := a.captureMSILifecycleState(ctx)
		if err != nil {
			return "", err
		}
		started, installErr := runAfterValidatedMSILifecycleState(
			before,
			a.lifecycleInstallExpectation(previous.Version, previousX64, "enabled"),
			func() error { return a.runMSI(ctx, "lifecycle-upgrade-arm64", "/i", candidateARM64.Path) },
		)
		if !started {
			return "", installErr
		}
		after, snapshotErr := a.captureMSILifecycleState(ctx)
		if snapshotErr != nil {
			return "", snapshotErr
		}
		switch candidate.CrossArchitecturePolicy {
		case windowsMSICrossArchConverge:
			if installErr != nil {
				return "", installErr
			}
			if err := a.requireLifecycleInstall(after, candidate, candidateARM64, "enabled"); err != nil {
				return "", err
			}
			return "x64 product removed; one ARM64 candidate ProductCode/payload and exact Run value remain", nil
		case windowsMSICrossArchBlock:
			if installErr == nil {
				return "", errors.New("manifest requires fail-closed cross-architecture handoff but ARM64 install succeeded")
			}
			if !equalMSILifecycleState(before, after) {
				return "", fmt.Errorf("blocked ARM64 handoff mutated the installed x64 state: before=%+v after=%+v", before, after)
			}
			return "ARM64 handoff failed closed before any x64 product/payload/Run mutation", nil
		default:
			return "", errors.New("invalid cross-architecture policy")
		}
	})
}

func (a *windowsAdapter) boundLifecycleReleases() (desktopacceptance.Release, desktopacceptance.Release, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	previous, previousOK := a.releases[a.config.Previous.Version]
	candidate, candidateOK := a.releases[a.config.Candidate.Version]
	if !previousOK || !candidateOK {
		return desktopacceptance.Release{}, desktopacceptance.Release{}, errors.New("both source-bound releases must be verified before MSI lifecycle execution")
	}
	return previous, candidate, nil
}

func distinctLifecycleProductCodes(releases ...desktopacceptance.Release) error {
	seen := make(map[string]string)
	for _, release := range releases {
		for _, artifact := range release.Artifacts {
			key := strings.ToUpper(artifact.ProductCode)
			if owner, exists := seen[key]; exists {
				return fmt.Errorf("ProductCode %s is reused by %s and %s", artifact.ProductCode, owner, release.Version+"/"+artifact.Architecture)
			}
			seen[key] = release.Version + "/" + artifact.Architecture
		}
	}
	return nil
}

func lifecycleRelease(release desktopacceptance.Release) msiLifecycleRelease {
	value := msiLifecycleRelease{
		Version: release.Version, SourceRevision: release.SourceRevision, UpgradeCode: release.UpgradeCode,
		CrossArchitecturePolicy: release.CrossArchitecturePolicy,
	}
	for _, artifact := range release.Artifacts {
		value.Artifacts = append(value.Artifacts, msiLifecycleArtifact{
			Architecture: artifact.Architecture, PackageSHA256: artifact.SHA256, ProductCode: artifact.ProductCode,
			CodeskSHA256: artifact.CodeskSHA256, AgentToolSHA256: artifact.AgentToolSHA256,
		})
	}
	return value
}

func compareMSIVersions(left, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for index := 0; index < 3; index++ {
		leftValue, _ := strconv.Atoi(leftParts[index])
		rightValue, _ := strconv.Atoi(rightParts[index])
		if leftValue != rightValue {
			return leftValue - rightValue
		}
	}
	return 0
}

func (a *windowsAdapter) captureMSILifecycleState(ctx context.Context) (msiLifecycleState, error) {
	state := msiLifecycleState{RunState: "absent"}
	registration, err := a.installedMSIRegistration()
	if err != nil {
		return state, err
	}
	if registration != nil {
		state.ProductCode = registration.productCode
		state.Version = registration.version
	}
	installInfo, err := os.Lstat(a.paths.installRoot)
	if err == nil {
		if !installInfo.IsDir() || installInfo.Mode()&os.ModeSymlink != 0 {
			return state, errors.New("MSI install root is not a real directory")
		}
		if reparse, reparseErr := windowsReparsePoint(a.paths.installRoot); reparseErr != nil || reparse {
			return state, errors.New("MSI install root is a reparse point or cannot be verified")
		}
		entries, readErr := os.ReadDir(a.paths.installRoot)
		if readErr != nil {
			return state, readErr
		}
		state.PayloadExists = true
		state.PayloadHashes = make(map[string]string, len(entries))
		state.PayloadMachines = make(map[string]string, len(entries))
		for _, entry := range entries {
			path := filepath.Join(a.paths.installRoot, entry.Name())
			info, infoErr := os.Lstat(path)
			if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return state, fmt.Errorf("MSI payload entry %s is not a regular file", entry.Name())
			}
			if reparse, reparseErr := windowsReparsePoint(path); reparseErr != nil || reparse {
				return state, fmt.Errorf("MSI payload entry %s is a reparse point or cannot be verified", entry.Name())
			}
			hash, hashErr := fileSHA256(path)
			if hashErr != nil {
				return state, hashErr
			}
			state.PayloadHashes[entry.Name()] = hash
			machine, machineErr := portableExecutableArchitecture(path)
			if machineErr != nil {
				return state, machineErr
			}
			state.PayloadMachines[entry.Name()] = machine
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	shortcutRoot := filepath.Dir(a.paths.shortcut)
	shortcutRootInfo, err := os.Lstat(shortcutRoot)
	if err == nil {
		if !shortcutRootInfo.IsDir() || shortcutRootInfo.Mode()&os.ModeSymlink != 0 {
			return state, errors.New("MSI shortcut root is not a real directory")
		}
		if reparse, reparseErr := windowsReparsePoint(shortcutRoot); reparseErr != nil || reparse {
			return state, errors.New("MSI shortcut root is a reparse point or cannot be verified")
		}
		state.ShortcutRoot = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	shortcutInfo, err := os.Lstat(a.paths.shortcut)
	if err == nil {
		if !shortcutInfo.Mode().IsRegular() || shortcutInfo.Mode()&os.ModeSymlink != 0 {
			return state, errors.New("MSI shortcut is not a regular file")
		}
		if reparse, reparseErr := windowsReparsePoint(a.paths.shortcut); reparseErr != nil || reparse {
			return state, errors.New("MSI shortcut is a reparse point or cannot be verified")
		}
		state.ShortcutExists = true
		shortcut, shortcutErr := a.inspectMSIShortcut(ctx)
		if shortcutErr != nil {
			return state, shortcutErr
		}
		state.ShortcutTarget = shortcut.Target
		state.ShortcutWorkingDirectory = shortcut.WorkingDirectory
		state.ShortcutArguments = shortcut.Arguments
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err == nil {
		state.RunKeyExists = true
		value, valueType, valueErr := key.GetStringValue("Codesk")
		if valueErr == nil {
			state.RunValueExists = true
			state.RunValue = value
			switch {
			case valueType != registry.SZ:
				state.RunState = "other"
			case value == "":
				state.RunState = "disabled"
			case value == quote(a.paths.desktop):
				state.RunState = "enabled"
			default:
				state.RunState = "other"
			}
		} else if !errors.Is(valueErr, windows.ERROR_FILE_NOT_FOUND) {
			key.Close()
			return state, valueErr
		}
		siblingName := a.msiSiblingName()
		sibling, siblingType, siblingErr := key.GetStringValue(siblingName)
		if siblingErr == nil {
			state.SiblingExists = true
			state.SiblingValue = sibling
			if siblingType != registry.SZ {
				state.SiblingValue = "not-a-string"
			}
		} else if !errors.Is(siblingErr, windows.ERROR_FILE_NOT_FOUND) {
			key.Close()
			return state, siblingErr
		}
		key.Close()
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return state, err
	}
	processes, err := enumerateProcesses()
	if err != nil {
		return state, err
	}
	for _, process := range processes {
		if strings.EqualFold(process.Executable, a.paths.desktop) {
			state.ResidentProcess++
		}
	}
	return state, nil
}

func (a *windowsAdapter) inspectMSIShortcut(ctx context.Context) (msiShortcutInspection, error) {
	output, err := a.runPowerShell(ctx, `$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$shortcut = $shell.CreateShortcut($env:CODESK_ACCEPT_SHORTCUT)
[pscustomobject]@{
    Target = [string]$shortcut.TargetPath
    WorkingDirectory = [string]$shortcut.WorkingDirectory
    Arguments = [string]$shortcut.Arguments
} | ConvertTo-Json -Compress`, map[string]string{"CODESK_ACCEPT_SHORTCUT": a.paths.shortcut})
	if err != nil {
		return msiShortcutInspection{}, err
	}
	var inspection msiShortcutInspection
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &inspection); err != nil {
		return msiShortcutInspection{}, fmt.Errorf("decode MSI shortcut identity: %w", err)
	}
	return inspection, nil
}

func (a *windowsAdapter) requireLifecycleInstall(
	state msiLifecycleState,
	release desktopacceptance.Release,
	artifact desktopacceptance.ReleaseArtifact,
	runState string,
) error {
	return requireMSILifecycleInstallState(state, a.lifecycleInstallExpectation(release.Version, artifact, runState))
}

func (a *windowsAdapter) lifecycleInstallExpectation(
	version string,
	artifact desktopacceptance.ReleaseArtifact,
	runState string,
) msiLifecycleInstallExpectation {
	runValue := ""
	if runState == "enabled" {
		runValue = quote(a.paths.desktop)
	}
	return msiLifecycleInstallExpectation{
		ProductCode:  artifact.ProductCode,
		Version:      version,
		Architecture: artifact.Architecture,
		PayloadHashes: map[string]string{
			"Codesk.exe":           artifact.CodeskSHA256,
			"notty-agent-tool.exe": artifact.AgentToolSHA256,
		},
		ShortcutTarget:           a.paths.desktop,
		ShortcutWorkingDirectory: a.paths.installRoot,
		ShortcutArguments:        "",
		RunState:                 runState,
		RunValue:                 runValue,
		SiblingValue:             msiSiblingValue,
	}
}

func requireLifecycleUninstalled(state msiLifecycleState) error {
	if state.ProductCode != "" || state.Version != "" || state.PayloadExists || state.ShortcutRoot || state.ShortcutExists || state.RunValueExists ||
		len(state.PayloadHashes) != 0 || len(state.PayloadMachines) != 0 ||
		state.ShortcutTarget != "" || state.ShortcutWorkingDirectory != "" || state.ShortcutArguments != "" ||
		!state.RunKeyExists || !state.SiblingExists || state.SiblingValue != msiSiblingValue || state.ResidentProcess != 0 {
		return fmt.Errorf("uninstall did not remove only Codesk-owned state: %+v", state)
	}
	return nil
}

func equalMSILifecycleState(left, right msiLifecycleState) bool {
	return left.ProductCode == right.ProductCode && left.Version == right.Version && left.PayloadExists == right.PayloadExists &&
		equalStringMap(left.PayloadHashes, right.PayloadHashes) && equalStringMap(left.PayloadMachines, right.PayloadMachines) &&
		left.ShortcutRoot == right.ShortcutRoot && left.ShortcutExists == right.ShortcutExists &&
		strings.EqualFold(left.ShortcutTarget, right.ShortcutTarget) &&
		strings.EqualFold(left.ShortcutWorkingDirectory, right.ShortcutWorkingDirectory) &&
		left.ShortcutArguments == right.ShortcutArguments &&
		left.RunKeyExists == right.RunKeyExists && left.RunValueExists == right.RunValueExists && left.RunValue == right.RunValue &&
		left.RunState == right.RunState && left.SiblingExists == right.SiblingExists && left.SiblingValue == right.SiblingValue &&
		left.ResidentProcess == right.ResidentProcess
}

func (a *windowsAdapter) msiSiblingName() string {
	return fmt.Sprintf("%s%d", msiSiblingValuePrefix, os.Getpid())
}

func (a *windowsAdapter) seedMSISibling() error {
	a.mu.Lock()
	if a.siblingSeeded {
		a.mu.Unlock()
		return errors.New("MSI sibling fixture is already armed")
	}
	a.siblingSeeded = true
	a.mu.Unlock()
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if _, _, err := key.GetStringValue(a.msiSiblingName()); err == nil {
		return errors.New("MSI sibling fixture value already exists")
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	return key.SetStringValue(a.msiSiblingName(), msiSiblingValue)
}

func (a *windowsAdapter) setCodeskRunValue(value string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue("Codesk", value)
}

func (a *windowsAdapter) cleanupLifecycleProducts(
	ctx context.Context,
	previous, candidate desktopacceptance.Release,
) error {
	known := make(map[string]string)
	for _, release := range []desktopacceptance.Release{candidate, previous} {
		for _, artifact := range release.Artifacts {
			known[strings.ToUpper(artifact.ProductCode)] = artifact.ProductCode
		}
	}
	return drainKnownMSIProducts(
		ctx,
		5*time.Second,
		250*time.Millisecond,
		func() ([]string, error) { return installedKnownMSIProductCodes(known) },
		func(cleanupCtx context.Context, code string) error {
			return a.runMSI(cleanupCtx, "lifecycle-failure-cleanup", "/x", code)
		},
	)
}

func installedKnownMSIProductCodes(known map[string]string) ([]string, error) {
	root, err := registry.OpenKey(registry.CURRENT_USER, uninstallKeyRoot, registry.ENUMERATE_SUB_KEYS)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}
	var codes []string
	var scanErr error
	for _, name := range names {
		if canonical, ok := canonicalKnownMSIProductCode(name, known); ok {
			codes = append(codes, canonical)
			continue
		}
		key, openErr := registry.OpenKey(registry.CURRENT_USER, uninstallKeyRoot+`\`+name, registry.QUERY_VALUE)
		if openErr != nil {
			continue
		}
		displayName, _, displayErr := key.GetStringValue("DisplayName")
		key.Close()
		if displayErr != nil || !strings.EqualFold(displayName, "Codesk") {
			continue
		}
		scanErr = errors.Join(scanErr, fmt.Errorf("refusing to remove unknown Codesk ProductCode %s", name))
	}
	slices.Sort(codes)
	return codes, scanErr
}

func (a *windowsAdapter) cleanupMSIFixtures(context.Context) error {
	a.mu.Lock()
	seeded := a.siblingSeeded
	a.mu.Unlock()
	if !seeded {
		return nil
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		a.mu.Lock()
		a.siblingSeeded = false
		a.mu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("open shared Run key for MSI fixture cleanup: %w", err)
	}
	defer key.Close()
	value, valueType, err := key.GetStringValue(a.msiSiblingName())
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		a.mu.Lock()
		a.siblingSeeded = false
		a.mu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("read MSI sibling fixture during cleanup: %w", err)
	}
	if valueType != registry.SZ || value != msiSiblingValue {
		return errors.New("MSI sibling fixture changed unexpectedly; refusing to delete it")
	}
	if err := key.DeleteValue(a.msiSiblingName()); err != nil {
		return fmt.Errorf("delete MSI sibling fixture: %w", err)
	}
	a.mu.Lock()
	a.siblingSeeded = false
	a.mu.Unlock()
	return nil
}

func writeMSILifecycleReport(directory string, report msiLifecycleReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode MSI lifecycle report: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".msi-lifecycle-*.tmp")
	if err != nil {
		return fmt.Errorf("create MSI lifecycle report: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	path := filepath.Join(directory, msiLifecycleFile)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("MSI lifecycle report already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(temporaryName, path)
}
