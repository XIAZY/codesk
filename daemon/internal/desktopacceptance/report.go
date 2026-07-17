package desktopacceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const reportSchemaVersion = 1

type Row struct {
	Name       string    `json:"name"`
	Status     Status    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Detail     string    `json:"detail"`
}

type Report struct {
	SchemaVersion        int                    `json:"schema_version"`
	RunID                string                 `json:"run_id"`
	Phase                Phase                  `json:"phase"`
	SourceRevision       string                 `json:"source_revision"`
	StartedAt            time.Time              `json:"started_at"`
	FinishedAt           time.Time              `json:"finished_at,omitempty"`
	Verdict              Status                 `json:"verdict"`
	Publishable          bool                   `json:"publishable"`
	Host                 Host                   `json:"host"`
	RunnerPath           string                 `json:"runner_path"`
	RunnerSHA256         string                 `json:"runner_sha256"`
	RunnerSourceRevision string                 `json:"runner_source_revision"`
	CheckpointSHA256     string                 `json:"checkpoint_sha256,omitempty"`
	TranscriptSHA256     string                 `json:"transcript_sha256,omitempty"`
	Candidate            Release                `json:"candidate"`
	Previous             *Release               `json:"previous,omitempty"`
	LegacyCLIState       LegacyStateFingerprint `json:"legacy_cli_state_baseline"`
	Rows                 []Row                  `json:"rows"`
	FinalState           State                  `json:"final_state"`
	SurfaceEvents        []SurfaceEvent         `json:"surface_events,omitempty"`
}

type recorder struct {
	mu         sync.Mutex
	directory  string
	reportPath string
	logPath    string
	report     Report
}

func newRecorder(config Config) (*recorder, error) {
	if strings.TrimSpace(config.EvidenceDir) == "" {
		return nil, errors.New("evidence directory is required")
	}
	directory, err := filepath.Abs(config.EvidenceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence directory: %w", err)
	}
	parent := filepath.Dir(directory)
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return nil, errors.New("evidence parent directory must already exist")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create new evidence directory: %w", err)
	}
	runnerPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve acceptance runner: %w", err)
	}
	runnerPath, err = filepath.Abs(runnerPath)
	if err != nil {
		return nil, fmt.Errorf("resolve acceptance runner: %w", err)
	}
	runnerHash, err := hashFile(runnerPath)
	if err != nil {
		return nil, fmt.Errorf("hash acceptance runner: %w", err)
	}
	now := time.Now().UTC()
	recorder := &recorder{
		directory:  directory,
		reportPath: filepath.Join(directory, "report.json"),
		logPath:    filepath.Join(directory, "transcript.log"),
		report: Report{
			SchemaVersion:        reportSchemaVersion,
			RunID:                fmt.Sprintf("%s-%s-%d", config.TargetPlatform, now.Format("20060102T150405Z"), os.Getpid()),
			Phase:                config.Phase,
			SourceRevision:       config.SourceRevision,
			StartedAt:            now,
			Verdict:              StatusBlocked,
			RunnerPath:           runnerPath,
			RunnerSHA256:         runnerHash,
			RunnerSourceRevision: config.RunnerSourceRevision,
		},
	}
	if err := recorder.persist(); err != nil {
		return nil, err
	}
	return recorder, nil
}

func openRecorder(config Config) (*recorder, error) {
	if config.Phase == PhaseResume {
		return resumeRecorder(config)
	}
	return newRecorder(config)
}

func resumeRecorder(config Config) (*recorder, error) {
	if strings.TrimSpace(config.EvidenceDir) == "" {
		return nil, errors.New("evidence directory is required")
	}
	directory, err := filepath.Abs(config.EvidenceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect prepared evidence directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("prepared evidence path is not a real directory")
	}
	recorder := &recorder{
		directory:  directory,
		reportPath: filepath.Join(directory, "report.json"),
		logPath:    filepath.Join(directory, "transcript.log"),
	}
	data, err := readBoundedFile(recorder.reportPath, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("read prepared acceptance report: %w", err)
	}
	if err := decodeCanonicalJSON(data, &recorder.report, "prepared acceptance report"); err != nil {
		return nil, err
	}
	if recorder.report.SchemaVersion != reportSchemaVersion || recorder.report.Phase != PhasePrepare ||
		recorder.report.Verdict != StatusPending || recorder.report.Publishable {
		return nil, errors.New("evidence directory does not contain a resumable prepare report")
	}
	if recorder.report.SourceRevision != config.SourceRevision || recorder.report.RunnerSourceRevision != config.RunnerSourceRevision {
		return nil, errors.New("prepared report source identity does not match the resume runner")
	}
	if recorder.report.Host.Platform != config.TargetPlatform || recorder.report.RunID == "" ||
		!validSHA256(recorder.report.RunnerSHA256) || !validSHA256(recorder.report.CheckpointSHA256) ||
		!validSHA256(recorder.report.TranscriptSHA256) {
		return nil, errors.New("prepared report platform or checkpoint identity does not match resume")
	}
	runnerPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve resume runner: %w", err)
	}
	runnerHash, err := hashFile(runnerPath)
	if err != nil {
		return nil, fmt.Errorf("hash resume runner: %w", err)
	}
	if runnerHash != recorder.report.RunnerSHA256 {
		return nil, errors.New("resume runner bytes do not match the prepare runner")
	}
	logInfo, err := os.Lstat(recorder.logPath)
	if err != nil || !logInfo.Mode().IsRegular() || logInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("prepared transcript is absent or not a regular file")
	}
	transcriptHash, err := hashFile(recorder.logPath)
	if err != nil {
		return nil, fmt.Errorf("hash prepared transcript: %w", err)
	}
	if transcriptHash != recorder.report.TranscriptSHA256 {
		return nil, errors.New("prepared transcript hash does not match the prepare report")
	}
	return recorder, nil
}

func (r *recorder) beginResume(host Host) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.report.Phase = PhaseResume
	r.report.Host = host
	r.report.FinishedAt = time.Time{}
	r.report.Verdict = StatusBlocked
	r.report.Publishable = false
	r.report.TranscriptSHA256 = ""
	r.report.FinalState = State{}
	r.report.SurfaceEvents = nil
	return r.persistLocked()
}

func (r *recorder) row(name string, status Status, started time.Time, detail string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	detail = redact(detail)
	row := Row{Name: name, Status: status, StartedAt: started.UTC(), FinishedAt: time.Now().UTC(), Detail: detail}
	r.report.Rows = append(r.report.Rows, row)
	line := fmt.Sprintf("%s %s %s: %s\n", row.FinishedAt.Format(time.RFC3339Nano), status, name, detail)
	file, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open acceptance transcript: %w", err)
	}
	_, writeErr := file.WriteString(line)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	return r.persistLocked()
}

func (r *recorder) persist() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persistLocked()
}

func (r *recorder) persistLocked() error {
	data, err := json.MarshalIndent(r.report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode acceptance report: %w", err)
	}
	data = append(data, '\n')
	if leaks := secretMatches(string(data)); len(leaks) != 0 {
		return errors.New("acceptance report contains a credential-shaped value")
	}
	temporary := r.reportPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write acceptance report: %w", err)
	}
	if err := replaceFile(temporary, r.reportPath); err != nil {
		return fmt.Errorf("publish acceptance report: %w", err)
	}
	return nil
}

func (r *recorder) finish(status Status, state State, events []SurfaceEvent, publishable bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.report.Verdict = status
	r.report.Publishable = publishable
	r.report.FinishedAt = time.Now().UTC()
	r.report.FinalState = state
	r.report.SurfaceEvents = append([]SurfaceEvent(nil), events...)
	for index := range r.report.SurfaceEvents {
		r.report.SurfaceEvents[index].Executable = redact(r.report.SurfaceEvents[index].Executable)
		r.report.SurfaceEvents[index].Class = redact(r.report.SurfaceEvents[index].Class)
	}
	transcriptHash, err := hashFile(r.logPath)
	if err != nil {
		return fmt.Errorf("hash acceptance transcript: %w", err)
	}
	r.report.TranscriptSHA256 = transcriptHash
	return r.persistLocked()
}

func (r *recorder) markFailedInMemory() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.report.Verdict = StatusFail
	r.report.Publishable = false
}

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{12,}`),
	regexp.MustCompile(`\b(?:sk_agent|sk_machine|nottyd)_[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
}

func redact(value string) string {
	for _, pattern := range credentialPatterns {
		value = pattern.ReplaceAllString(value, "<redacted>")
	}
	return value
}

func secretMatches(value string) []string {
	var matches []string
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(value) {
			matches = append(matches, pattern.String())
		}
	}
	return matches
}
