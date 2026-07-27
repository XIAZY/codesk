//go:build regression

package regression

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type modelProfileRuntimeModel struct {
	Model                  string   `json:"model"`
	DisplayName            string   `json:"displayName"`
	IsDefault              bool     `json:"isDefault"`
	ReasoningEfforts       []string `json:"reasoningEfforts"`
	DefaultReasoningEffort string   `json:"defaultReasoningEffort"`
}

type modelProfileCatalog struct {
	Models []modelProfileRuntimeModel `json:"models"`
	Error  string                     `json:"error"`
}

type modelProfileRuntimeDetection struct {
	Kind         string               `json:"kind"`
	Available    bool                 `json:"available"`
	ModelCatalog *modelProfileCatalog `json:"modelCatalog"`
}

type modelProfileDaemon struct {
	ID       string                         `json:"id"`
	Runtimes []modelProfileRuntimeDetection `json:"runtimes"`
}

type modelProfileAgent struct {
	ID              string `json:"id"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	SessionID       string `json:"sessionId"`
	Status          string `json:"status"`
	CurrentActivity string `json:"currentActivity"`
}

type modelProfileFixtureEvent struct {
	ProcessCWD string                     `json:"processCwd"`
	Method     string                     `json:"method"`
	Params     map[string]json.RawMessage `json:"params"`
}

type modelProfileFixtureModel struct {
	Model                     string `json:"model"`
	DisplayName               string `json:"displayName"`
	IsDefault                 bool   `json:"isDefault"`
	DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

func TestModelProfileFlowsFromDaemonDetectionThroughServerToRuntime(t *testing.T) {
	stack := newRegressionStack(t)
	stack.daemonEnv = map[string]string{
		"NOTTY_CODEX_COMMAND":               "/opt/notty-test/fixtures/fake-codex.mjs",
		"NOTTY_TEST_MODEL_PROFILE_DELAY_MS": "3500",
	}
	stack.up(t)

	baseCatalog := loadModelProfileFixtureCatalog(t, stack.root)
	startupCatalog := stack.waitForModelProfileCatalog(t, 30*time.Second, func(catalog *modelProfileCatalog) bool {
		return catalog.Error == "" && len(catalog.Models) == 0
	})
	if startupCatalog == nil {
		t.Fatal("daemon startup did not publish an explicit empty pre-discovery catalog")
	}
	if events := stack.modelProfileFixtureEvents(t); len(events) != 0 {
		t.Fatalf("daemon startup touched the Codex app-server before publishing the empty catalog: %#v", events)
	}
	reported := stack.waitForModelProfileCatalog(t, 60*time.Second, func(catalog *modelProfileCatalog) bool {
		return catalogDefaultModel(catalog) == "gpt-5.6-sol" && catalogModel(catalog, "gpt-5.6-sol") != nil
	})
	if !reflect.DeepEqual(reported, baseCatalog) {
		t.Fatalf("server catalog differs from the detector fixture:\n got %#v\nwant %#v", reported, baseCatalog)
	}
	persisted := stack.postgresModelProfileCatalog(t)
	if !reflect.DeepEqual(persisted, baseCatalog) {
		t.Fatalf("Postgres catalog differs from the authenticated server read:\n got %#v\nwant %#v", persisted, baseCatalog)
	}

	explicit := stack.createModelProfileAgent(t, "profile-explicit", "gpt-5.6-sol", "ultra", http.StatusCreated)
	stack.assertPostgresAgentProfile(t, explicit.ID, "gpt-5.6-sol", "ultra")
	explicit = stack.waitForModelProfileAgent(t, explicit.ID, 60*time.Second, func(agent modelProfileAgent) bool {
		return agent.Status == "idle" && agent.SessionID != ""
	})
	explicitStart := stack.waitForModelProfileRuntimeRequest(t, explicit.ID, "thread/start", 60*time.Second)
	assertModelProfileRuntimeParams(t, explicitStart, true, "gpt-5.6-sol", "ultra")

	inherited := stack.createModelProfileAgent(t, "profile-inherited", "", "ultra", http.StatusCreated)
	stack.assertPostgresAgentProfile(t, inherited.ID, "", "ultra")
	inherited = stack.waitForModelProfileAgent(t, inherited.ID, 60*time.Second, func(agent modelProfileAgent) bool {
		return agent.Status == "idle" && agent.SessionID != ""
	})
	inheritedStart := stack.waitForModelProfileRuntimeRequest(t, inherited.ID, "thread/start", 60*time.Second)
	assertModelProfileRuntimeParams(t, inheritedStart, false, "", "ultra")

	localDefault := stack.createModelProfileAgent(t, "profile-local-default", "", "", http.StatusCreated)
	stack.assertPostgresAgentProfile(t, localDefault.ID, "", "")
	localDefault = stack.waitForModelProfileAgent(t, localDefault.ID, 60*time.Second, func(agent modelProfileAgent) bool {
		return agent.Status == "idle" && agent.SessionID != ""
	})
	localDefaultStart := stack.waitForModelProfileRuntimeRequest(t, localDefault.ID, "thread/start", 60*time.Second)
	assertModelProfileLocalDefaultRuntimeParams(t, localDefaultStart)

	beforeRejected := stack.modelProfileRuntimeRequests(t)
	stack.createModelProfileAgent(t, "profile-unknown", "gpt-5.7-missing", "", http.StatusBadRequest)
	stack.createModelProfileAgent(t, "profile-bad-effort", "gpt-5.5", "ultra", http.StatusBadRequest)
	if got := stack.postgresAgentCount(t); got != 3 {
		t.Fatalf("rejected creates persisted rows: got %d agents, want 3", got)
	}
	if after := stack.modelProfileRuntimeRequests(t); len(after) != len(beforeRejected) {
		t.Fatalf("rejected creates reached the runtime: before=%d after=%d", len(beforeRejected), len(after))
	}

	stack.setModelProfileCatalogMode(t, "vanished")
	beforeVanishedRestart := stack.modelProfileRuntimeRequests(t)
	stack.restartModelProfileDaemon(t)
	stack.waitForModelProfileCatalog(t, 30*time.Second, func(catalog *modelProfileCatalog) bool {
		return catalog.Error == "" && len(catalog.Models) == 0
	})
	localDefaultResume := stack.waitForModelProfileRuntimeRequestAfter(t, localDefault.ID, "thread/resume", len(beforeVanishedRestart), 60*time.Second)
	assertModelProfileLocalDefaultRuntimeParams(t, localDefaultResume)
	assertOnlyModelProfileRequestsForAgent(t, stack.modelProfileRuntimeRequests(t)[len(beforeVanishedRestart):], localDefault.ID)
	stack.waitForModelProfileCatalog(t, 60*time.Second, func(catalog *modelProfileCatalog) bool {
		return catalogDefaultModel(catalog) == "gpt-5.6-luna" && catalogModel(catalog, "gpt-5.6-sol") == nil
	})
	stack.waitForModelProfileAgent(t, explicit.ID, 60*time.Second, func(agent modelProfileAgent) bool {
		return agent.Status == "disconnected" && strings.Contains(agent.CurrentActivity, "is no longer available")
	})
	stack.waitForModelProfileAgent(t, inherited.ID, 60*time.Second, func(agent modelProfileAgent) bool {
		return agent.Status == "disconnected" && strings.Contains(agent.CurrentActivity, "runtime default is now")
	})
	afterVanished := stack.modelProfileRuntimeRequests(t)
	if got := afterVanished[len(beforeVanishedRestart):]; len(got) != 1 || !modelProfileEventBelongsToAgent(got[0], localDefault.ID) {
		t.Fatalf("vanished/default-drift profiles spawned a fallback runtime: %#v", got)
	}

	stack.setModelProfileCatalogMode(t, "default-moved")
	beforeDefaultMove := stack.modelProfileRuntimeRequests(t)
	stack.restartModelProfileDaemon(t)
	stack.waitForModelProfileCatalog(t, 30*time.Second, func(catalog *modelProfileCatalog) bool {
		return catalog.Error == "" && len(catalog.Models) == 0
	})
	localDefaultResume = stack.waitForModelProfileRuntimeRequestAfter(t, localDefault.ID, "thread/resume", len(beforeDefaultMove), 60*time.Second)
	assertModelProfileLocalDefaultRuntimeParams(t, localDefaultResume)
	assertOnlyModelProfileRequestsForAgent(t, stack.modelProfileRuntimeRequests(t)[len(beforeDefaultMove):], localDefault.ID)
	stack.waitForModelProfileCatalog(t, 60*time.Second, func(catalog *modelProfileCatalog) bool {
		return catalogDefaultModel(catalog) == "gpt-5.6-luna" && catalogModel(catalog, "gpt-5.6-sol") != nil
	})
	explicitResume := stack.waitForModelProfileRuntimeRequestAfter(t, explicit.ID, "thread/resume", len(beforeDefaultMove), 90*time.Second)
	assertModelProfileRuntimeParams(t, explicitResume, true, "gpt-5.6-sol", "ultra")
	stack.waitForModelProfileAgent(t, inherited.ID, 60*time.Second, func(agent modelProfileAgent) bool {
		return agent.Status == "disconnected" && strings.Contains(agent.CurrentActivity, "runtime default is now")
	})

	afterDefaultMove := stack.modelProfileRuntimeRequests(t)
	newRequests := afterDefaultMove[len(beforeDefaultMove):]
	if len(newRequests) != 2 ||
		!modelProfileEventBelongsToAgent(newRequests[0], localDefault.ID) ||
		!modelProfileEventBelongsToAgent(newRequests[1], explicit.ID) ||
		newRequests[0].Method != "thread/resume" ||
		newRequests[1].Method != "thread/resume" {
		t.Fatalf("default move must resume local-default before discovery and explicit after discovery, got %#v", newRequests)
	}
}

func loadModelProfileFixtureCatalog(t *testing.T, root string) *modelProfileCatalog {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "test", "regression", "fixtures", "model-profile-models.json"))
	if err != nil {
		t.Fatalf("read model fixture: %v", err)
	}
	var fixture []modelProfileFixtureModel
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode model fixture: %v", err)
	}
	catalog := &modelProfileCatalog{Models: make([]modelProfileRuntimeModel, 0, len(fixture))}
	for _, model := range fixture {
		projected := modelProfileRuntimeModel{
			Model:                  model.Model,
			DisplayName:            model.DisplayName,
			IsDefault:              model.IsDefault,
			DefaultReasoningEffort: model.DefaultReasoningEffort,
		}
		for _, effort := range model.SupportedReasoningEfforts {
			projected.ReasoningEfforts = append(projected.ReasoningEfforts, effort.ReasoningEffort)
		}
		catalog.Models = append(catalog.Models, projected)
	}
	return catalog
}

func (s *regressionStack) waitForModelProfileCatalog(t *testing.T, timeout time.Duration, accept func(*modelProfileCatalog) bool) *modelProfileCatalog {
	t.Helper()
	deadline := time.Now().Add(regressionScaledTimeout(t, timeout))
	var last string
	for time.Now().Before(deadline) {
		daemons, err := s.fetchModelProfileDaemons(t)
		if err == nil {
			for _, daemon := range daemons {
				if daemon.ID != s.daemonID {
					continue
				}
				for _, runtime := range daemon.Runtimes {
					if runtime.Kind == "codex" && runtime.Available && runtime.ModelCatalog != nil {
						last = fmt.Sprintf("%#v", runtime.ModelCatalog)
						if accept(runtime.ModelCatalog) {
							return runtime.ModelCatalog
						}
					}
				}
			}
		} else {
			last = err.Error()
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("daemon model catalog did not reach expected state; last=%s", last)
	return nil
}

func (s *regressionStack) fetchModelProfileDaemons(t *testing.T) ([]modelProfileDaemon, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.backendURL(t)+s.workspaceAPIPath("/daemons"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.authToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET daemons status %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Daemons []modelProfileDaemon `json:"daemons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Daemons, nil
}

func (s *regressionStack) postgresModelProfileCatalog(t *testing.T) *modelProfileCatalog {
	t.Helper()
	query := "SELECT runtime_detections::text FROM daemons WHERE id = " + sqlQuote(s.daemonID)
	raw := strings.TrimSpace(s.execService(t, "postgres", "psql -X -U notty -d notty -Atq -v ON_ERROR_STOP=1 -c "+shellQuote(query)))
	var detections []modelProfileRuntimeDetection
	if err := json.Unmarshal([]byte(raw), &detections); err != nil {
		t.Fatalf("decode persisted runtime detections %q: %v", raw, err)
	}
	for _, detection := range detections {
		if detection.Kind == "codex" {
			if detection.ModelCatalog == nil {
				t.Fatal("persisted codex detection dropped modelCatalog")
			}
			return detection.ModelCatalog
		}
	}
	t.Fatalf("persisted runtime detections have no codex entry: %#v", detections)
	return nil
}

func (s *regressionStack) createModelProfileAgent(t *testing.T, handle, model, effort string, wantStatus int) modelProfileAgent {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"handle":          handle,
		"name":            handle,
		"role":            "model profile regression",
		"kind":            "codex",
		"model":           model,
		"reasoningEffort": effort,
	})
	if err != nil {
		t.Fatalf("marshal create agent: %v", err)
	}
	path := s.workspaceAPIPath("/daemons/" + s.daemonID + "/agents")
	req, err := http.NewRequest(http.MethodPost, s.backendURL(t)+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new create agent request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.authToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read create agent response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("create agent %s status %d, want %d: %s", handle, resp.StatusCode, wantStatus, body)
	}
	if wantStatus != http.StatusCreated {
		return modelProfileAgent{}
	}
	var agent modelProfileAgent
	if err := json.Unmarshal(body, &agent); err != nil {
		t.Fatalf("decode create agent response: %v body=%s", err, body)
	}
	if agent.ID == "" || agent.Model != model || agent.ReasoningEffort != effort {
		t.Fatalf("create agent response dropped profile: %#v", agent)
	}
	return agent
}

func (s *regressionStack) assertPostgresAgentProfile(t *testing.T, agentID, model, effort string) {
	t.Helper()
	query := "SELECT model || '|' || reasoning_effort FROM agents WHERE id = " + sqlQuote(agentID)
	got := strings.TrimSpace(s.execService(t, "postgres", "psql -X -U notty -d notty -Atq -v ON_ERROR_STOP=1 -c "+shellQuote(query)))
	want := model + "|" + effort
	if got != want {
		t.Fatalf("persisted agent profile = %q, want %q", got, want)
	}
}

func (s *regressionStack) postgresAgentCount(t *testing.T) int {
	t.Helper()
	query := "SELECT COUNT(*) FROM agents WHERE workspace_id = " + sqlQuote(s.workspaceID)
	raw := strings.TrimSpace(s.execService(t, "postgres", "psql -X -U notty -d notty -Atq -v ON_ERROR_STOP=1 -c "+shellQuote(query)))
	var count int
	if _, err := fmt.Sscanf(raw, "%d", &count); err != nil {
		t.Fatalf("parse agent count %q: %v", raw, err)
	}
	return count
}

func (s *regressionStack) waitForModelProfileAgent(t *testing.T, agentID string, timeout time.Duration, accept func(modelProfileAgent) bool) modelProfileAgent {
	t.Helper()
	deadline := time.Now().Add(regressionScaledTimeout(t, timeout))
	var last modelProfileAgent
	for time.Now().Before(deadline) {
		var snapshot struct {
			Agents []modelProfileAgent `json:"agents"`
		}
		err := s.fetchModelProfileJSON(t, s.workspaceAPIPath("/workspace"), &snapshot)
		if err == nil {
			for _, agent := range snapshot.Agents {
				if agent.ID == agentID {
					last = agent
					if accept(agent) {
						return agent
					}
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("agent %s did not reach expected state; last=%#v", agentID, last)
	return modelProfileAgent{}
}

func (s *regressionStack) fetchModelProfileJSON(t *testing.T, path string, out any) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.backendURL(t)+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.authToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s status %d: %s", path, resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *regressionStack) modelProfileRuntimeRequests(t *testing.T) []modelProfileFixtureEvent {
	t.Helper()
	var events []modelProfileFixtureEvent
	for _, event := range s.modelProfileFixtureEvents(t) {
		if event.Method == "thread/start" || event.Method == "thread/resume" {
			events = append(events, event)
		}
	}
	return events
}

func (s *regressionStack) modelProfileFixtureEvents(t *testing.T) []modelProfileFixtureEvent {
	t.Helper()
	raw := s.execService(t, "daemon", "if [ -f /workspace/model-profile-fixture-events.jsonl ]; then cat /workspace/model-profile-fixture-events.jsonl; fi")
	var events []modelProfileFixtureEvent
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event modelProfileFixtureEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode fake Codex event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func (s *regressionStack) waitForModelProfileRuntimeRequest(t *testing.T, agentID, method string, timeout time.Duration) modelProfileFixtureEvent {
	t.Helper()
	deadline := time.Now().Add(regressionScaledTimeout(t, timeout))
	for time.Now().Before(deadline) {
		for _, event := range s.modelProfileRuntimeRequests(t) {
			if event.Method == method && modelProfileEventBelongsToAgent(event, agentID) {
				return event
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("agent %s did not issue %s to fake Codex", agentID, method)
	return modelProfileFixtureEvent{}
}

func (s *regressionStack) waitForModelProfileRuntimeRequestAfter(t *testing.T, agentID, method string, after int, timeout time.Duration) modelProfileFixtureEvent {
	t.Helper()
	deadline := time.Now().Add(regressionScaledTimeout(t, timeout))
	for time.Now().Before(deadline) {
		events := s.modelProfileRuntimeRequests(t)
		if after > len(events) {
			t.Fatalf("runtime request cursor %d exceeds %d events", after, len(events))
		}
		for _, event := range events[after:] {
			if event.Method == method && modelProfileEventBelongsToAgent(event, agentID) {
				return event
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("agent %s did not issue %s after request %d", agentID, method, after)
	return modelProfileFixtureEvent{}
}

func modelProfileEventBelongsToAgent(event modelProfileFixtureEvent, agentID string) bool {
	return filepath.Base(event.ProcessCWD) == agentID
}

func assertOnlyModelProfileRequestsForAgent(t *testing.T, events []modelProfileFixtureEvent, agentID string) {
	t.Helper()
	if len(events) != 1 || !modelProfileEventBelongsToAgent(events[0], agentID) {
		t.Fatalf("runtime requests = %#v, want exactly one for agent %s", events, agentID)
	}
}

func assertModelProfileRuntimeParams(t *testing.T, event modelProfileFixtureEvent, expectModel bool, model, effort string) {
	t.Helper()
	rawModel, hasModel := event.Params["model"]
	if hasModel != expectModel {
		t.Fatalf("%s model presence = %v, want %v params=%#v", event.Method, hasModel, expectModel, event.Params)
	}
	if expectModel {
		var got string
		if err := json.Unmarshal(rawModel, &got); err != nil || got != model {
			t.Fatalf("%s model = %q err=%v, want %q", event.Method, got, err, model)
		}
	}
	var config map[string]string
	if err := json.Unmarshal(event.Params["config"], &config); err != nil {
		t.Fatalf("%s config decode: %v params=%#v", event.Method, err, event.Params)
	}
	if got := config["model_reasoning_effort"]; got != effort {
		t.Fatalf("%s reasoning effort = %q, want %q", event.Method, got, effort)
	}
	if event.Method == "thread/start" {
		if _, exists := event.Params["threadId"]; exists {
			t.Fatalf("fresh start unexpectedly carried threadId: %#v", event.Params)
		}
		return
	}
	var threadID string
	if err := json.Unmarshal(event.Params["threadId"], &threadID); err != nil || threadID == "" {
		t.Fatalf("resume threadId = %q err=%v params=%#v", threadID, err, event.Params)
	}
	var excludeTurns bool
	if err := json.Unmarshal(event.Params["excludeTurns"], &excludeTurns); err != nil || !excludeTurns {
		t.Fatalf("resume excludeTurns = %v err=%v params=%#v", excludeTurns, err, event.Params)
	}
}

func assertModelProfileLocalDefaultRuntimeParams(t *testing.T, event modelProfileFixtureEvent) {
	t.Helper()
	if _, exists := event.Params["model"]; exists {
		t.Fatalf("%s local-default profile unexpectedly sent model: %#v", event.Method, event.Params)
	}
	if _, exists := event.Params["config"]; exists {
		t.Fatalf("%s local-default profile unexpectedly sent config: %#v", event.Method, event.Params)
	}
}

func (s *regressionStack) setModelProfileCatalogMode(t *testing.T, mode string) {
	t.Helper()
	s.execService(t, "daemon", "printf '%s\\n' "+shellQuote(mode)+" > /workspace/model-profile-catalog-mode")
}

func (s *regressionStack) restartModelProfileDaemon(t *testing.T) {
	t.Helper()
	s.run(t, "restart", "daemon")
}

func catalogDefaultModel(catalog *modelProfileCatalog) string {
	if catalog == nil {
		return ""
	}
	var found string
	for _, model := range catalog.Models {
		if !model.IsDefault {
			continue
		}
		if found != "" {
			return ""
		}
		found = model.Model
	}
	return found
}

func catalogModel(catalog *modelProfileCatalog, name string) *modelProfileRuntimeModel {
	if catalog == nil {
		return nil
	}
	for i := range catalog.Models {
		if catalog.Models[i].Model == name {
			return &catalog.Models[i]
		}
	}
	return nil
}
