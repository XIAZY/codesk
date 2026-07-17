//go:build windows

package main

import (
	"bytes"
	"context"
	"debug/pe"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"notty/daemon/internal/desktopacceptance"
	"notty/daemon/internal/desktopstate"
)

const (
	runKeyPath                = `Software\Microsoft\Windows\CurrentVersion\Run`
	uninstallKeyRoot          = `Software\Microsoft\Windows\CurrentVersion\Uninstall`
	legacyPrefix              = "Codesk daemon "
	secondLaunchExitTimeout   = 15 * time.Second
	secondLaunchRootQuietTime = 750 * time.Millisecond
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procIsProcessInJob        = kernel32.NewProc("IsProcessInJob")
	procIsWow64Process2       = kernel32.NewProc("IsWow64Process2")
	plaintextCredentialShapes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{12,}`),
		regexp.MustCompile(`\b(?:sk_agent|sk_machine|nottyd)_[A-Za-z0-9_-]{8,}`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	}
)

type windowsPaths struct {
	dataRoot       string
	legacyRoot     string
	logsRoot       string
	installRoot    string
	desktop        string
	agentTool      string
	shortcut       string
	startup        string
	secrets        string
	desktopLog     string
	runtimeReceipt string
	powerShell     string
	msiexec        string
}

type windowsAdapter struct {
	paths              windowsPaths
	evidenceDir        string
	surfaceFiles       []string
	surfaceDirectories []string
	mu                 sync.Mutex
	known              map[int]string
	logOffset          int64
	config             desktopacceptance.Config
	releases           map[string]desktopacceptance.Release
	lifecycleStarted   bool
	lifecycleFinished  bool
	lifecycleErr       error
	msiLogSequence     uint64
	siblingSeeded      bool
}

type tokenStatistics struct {
	TokenID            windows.LUID
	AuthenticationID   windows.LUID
	ExpirationTime     int64
	TokenType          uint32
	ImpersonationLevel uint32
	DynamicCharged     uint32
	DynamicAvailable   uint32
	GroupCount         uint32
	PrivilegeCount     uint32
	ModifiedID         windows.LUID
}

func newNativeAdapter(config desktopacceptance.Config) (desktopacceptance.NativeAdapter, error) {
	paths, err := resolvePaths()
	if err != nil {
		return nil, err
	}
	runnerPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve acceptance runner: %w", err)
	}
	protected := []string{config.EvidenceDir, config.Candidate.Directory, runnerPath}
	if config.Previous != nil {
		protected = append(protected, config.Previous.Directory)
	}
	for _, path := range protected {
		inside, err := pathWithin(paths.dataRoot, path)
		if err != nil {
			return nil, err
		}
		if inside {
			return nil, fmt.Errorf("acceptance input or evidence path must be outside the user data reset root: %s", path)
		}
		inside, err = pathWithinResolved(paths.dataRoot, path)
		if err != nil {
			return nil, err
		}
		if inside {
			return nil, fmt.Errorf("acceptance input or evidence path resolves inside the user data reset root: %s", path)
		}
	}
	surfaceDirectories := []string{config.Candidate.Directory, paths.installRoot}
	if config.Previous != nil {
		surfaceDirectories = append(surfaceDirectories, config.Previous.Directory)
	}
	return &windowsAdapter{
		paths:              paths,
		evidenceDir:        config.EvidenceDir,
		surfaceFiles:       []string{runnerPath, paths.desktop},
		surfaceDirectories: surfaceDirectories,
		known:              make(map[int]string),
		config:             config,
		releases:           make(map[string]desktopacceptance.Release),
	}, nil
}

func resolvePaths() (windowsPaths, error) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, err
	}
	profile, err := windows.KnownFolderPath(windows.FOLDERID_Profile, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return windowsPaths{}, err
	}
	userPrograms, err := windows.KnownFolderPath(windows.FOLDERID_UserProgramFiles, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, err
	}
	programs, err := windows.KnownFolderPath(windows.FOLDERID_Programs, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, err
	}
	startup, err := windows.KnownFolderPath(windows.FOLDERID_Startup, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, err
	}
	system, err := windows.KnownFolderPath(windows.FOLDERID_System, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return windowsPaths{}, err
	}
	dataRoot := filepath.Join(localAppData, "Codesk")
	installRoot := filepath.Join(userPrograms, "Codesk")
	logsRoot := filepath.Join(dataRoot, "Logs")
	paths := windowsPaths{
		dataRoot:       dataRoot,
		legacyRoot:     filepath.Join(profile, ".notty"),
		logsRoot:       logsRoot,
		installRoot:    installRoot,
		desktop:        filepath.Join(installRoot, "Codesk.exe"),
		agentTool:      filepath.Join(installRoot, "notty-agent-tool.exe"),
		shortcut:       filepath.Join(programs, "Codesk", "Codesk.lnk"),
		startup:        startup,
		secrets:        filepath.Join(dataRoot, "Secrets"),
		desktopLog:     filepath.Join(logsRoot, "codesk-desktop.log"),
		runtimeReceipt: filepath.Join(logsRoot, "codesk-desktop.log"),
		powerShell:     filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe"),
		msiexec:        filepath.Join(system, "msiexec.exe"),
	}
	for _, path := range []string{
		paths.dataRoot, paths.legacyRoot, paths.logsRoot, paths.installRoot, paths.desktop,
		paths.agentTool, paths.shortcut, paths.startup, paths.secrets, paths.desktopLog,
		paths.runtimeReceipt, paths.powerShell, paths.msiexec,
	} {
		if err := validateResolvedWindowsPath(path); err != nil {
			return windowsPaths{}, err
		}
	}
	return paths, nil
}

func validateResolvedWindowsPath(path string) error {
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\x00\"") ||
		!filepath.IsAbs(path) || path != filepath.Clean(path) {
		return errors.New("native acceptance resolved an invalid Windows path")
	}
	return nil
}

func (a *windowsAdapter) LegacyCLIState(ctx context.Context) (desktopacceptance.LegacyStateFingerprint, error) {
	value, err := fingerprintLegacyTree(ctx, a.paths.legacyRoot, windowsReparsePoint)
	if err != nil {
		if ctx.Err() != nil {
			return desktopacceptance.LegacyStateFingerprint{}, ctx.Err()
		}
		return desktopacceptance.LegacyStateFingerprint{}, errors.New("inspect legacy CLI state")
	}
	return value, nil
}

func windowsReparsePoint(path string) (bool, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func (a *windowsAdapter) Host(context.Context) (desktopacceptance.Host, error) {
	architecture, err := nativeArchitecture()
	if err != nil {
		return desktopacceptance.Host{}, err
	}
	if runtime.GOARCH != architecture {
		return desktopacceptance.Host{}, fmt.Errorf("acceptance runner architecture %s is emulated on native %s; use the native runner", runtime.GOARCH, architecture)
	}
	runnerInJob, err := processInJob(os.Getpid())
	if err != nil {
		return desktopacceptance.Host{}, fmt.Errorf("inspect acceptance runner job membership: %w", err)
	}
	if runnerInJob {
		return desktopacceptance.Host{}, desktopacceptance.Blocked("acceptance runner is already inside a Job Object; product-owned containment cannot be distinguished")
	}
	version := windows.RtlGetVersion()
	hostname, err := os.Hostname()
	if err != nil {
		return desktopacceptance.Host{}, err
	}
	current, err := user.Current()
	if err != nil {
		return desktopacceptance.Host{}, err
	}
	sessionIdentity, err := loginSessionIdentity()
	if err != nil {
		return desktopacceptance.Host{}, err
	}
	return desktopacceptance.Host{
		Platform:        "windows",
		Architecture:    architecture,
		OSVersion:       fmt.Sprintf("%d.%d.%d", version.MajorVersion, version.MinorVersion, version.BuildNumber),
		Hostname:        hostname,
		Username:        current.Username,
		SessionIdentity: sessionIdentity,
	}, nil
}

func loginSessionIdentity() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", fmt.Errorf("open acceptance process token: %w", err)
	}
	defer token.Close()
	var statistics tokenStatistics
	var returned uint32
	if err := windows.GetTokenInformation(
		token,
		windows.TokenStatistics,
		(*byte)(unsafe.Pointer(&statistics)),
		uint32(unsafe.Sizeof(statistics)),
		&returned,
	); err != nil {
		return "", fmt.Errorf("read native logon session identity: %w", err)
	}
	if returned < uint32(unsafe.Sizeof(statistics)) {
		return "", errors.New("native logon session identity was truncated")
	}
	return fmt.Sprintf("%08x%08x", uint32(statistics.AuthenticationID.HighPart), statistics.AuthenticationID.LowPart), nil
}

func nativeArchitecture() (string, error) {
	var processMachine uint16
	var nativeMachine uint16
	result, _, callErr := procIsWow64Process2.Call(
		uintptr(windows.CurrentProcess()),
		uintptr(unsafe.Pointer(&processMachine)),
		uintptr(unsafe.Pointer(&nativeMachine)),
	)
	if result == 0 {
		return "", fmt.Errorf("IsWow64Process2: %w", callErr)
	}
	switch nativeMachine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "amd64", nil
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported native Windows machine 0x%x", nativeMachine)
	}
}

func portableExecutableArchitecture(path string) (string, error) {
	file, err := pe.Open(path)
	if err != nil {
		return "", fmt.Errorf("inspect installed PE %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	switch file.FileHeader.Machine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "amd64", nil
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "arm64", nil
	default:
		return "", fmt.Errorf("installed PE %s has unsupported machine 0x%x", filepath.Base(path), file.FileHeader.Machine)
	}
}

func (a *windowsAdapter) VerifyRelease(ctx context.Context, input desktopacceptance.ReleaseInput) (desktopacceptance.Release, error) {
	release, err := verifyWindowsRelease(input, func(path string) (artifactInspection, error) {
		return a.inspectArtifact(ctx, path)
	})
	if err != nil {
		return desktopacceptance.Release{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for version, existing := range a.releases {
		if version == release.Version {
			continue
		}
		for _, left := range existing.Artifacts {
			for _, right := range release.Artifacts {
				if strings.EqualFold(left.ProductCode, right.ProductCode) {
					return desktopacceptance.Release{}, fmt.Errorf("MSI ProductCode %s is reused by releases %s and %s", right.ProductCode, version, release.Version)
				}
			}
		}
	}
	a.releases[release.Version] = release
	return release, nil
}

func (a *windowsAdapter) inspectArtifact(ctx context.Context, path string) (artifactInspection, error) {
	output, err := a.runPowerShell(ctx, `$ErrorActionPreference = 'Stop'
$installer = New-Object -ComObject WindowsInstaller.Installer
$database = $installer.OpenDatabase($env:CODESK_ACCEPT_ARTIFACT, 0)
function Read-MsiProperty([string]$Name) {
    $tick = [char]96
    $query = 'SELECT ' + $tick + 'Value' + $tick + ' FROM ' + $tick + 'Property' + $tick + ' WHERE ' + $tick + 'Property' + $tick + " = '$Name'"
    $view = $database.OpenView($query)
    $view.Execute()
    $record = $view.Fetch()
    if ($null -eq $record) { return '' }
    return [string]$record.StringData(1)
}
$summary = $database.SummaryInformation(0)
$template = [string]$summary.Property(7)
$platform = (($template -split ';')[0]).ToLowerInvariant()
switch ($platform) {
    'x64' { $architecture = 'amd64' }
    'intel64' { $architecture = 'amd64' }
    'arm64' { $architecture = 'arm64' }
    default { throw "unsupported MSI template platform: $template" }
}
$allUsers = Read-MsiProperty 'ALLUSERS'
$perUserFlag = Read-MsiProperty 'MSIINSTALLPERUSER'
$perUser = [string]::IsNullOrEmpty($allUsers) -or ($allUsers -eq '2' -and $perUserFlag -eq '1')
$signature = Get-AuthenticodeSignature -LiteralPath $env:CODESK_ACCEPT_ARTIFACT -ErrorAction Stop
[pscustomobject]@{
    Architecture = $architecture
    ProductName = Read-MsiProperty 'ProductName'
    Manufacturer = Read-MsiProperty 'Manufacturer'
    ProductVersion = Read-MsiProperty 'ProductVersion'
    ProductCode = Read-MsiProperty 'ProductCode'
    UpgradeCode = Read-MsiProperty 'UpgradeCode'
    PerUser = $perUser
    SignaturePresent = ($null -ne $signature.SignerCertificate)
    SignatureValid = ($signature.Status -eq 'Valid')
    SignatureError = $signature.Status.ToString()
} | ConvertTo-Json -Compress`, map[string]string{"CODESK_ACCEPT_ARTIFACT": path})
	if err != nil {
		return artifactInspection{}, err
	}
	var inspection struct {
		Architecture     string
		ProductName      string
		Manufacturer     string
		ProductVersion   string
		ProductCode      string
		UpgradeCode      string
		PerUser          bool
		SignaturePresent bool
		SignatureValid   bool
		SignatureError   string
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inspection); err != nil {
		return artifactInspection{}, fmt.Errorf("decode native MSI inspection: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return artifactInspection{}, errors.New("decode native MSI inspection: trailing JSON value")
	}
	return artifactInspection{
		Architecture:     inspection.Architecture,
		ProductName:      inspection.ProductName,
		Manufacturer:     inspection.Manufacturer,
		ProductVersion:   inspection.ProductVersion,
		ProductCode:      inspection.ProductCode,
		UpgradeCode:      inspection.UpgradeCode,
		PerUser:          inspection.PerUser,
		SignaturePresent: inspection.SignaturePresent,
		SignatureValid:   inspection.SignatureValid,
		SignatureError:   inspection.SignatureError,
	}, nil
}

func (a *windowsAdapter) Snapshot(ctx context.Context) (desktopacceptance.State, error) {
	processes, err := enumerateProcesses()
	if err != nil {
		return desktopacceptance.State{}, err
	}
	state := desktopacceptance.State{CapturedAt: time.Now().UTC()}
	registration, err := a.installedMSIRegistration()
	if err != nil {
		return desktopacceptance.State{}, err
	}
	if registration != nil {
		state.RemovalRegistration = true
		state.InstalledVersion = registration.version
		state.Installed, err = a.installedPayloadMatches(registration.artifact)
		if err != nil {
			return desktopacceptance.State{}, err
		}
	}
	state.AutostartRegistration = registryStringExact(registry.CURRENT_USER, runKeyPath, "Codesk", quote(a.paths.desktop))
	state.UserLaunchEntry, err = a.installedShortcutTopologyPresent()
	if err != nil {
		return desktopacceptance.State{}, err
	}
	state.LegacyLaunchers, err = a.legacyLaunchers(ctx)
	if err != nil {
		return desktopacceptance.State{}, err
	}
	for _, process := range processes {
		if strings.EqualFold(process.Executable, a.paths.desktop) {
			state.Application = append(state.Application, process)
		}
	}
	rootIDs := processIDSet(state.Application)
	for _, process := range processes {
		if descendantOf(process.PID, rootIDs, processes) {
			state.ManagedDescendants = append(state.ManagedDescendants, process)
		}
	}
	a.mu.Lock()
	for _, process := range state.ManagedDescendants {
		if process.StartedAt != "" {
			a.known[process.PID] = process.StartedAt
		}
	}
	alive := make(map[int]desktopacceptance.Process, len(processes))
	for _, process := range processes {
		alive[process.PID] = process
	}
	for pid, startedAt := range a.known {
		process, ok := alive[pid]
		if !ok || process.StartedAt == "" || process.StartedAt != startedAt {
			delete(a.known, pid)
			continue
		}
		if !containsProcess(state.ManagedDescendants, pid) && !containsProcess(state.Application, pid) {
			state.ManagedDescendants = append(state.ManagedDescendants, process)
		}
	}
	a.mu.Unlock()
	slices.SortFunc(state.Application, compareProcess)
	slices.SortFunc(state.ManagedDescendants, compareProcess)
	if len(state.Application) == 1 {
		state.ProcessContained, err = processInJob(state.Application[0].PID)
		if err != nil {
			return desktopacceptance.State{}, err
		}
	}
	configurationStore, err := desktopstate.NewFileConfigurationStore(a.paths.dataRoot)
	if err != nil {
		return desktopacceptance.State{}, errors.New("construct native configuration inspector")
	}
	configurationFingerprint, err := configurationStore.Fingerprint()
	if err != nil {
		return desktopacceptance.State{}, errors.New("fingerprint native configuration")
	}
	state.ConfigurationSHA256 = configurationFingerprint.SHA256
	if configurationFingerprint.Present {
		_, loadErr := configurationStore.Load()
		state.ConfigurationValid = loadErr == nil
	}
	credentialStore, err := desktopstate.NewWindowsSecretStore(a.paths.dataRoot)
	if err != nil {
		return desktopacceptance.State{}, errors.New("construct native credential inspector")
	}
	credentialFingerprint, err := credentialStore.ProtectedFingerprint(desktopstate.SecretKeyDaemonToken)
	if err != nil {
		return desktopacceptance.State{}, errors.New("fingerprint native protected credential")
	}
	state.ProtectedCredentialSHA256 = credentialFingerprint.SHA256
	if credentialFingerprint.Present {
		secret, loadErr := credentialStore.Load(desktopstate.SecretKeyDaemonToken)
		state.ProtectedCredentialValid = loadErr == nil && len(secret) != 0
		clear(secret)
	}
	state.Connected = state.ConfigurationValid && state.ProtectedCredentialValid
	a.mu.Lock()
	logOffset := a.logOffset
	a.mu.Unlock()
	logData := readLogTail(a.paths.runtimeReceipt, logOffset)
	state.ServiceGeneration = desktopacceptance.ParseServiceGenerationLog(logData)
	state.Runtime = desktopacceptance.ParseRuntimeLog(logData)
	state.PlaintextSecretLeakPaths, err = scanCredentialShapes(
		[]string{a.paths.dataRoot, a.evidenceDir},
		a.paths.secrets,
	)
	if err != nil {
		return desktopacceptance.State{}, err
	}
	return state, nil
}

func (a *windowsAdapter) StartSurfaceObserver(context.Context) (desktopacceptance.SurfaceObserver, error) {
	a.mu.Lock()
	a.logOffset = regularFileSize(a.paths.desktopLog)
	a.mu.Unlock()
	return startWindowObserver(windowObserverScope{
		RootPIDs:         map[int]struct{}{os.Getpid(): {}},
		ExactExecutables: append([]string(nil), a.surfaceFiles...),
		ExecutableRoots:  append([]string(nil), a.surfaceDirectories...),
	})
}

func (a *windowsAdapter) Install(ctx context.Context, installer string) error {
	if err := a.ensureMSILifecycle(ctx); err != nil {
		return err
	}
	if err := a.runMSI(ctx, "install", "/i", installer); err != nil {
		return err
	}
	return a.requireInstalledMSITopology(ctx)
}

func (a *windowsAdapter) Launch(ctx context.Context) error {
	a.mu.Lock()
	a.logOffset = regularFileSize(a.paths.runtimeReceipt)
	a.mu.Unlock()
	return startProcess(ctx, a.paths.desktop)
}

func (a *windowsAdapter) LaunchSecond(ctx context.Context) error {
	forgedRoot := filepath.Join(a.evidenceDir, fmt.Sprintf("forged-second-launch-profile-%d", os.Getpid()))
	if _, err := os.Lstat(forgedRoot); err == nil {
		return errors.New("forged second-launch profile path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect forged second-launch profile: %w", err)
	}
	command := exec.Command(a.paths.desktop)
	command.SysProcAttr = &syscall.SysProcAttr{NoInheritHandles: true}
	command.Env = environmentWithOverrides(map[string]string{
		"APPDATA":      filepath.Join(forgedRoot, "AppData", "Roaming"),
		"HOME":         filepath.Join(forgedRoot, "Home"),
		"LOCALAPPDATA": filepath.Join(forgedRoot, "AppData", "Local"),
		"USERPROFILE":  filepath.Join(forgedRoot, "Profile"),
	})
	if err := command.Start(); err != nil {
		return fmt.Errorf("start altered-profile second instance: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	timer := time.NewTimer(secondLaunchExitTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("altered-profile second instance exited unsuccessfully: %w", err)
		}
		quietTimer := time.NewTimer(secondLaunchRootQuietTime)
		defer quietTimer.Stop()
		select {
		case <-quietTimer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		if _, err := os.Lstat(forgedRoot); err == nil {
			return errors.New("altered-profile second instance created a redirected user root")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect altered-profile second-instance root: %w", err)
		}
		return nil
	case <-timer.C:
		_ = command.Process.Kill()
		<-done
		return errors.New("altered-profile second instance remained resident and obtained independent process authority")
	case <-ctx.Done():
		_ = command.Process.Kill()
		<-done
		return ctx.Err()
	}
}

func (a *windowsAdapter) CrashRuntime(ctx context.Context, runtimeState desktopacceptance.RuntimeState) error {
	if runtimeState.PID <= 1 {
		return errors.New("runtime receipt did not provide a crashable PID")
	}
	state, err := a.Snapshot(ctx)
	if err != nil {
		return err
	}
	if state.Runtime.ServiceGeneration != runtimeState.ServiceGeneration || state.Runtime.Generation != runtimeState.Generation ||
		state.Runtime.PID != runtimeState.PID || state.Runtime.ObservationSequence < runtimeState.ObservationSequence {
		return errors.New("runtime observation changed before the crash action")
	}
	var expected desktopacceptance.Process
	for _, process := range state.ManagedDescendants {
		if process.PID == runtimeState.PID {
			expected = process
			break
		}
	}
	if expected.PID == 0 {
		return errors.New("observed runtime is not a managed desktop descendant")
	}
	return terminateObservedProcess(ctx, expected, 86, "runtime")
}

func (a *windowsAdapter) CrashApplication(
	ctx context.Context,
	expected desktopacceptance.Process,
	expectedDescendants []desktopacceptance.Process,
) error {
	if expected.PID <= 1 || expected.StartedAt == "" {
		return errors.New("application observation did not provide a crashable process identity")
	}
	if len(expectedDescendants) == 0 {
		return errors.New("application observation did not provide managed descendant identities")
	}
	state, err := a.Snapshot(ctx)
	if err != nil {
		return err
	}
	if len(state.Application) != 1 || !sameObservedProcess(state.Application[0], expected) ||
		!state.ProcessContained || !sameObservedProcesses(state.ManagedDescendants, expectedDescendants) {
		return errors.New("application identity or managed descendants changed before the containment crash")
	}

	identities := make([]desktopacceptance.Process, 0, 1+len(expectedDescendants))
	identities = append(identities, expected)
	identities = append(identities, expectedDescendants...)
	bound := make([]boundProcess, 0, len(identities))
	for index, identity := range identities {
		access := uint32(windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION)
		label := "managed descendant"
		if index == 0 {
			access |= windows.PROCESS_TERMINATE
			label = "application"
		}
		process, err := bindObservedProcess(identity, access, label)
		if err != nil {
			closeBoundProcesses(bound)
			return err
		}
		bound = append(bound, process)
	}
	defer closeBoundProcesses(bound)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := windows.TerminateProcess(bound[0].handle, 87); err != nil {
		return fmt.Errorf("terminate application pid %d: %w", expected.PID, err)
	}
	for _, process := range bound {
		if err := waitBoundProcessExit(ctx, process); err != nil {
			return err
		}
	}
	return nil
}

type boundProcess struct {
	identity desktopacceptance.Process
	handle   windows.Handle
	label    string
}

func bindObservedProcess(expected desktopacceptance.Process, access uint32, label string) (boundProcess, error) {
	if expected.PID <= 1 || expected.Executable == "" || expected.StartedAt == "" {
		return boundProcess{}, fmt.Errorf("%s observation did not provide a complete process identity", label)
	}
	handle, err := windows.OpenProcess(access, false, uint32(expected.PID))
	if err != nil {
		return boundProcess{}, fmt.Errorf("open %s pid %d: %w", label, expected.PID, err)
	}
	actualPath, actualStarted := processDetailsHandle(handle)
	if !strings.EqualFold(actualPath, expected.Executable) || actualStarted == "" || actualStarted != expected.StartedAt {
		windows.CloseHandle(handle)
		return boundProcess{}, fmt.Errorf("%s process identity changed before the crash action", label)
	}
	return boundProcess{identity: expected, handle: handle, label: label}, nil
}

func closeBoundProcesses(processes []boundProcess) {
	for _, process := range processes {
		windows.CloseHandle(process.handle)
	}
}

func waitBoundProcessExit(ctx context.Context, process boundProcess) error {
	for {
		result, err := windows.WaitForSingleObject(process.handle, 100)
		if err != nil {
			return fmt.Errorf("wait for %s pid %d: %w", process.label, process.identity.PID, err)
		}
		if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("wait for %s pid %d returned 0x%x", process.label, process.identity.PID, result)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func terminateObservedProcess(ctx context.Context, expected desktopacceptance.Process, exitCode uint32, label string) error {
	handle, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(expected.PID),
	)
	if err != nil {
		return fmt.Errorf("open %s pid %d: %w", label, expected.PID, err)
	}
	defer windows.CloseHandle(handle)
	actualPath, actualStarted := processDetailsHandle(handle)
	if !strings.EqualFold(actualPath, expected.Executable) || actualStarted == "" || actualStarted != expected.StartedAt {
		return fmt.Errorf("%s process identity changed before the crash action", label)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := windows.TerminateProcess(handle, exitCode); err != nil {
		return fmt.Errorf("terminate %s pid %d: %w", label, expected.PID, err)
	}
	for {
		result, err := windows.WaitForSingleObject(handle, 100)
		if err != nil {
			return fmt.Errorf("wait for terminated %s pid %d: %w", label, expected.PID, err)
		}
		if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("wait for terminated %s pid %d returned 0x%x", label, expected.PID, result)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func sameObservedProcess(left, right desktopacceptance.Process) bool {
	return left.PID == right.PID && left.StartedAt != "" && left.StartedAt == right.StartedAt &&
		strings.EqualFold(left.Executable, right.Executable)
}

func sameObservedProcesses(left, right []desktopacceptance.Process) bool {
	if len(left) != len(right) {
		return false
	}
	rightByPID := make(map[int]desktopacceptance.Process, len(right))
	for _, process := range right {
		if process.PID <= 1 {
			return false
		}
		if _, exists := rightByPID[process.PID]; exists {
			return false
		}
		rightByPID[process.PID] = process
	}
	leftPIDs := make(map[int]struct{}, len(left))
	for _, process := range left {
		if _, exists := leftPIDs[process.PID]; exists {
			return false
		}
		leftPIDs[process.PID] = struct{}{}
		expected, ok := rightByPID[process.PID]
		if !ok || !sameObservedProcess(process, expected) {
			return false
		}
	}
	return true
}

func (a *windowsAdapter) Uninstall(ctx context.Context, version string) error {
	registration, err := a.installedMSIRegistration()
	if err != nil {
		return err
	}
	if registration == nil || registration.version != version {
		return fmt.Errorf("installed MSI registration for version %s is absent", version)
	}
	return a.runMSI(ctx, "uninstall", "/x", registration.productCode)
}

func (a *windowsAdapter) ResetUserData(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	info, err := os.Lstat(a.paths.dataRoot)
	if errors.Is(err, os.ErrNotExist) {
		a.resetObservations()
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect user data reset root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("user data reset root is not a real directory")
	}
	if unsafe, err := windowsReparsePoint(a.paths.dataRoot); err != nil || unsafe {
		return errors.New("user data reset root is a reparse point or cannot be verified")
	}
	if err := rejectResetTreeLinks(ctx, a.paths.dataRoot); err != nil {
		return err
	}
	if err := os.RemoveAll(a.paths.dataRoot); err != nil {
		return fmt.Errorf("remove dedicated-account user data: %w", err)
	}
	a.resetObservations()
	return nil
}

func rejectResetTreeLinks(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect user data reset tree: %w", walkErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("user data reset tree contains a symlink")
		}
		unsafe, err := windowsReparsePoint(path)
		if err != nil || unsafe {
			return errors.New("user data reset tree contains a reparse point or unstable path")
		}
		return nil
	})
}

func (a *windowsAdapter) resetObservations() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.known = make(map[int]string)
	a.logOffset = 0
}

func (a *windowsAdapter) CleanupFixtures(ctx context.Context) error {
	return a.cleanupMSIFixtures(ctx)
}

func (a *windowsAdapter) legacyLaunchers(ctx context.Context) ([]string, error) {
	var found []string
	output, err := a.runPowerShell(ctx, `Get-ScheduledTask -ErrorAction Stop | Where-Object { ([string]$_.TaskName).StartsWith('Codesk daemon ', [StringComparison]::Ordinal) } | ForEach-Object { [Console]::Out.WriteLine('task:' + $_.TaskPath + $_.TaskName) }`, nil)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			found = append(found, line)
		}
	}
	entries, err := os.ReadDir(a.paths.startup)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, legacyPrefix) && strings.HasSuffix(strings.ToLower(name), ".lnk") {
			found = append(found, "startup:"+name)
		}
	}
	slices.Sort(found)
	return found, nil
}

func (a *windowsAdapter) runPowerShell(ctx context.Context, script string, values map[string]string) (string, error) {
	command := exec.CommandContext(ctx, a.paths.powerShell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	command.Env = environmentWithOverrides(values)
	command.SysProcAttr = hiddenProcessAttributes()
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("hidden PowerShell native fact failed: %w: %s", err, strings.TrimSpace(limitString(output.String(), 4096)))
	}
	return limitString(output.String(), 1<<20), nil
}

func (a *windowsAdapter) runMSI(ctx context.Context, label string, operation ...string) error {
	a.mu.Lock()
	a.msiLogSequence++
	sequence := a.msiLogSequence
	a.mu.Unlock()
	logPath := filepath.Join(a.evidenceDir, fmt.Sprintf("msiexec-%03d-%s.log", sequence, label))
	arguments := append([]string(nil), operation...)
	arguments = append(arguments, "/qn", "/norestart", "REBOOT=ReallySuppress", "/l*v", logPath)
	command := exec.CommandContext(ctx, a.paths.msiexec, arguments...)
	command.SysProcAttr = hiddenProcessAttributes()
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("msiexec %s failed: %w: %s (log %s)", label, err, strings.TrimSpace(limitString(output.String(), 4096)), logPath)
	}
	return nil
}

func startProcess(ctx context.Context, executable string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	command := exec.Command(executable)
	command.SysProcAttr = &syscall.SysProcAttr{NoInheritHandles: true}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func hiddenProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags:    windows.CREATE_NO_WINDOW,
		HideWindow:       true,
		NoInheritHandles: true,
	}
}

func enumerateProcesses() ([]desktopacceptance.Process, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	var processes []desktopacceptance.Process
	for {
		pid := int(entry.ProcessID)
		if pid > 0 {
			path, started := processDetails(uint32(pid))
			if path == "" {
				path = windows.UTF16ToString(entry.ExeFile[:])
			}
			processes = append(processes, desktopacceptance.Process{PID: pid, ParentPID: int(entry.ParentProcessID), Executable: path, StartedAt: started})
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, err
		}
	}
	return processes, nil
}

func processDetails(pid uint32) (string, string) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", ""
	}
	defer windows.CloseHandle(handle)
	return processDetailsHandle(handle)
}

func processDetailsHandle(handle windows.Handle) (string, string) {
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", ""
	}
	var created, exited, kernel, user windows.Filetime
	started := ""
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err == nil {
		started = time.Unix(0, created.Nanoseconds()).UTC().Format(time.RFC3339Nano)
	}
	return windows.UTF16ToString(buffer[:size]), started
}

func descendantOf(pid int, roots map[int]struct{}, processes []desktopacceptance.Process) bool {
	parents := make(map[int]int, len(processes))
	for _, process := range processes {
		parents[process.PID] = process.ParentPID
	}
	seen := make(map[int]struct{})
	current := pid
	for current > 1 {
		parent, ok := parents[current]
		if !ok {
			return false
		}
		if _, ok := roots[parent]; ok {
			return true
		}
		if _, ok := seen[parent]; ok {
			return false
		}
		seen[parent] = struct{}{}
		current = parent
	}
	return false
}

func processInJob(pid int) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(handle)
	var inJob int32
	result, _, callErr := procIsProcessInJob.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&inJob)))
	if result == 0 {
		return false, callErr
	}
	return inJob != 0, nil
}

func processIDSet(processes []desktopacceptance.Process) map[int]struct{} {
	result := make(map[int]struct{}, len(processes))
	for _, process := range processes {
		result[process.PID] = struct{}{}
	}
	return result
}

func containsProcess(processes []desktopacceptance.Process, pid int) bool {
	for _, process := range processes {
		if process.PID == pid {
			return true
		}
	}
	return false
}

func compareProcess(left, right desktopacceptance.Process) int { return left.PID - right.PID }

func registryStringExact(root registry.Key, path, name, expected string) bool {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	value, valueType, err := key.GetStringValue(name)
	return err == nil && valueType == registry.SZ && value == expected
}

type installedMSIRegistration struct {
	version     string
	productCode string
	artifact    desktopacceptance.ReleaseArtifact
}

func (a *windowsAdapter) installedMSIRegistration() (*installedMSIRegistration, error) {
	root, err := registry.OpenKey(registry.CURRENT_USER, uninstallKeyRoot, registry.ENUMERATE_SUB_KEYS)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open per-user uninstall registry: %w", err)
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("enumerate per-user uninstall registry: %w", err)
	}
	a.mu.Lock()
	knownCodes := make(map[string]string)
	knownRegistrations := make(map[string]installedMSIRegistration)
	for _, release := range a.releases {
		for _, artifact := range release.Artifacts {
			key := strings.ToUpper(artifact.ProductCode)
			knownCodes[key] = artifact.ProductCode
			knownRegistrations[key] = installedMSIRegistration{
				version: release.Version, productCode: artifact.ProductCode, artifact: artifact,
			}
		}
	}
	a.mu.Unlock()
	var found *installedMSIRegistration
	for _, name := range names {
		key, err := registry.OpenKey(registry.CURRENT_USER, uninstallKeyRoot+`\`+name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		displayName, displayType, displayErr := key.GetStringValue("DisplayName")
		version, versionType, versionErr := key.GetStringValue("DisplayVersion")
		publisher, publisherType, publisherErr := key.GetStringValue("Publisher")
		windowsInstaller, installerType, installerErr := key.GetIntegerValue("WindowsInstaller")
		key.Close()
		canonical, recognized := canonicalKnownMSIProductCode(name, knownCodes)
		if !recognized {
			if displayErr == nil && strings.EqualFold(displayName, "Codesk") {
				return nil, fmt.Errorf("unrecognized Codesk MSI ProductCode %s is installed", name)
			}
			continue
		}
		matched := knownRegistrations[strings.ToUpper(canonical)]
		if found != nil {
			return nil, errors.New("multiple per-user Codesk products are registered")
		}
		metadata := msiRegistrationMetadata{
			DisplayName: displayName, DisplayNameValid: displayErr == nil && displayType == registry.SZ,
			Version: version, VersionValid: versionErr == nil && versionType == registry.SZ,
			Publisher: publisher, PublisherValid: publisherErr == nil && publisherType == registry.SZ,
			WindowsInstaller: windowsInstaller, WindowsInstallerValid: installerErr == nil && installerType == registry.DWORD,
		}
		if err := requireSourceBoundMSIRegistration(metadata, matched.version); err != nil {
			return nil, fmt.Errorf("Codesk MSI ARP registration %s does not match its source-bound release", name)
		}
		matchedCopy := matched
		found = &matchedCopy
	}
	return found, nil
}

func (a *windowsAdapter) installedPayloadMatches(artifact desktopacceptance.ReleaseArtifact) (bool, error) {
	root, err := os.Lstat(a.paths.installRoot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect MSI install root: %w", err)
	}
	if !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("MSI install root is not a real directory")
	}
	if reparse, err := windowsReparsePoint(a.paths.installRoot); err != nil || reparse {
		return false, errors.New("MSI install root is a reparse point or cannot be verified")
	}
	entries, err := os.ReadDir(a.paths.installRoot)
	if err != nil {
		return false, fmt.Errorf("read MSI install root: %w", err)
	}
	if len(entries) != 2 {
		return false, nil
	}
	expected := map[string]string{
		"Codesk.exe":           artifact.CodeskSHA256,
		"notty-agent-tool.exe": artifact.AgentToolSHA256,
	}
	for _, entry := range entries {
		want, ok := expected[entry.Name()]
		path := filepath.Join(a.paths.installRoot, entry.Name())
		info, infoErr := os.Lstat(path)
		if !ok || infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
		if reparse, err := windowsReparsePoint(path); err != nil || reparse {
			return false, fmt.Errorf("MSI payload entry %s is a reparse point or cannot be verified", entry.Name())
		}
		hash, err := fileSHA256(path)
		if err != nil {
			return false, err
		}
		if hash != want {
			return false, nil
		}
		machine, err := portableExecutableArchitecture(path)
		if err != nil {
			return false, err
		}
		if machine != artifact.Architecture {
			return false, nil
		}
	}
	return true, nil
}

func (a *windowsAdapter) installedShortcutTopologyPresent() (bool, error) {
	root := filepath.Dir(a.paths.shortcut)
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect MSI shortcut root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("MSI shortcut root is not a real directory")
	}
	if reparse, err := windowsReparsePoint(root); err != nil || reparse {
		return false, errors.New("MSI shortcut root is a reparse point or cannot be verified")
	}
	shortcutInfo, err := os.Lstat(a.paths.shortcut)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect MSI shortcut: %w", err)
	}
	if !shortcutInfo.Mode().IsRegular() || shortcutInfo.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("MSI shortcut is not a regular file")
	}
	if reparse, err := windowsReparsePoint(a.paths.shortcut); err != nil || reparse {
		return false, errors.New("MSI shortcut is a reparse point or cannot be verified")
	}
	return true, nil
}

func (a *windowsAdapter) requireInstalledMSITopology(ctx context.Context) error {
	registration, err := a.installedMSIRegistration()
	if err != nil {
		return err
	}
	if registration == nil {
		return errors.New("installed MSI registration is absent after install")
	}
	payloadMatches, err := a.installedPayloadMatches(registration.artifact)
	if err != nil {
		return err
	}
	if !payloadMatches {
		return errors.New("installed MSI payload does not match the source-bound release")
	}
	shortcutPresent, err := a.installedShortcutTopologyPresent()
	if err != nil {
		return err
	}
	if !shortcutPresent {
		return errors.New("installed MSI shortcut topology is absent")
	}
	shortcut, err := a.inspectMSIShortcut(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(shortcut.Target, a.paths.desktop) ||
		!strings.EqualFold(shortcut.WorkingDirectory, a.paths.installRoot) || shortcut.Arguments != "" {
		return fmt.Errorf("installed MSI shortcut does not target the source-bound payload: %+v", shortcut)
	}
	return nil
}

func quote(path string) string { return `"` + path + `"` }

func regularFileSize(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

func readLogTail(path string, offset int64) []byte {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	if info.Size()-offset > 16<<20 {
		return nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, 16<<20+1))
	if err != nil || len(data) > 16<<20 {
		return nil
	}
	return data
}

func scanCredentialShapes(roots []string, excluded ...string) ([]string, error) {
	var matches []string
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		info, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("secret scan root is a symlink: %s", root)
		}
		if unsafe, err := windowsReparsePoint(root); err != nil || unsafe {
			return nil, fmt.Errorf("secret scan root is a reparse point or cannot be verified: %s", root)
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if containsPath(excluded, path) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("secret scan encountered a symlink: %s", path)
			}
			if unsafe, err := windowsReparsePoint(path); err != nil || unsafe {
				return fmt.Errorf("secret scan encountered a reparse point or unstable path: %s", path)
			}
			if entry.IsDir() {
				return nil
			}
			fileInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if !fileInfo.Mode().IsRegular() || fileInfo.Size() > 16<<20 {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, pattern := range plaintextCredentialShapes {
				if pattern.Match(data) {
					matches = append(matches, path)
					break
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(matches)
	return slices.Compact(matches), nil
}

func containsPath(paths []string, candidate string) bool {
	for _, path := range paths {
		if samePath(path, candidate) {
			return true
		}
	}
	return false
}

func limitString(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "...<truncated>"
}
