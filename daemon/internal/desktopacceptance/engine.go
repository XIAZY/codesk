package desktopacceptance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type blockedError struct {
	reason string
}

var errResumeRequired = errors.New("real native logout/login required before resume")

func (e blockedError) Error() string { return e.reason }

// Blocked reports an environment or operator prerequisite that prevented a
// conclusive product verdict.
func Blocked(reason string) error { return blockedError{reason: reason} }

type RunError struct {
	Status Status
	Err    error
}

func (e *RunError) Error() string { return e.Err.Error() }
func (e *RunError) Unwrap() error { return e.Err }

type Engine struct {
	Adapter  NativeAdapter
	Operator Operator
	Poll     time.Duration
}

func (e Engine) Run(ctx context.Context, config Config) (Report, error) {
	if e.Adapter == nil || e.Operator == nil {
		return Report{}, errors.New("native adapter and operator are required")
	}
	if err := validateConfig(config); err != nil {
		return Report{}, err
	}
	if e.Poll <= 0 {
		e.Poll = 250 * time.Millisecond
	}
	recorder, err := openRecorder(config)
	if err != nil {
		return Report{}, err
	}

	var observer SurfaceObserver
	var surfaceEvents []SurfaceEvent
	var finalState State
	publishable := false
	runErr := e.run(ctx, config, recorder, &observer, &publishable)
	pending := errors.Is(runErr, errResumeRequired)
	if pending {
		runErr = nil
		publishable = false
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	cleanupStarted := time.Now().UTC()
	cleanupErr := e.Adapter.CleanupFixtures(cleanupCtx)
	cancel()
	cleanupStatus := StatusPass
	cleanupDetail := "acceptance-owned native fixtures removed or already absent"
	if cleanupErr != nil {
		cleanupStatus = StatusFail
		cleanupDetail = cleanupErr.Error()
		// Cleanup integrity is conclusive: a prerequisite-shaped adapter error
		// cannot downgrade residue or uncertain residue to BLOCKED.
		runErr = errors.Join(runErr, fmt.Errorf("clean acceptance-owned native fixtures: %v", cleanupErr))
	}
	if rowErr := recorder.row("acceptance-fixture-cleanup", cleanupStatus, cleanupStarted, cleanupDetail); rowErr != nil {
		runErr = errors.Join(runErr, rowErr)
	}

	if observer != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), config.Timeout)
		events, stopErr := observer.Stop(stopCtx)
		cancel()
		surfaceEvents = events
		var surfaceErr error
		if stopErr != nil {
			surfaceErr = fmt.Errorf("stop native-surface observer: %w", stopErr)
		}
		for _, event := range events {
			if event.Forbidden {
				surfaceErr = errors.Join(surfaceErr, fmt.Errorf("forbidden native surface pid=%d executable=%s class=%s", event.PID, event.Executable, event.Class))
				break
			}
		}
		runErr = errors.Join(runErr, surfaceErr)
		status := StatusPass
		if surfaceErr != nil {
			status = statusForError(surfaceErr)
		}
		if rowErr := recorder.row("zero-forbidden-native-surfaces", status, time.Now(), surfaceDetail(events, surfaceErr)); rowErr != nil {
			runErr = errors.Join(runErr, rowErr)
		}
	}

	stateCtx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	state, stateErr := e.Adapter.Snapshot(stateCtx)
	cancel()
	if stateErr == nil {
		finalState = state
	} else {
		runErr = errors.Join(runErr, fmt.Errorf("capture final native state: %w", stateErr))
	}
	status := StatusPass
	if runErr != nil {
		status = statusForError(runErr)
		publishable = false
	} else if pending {
		status = StatusPending
	}
	if err := recorder.finish(status, finalState, surfaceEvents, publishable); err != nil {
		runErr = errors.Join(runErr, err)
		status = StatusFail
		recorder.markFailedInMemory()
	}
	report := recorder.report
	if runErr != nil {
		return report, &RunError{Status: status, Err: runErr}
	}
	return report, nil
}

func (e Engine) run(
	ctx context.Context,
	config Config,
	recorder *recorder,
	observer *SurfaceObserver,
	publishable *bool,
) error {
	var host Host
	if err := e.step(ctx, recorder, "native-host", func(stepCtx context.Context) (string, error) {
		value, err := e.Adapter.Host(stepCtx)
		if err != nil {
			return "", err
		}
		if value.Platform != config.TargetPlatform {
			return "", fmt.Errorf("native %s host required, got %s", config.TargetPlatform, value.Platform)
		}
		if strings.TrimSpace(value.Architecture) == "" {
			return "", errors.New("native host architecture is empty")
		}
		host = value
		if config.Phase != PhaseResume {
			recorder.report.Host = value
		}
		return fmt.Sprintf("platform=%s architecture=%s os=%s", value.Platform, value.Architecture, value.OSVersion), nil
	}, config.Timeout); err != nil {
		return err
	}

	var prepared *checkpoint
	if config.Phase == PhaseResume {
		value, err := readCheckpoint(recorder, config)
		if err != nil {
			return e.recordFailure(recorder, "prepare-checkpoint-binding", err)
		}
		prepared = &value
		if !sameHostAccount(value.Host, host) || value.Host.SessionIdentity == "" || host.SessionIdentity == "" {
			return e.recordFailure(recorder, "real-login-session", errors.New("native login session did not change on the same host account"))
		}
		if value.Host.SessionIdentity == host.SessionIdentity {
			if err := recorder.row("real-login-session", StatusPending, time.Now(), "native login session is unchanged; sign out and back in before retrying resume"); err != nil {
				return err
			}
			return errResumeRequired
		}
	}

	if err := e.step(ctx, recorder, "candidate-artifact-binding", func(stepCtx context.Context) (string, error) {
		candidate := config.Candidate
		candidate.SourceRevision = config.SourceRevision
		release, err := e.Adapter.VerifyRelease(stepCtx, candidate)
		if err != nil {
			return "", err
		}
		if err := validateReleaseBinding(release, candidate, config.TargetPlatform); err != nil {
			return "", fmt.Errorf("candidate release binding: %w", err)
		}
		recorder.report.Candidate = release
		artifact, ok := release.Artifact(host.Architecture)
		if !ok {
			return "", fmt.Errorf("candidate has no artifact for native %s", host.Architecture)
		}
		return fmt.Sprintf("version=%s native_sha256=%s manifest_sha256=%s", release.Version, artifact.SHA256, release.ManifestSHA256), nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "previous-artifact-binding", func(stepCtx context.Context) (string, error) {
		release, err := e.Adapter.VerifyRelease(stepCtx, *config.Previous)
		if err != nil {
			return "", err
		}
		if err := validateReleaseBinding(release, *config.Previous, config.TargetPlatform); err != nil {
			return "", fmt.Errorf("previous release binding: %w", err)
		}
		recorder.report.Previous = &release
		artifact, ok := release.Artifact(host.Architecture)
		if !ok {
			return "", fmt.Errorf("previous release has no artifact for native %s", host.Architecture)
		}
		return fmt.Sprintf("version=%s native_sha256=%s manifest_sha256=%s", release.Version, artifact.SHA256, release.ManifestSHA256), nil
	}, config.Timeout); err != nil {
		return err
	}

	candidateTrust, candidateDetail, err := releaseTrust(recorder.report.Candidate, config.AllowUnsignedFunctional)
	if err != nil {
		return e.recordFailure(recorder, "candidate-artifact-trust", err)
	}
	if err := recorder.row("candidate-artifact-trust", candidateTrust, time.Now(), candidateDetail); err != nil {
		return err
	}
	previousTrust, previousDetail, err := releaseTrust(*recorder.report.Previous, config.AllowUnsignedFunctional)
	if err != nil {
		return e.recordFailure(recorder, "previous-artifact-trust", err)
	}
	if err := recorder.row("previous-artifact-trust", previousTrust, time.Now(), previousDetail); err != nil {
		return err
	}
	*publishable = candidateTrust == StatusPass && previousTrust == StatusPass

	var legacyBaseline LegacyStateFingerprint
	if prepared != nil {
		legacyBaseline = prepared.LegacyCLIState
		if err := validateLegacyStateFingerprint(legacyBaseline); err != nil {
			return e.recordFailure(recorder, "legacy-cli-state-baseline", err)
		}
	} else if err := e.step(ctx, recorder, "legacy-cli-state-baseline", func(stepCtx context.Context) (string, error) {
		value, err := e.Adapter.LegacyCLIState(stepCtx)
		if err != nil {
			return "", errors.New("native legacy CLI state fingerprint failed")
		}
		if err := validateLegacyStateFingerprint(value); err != nil {
			return "", err
		}
		legacyBaseline = value
		recorder.report.LegacyCLIState = value
		return legacyStateDetail(value), nil
	}, config.Timeout); err != nil {
		return err
	}

	var previousConnected State
	if config.Phase == PhaseResume {
		if prepared == nil || !sameReleaseIdentity(prepared.Candidate, recorder.report.Candidate) || recorder.report.Previous == nil ||
			!sameReleaseIdentity(prepared.Previous, *recorder.report.Previous) {
			return e.recordFailure(recorder, "prepare-checkpoint-binding", errors.New("release identity changed between prepare and resume"))
		}
		if err := recorder.beginResume(host); err != nil {
			return err
		}
		previousConnected = prepared.PreviousConnected
		if err := recorder.row("prepare-checkpoint-binding", StatusPass, time.Now(), fmt.Sprintf("checkpoint_sha256=%s", recorder.report.CheckpointSHA256)); err != nil {
			return err
		}
		if err := e.step(ctx, recorder, "real-login-autostart", func(stepCtx context.Context) (string, error) {
			state, err := e.waitState(stepCtx, func(state State) bool {
				return registeredInstallState(state, prepared.Previous.Version) == nil && state.AutostartRegistration &&
					runningSingleton(state) && connectedState(state)
			})
			if err != nil {
				return "", blockedError{"Codesk did not autostart as a connected singleton after the real native login"}
			}
			if state.ConfigurationSHA256 != prepared.PreviousConnected.ConfigurationSHA256 ||
				state.ProtectedCredentialSHA256 != prepared.PreviousConnected.ProtectedCredentialSHA256 {
				return "", errors.New("configuration or protected credential changed across the real login")
			}
			if err := rejectLeaks(state); err != nil {
				return "", err
			}
			if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
				return "", err
			}
			if len(state.Application) != 1 || state.Application[0].Executable == "" || state.Application[0].StartedAt == "" {
				return "", blockedError{"post-login application identity is incomplete"}
			}
			if err := e.Operator.Perform(stepCtx, ActionConfirmLogin, "Confirm that Codesk appeared automatically after login and that no forbidden native console or application surface flashed during login."); err != nil {
				return "", err
			}
			return fmt.Sprintf("session=%s->%s resident_pid=%d executable=%s started_at=%s", prepared.Host.SessionIdentity, host.SessionIdentity, state.Application[0].PID, state.Application[0].Executable, state.Application[0].StartedAt), nil
		}, config.Timeout); err != nil {
			return err
		}
	} else {
		if err := e.preparePrevious(ctx, config, recorder, host, legacyBaseline, &previousConnected); err != nil {
			return err
		}
		if config.Phase == PhasePrepare {
			if err := e.step(ctx, recorder, "prepare-checkpoint", func(context.Context) (string, error) {
				if err := writeCheckpoint(recorder, config, host, previousConnected); err != nil {
					return "", err
				}
				return fmt.Sprintf("checkpoint_sha256=%s", recorder.report.CheckpointSHA256), nil
			}, config.Timeout); err != nil {
				return err
			}
			if err := recorder.row("real-login-required", StatusPending, time.Now(), "sign out and back in without manually launching Codesk, then run the resume phase with identical inputs"); err != nil {
				return err
			}
			return errResumeRequired
		}
	}
	if config.Phase == PhaseContinuous {
		if err := e.step(ctx, recorder, "previous-pre-upgrade-launch", func(stepCtx context.Context) (string, error) {
			if err := e.Adapter.Launch(stepCtx); err != nil {
				return "", err
			}
			state, err := e.waitState(stepCtx, func(state State) bool {
				return registeredInstallState(state, config.Previous.Version) == nil && state.AutostartRegistration &&
					runningSingleton(state) && connectedState(state)
			})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("resident_pid=%d", state.Application[0].PID), nil
		}, config.Timeout); err != nil {
			return err
		}
	}
	if err := e.step(ctx, recorder, "previous-disable-autostart", func(stepCtx context.Context) (string, error) {
		before, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		if err := registeredInstallState(before, config.Previous.Version); err != nil || !before.AutostartRegistration ||
			!runningSingleton(before) || !connectedState(before) {
			return "", errors.Join(errors.New("previous release was not an enabled connected singleton before disabling autostart"), err)
		}
		if err := e.Operator.Perform(stepCtx, ActionDisableAutostart, "Turn off Launch at login from the Codesk native menu."); err != nil {
			return "", err
		}
		after, err := e.waitState(stepCtx, func(state State) bool {
			return registeredInstallState(state, config.Previous.Version) == nil && !state.AutostartRegistration &&
				runningSingleton(state) && connectedState(state)
		})
		if err != nil {
			return "", err
		}
		if after.ConfigurationSHA256 != previousConnected.ConfigurationSHA256 ||
			after.ProtectedCredentialSHA256 != previousConnected.ProtectedCredentialSHA256 {
			return "", errors.New("disabling autostart changed configuration or OS-protected credential")
		}
		if err := rejectLeaks(after); err != nil {
			return "", err
		}
		return "disabled choice committed without changing connection state", nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "previous-pre-upgrade-quit", func(stepCtx context.Context) (string, error) {
		if err := e.Operator.Perform(stepCtx, ActionQuit, "Choose Quit Codesk from the native menu before the replacement upgrade."); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, noResidentProcesses); err != nil {
			return "", err
		}
		return "desktop and all managed descendants exited with autostart disabled", nil
	}, config.Timeout); err != nil {
		return err
	}

	if err := e.step(ctx, recorder, "native-surface-observer", func(stepCtx context.Context) (string, error) {
		value, err := e.Adapter.StartSurfaceObserver(stepCtx)
		if err != nil {
			return "", err
		}
		*observer = value
		return "native presentation observation active before candidate setup", nil
	}, config.Timeout); err != nil {
		return err
	}
	candidateArtifact, _ := recorder.report.Candidate.Artifact(host.Architecture)
	if err := e.step(ctx, recorder, "candidate-upgrade-install", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Install(stepCtx, candidateArtifact.Path); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return installedState(state, config.Candidate.Version) == nil
		})
		if err != nil {
			return "", err
		}
		if !connectedState(state) {
			return "", errors.New("upgrade did not preserve a valid connection")
		}
		if state.AutostartRegistration {
			return "", errors.New("upgrade re-enabled the user's disabled autostart choice")
		}
		if state.ConfigurationSHA256 != previousConnected.ConfigurationSHA256 || state.ProtectedCredentialSHA256 != previousConnected.ProtectedCredentialSHA256 {
			return "", errors.New("upgrade changed configuration or OS-protected credential")
		}
		if err := rejectLeaks(state); err != nil {
			return "", err
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return "exact candidate MSI installed; prior product registration replaced; configuration and credential preserved", nil
	}, config.Timeout); err != nil {
		return err
	}

	var appPIDs []int
	if err := e.step(ctx, recorder, "candidate-launch-single-instance", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Launch(stepCtx); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return runningSingleton(state) && connectedState(state)
		})
		if err != nil {
			return "", err
		}
		appPIDs = processIDs(state.Application)
		if err := e.Adapter.LaunchSecond(stepCtx); err != nil {
			return "", err
		}
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-stepCtx.Done():
			return "", stepCtx.Err()
		case <-timer.C:
		}
		second, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		if !slices.Equal(processIDs(second.Application), appPIDs) {
			return "", errors.New("second launch changed the resident Codesk process set")
		}
		if !second.ProcessContained || !connectedState(second) {
			return "", errors.New("resident Codesk process lost native containment or connection state")
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return fmt.Sprintf("resident_pid=%d process_contained=true altered_profile_redirected=false independent_authority=false", appPIDs[0]), nil
	}, config.Timeout); err != nil {
		return err
	}

	initialRuntime, err := e.runTurn(ctx, config, recorder, ActionTurnInitial, "real-codex-turn-initial", RuntimeState{})
	if err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "runtime-crash", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.CrashRuntime(stepCtx, initialRuntime); err != nil {
			return "", err
		}
		return fmt.Sprintf("terminated_runtime_pid=%d generation=%d", initialRuntime.PID, initialRuntime.Generation), nil
	}, config.Timeout); err != nil {
		return err
	}
	crashRuntime, err := e.runTurn(ctx, config, recorder, ActionTurnAfterCrash, "real-codex-turn-after-crash", initialRuntime)
	if err != nil {
		return err
	}
	if !runtimeAfter(crashRuntime, initialRuntime) {
		return e.recordFailure(recorder, "runtime-crash-replacement", errors.New("runtime identity did not advance after crash"))
	}
	if err := recorder.row("runtime-crash-replacement", StatusPass, time.Now(), fmt.Sprintf("runtime_pid=%d->%d generation=%d->%d pid_reused=%t", initialRuntime.PID, crashRuntime.PID, initialRuntime.Generation, crashRuntime.Generation, initialRuntime.PID == crashRuntime.PID)); err != nil {
		return err
	}

	var crashedApplication Process
	if err := e.step(ctx, recorder, "application-crash-containment", func(stepCtx context.Context) (string, error) {
		state, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		if !runningSingleton(state) || !connectedState(state) || !containsPID(state.ManagedDescendants, crashRuntime.PID) {
			return "", errors.New("application and runtime were not resident before the containment crash")
		}
		crashedApplication = state.Application[0]
		if crashedApplication.StartedAt == "" {
			return "", blockedError{"native process identity did not include an application start time"}
		}
		for _, descendant := range state.ManagedDescendants {
			if descendant.PID <= 1 || descendant.Executable == "" || descendant.StartedAt == "" {
				return "", blockedError{"native process identity did not include complete descendant creation evidence"}
			}
		}
		boundDescendants := append([]Process(nil), state.ManagedDescendants...)
		if err := e.Adapter.CrashApplication(stepCtx, crashedApplication, boundDescendants); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, noResidentProcesses); err != nil {
			return "", errors.New("managed descendants survived abnormal application termination")
		}
		return fmt.Sprintf("terminated_application_pid=%d runtime_pid=%d bound_processes=%d all_bound_handles_exited=true", crashedApplication.PID, crashRuntime.PID, 1+len(boundDescendants)), nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "application-crash-relaunch", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Launch(stepCtx); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return registeredInstallState(state, config.Candidate.Version) == nil && !state.AutostartRegistration &&
				runningSingleton(state) && connectedState(state)
		})
		if err != nil {
			return "", err
		}
		if state.Application[0].StartedAt == "" || sameProcessIdentity(state.Application[0], crashedApplication) {
			return "", errors.New("application identity did not advance after abnormal termination")
		}
		appPIDs = processIDs(state.Application)
		return fmt.Sprintf("application_pid=%d->%d pid_reused=%t", crashedApplication.PID, state.Application[0].PID, crashedApplication.PID == state.Application[0].PID), nil
	}, config.Timeout); err != nil {
		return err
	}
	if _, err := e.runTurn(ctx, config, recorder, ActionTurnAfterAppCrash, "real-codex-turn-after-app-crash", RuntimeState{}); err != nil {
		return err
	}

	var generationBefore uint64
	if err := e.step(ctx, recorder, "restart-daemon", func(stepCtx context.Context) (string, error) {
		before, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		generationBefore = before.ServiceGeneration
		if generationBefore == 0 {
			return "", blockedError{"desktop log did not expose an online service generation"}
		}
		if err := e.Operator.Perform(stepCtx, ActionRestart, "Choose Restart daemon from the Codesk native menu."); err != nil {
			return "", err
		}
		after, err := e.waitState(stepCtx, func(state State) bool {
			return state.ServiceGeneration > generationBefore && runningSingleton(state) && connectedState(state) &&
				slices.Equal(processIDs(state.Application), appPIDs)
		})
		if err != nil {
			return "", errors.New("daemon generation did not advance while the desktop PID remained stable")
		}
		return fmt.Sprintf("service_generation=%d->%d desktop_pid=%d", generationBefore, after.ServiceGeneration, appPIDs[0]), nil
	}, config.Timeout); err != nil {
		return err
	}
	if _, err := e.runTurn(ctx, config, recorder, ActionTurnRestart, "real-codex-turn-after-restart", RuntimeState{ServiceGeneration: generationBefore}); err != nil {
		return err
	}

	var connectedBeforeUninstall State
	if err := e.step(ctx, recorder, "candidate-quit", func(stepCtx context.Context) (string, error) {
		state, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		connectedBeforeUninstall = state
		if err := registeredInstallState(state, config.Candidate.Version); err != nil || state.AutostartRegistration || !runningSingleton(state) || !connectedState(state) {
			return "", errors.Join(errors.New("candidate state is incomplete before quit"), err)
		}
		if err := e.Operator.Perform(stepCtx, ActionQuit, "Choose Quit Codesk from the native menu. Do not force-terminate it with an operating-system process tool."); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, noResidentProcesses); err != nil {
			return "", err
		}
		return "normal quit removed desktop and all managed descendants", nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "candidate-uninstall", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Uninstall(stepCtx, config.Candidate.Version); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return !state.Installed && !state.AutostartRegistration && !state.RemovalRegistration && !state.UserLaunchEntry && len(state.Application) == 0 && len(state.ManagedDescendants) == 0
		})
		if err != nil {
			return "", err
		}
		if state.ConfigurationSHA256 != connectedBeforeUninstall.ConfigurationSHA256 || state.ProtectedCredentialSHA256 != connectedBeforeUninstall.ProtectedCredentialSHA256 {
			return "", errors.New("uninstall changed preserved configuration or OS-protected credential")
		}
		if !connectedState(state) {
			return "", errors.New("uninstall did not preserve a valid connection")
		}
		if len(state.LegacyLaunchers) != 0 {
			return "", errors.New("legacy Codesk launcher remained after uninstall")
		}
		if err := rejectLeaks(state); err != nil {
			return "", err
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return "application, registration, launch entry, and processes removed; user data and protected credential preserved", nil
	}, config.Timeout); err != nil {
		return err
	}

	if err := e.step(ctx, recorder, "candidate-reset-user-data", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.ResetUserData(stepCtx); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, cleanUserDataState); err != nil {
			return "", errors.New("dedicated-account configuration or credential remained after explicit reset")
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return "preserved upgrade data removed before fresh-install acceptance", nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "candidate-fresh-install", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Install(stepCtx, candidateArtifact.Path); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, func(state State) bool {
			return installedState(state, config.Candidate.Version) == nil && cleanUserDataState(state)
		}); err != nil {
			return "", err
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return "exact candidate installed without launching or recreating user connection data", nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "candidate-fresh-connect", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Launch(stepCtx); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, runningSingleton); err != nil {
			return "", err
		}
		if err := e.Operator.Perform(stepCtx, ActionConnectFresh, "Open the candidate Codesk native menu, choose Connect, and complete the browser handoff for this clean dedicated account."); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return registeredInstallState(state, config.Candidate.Version) == nil && runningSingleton(state) && connectedState(state)
		})
		if err != nil {
			return "", blockedError{"fresh candidate browser handoff did not produce token-free configuration plus an OS-protected credential"}
		}
		if err := rejectLeaks(state); err != nil {
			return "", err
		}
		return fmt.Sprintf("config_sha256=%s protected_credential_sha256=%s", state.ConfigurationSHA256, state.ProtectedCredentialSHA256), nil
	}, config.Timeout); err != nil {
		return err
	}
	if _, err := e.runTurn(ctx, config, recorder, ActionTurnFresh, "real-codex-turn-fresh-connect", RuntimeState{}); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "candidate-fresh-quit", func(stepCtx context.Context) (string, error) {
		if err := e.Operator.Perform(stepCtx, ActionQuit, "Choose Quit Codesk from the native menu after the fresh-connect turn completes."); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, noResidentProcesses); err != nil {
			return "", err
		}
		return "normal quit removed the fresh desktop and all managed descendants", nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "candidate-fresh-uninstall", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Uninstall(stepCtx, config.Candidate.Version); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return !state.Installed && !state.AutostartRegistration && !state.RemovalRegistration &&
				!state.UserLaunchEntry && noResidentProcesses(state)
		})
		if err != nil {
			return "", err
		}
		if !connectedState(state) {
			return "", errors.New("fresh uninstall did not preserve the candidate connection data")
		}
		if err := rejectLeaks(state); err != nil {
			return "", err
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return "fresh installation removed while connection data remained preserved", nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "final-user-data-reset", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.ResetUserData(stepCtx); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return cleanUserDataState(state) && !state.Installed && !state.AutostartRegistration &&
				!state.RemovalRegistration && !state.UserLaunchEntry && noResidentProcesses(state)
		})
		if err != nil {
			return "", errors.New("dedicated account did not return to the clean baseline")
		}
		if len(state.LegacyLaunchers) != 0 || len(state.PlaintextSecretLeakPaths) != 0 {
			return "", errors.New("clean baseline retained a legacy launcher or credential-shaped plaintext")
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return "dedicated account restored to the clean baseline; " + legacyStateDetail(legacyBaseline), nil
	}, config.Timeout); err != nil {
		return err
	}
	return nil
}

func (e Engine) preparePrevious(
	ctx context.Context,
	config Config,
	recorder *recorder,
	host Host,
	legacyBaseline LegacyStateFingerprint,
	connected *State,
) error {
	if err := e.step(ctx, recorder, "dedicated-account-clean-state", func(stepCtx context.Context) (string, error) {
		state, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		if state.Installed || state.AutostartRegistration || state.RemovalRegistration || state.UserLaunchEntry || len(state.Application) != 0 ||
			len(state.ManagedDescendants) != 0 || len(state.LegacyLaunchers) != 0 || state.Connected || state.ConfigurationSHA256 != "" ||
			state.ProtectedCredentialSHA256 != "" || state.ConfigurationValid || state.ProtectedCredentialValid {
			return "", errors.New("dedicated test account is not clean; uninstall Codesk and remove preserved Codesk data before retrying")
		}
		if len(state.PlaintextSecretLeakPaths) != 0 {
			return "", fmt.Errorf("credential-shaped plaintext exists before setup: %v", state.PlaintextSecretLeakPaths)
		}
		return "no installation, registration, resident process, configuration, or credential", nil
	}, config.Timeout); err != nil {
		return err
	}

	previousArtifact, _ := recorder.report.Previous.Artifact(host.Architecture)
	if err := e.step(ctx, recorder, "previous-install", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Install(stepCtx, previousArtifact.Path); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return installedState(state, config.Previous.Version) == nil
		})
		if err != nil {
			return "", err
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return fmt.Sprintf("version=%s registration=true", state.InstalledVersion), nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "previous-connect", func(stepCtx context.Context) (string, error) {
		if err := e.Adapter.Launch(stepCtx); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, runningSingleton); err != nil {
			return "", err
		}
		if err := e.Operator.Perform(stepCtx, ActionConnect, "Open the Codesk native menu, choose Connect, and complete the browser handoff in this dedicated test account."); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			return registeredInstallState(state, config.Previous.Version) == nil && runningSingleton(state) && connectedState(state)
		})
		if err != nil {
			return "", blockedError{"browser handoff did not produce token-free configuration plus an OS-protected credential"}
		}
		if !state.AutostartRegistration {
			if err := e.Operator.Perform(stepCtx, ActionEnableAutostart, "Turn on Launch at login from the Codesk native menu for the real logout/login row."); err != nil {
				return "", err
			}
			state, err = e.waitState(stepCtx, func(state State) bool {
				return registeredInstallState(state, config.Previous.Version) == nil && state.AutostartRegistration &&
					runningSingleton(state) && connectedState(state)
			})
			if err != nil {
				return "", errors.New("previous release did not commit enabled autostart for the login-cycle row")
			}
		}
		if err := rejectLeaks(state); err != nil {
			return "", err
		}
		if err := e.requireLegacyCLIState(stepCtx, legacyBaseline); err != nil {
			return "", err
		}
		return fmt.Sprintf("config_sha256=%s protected_credential_sha256=%s", state.ConfigurationSHA256, state.ProtectedCredentialSHA256), nil
	}, config.Timeout); err != nil {
		return err
	}
	if err := e.step(ctx, recorder, "previous-quit", func(stepCtx context.Context) (string, error) {
		state, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		if err := preparedConnectedState(state, config.Previous.Version); err != nil {
			return "", err
		}
		*connected = state
		if err := e.Operator.Perform(stepCtx, ActionQuit, "Choose Quit Codesk from the native menu. Do not force-terminate it with an operating-system process tool."); err != nil {
			return "", err
		}
		if _, err := e.waitState(stepCtx, noResidentProcesses); err != nil {
			return "", err
		}
		return "desktop and all managed descendants exited", nil
	}, config.Timeout); err != nil {
		return err
	}
	return nil
}

func (e Engine) runTurn(ctx context.Context, config Config, recorder *recorder, action Action, row string, baseline RuntimeState) (RuntimeState, error) {
	var runtime RuntimeState
	err := e.step(ctx, recorder, row, func(stepCtx context.Context) (string, error) {
		before, err := e.Adapter.Snapshot(stepCtx)
		if err != nil {
			return "", err
		}
		marker := strings.ToUpper(strings.ReplaceAll(recorder.report.RunID+"-"+string(action), "_", "-"))
		instruction := fmt.Sprintf("From the authenticated Codesk UI, trigger one real Codex agent turn whose prompt asks it to reply exactly %s. Wait for that turn to finish before pressing Enter here.", marker)
		if err := e.Operator.Perform(stepCtx, action, instruction); err != nil {
			return "", err
		}
		state, err := e.waitState(stepCtx, func(state State) bool {
			runtime := state.Runtime
			newIdentity := !sameRuntimeIdentity(runtime, before.Runtime)
			newTurn := newIdentity && runtime.TurnStartedSequence > 0 && runtime.TurnTerminalSequence > 0 ||
				!newIdentity && runtime.ObservationSequence > before.Runtime.ObservationSequence &&
					runtime.TurnStartedSequence > before.Runtime.TurnStartedSequence && runtime.TurnTerminalSequence > before.Runtime.TurnTerminalSequence
			requiredReplacement := action != ActionTurnAfterCrash || runtimeAfter(runtime, baseline)
			requiredRestart := action != ActionTurnRestart || runtime.ServiceGeneration > baseline.ServiceGeneration
			return runningSingleton(state) && connectedState(state) && strings.EqualFold(runtime.Kind, "codex") && runtime.PID > 0 &&
				runtime.ObservationSequence > 0 && newTurn && requiredReplacement && requiredRestart &&
				successfulTerminalTurn(runtime) && containsPID(state.ManagedDescendants, runtime.PID)
		})
		if err != nil {
			return "", blockedError{"token-free runtime/turn receipt did not prove a surviving real Codex turn"}
		}
		if err := rejectLeaks(state); err != nil {
			return "", err
		}
		runtime = state.Runtime
		return fmt.Sprintf("runtime_pid=%d generation=%d turn_started=%d turn_terminal=%d status=%s", runtime.PID, runtime.Generation, runtime.TurnStartedSequence, runtime.TurnTerminalSequence, runtime.TurnStatus), nil
	}, config.Timeout)
	return runtime, err
}

func (e Engine) step(ctx context.Context, recorder *recorder, name string, operation func(context.Context) (string, error), timeout time.Duration) error {
	started := time.Now().UTC()
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	detail, err := operation(stepCtx)
	if err == nil {
		err = stepCtx.Err()
	}
	if err != nil {
		status := statusForError(err)
		return errors.Join(err, recorder.row(name, status, started, err.Error()))
	}
	if err := recorder.row(name, StatusPass, started, detail); err != nil {
		return err
	}
	return nil
}

func (e Engine) recordFailure(recorder *recorder, name string, err error) error {
	return errors.Join(err, recorder.row(name, statusForError(err), time.Now(), err.Error()))
}

func (e Engine) waitState(ctx context.Context, predicate func(State) bool) (State, error) {
	ticker := time.NewTicker(e.Poll)
	defer ticker.Stop()
	for {
		state, err := e.Adapter.Snapshot(ctx)
		if err != nil {
			return State{}, err
		}
		if predicate(state) {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return State{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func validateConfig(config Config) error {
	switch config.Phase {
	case PhaseContinuous, PhasePrepare, PhaseResume:
	default:
		return errors.New("acceptance phase must be continuous, prepare, or resume")
	}
	if !config.Destructive {
		return errors.New("destructive acceptance must be explicitly enabled for a dedicated native test account")
	}
	if !validPlatform(config.TargetPlatform) {
		return errors.New("target platform must be a lowercase native platform identifier")
	}
	if config.Previous == nil {
		return errors.New("previous release is required; upgrade acceptance cannot be skipped")
	}
	if strings.TrimSpace(config.Candidate.Directory) == "" || strings.TrimSpace(config.Previous.Directory) == "" {
		return errors.New("candidate and previous release directories are required")
	}
	if strings.TrimSpace(config.Candidate.Version) == "" || strings.TrimSpace(config.Previous.Version) == "" {
		return errors.New("candidate and previous release versions are required")
	}
	if config.Candidate.SourceRevision != "" && config.Candidate.SourceRevision != config.SourceRevision {
		return errors.New("candidate release source revision does not match the exact target revision")
	}
	if config.Previous.SourceRevision != "" {
		if err := validateExactRevision(config.Previous.SourceRevision); err != nil {
			return fmt.Errorf("previous release source revision: %w", err)
		}
	}
	if config.Previous.Version == config.Candidate.Version {
		return errors.New("previous and candidate versions must differ")
	}
	if config.Timeout < 30*time.Second {
		return errors.New("acceptance timeout must be at least 30 seconds")
	}
	if config.SourceRevision != strings.TrimSpace(config.SourceRevision) {
		return errors.New("exact source revision must not contain surrounding whitespace")
	}
	if err := validateExactRevision(config.SourceRevision); err != nil {
		return err
	}
	if err := validateExactRevision(config.RunnerSourceRevision); err != nil {
		return fmt.Errorf("acceptance runner source revision: %w", err)
	}
	return nil
}

func validPlatform(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			return false
		}
	}
	return true
}

func statusForError(err error) Status {
	if err != nil && onlyBlockedErrors(err) {
		return StatusBlocked
	}
	return StatusFail
}

func installedState(state State, version string) error {
	if err := registeredInstallState(state, version); err != nil {
		return err
	}
	if len(state.Application) != 0 || len(state.ManagedDescendants) != 0 {
		return errors.New("MSI installation unexpectedly left resident processes")
	}
	return nil
}

func registeredInstallState(state State, version string) error {
	if !state.Installed || state.InstalledVersion != version || !state.RemovalRegistration || !state.UserLaunchEntry {
		return errors.New("installation or per-user registration is incomplete")
	}
	return nil
}

func runningSingleton(state State) bool {
	return len(state.Application) == 1 && state.ProcessContained
}

func connectedState(state State) bool {
	return state.Connected && state.ConfigurationValid && state.ProtectedCredentialValid &&
		validSHA256(state.ConfigurationSHA256) && validSHA256(state.ProtectedCredentialSHA256)
}

func noResidentProcesses(state State) bool {
	return len(state.Application) == 0 && len(state.ManagedDescendants) == 0
}

func cleanUserDataState(state State) bool {
	return !state.Connected && !state.ConfigurationValid && !state.ProtectedCredentialValid &&
		state.ConfigurationSHA256 == "" && state.ProtectedCredentialSHA256 == ""
}

func rejectLeaks(state State) error {
	if len(state.PlaintextSecretLeakPaths) != 0 {
		return fmt.Errorf("credential-shaped plaintext found in: %v", state.PlaintextSecretLeakPaths)
	}
	return nil
}

func validateLegacyStateFingerprint(value LegacyStateFingerprint) error {
	if !value.Present {
		if value.DigestSHA256 != "" || value.EntryCount != 0 || value.ByteCount != 0 {
			return errors.New("absent legacy CLI state has a nonempty aggregate fingerprint")
		}
		return nil
	}
	if !validSHA256(value.DigestSHA256) {
		return errors.New("present legacy CLI state has an invalid aggregate fingerprint")
	}
	return nil
}

func legacyStateDetail(value LegacyStateFingerprint) string {
	return fmt.Sprintf(
		"present=%t digest_sha256=%s entries=%d bytes=%d",
		value.Present,
		value.DigestSHA256,
		value.EntryCount,
		value.ByteCount,
	)
}

func (e Engine) requireLegacyCLIState(ctx context.Context, expected LegacyStateFingerprint) error {
	actual, err := e.Adapter.LegacyCLIState(ctx)
	if err != nil {
		return errors.New("native legacy CLI state fingerprint failed")
	}
	if err := validateLegacyStateFingerprint(actual); err != nil {
		return err
	}
	if actual != expected {
		return errors.New("legacy CLI state changed during the desktop lifecycle")
	}
	return nil
}

func processIDs(processes []Process) []int {
	ids := make([]int, 0, len(processes))
	for _, process := range processes {
		ids = append(ids, process.PID)
	}
	slices.Sort(ids)
	return ids
}

func containsPID(processes []Process, pid int) bool {
	for _, process := range processes {
		if process.PID == pid {
			return true
		}
	}
	return false
}

func sameProcessIdentity(left, right Process) bool {
	return left.PID == right.PID && left.StartedAt != "" && left.StartedAt == right.StartedAt &&
		strings.EqualFold(left.Executable, right.Executable)
}

func terminalSuccess(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "success", "succeeded", "turn_completed":
		return true
	default:
		return false
	}
}

func successfulTerminalTurn(runtime RuntimeState) bool {
	return runtime.TurnStartedSequence > 0 && runtime.TurnTerminalSequence == runtime.TurnStartedSequence &&
		terminalSuccess(runtime.TurnStatus)
}

func releaseTrust(release Release, allowUnsigned bool) (Status, string, error) {
	unsigned := false
	for _, artifact := range release.Artifacts {
		if artifact.ManifestSigned {
			if !artifact.SignaturePresent || !artifact.SignatureValid {
				return StatusFail, "", fmt.Errorf("%s %s artifact claims signing but is not trusted by the native signature verifier: %s", release.Version, artifact.Architecture, artifact.SignatureError)
			}
			continue
		}
		if artifact.SignaturePresent || artifact.SignatureValid {
			return StatusFail, "", fmt.Errorf("%s %s artifact has a native signature but the manifest claims unsigned", release.Version, artifact.Architecture)
		}
		unsigned = true
	}
	if unsigned {
		if !allowUnsigned {
			return StatusFail, "", fmt.Errorf("release %s is unsigned", release.Version)
		}
		return StatusWaived, "unsigned functional mode; artifact trust and publishability are NOT_ESTABLISHED", nil
	}
	return StatusPass, "native signature trust verified for every release artifact", nil
}

func validateReleaseBinding(release Release, input ReleaseInput, platform string) error {
	if release.Platform != platform {
		return fmt.Errorf("platform is %s, want %s", release.Platform, platform)
	}
	if release.Version != input.Version {
		return fmt.Errorf("version is %q, want %q", release.Version, input.Version)
	}
	if err := validateExactRevision(release.SourceRevision); err != nil {
		return fmt.Errorf("source revision: %w", err)
	}
	if input.SourceRevision != "" && release.SourceRevision != input.SourceRevision {
		return fmt.Errorf("source revision is %s, want %s", release.SourceRevision, input.SourceRevision)
	}
	if !validSHA256(release.ManifestSHA256) || !validSHA256(release.SumsSHA256) {
		return errors.New("manifest or checksum-list fingerprint is not a canonical SHA-256 value")
	}
	if strings.TrimSpace(release.ManifestPath) == "" || strings.TrimSpace(release.SumsPath) == "" {
		return errors.New("manifest or checksum-list path is absent")
	}
	if len(release.Toolchain) == 0 || len(release.Artifacts) == 0 {
		return errors.New("toolchain or artifact identity is absent")
	}
	if platform == "windows" {
		if release.UpgradeCode == "" {
			return errors.New("Windows MSI UpgradeCode is absent")
		}
		if release.CrossArchitecturePolicy != "converge" && release.CrossArchitecturePolicy != "block" {
			return errors.New("Windows MSI cross-architecture policy is invalid")
		}
	}
	seen := make(map[string]struct{}, len(release.Artifacts))
	for _, artifact := range release.Artifacts {
		if artifact.Architecture == "" || artifact.Path == "" || artifact.Size <= 0 ||
			!validSHA256(artifact.SHA256) || artifact.NativeFormat == "" || artifact.NativeArchitecture == "" {
			return fmt.Errorf("artifact %q has an incomplete normalized identity", artifact.Architecture)
		}
		if artifact.ManifestSigned != release.Signed {
			return fmt.Errorf("artifact %q signing claim differs from the release manifest", artifact.Architecture)
		}
		if platform == "windows" && (artifact.NativeFormat != "msi" || artifact.ProductCode == "" ||
			!validSHA256(artifact.CodeskSHA256) || !validSHA256(artifact.AgentToolSHA256)) {
			return fmt.Errorf("Windows MSI artifact %q has an incomplete product or installed-payload identity", artifact.Architecture)
		}
		if _, exists := seen[artifact.Architecture]; exists {
			return fmt.Errorf("artifact architecture %q is duplicated", artifact.Architecture)
		}
		seen[artifact.Architecture] = struct{}{}
	}
	return nil
}

func sameRuntimeIdentity(left, right RuntimeState) bool {
	return left.ServiceGeneration == right.ServiceGeneration && left.Generation == right.Generation && left.PID == right.PID && strings.EqualFold(left.Kind, right.Kind)
}

func runtimeAfter(candidate, baseline RuntimeState) bool {
	return candidate.ServiceGeneration > baseline.ServiceGeneration ||
		candidate.ServiceGeneration == baseline.ServiceGeneration && candidate.Generation > baseline.Generation
}

func onlyBlockedErrors(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyBlockedErrors(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyBlockedErrors(wrapped.Unwrap())
	}
	_, blocked := err.(blockedError)
	return blocked
}

func surfaceDetail(events []SurfaceEvent, runErr error) string {
	forbidden := 0
	for _, event := range events {
		if event.Forbidden {
			forbidden++
		}
	}
	if runErr != nil && forbidden != 0 {
		return fmt.Sprintf("captured=%d forbidden=%d: %v", len(events), forbidden, runErr)
	}
	return fmt.Sprintf("captured=%d forbidden=%d", len(events), forbidden)
}
