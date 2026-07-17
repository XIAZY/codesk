package desktopacceptance

import (
	"context"
	"time"
)

type Status string

const (
	StatusPass    Status = "PASS"
	StatusFail    Status = "FAIL"
	StatusBlocked Status = "BLOCKED"
	StatusWaived  Status = "WAIVED"
	StatusPending Status = "PENDING"
)

type Action string

const (
	ActionConnect           Action = "connect-upgrade"
	ActionConfirmLogin      Action = "confirm-login"
	ActionEnableAutostart   Action = "enable-autostart"
	ActionDisableAutostart  Action = "disable-autostart"
	ActionTurnInitial       Action = "turn-initial"
	ActionTurnAfterCrash    Action = "turn-after-crash"
	ActionTurnAfterAppCrash Action = "turn-after-app-crash"
	ActionRestart           Action = "restart-daemon"
	ActionTurnRestart       Action = "turn-after-restart"
	ActionQuit              Action = "quit"
	ActionConnectFresh      Action = "connect-fresh"
	ActionTurnFresh         Action = "turn-fresh"
)

type Phase string

const (
	PhaseContinuous Phase = "continuous"
	PhasePrepare    Phase = "prepare"
	PhaseResume     Phase = "resume"
)

type ReleaseInput struct {
	Directory      string
	Version        string
	SourceRevision string
}

type Config struct {
	Phase                   Phase
	TargetPlatform          string
	SourceRevision          string
	RunnerSourceRevision    string
	Candidate               ReleaseInput
	Previous                *ReleaseInput
	EvidenceDir             string
	Timeout                 time.Duration
	Destructive             bool
	AllowUnsignedFunctional bool
}

type Host struct {
	Platform        string `json:"platform"`
	Architecture    string `json:"architecture"`
	OSVersion       string `json:"os_version"`
	Hostname        string `json:"hostname"`
	Username        string `json:"username"`
	SessionIdentity string `json:"session_identity"`
}

type Process struct {
	PID        int    `json:"pid"`
	ParentPID  int    `json:"parent_pid"`
	Executable string `json:"executable"`
	StartedAt  string `json:"started_at,omitempty"`
}

type RuntimeState struct {
	Kind                 string `json:"kind,omitempty"`
	PID                  int    `json:"pid,omitempty"`
	ServiceGeneration    uint64 `json:"service_generation,omitempty"`
	ObservationSequence  uint64 `json:"observation_sequence,omitempty"`
	Generation           uint64 `json:"generation,omitempty"`
	TurnStartedSequence  uint64 `json:"turn_started_sequence,omitempty"`
	TurnTerminalSequence uint64 `json:"turn_terminal_sequence,omitempty"`
	TurnStatus           string `json:"turn_status,omitempty"`
}

type State struct {
	CapturedAt                time.Time    `json:"captured_at"`
	Installed                 bool         `json:"installed"`
	InstalledVersion          string       `json:"installed_version,omitempty"`
	AutostartRegistration     bool         `json:"autostart_registration"`
	RemovalRegistration       bool         `json:"removal_registration"`
	UserLaunchEntry           bool         `json:"user_launch_entry"`
	LegacyLaunchers           []string     `json:"legacy_launchers,omitempty"`
	Application               []Process    `json:"application,omitempty"`
	ManagedDescendants        []Process    `json:"managed_descendants,omitempty"`
	ProcessContained          bool         `json:"process_contained"`
	Connected                 bool         `json:"connected"`
	ConfigurationValid        bool         `json:"configuration_valid"`
	ConfigurationSHA256       string       `json:"configuration_sha256,omitempty"`
	ProtectedCredentialValid  bool         `json:"protected_credential_valid"`
	ProtectedCredentialSHA256 string       `json:"protected_credential_sha256,omitempty"`
	PlaintextSecretLeakPaths  []string     `json:"plaintext_secret_leak_paths,omitempty"`
	ServiceGeneration         uint64       `json:"service_generation,omitempty"`
	Runtime                   RuntimeState `json:"runtime"`
}

type SurfaceEvent struct {
	At         time.Time `json:"at"`
	PID        int       `json:"pid"`
	ParentPID  int       `json:"parent_pid,omitempty"`
	Executable string    `json:"executable,omitempty"`
	StartedAt  string    `json:"started_at,omitempty"`
	Class      string    `json:"class,omitempty"`
	Forbidden  bool      `json:"forbidden"`
}

// LegacyStateFingerprint is an opaque aggregate of the platform-native legacy
// CLI root. Paths, names, contents, and per-file hashes never cross the native
// adapter boundary.
type LegacyStateFingerprint struct {
	Present      bool   `json:"present"`
	DigestSHA256 string `json:"digest_sha256,omitempty"`
	EntryCount   uint64 `json:"entry_count"`
	ByteCount    uint64 `json:"byte_count"`
}

type SurfaceObserver interface {
	Stop(context.Context) ([]SurfaceEvent, error)
}

type NativeAdapter interface {
	Host(context.Context) (Host, error)
	VerifyRelease(context.Context, ReleaseInput) (Release, error)
	LegacyCLIState(context.Context) (LegacyStateFingerprint, error)
	Snapshot(context.Context) (State, error)
	StartSurfaceObserver(context.Context) (SurfaceObserver, error)
	SeedLegacyLaunchers(context.Context) (int, error)
	Install(context.Context, string) error
	Launch(context.Context) error
	LaunchSecond(context.Context) error
	CrashRuntime(context.Context, RuntimeState) error
	CrashApplication(context.Context, Process, []Process) error
	Uninstall(context.Context, string) error
	ResetUserData(context.Context) error
	CleanupFixtures(context.Context) error
}

type Operator interface {
	Perform(context.Context, Action, string) error
}
