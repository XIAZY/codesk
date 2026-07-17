package desktopacceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
)

const checkpointSchemaVersion = 1

type checkpoint struct {
	SchemaVersion        int                    `json:"schema_version"`
	RunID                string                 `json:"run_id"`
	TargetPlatform       string                 `json:"target_platform"`
	SourceRevision       string                 `json:"source_revision"`
	RunnerSourceRevision string                 `json:"runner_source_revision"`
	RunnerSHA256         string                 `json:"runner_sha256"`
	Host                 Host                   `json:"host"`
	Candidate            Release                `json:"candidate"`
	Previous             Release                `json:"previous"`
	LegacyCLIState       LegacyStateFingerprint `json:"legacy_cli_state_baseline"`
	PreviousConnected    State                  `json:"previous_connected"`
}

func writeCheckpoint(recorder *recorder, config Config, host Host, previousConnected State) error {
	if recorder.report.Previous == nil {
		return errors.New("previous release identity is absent at checkpoint")
	}
	if err := preparedConnectedState(previousConnected, recorder.report.Previous.Version); err != nil {
		return fmt.Errorf("prepared connected state: %w", err)
	}
	value := checkpoint{
		SchemaVersion:        checkpointSchemaVersion,
		RunID:                recorder.report.RunID,
		TargetPlatform:       config.TargetPlatform,
		SourceRevision:       config.SourceRevision,
		RunnerSourceRevision: config.RunnerSourceRevision,
		RunnerSHA256:         recorder.report.RunnerSHA256,
		Host:                 host,
		Candidate:            recorder.report.Candidate,
		Previous:             *recorder.report.Previous,
		LegacyCLIState:       recorder.report.LegacyCLIState,
		PreviousConnected:    previousConnected,
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode acceptance checkpoint: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(recorder.directory, "checkpoint.json")
	if _, err := os.Lstat(path); err == nil {
		return errors.New("acceptance checkpoint already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect acceptance checkpoint: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write acceptance checkpoint: %w", err)
	}
	if err := replaceFile(temporary, path); err != nil {
		return fmt.Errorf("publish acceptance checkpoint: %w", err)
	}
	recorder.report.CheckpointSHA256 = hashBytes(data)
	return recorder.persist()
}

func readCheckpoint(recorder *recorder, config Config) (checkpoint, error) {
	path := filepath.Join(recorder.directory, "checkpoint.json")
	data, err := readBoundedFile(path, 4<<20)
	if err != nil {
		return checkpoint{}, fmt.Errorf("read acceptance checkpoint: %w", err)
	}
	if hashBytes(data) != recorder.report.CheckpointSHA256 {
		return checkpoint{}, errors.New("acceptance checkpoint hash does not match the prepare report")
	}
	var value checkpoint
	if err := decodeCanonicalJSON(data, &value, "acceptance checkpoint"); err != nil {
		return checkpoint{}, err
	}
	if value.SchemaVersion != checkpointSchemaVersion || value.RunID != recorder.report.RunID ||
		value.TargetPlatform != config.TargetPlatform || value.SourceRevision != config.SourceRevision ||
		value.RunnerSourceRevision != config.RunnerSourceRevision || value.RunnerSHA256 != recorder.report.RunnerSHA256 {
		return checkpoint{}, errors.New("acceptance checkpoint identity does not match resume configuration")
	}
	if value.Host != recorder.report.Host || value.LegacyCLIState != recorder.report.LegacyCLIState ||
		!sameReleaseIdentity(value.Candidate, recorder.report.Candidate) ||
		recorder.report.Previous == nil || !sameReleaseIdentity(value.Previous, *recorder.report.Previous) {
		return checkpoint{}, errors.New("acceptance checkpoint identity does not match the prepare report")
	}
	if err := preparedConnectedState(value.PreviousConnected, value.Previous.Version); err != nil {
		return checkpoint{}, fmt.Errorf("acceptance checkpoint connected state: %w", err)
	}
	return value, nil
}

func preparedConnectedState(state State, version string) error {
	if err := registeredInstallState(state, version); err != nil {
		return err
	}
	if !state.AutostartRegistration {
		return errors.New("previous autostart registration was not enabled")
	}
	if !runningSingleton(state) {
		return errors.New("previous desktop was not a contained singleton")
	}
	if !connectedState(state) {
		return errors.New("previous connection fingerprints are invalid")
	}
	return rejectLeaks(state)
}

func sameReleaseIdentity(left, right Release) bool {
	if left.Platform != right.Platform || left.Version != right.Version || left.SourceRevision != right.SourceRevision ||
		left.Signed != right.Signed || left.ManifestPath != right.ManifestPath || left.ManifestSHA256 != right.ManifestSHA256 ||
		left.SumsPath != right.SumsPath || left.SumsSHA256 != right.SumsSHA256 ||
		!maps.Equal(left.Toolchain, right.Toolchain) || len(left.Artifacts) != len(right.Artifacts) {
		return false
	}
	rightArtifacts := make(map[string]ReleaseArtifact, len(right.Artifacts))
	for _, artifact := range right.Artifacts {
		if artifact.Architecture == "" {
			return false
		}
		rightArtifacts[artifact.Architecture] = artifact
	}
	if len(rightArtifacts) != len(right.Artifacts) {
		return false
	}
	leftArtifacts := make(map[string]struct{}, len(left.Artifacts))
	for _, artifact := range left.Artifacts {
		if artifact.Architecture == "" {
			return false
		}
		if _, exists := leftArtifacts[artifact.Architecture]; exists {
			return false
		}
		leftArtifacts[artifact.Architecture] = struct{}{}
		rightArtifact, ok := rightArtifacts[artifact.Architecture]
		if !ok || artifact != rightArtifact {
			return false
		}
	}
	return true
}

func sameHostAccount(left, right Host) bool {
	return left.Platform == right.Platform && left.Architecture == right.Architecture &&
		left.Hostname == right.Hostname && left.Username == right.Username
}
