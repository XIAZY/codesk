//go:build regression

package regression

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLocalDeleteReappearanceRestoresSameDocumentBeforeReverseDeadline(t *testing.T) {
	runLocalDeleteReappearanceRegression(t, false)
}

func TestLocalDeleteReappearanceRestoresSameDocumentAfterDaemonRestart(t *testing.T) {
	runLocalDeleteReappearanceRegression(t, true)
}

func runLocalDeleteReappearanceRegression(t *testing.T, restartDaemon bool) {
	t.Helper()
	stack := newRegressionStack(t)
	stack.up(t)

	path := uniquePath("reverse-window-restore", ".md")
	initial := "original content before the local delete\n"
	reappeared := "content restored under the original document identity\n"
	stack.writeLocalFile(t, path, initial)
	stack.waitForBackendContentByPath(t, path, initial, 90*time.Second)
	documentID, err := stack.documentIDForRootPath(path)
	if err != nil {
		t.Fatalf("lookup original document id: %v", err)
	}
	stack.waitForRootEntry(t, documentID, path, false, 30*time.Second)
	documentCount := stack.postgresWorkspaceDocumentCount(t)

	stack.removeLocalFile(t, path)
	stack.waitForRootEntry(t, documentID, path, true, 90*time.Second)
	stack.assertLocalMissing(t, path, 30*time.Second)
	opened := stack.waitForReverseWindowState(t, documentID, 30*time.Second, func(state regressionReverseWindowState) bool {
		return state.Found && state.Live && !state.Consumed && state.WindowGeneration > 0
	})
	if opened.OriginDaemonID != stack.daemonID {
		t.Fatalf("reverse-window origin daemon = %q, want %q", opened.OriginDaemonID, stack.daemonID)
	}
	if opened.DesiredPath != normalizeRegressionRootPath(path) {
		t.Fatalf("reverse-window path = %q, want %q", opened.DesiredPath, normalizeRegressionRootPath(path))
	}

	if restartDaemon {
		stack.restartDaemonAndWait(t)
		reopened := stack.waitForReverseWindowState(t, documentID, 30*time.Second, func(state regressionReverseWindowState) bool {
			return state.Found && state.Live && !state.Consumed
		})
		if reopened.WindowGeneration != opened.WindowGeneration || reopened.TombstoneOperationID != opened.TombstoneOperationID {
			t.Fatalf("daemon restart changed the durable reverse-window identity: before=%#v after=%#v", opened, reopened)
		}
	}

	stillOpen, err := stack.reverseWindowState(documentID)
	if err != nil {
		t.Fatalf("read reverse window immediately before reappearance: %v", err)
	}
	if !stillOpen.Found || !stillOpen.Live || stillOpen.Consumed {
		t.Fatalf("file would not reappear inside a live unconsumed window: %#v", stillOpen)
	}
	stack.writeLocalFile(t, path, reappeared)

	stack.waitForRootEntry(t, documentID, path, false, 120*time.Second)
	stack.waitForBackendContent(t, documentID, reappeared, 120*time.Second)
	stack.waitForLocalContent(t, path, reappeared, 60*time.Second)
	consumed := stack.waitForReverseWindowState(t, documentID, 30*time.Second, func(state regressionReverseWindowState) bool {
		return state.Found && state.Consumed && state.ConsumedBeforeDeadline && state.RestoreOperationID != "" && state.RestoreUpdateID > 0
	})
	if consumed.WindowGeneration != opened.WindowGeneration || consumed.TombstoneOperationID != opened.TombstoneOperationID {
		t.Fatalf("consumed reverse-window identity changed: opened=%#v consumed=%#v", opened, consumed)
	}
	if consumed.RestoreUpdateID <= consumed.TombstoneUpdateID {
		t.Fatalf("restore update id = %d, want greater than tombstone update id %d", consumed.RestoreUpdateID, consumed.TombstoneUpdateID)
	}

	restoredDocumentID, err := stack.documentIDForRootPath(path)
	if err != nil {
		t.Fatalf("lookup restored document id: %v", err)
	}
	if restoredDocumentID != documentID {
		t.Fatalf("restored path document id = %q, want original %q", restoredDocumentID, documentID)
	}
	stack.assertOnlyRootEntryForPath(t, path, documentID)
	if got := stack.postgresWorkspaceDocumentCount(t); got != documentCount {
		t.Fatalf("workspace document count after restore = %d, want unchanged %d", got, documentCount)
	}
}

type regressionReverseWindowState struct {
	Found                  bool
	WindowGeneration       int64
	TombstoneOperationID   string
	RestoreOperationID     string
	TombstoneUpdateID      int64
	RestoreUpdateID        int64
	Live                   bool
	Consumed               bool
	ConsumedBeforeDeadline bool
	OriginDaemonID         string
	DesiredPath            string
}

func (s *regressionStack) waitForReverseWindowState(
	t *testing.T,
	documentID string,
	timeout time.Duration,
	accept func(regressionReverseWindowState) bool,
) regressionReverseWindowState {
	t.Helper()
	deadline := time.Now().Add(regressionScaledTimeout(t, timeout))
	var last regressionReverseWindowState
	var lastErr error
	for time.Now().Before(deadline) {
		state, err := s.reverseWindowState(documentID)
		if err == nil {
			last = state
			if accept(state) {
				return state
			}
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("reverse window for %s did not reach the expected state: last=%#v err=%v logs=%q", documentID, last, lastErr, s.daemonLogs(t))
	return regressionReverseWindowState{}
}

func (s *regressionStack) reverseWindowState(documentID string) (regressionReverseWindowState, error) {
	query := fmt.Sprintf(`SELECT
		window_generation::text || chr(9) ||
		tombstone_operation_id::text || chr(9) ||
		COALESCE(restore_operation_id::text, '') || chr(9) ||
		tombstone_update_id::text || chr(9) ||
		COALESCE(restore_update_id::text, '') || chr(9) ||
		(reverse_until > now())::text || chr(9) ||
		(consumed_at IS NOT NULL)::text || chr(9) ||
		COALESCE((consumed_at < reverse_until)::text, '') || chr(9) ||
		origin_daemon_id::text || chr(9) ||
		desired_path
	FROM document_reverse_windows
	WHERE workspace_id=%s AND document_id=%s`, sqlQuote(s.workspaceID), sqlQuote(documentID))
	ctx, cancel := context.WithTimeout(context.Background(), scaleRegressionTimeout(2*time.Minute, s.deadlineScale))
	defer cancel()
	outputBytes, err := s.command(ctx, "exec", "-T", "postgres", "psql", "-U", "notty", "-d", "notty", "-At", "-c", query).CombinedOutput()
	if err != nil {
		return regressionReverseWindowState{}, fmt.Errorf("query reverse window: %w: %s", err, strings.TrimSpace(string(outputBytes)))
	}
	output := strings.TrimSpace(string(outputBytes))
	if output == "" {
		return regressionReverseWindowState{}, nil
	}
	fields := strings.Split(output, "\t")
	if len(fields) != 10 {
		return regressionReverseWindowState{}, fmt.Errorf("bad reverse-window row %q", output)
	}
	parseInt := func(name, value string, optional bool) (int64, error) {
		if optional && strings.TrimSpace(value) == "" {
			return 0, nil
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %s %q: %w", name, value, err)
		}
		return parsed, nil
	}
	generation, err := parseInt("window generation", fields[0], false)
	if err != nil {
		return regressionReverseWindowState{}, err
	}
	tombstoneUpdateID, err := parseInt("tombstone update id", fields[3], false)
	if err != nil {
		return regressionReverseWindowState{}, err
	}
	restoreUpdateID, err := parseInt("restore update id", fields[4], true)
	if err != nil {
		return regressionReverseWindowState{}, err
	}
	parseBool := func(name, value string, optional bool) (bool, error) {
		if optional && strings.TrimSpace(value) == "" {
			return false, nil
		}
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return false, fmt.Errorf("parse %s %q: %w", name, value, err)
		}
		return parsed, nil
	}
	live, err := parseBool("live", fields[5], false)
	if err != nil {
		return regressionReverseWindowState{}, err
	}
	consumed, err := parseBool("consumed", fields[6], false)
	if err != nil {
		return regressionReverseWindowState{}, err
	}
	consumedBeforeDeadline, err := parseBool("consumed before deadline", fields[7], true)
	if err != nil {
		return regressionReverseWindowState{}, err
	}
	return regressionReverseWindowState{
		Found:                  true,
		WindowGeneration:       generation,
		TombstoneOperationID:   fields[1],
		RestoreOperationID:     fields[2],
		TombstoneUpdateID:      tombstoneUpdateID,
		RestoreUpdateID:        restoreUpdateID,
		Live:                   live,
		Consumed:               consumed,
		ConsumedBeforeDeadline: consumedBeforeDeadline,
		OriginDaemonID:         fields[8],
		DesiredPath:            normalizeRegressionRootPath(fields[9]),
	}, nil
}

func (s *regressionStack) postgresWorkspaceDocumentCount(t *testing.T) int {
	t.Helper()
	query := fmt.Sprintf("SELECT COUNT(*) FROM documents WHERE workspace_id=%s", sqlQuote(s.workspaceID))
	count, err := strconv.Atoi(strings.TrimSpace(s.psql(query)))
	if err != nil {
		t.Fatalf("parse workspace document count: %v", err)
	}
	return count
}

func (s *regressionStack) assertOnlyRootEntryForPath(t *testing.T, path, documentID string) {
	t.Helper()
	entries, err := s.databaseRootEntries(s.workspaceRootDocumentIDForTest(t))
	if err != nil {
		t.Fatalf("decode root entries: %v", err)
	}
	normalized := normalizeRegressionRootPath(path)
	matching := make([]string, 0)
	for id, entry := range entries {
		if entry.Path == normalized {
			matching = append(matching, id+":"+strconv.FormatBool(entry.Deleted))
		}
	}
	sort.Strings(matching)
	want := []string{documentID + ":false"}
	if strings.Join(matching, ",") != strings.Join(want, ",") {
		t.Fatalf("root entries for %q = %v, want %v", normalized, matching, want)
	}
}

func (s *regressionStack) restartDaemonAndWait(t *testing.T) {
	t.Helper()
	const startupMarker = "notty daemon syncing "
	before := strings.Count(s.runOutput(t, "logs", "--no-color", "daemon"), startupMarker)
	s.run(t, "restart", "daemon")
	deadline := time.Now().Add(regressionScaledTimeout(t, 30*time.Second))
	var logs string
	for time.Now().Before(deadline) {
		logs = s.runOutput(t, "logs", "--no-color", "daemon")
		if strings.Count(logs, startupMarker) > before {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("restarted daemon did not emit a fresh startup marker: before=%d logs=%q", before, logs)
}
