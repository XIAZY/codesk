package desktopacceptance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeObserver struct {
	events []SurfaceEvent
	err    error
}

func (o fakeObserver) Stop(context.Context) ([]SurfaceEvent, error) {
	return append([]SurfaceEvent(nil), o.events...), o.err
}

type fakeAdapter struct {
	mu                       sync.Mutex
	state                    State
	platform                 string
	sessionIdentity          string
	events                   []SurfaceEvent
	observerStopErr          error
	signatureValid           bool
	secondLaunchMutation     bool
	upgradeHashMutation      bool
	upgradeAutostartMutation bool
	legacyStateMutation      bool
	plaintextLeakMutation    bool
	reuseRuntimePID          bool
	releaseVersionMutation   bool
	freshInstallAutostart    bool
	candidateInstallFailure  bool
	cleanupFailure           bool
	cleanupBlocked           bool
	cleanupCalled            bool
	finalSnapshotHook        func()
	legacyState              LegacyStateFingerprint
	launchSequence           int
	actions                  []Action
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{
		state: State{CapturedAt: time.Now().UTC()}, platform: "windows", sessionIdentity: "session-1",
		signatureValid: true, freshInstallAutostart: true,
		legacyState: LegacyStateFingerprint{Present: true, DigestSHA256: strings.Repeat("c", 64), EntryCount: 2, ByteCount: 17},
	}
}

func (a *fakeAdapter) Host(context.Context) (Host, error) {
	return Host{Platform: a.platform, Architecture: "amd64", OSVersion: "test-native", Hostname: "acceptance", Username: "tester", SessionIdentity: a.sessionIdentity}, nil
}

func (a *fakeAdapter) VerifyRelease(_ context.Context, input ReleaseInput) (Release, error) {
	signed := !strings.Contains(filepath.Base(input.Directory), "unsigned")
	signatureError := ""
	if !a.signatureValid {
		signatureError = "NotSigned"
	}
	release := Release{
		Platform:       a.platform,
		Version:        input.Version,
		SourceRevision: input.SourceRevision,
		Signed:         signed,
		Toolchain:      map[string]string{"go": "test"},
		ManifestPath:   filepath.Join(input.Directory, "manifest.json"),
		ManifestSHA256: strings.Repeat("d", 64),
		SumsPath:       filepath.Join(input.Directory, "SHA256SUMS"),
		SumsSHA256:     strings.Repeat("e", 64),
	}
	for index, architecture := range []string{"amd64", "arm64"} {
		release.Artifacts = append(release.Artifacts, ReleaseArtifact{
			Architecture:       architecture,
			Path:               filepath.Join(input.Directory, fmt.Sprintf("CodeskSetup_%s_windows_%s.exe", input.Version, architecture)),
			SHA256:             strings.Repeat(string(rune('a'+index)), 64),
			Size:               1024,
			ManifestSigned:     signed,
			NativeFormat:       "pe",
			NativeArchitecture: architecture,
			SignatureValid:     a.signatureValid,
			SignatureError:     signatureError,
		})
	}
	if a.releaseVersionMutation {
		release.Version += "-mutated"
	}
	return release, nil
}

func (a *fakeAdapter) LegacyCLIState(context.Context) (LegacyStateFingerprint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.legacyState, nil
}

func (a *fakeAdapter) Snapshot(context.Context) (State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.state
	state.Application = append([]Process(nil), state.Application...)
	state.ManagedDescendants = append([]Process(nil), state.ManagedDescendants...)
	state.LegacyLaunchers = append([]string(nil), state.LegacyLaunchers...)
	state.PlaintextSecretLeakPaths = append([]string(nil), state.PlaintextSecretLeakPaths...)
	state.CapturedAt = time.Now().UTC()
	if a.cleanupCalled && a.finalSnapshotHook != nil {
		hook := a.finalSnapshotHook
		a.finalSnapshotHook = nil
		hook()
	}
	return state, nil
}

func (a *fakeAdapter) StartSurfaceObserver(context.Context) (SurfaceObserver, error) {
	return fakeObserver{events: a.events, err: a.observerStopErr}, nil
}

func (a *fakeAdapter) SeedLegacyLaunchers(context.Context) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.LegacyLaunchers = []string{"task:Codesk daemon acceptance", "startup:Codesk daemon acceptance.lnk"}
	return len(a.state.LegacyLaunchers), nil
}

func (a *fakeAdapter) Install(_ context.Context, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	name := filepath.Base(path)
	prefix := "CodeskSetup_"
	suffix := "_windows_amd64.exe"
	version := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if a.candidateInstallFailure && version == "2.0.0" {
		return errors.New("injected candidate install failure")
	}
	hadInstall := a.state.Installed
	priorAutostart := a.state.AutostartRegistration
	a.state.Installed = true
	a.state.InstalledVersion = version
	a.state.AutostartRegistration = a.freshInstallAutostart
	if hadInstall {
		a.state.AutostartRegistration = priorAutostart
	}
	if a.upgradeAutostartMutation && hadInstall && version == "2.0.0" {
		a.state.AutostartRegistration = true
	}
	a.state.RemovalRegistration = true
	a.state.UserLaunchEntry = true
	a.state.LegacyLaunchers = nil
	a.state.Application = nil
	a.state.ManagedDescendants = nil
	a.state.ProcessContained = false
	if a.legacyStateMutation {
		a.legacyState.DigestSHA256 = strings.Repeat("d", 64)
	}
	if a.upgradeHashMutation && a.state.Connected && version == "2.0.0" {
		a.state.ConfigurationSHA256 = strings.Repeat("f", 64)
	}
	return nil
}

func (a *fakeAdapter) Launch(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.launchSequence++
	a.state.Application = []Process{{PID: 100, Executable: `C:\Codesk\Codesk.exe`, StartedAt: fmt.Sprintf("launch-%d", a.launchSequence)}}
	a.state.ProcessContained = true
	if a.state.ServiceGeneration == 0 {
		a.state.ServiceGeneration = 1
	}
	return nil
}

func (a *fakeAdapter) CrashApplication(_ context.Context, process Process, descendants []Process) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Application) != 1 || !sameProcessIdentity(a.state.Application[0], process) {
		return errors.New("unexpected application process identity")
	}
	if len(a.state.ManagedDescendants) != len(descendants) {
		return errors.New("unexpected application descendant count")
	}
	for index := range descendants {
		if !sameProcessIdentity(a.state.ManagedDescendants[index], descendants[index]) {
			return errors.New("unexpected application descendant identity")
		}
	}
	a.state.Application = nil
	a.state.ManagedDescendants = nil
	a.state.ProcessContained = false
	a.state.ServiceGeneration = 0
	a.state.Runtime = RuntimeState{}
	return nil
}

func (a *fakeAdapter) LaunchSecond(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.secondLaunchMutation {
		a.state.Application = append(a.state.Application, Process{PID: 101, Executable: `C:\Codesk\Codesk.exe`})
	}
	return nil
}

func (a *fakeAdapter) CrashRuntime(_ context.Context, runtime RuntimeState) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.ManagedDescendants = nil
	a.state.Runtime.ObservationSequence++
	a.state.Runtime.TurnStatus = "stopped_transient"
	if a.state.Runtime.PID != runtime.PID {
		return fmt.Errorf("unexpected runtime pid %d", runtime.PID)
	}
	return nil
}

func (a *fakeAdapter) Uninstall(context.Context, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Installed = false
	a.state.InstalledVersion = ""
	a.state.AutostartRegistration = false
	a.state.RemovalRegistration = false
	a.state.UserLaunchEntry = false
	a.state.Application = nil
	a.state.ManagedDescendants = nil
	a.state.ProcessContained = false
	return nil
}

func (a *fakeAdapter) ResetUserData(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Connected = false
	a.state.ConfigurationValid = false
	a.state.ConfigurationSHA256 = ""
	a.state.ProtectedCredentialValid = false
	a.state.ProtectedCredentialSHA256 = ""
	a.state.PlaintextSecretLeakPaths = nil
	a.state.ServiceGeneration = 0
	a.state.Runtime = RuntimeState{}
	return nil
}

func (a *fakeAdapter) CleanupFixtures(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupCalled = true
	if a.cleanupFailure {
		return errors.New("injected fixture cleanup failure")
	}
	if a.cleanupBlocked {
		return blockedError{"injected prerequisite-shaped cleanup failure"}
	}
	a.state.LegacyLaunchers = nil
	return nil
}

type fakeOperator struct {
	adapter     *fakeAdapter
	blockAction Action
}

func (o fakeOperator) Perform(_ context.Context, action Action, _ string) error {
	if action == o.blockAction {
		return blockedError{"injected operator block"}
	}
	o.adapter.mu.Lock()
	defer o.adapter.mu.Unlock()
	state := &o.adapter.state
	o.adapter.actions = append(o.adapter.actions, action)
	switch action {
	case ActionConnect, ActionConnectFresh:
		state.Connected = true
		state.ConfigurationValid = true
		state.ConfigurationSHA256 = strings.Repeat("a", 64)
		state.ProtectedCredentialValid = true
		state.ProtectedCredentialSHA256 = strings.Repeat("b", 64)
		if o.adapter.plaintextLeakMutation {
			state.PlaintextSecretLeakPaths = []string{`C:\Codesk\Logs\codesk-desktop.log`}
		}
	case ActionQuit:
		state.Application = nil
		state.ManagedDescendants = nil
		state.ProcessContained = false
	case ActionEnableAutostart:
		state.AutostartRegistration = true
	case ActionDisableAutostart:
		state.AutostartRegistration = false
	case ActionTurnInitial:
		state.Runtime = RuntimeState{Kind: "codex", PID: 201, ServiceGeneration: 1, ObservationSequence: 2, Generation: 1, TurnStartedSequence: 1, TurnTerminalSequence: 1, TurnStatus: "turn_completed"}
		state.ManagedDescendants = []Process{{PID: 201, ParentPID: 100, Executable: `C:\Codex\codex.exe`, StartedAt: "runtime-1"}}
	case ActionTurnAfterCrash:
		pid := 202
		if o.adapter.reuseRuntimePID {
			pid = 201
		}
		state.Runtime = RuntimeState{Kind: "codex", PID: pid, ServiceGeneration: 1, ObservationSequence: 4, Generation: 2, TurnStartedSequence: 1, TurnTerminalSequence: 1, TurnStatus: "turn_completed"}
		state.ManagedDescendants = []Process{{PID: pid, ParentPID: 100, Executable: `C:\Codex\codex.exe`, StartedAt: "runtime-2"}}
	case ActionTurnAfterAppCrash:
		state.Runtime = RuntimeState{Kind: "codex", PID: 205, ServiceGeneration: 1, ObservationSequence: 2, Generation: 1, TurnStartedSequence: 1, TurnTerminalSequence: 1, TurnStatus: "turn_completed"}
		state.ManagedDescendants = []Process{{PID: 205, ParentPID: 100, Executable: `C:\Codex\codex.exe`, StartedAt: "runtime-app-relaunch"}}
	case ActionRestart:
		state.ServiceGeneration++
		state.Runtime.ObservationSequence++
	case ActionTurnRestart:
		state.Runtime = RuntimeState{Kind: "codex", PID: 203, ServiceGeneration: 2, ObservationSequence: 6, Generation: 3, TurnStartedSequence: 1, TurnTerminalSequence: 1, TurnStatus: "turn_completed"}
		state.ManagedDescendants = []Process{{PID: 203, ParentPID: 100, Executable: `C:\Codex\codex.exe`, StartedAt: "runtime-restart"}}
	case ActionTurnFresh:
		state.Runtime = RuntimeState{Kind: "codex", PID: 204, ServiceGeneration: 1, ObservationSequence: 2, Generation: 1, TurnStartedSequence: 1, TurnTerminalSequence: 1, TurnStatus: "turn_completed"}
		state.ManagedDescendants = []Process{{PID: 204, ParentPID: 100, Executable: `C:\Codex\codex.exe`, StartedAt: "runtime-fresh"}}
	}
	return nil
}

func TestEngineCompleteScenario(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != StatusPass || !report.Publishable {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Rows) < 23 {
		t.Fatalf("got %d rows, want complete scenario", len(report.Rows))
	}
	if report.TranscriptSHA256 == "" {
		t.Fatal("report did not bind its transcript")
	}
	foundAlteredProfileEvidence := false
	for _, row := range report.Rows {
		foundAlteredProfileEvidence = foundAlteredProfileEvidence || row.Name == "candidate-launch-single-instance" &&
			strings.Contains(row.Detail, "altered_profile_redirected=false") && strings.Contains(row.Detail, "independent_authority=false")
	}
	if !foundAlteredProfileEvidence {
		t.Fatal("single-instance row did not bind altered-profile authority evidence")
	}
	if report.SourceRevision == report.RunnerSourceRevision || report.RunnerSourceRevision != config.RunnerSourceRevision {
		t.Fatalf("target and runner identities were not independently bound: target=%s runner=%s", report.SourceRevision, report.RunnerSourceRevision)
	}
	data, err := os.ReadFile(filepath.Join(config.EvidenceDir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(secretMatches(string(data))) != 0 {
		t.Fatal("report leaked a credential-shaped value")
	}
}

func TestEngineLifecycleIsPlatformNeutral(t *testing.T) {
	config := testConfig(t, true)
	config.TargetPlatform = "darwin"
	adapter := newFakeAdapter()
	adapter.platform = "darwin"
	adapter.freshInstallAutostart = false
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Host.Platform != "darwin" || report.Candidate.Platform != "darwin" || !strings.HasPrefix(report.RunID, "darwin-") {
		t.Fatalf("report identities = host:%s release:%s run:%s", report.Host.Platform, report.Candidate.Platform, report.RunID)
	}
	if !slices.Contains(adapter.actions, ActionEnableAutostart) || !slices.Contains(adapter.actions, ActionDisableAutostart) {
		t.Fatalf("platform-neutral actions = %v, want enable and disable autostart", adapter.actions)
	}
}

func TestEnginePrepareResumeBindsRealLoginSession(t *testing.T) {
	config := testConfig(t, true)
	config.Phase = PhasePrepare
	adapter := newFakeAdapter()
	prepare, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if prepare.Verdict != StatusPending || prepare.Phase != PhasePrepare || prepare.CheckpointSHA256 == "" {
		t.Fatalf("prepare report = %+v", prepare)
	}
	if err := adapter.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}

	config.Phase = PhaseResume
	pending, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Verdict != StatusPending || pending.Phase != PhasePrepare {
		t.Fatalf("same-session resume report = %+v", pending)
	}

	adapter.sessionIdentity = "session-2"
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != StatusPass || report.Phase != PhaseResume || report.CheckpointSHA256 != prepare.CheckpointSHA256 {
		t.Fatalf("resume report = %+v", report)
	}
}

func TestEngineResumeRejectsCheckpointMutation(t *testing.T) {
	config := testConfig(t, true)
	config.Phase = PhasePrepare
	adapter := newFakeAdapter()
	if _, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.EvidenceDir, "checkpoint.json")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(" "); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	config.Phase = PhaseResume
	_, err = (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "checkpoint hash")
}

func TestEngineResumeRejectsTranscriptMutation(t *testing.T) {
	config := testConfig(t, true)
	config.Phase = PhasePrepare
	adapter := newFakeAdapter()
	if _, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.EvidenceDir, "transcript.log")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("mutated\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	config.Phase = PhaseResume
	_, err = (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "transcript hash") {
		t.Fatalf("Run error = %v, want transcript hash failure", err)
	}
}

func TestEngineResumeRejectsNoncanonicalPrepareReport(t *testing.T) {
	config := testConfig(t, true)
	config.Phase = PhasePrepare
	adapter := newFakeAdapter()
	if _, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.EvidenceDir, "report.json")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(" "); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	config.Phase = PhaseResume
	_, err = (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "not canonical JSON") {
		t.Fatalf("Run error = %v, want canonical report failure", err)
	}
}

func TestEngineRejectsAdapterReleaseIdentityMutation(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.releaseVersionMutation = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "version is")
}

func TestPreparedConnectedStateRequiresRegisteredContainedInstall(t *testing.T) {
	state := State{
		Installed: true, InstalledVersion: "1.0.0", AutostartRegistration: true,
		RemovalRegistration: true, UserLaunchEntry: true, ProcessContained: true,
		Application: []Process{{PID: 100}}, Connected: true, ConfigurationValid: true,
		ProtectedCredentialValid: true, ConfigurationSHA256: strings.Repeat("a", 64),
		ProtectedCredentialSHA256: strings.Repeat("b", 64),
	}
	if err := preparedConnectedState(state, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	state.AutostartRegistration = false
	if err := preparedConnectedState(state, "1.0.0"); err == nil || !strings.Contains(err.Error(), "registration") {
		t.Fatalf("preparedConnectedState error = %v, want registration failure", err)
	}
}

func TestEngineRejectsSecondResidentInstance(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.secondLaunchMutation = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "second launch changed")
}

func TestEngineRejectsVisibleConsoleEvent(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.events = []SurfaceEvent{{At: time.Now(), PID: 99, Executable: "powershell.exe", Class: "ConsoleWindowClass", Forbidden: true}}
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "forbidden native surface")
}

func TestEngineBlockedActionAndVisibleConsoleIsFailure(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.events = []SurfaceEvent{{At: time.Now(), PID: 99, Executable: "powershell.exe", Class: "ConsoleWindowClass", Forbidden: true}}
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter, blockAction: ActionTurnInitial}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "forbidden native surface")
}

func TestEngineCleansFixturesAfterCandidateInstallFailure(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.candidateInstallFailure = true
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "injected candidate install failure")
	if !adapter.cleanupCalled || len(report.FinalState.LegacyLaunchers) != 0 {
		t.Fatalf("cleanup called=%t final legacy launchers=%v", adapter.cleanupCalled, report.FinalState.LegacyLaunchers)
	}
}

func TestEngineCleanupFailureCannotRemainBlocked(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.cleanupFailure = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter, blockAction: ActionTurnInitial}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "injected fixture cleanup failure")
}

func TestEngineCleanupFailureCannotRemainPending(t *testing.T) {
	config := testConfig(t, true)
	config.Phase = PhasePrepare
	adapter := newFakeAdapter()
	adapter.cleanupFailure = true
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "injected fixture cleanup failure")
	if report.Verdict != StatusFail {
		t.Fatalf("report verdict = %s, want %s", report.Verdict, StatusFail)
	}
}

func TestEnginePrerequisiteShapedCleanupFailureIsFailure(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.cleanupBlocked = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter, blockAction: ActionTurnInitial}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "prerequisite-shaped cleanup failure")
}

func TestEngineFinalPersistenceFailureDowngradesReturnedReport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce owner write bits")
	}
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.finalSnapshotHook = func() {
		if err := os.Chmod(config.EvidenceDir, 0o500); err != nil {
			t.Errorf("make evidence directory read-only: %v", err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(config.EvidenceDir, 0o700) })
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "write acceptance report")
	if report.Verdict != StatusFail || report.Publishable {
		t.Fatalf("returned report verdict=%s publishable=%t, want FAIL/non-publishable", report.Verdict, report.Publishable)
	}
}

func TestSuccessfulTerminalTurnRequiresMatchingSequence(t *testing.T) {
	valid := RuntimeState{TurnStartedSequence: 7, TurnTerminalSequence: 7, TurnStatus: "turn_completed"}
	if !successfulTerminalTurn(valid) {
		t.Fatal("matching successful terminal receipt was rejected")
	}
	valid.TurnTerminalSequence = 8
	if successfulTerminalTurn(valid) {
		t.Fatal("terminal receipt without a matching start was accepted")
	}
}

func TestEngineRejectsUpgradePersistenceMutation(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.upgradeHashMutation = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "upgrade changed configuration")
}

func TestEngineRejectsUpgradeAutostartMutation(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.upgradeAutostartMutation = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "upgrade re-enabled")
}

func TestEngineRejectsLegacyCLIStateMutation(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.legacyStateMutation = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "legacy CLI state changed")
}

func TestEngineRejectsCredentialLeak(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.plaintextLeakMutation = true
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "credential-shaped plaintext")
}

func TestEnginePreservesBlockedVerdict(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter, blockAction: ActionTurnInitial}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusBlocked, "injected operator block")
}

func TestEngineMixedBlockedAndNativeFailureIsFailure(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.observerStopErr = errors.New("injected observer failure")
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter, blockAction: ActionTurnInitial}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusFail, "injected observer failure")
}

func TestEngineHonorsCanceledParentContext(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(ctx, config)
	assertRunStatus(t, err, StatusFail, context.Canceled.Error())
}

func TestEngineUnsignedFunctionalIsNotPublishable(t *testing.T) {
	config := testConfig(t, false)
	config.AllowUnsignedFunctional = true
	adapter := newFakeAdapter()
	adapter.signatureValid = false
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Publishable {
		t.Fatal("unsigned functional report is publishable")
	}
	found := false
	for _, row := range report.Rows {
		found = found || row.Name == "candidate-artifact-trust" && row.Status == StatusWaived
	}
	if !found {
		t.Fatal("unsigned trust waiver row is absent")
	}
}

func TestEngineAcceptsReplacementWithReusedPID(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	adapter.reuseRuntimePID = true
	report, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter}, Poll: time.Millisecond}).Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range report.Rows {
		found = found || row.Name == "runtime-crash-replacement" && strings.Contains(row.Detail, "pid_reused=true")
	}
	if !found {
		t.Fatal("runtime replacement did not preserve PID-reuse evidence")
	}
}

func TestEngineRequiresObservedFreshCandidateConnect(t *testing.T) {
	config := testConfig(t, true)
	adapter := newFakeAdapter()
	_, err := (Engine{Adapter: adapter, Operator: fakeOperator{adapter: adapter, blockAction: ActionConnectFresh}, Poll: time.Millisecond}).Run(context.Background(), config)
	assertRunStatus(t, err, StatusBlocked, "injected operator block")
}

func TestValidateConfigRequiresUpgradeRelease(t *testing.T) {
	config := Config{Phase: PhaseContinuous, TargetPlatform: "windows", Destructive: true, Timeout: 30 * time.Second, SourceRevision: strings.Repeat("a", 40)}
	if err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "previous release") {
		t.Fatalf("validateConfig error = %v", err)
	}
}

func TestValidateConfigRequiresEmbeddedRunnerRevisionEvenWhenUnsigned(t *testing.T) {
	config := testConfig(t, false)
	config.AllowUnsignedFunctional = true
	config.RunnerSourceRevision = ""
	if err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "runner source revision") {
		t.Fatalf("validateConfig error = %v", err)
	}
}

func TestRedactCredentialShapes(t *testing.T) {
	value := redact("Bearer abcdefghijklmnop sk_agent_12345678 eyJabcdefgh.ijklmnop.qrstuvwx")
	if strings.Contains(value, "abcdefghijklmnop") || len(secretMatches(value)) != 0 {
		t.Fatalf("redacted value = %q", value)
	}
}

func testConfig(t *testing.T, signed bool) Config {
	t.Helper()
	parent := t.TempDir()
	sourceRevision := strings.Repeat("a", 40)
	trust := "signed"
	if !signed {
		trust = "unsigned"
	}
	previous := ReleaseInput{
		Directory:      filepath.Join(parent, trust+"-previous"),
		Version:        "1.0.0",
		SourceRevision: strings.Repeat("b", 40),
	}
	candidate := ReleaseInput{
		Directory:      filepath.Join(parent, trust+"-candidate"),
		Version:        "2.0.0",
		SourceRevision: sourceRevision,
	}
	return Config{
		Phase:                PhaseContinuous,
		TargetPlatform:       "windows",
		SourceRevision:       sourceRevision,
		RunnerSourceRevision: strings.Repeat("c", 40),
		Candidate:            candidate, Previous: &previous,
		EvidenceDir: filepath.Join(parent, "evidence"),
		Timeout:     30 * time.Second, Destructive: true,
	}
}

func assertRunStatus(t *testing.T, err error, status Status, contains string) {
	t.Helper()
	if err == nil {
		t.Fatal("Run succeeded")
	}
	runError, ok := err.(*RunError)
	if !ok || runError.Status != status || !strings.Contains(err.Error(), contains) {
		t.Fatalf("Run error = %#v, want status=%s containing %q", err, status, contains)
	}
}
