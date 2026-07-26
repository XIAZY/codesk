package notty

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// standardModelCatalog mirrors the real Codex detector shape Deniz observed
// (gpt-5.6-sol / low as the single default) with a second model whose reasoning
// efforts differ, so effort validation is proven per-model rather than against a
// global list: "ultra" is valid for luna but not for the default sol.
func standardModelCatalog() *RuntimeModelCatalog {
	return &RuntimeModelCatalog{
		Models: []RuntimeModel{
			{
				Model:                  "gpt-5.6-sol",
				DisplayName:            "GPT-5.6 Sol",
				IsDefault:              true,
				ReasoningEfforts:       []string{"low", "medium", "high"},
				DefaultReasoningEffort: "low",
			},
			{
				Model:            "gpt-5.6-luna",
				DisplayName:      "GPT-5.6 Luna",
				IsDefault:        false,
				ReasoningEfforts: []string{"medium", "high", "ultra"},
			},
		},
	}
}

func daemonWithRuntime(kind string, catalog *RuntimeModelCatalog) *Daemon {
	return &Daemon{
		ID: "00000000-0000-0000-0000-000000000abc",
		Runtimes: []RuntimeDetection{
			{Kind: kind, Available: true, ModelCatalog: catalog},
		},
	}
}

// TestRuntimeDetectionDecodesModelCatalog is the anti-silent-drop guard: the
// mirror must decode the daemon's exact camelCase wire bytes into a fully
// populated catalog. encoding/json ignores unknown fields, so a single tag-casing
// mismatch would silently drop the field and leave a supported feature a no-op.
func TestRuntimeDetectionDecodesModelCatalog(t *testing.T) {
	wire := []byte(`{
		"kind": "codex",
		"available": true,
		"version": "codex 1.2.3",
		"modelCatalog": {
			"models": [
				{"model":"gpt-5.6-sol","displayName":"GPT-5.6 Sol","isDefault":true,"reasoningEfforts":["low","medium","high"],"defaultReasoningEffort":"low"},
				{"model":"gpt-5.6-luna","displayName":"GPT-5.6 Luna","isDefault":false,"reasoningEfforts":["medium","high","ultra"]}
			]
		}
	}`)

	var got RuntimeDetection
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("decode runtime detection: %v", err)
	}
	if got.ModelCatalog == nil {
		t.Fatal("modelCatalog silently dropped (nil after decode) — check json tag casing")
	}
	if len(got.ModelCatalog.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(got.ModelCatalog.Models))
	}
	sol := got.ModelCatalog.Models[0]
	if sol.Model != "gpt-5.6-sol" || sol.DisplayName != "GPT-5.6 Sol" || !sol.IsDefault {
		t.Fatalf("model/displayName/isDefault dropped or wrong: %#v", sol)
	}
	if sol.DefaultReasoningEffort != "low" {
		t.Fatalf("defaultReasoningEffort dropped or wrong: %q", sol.DefaultReasoningEffort)
	}
	if !reflect.DeepEqual(sol.ReasoningEfforts, []string{"low", "medium", "high"}) {
		t.Fatalf("reasoningEfforts dropped or wrong: %#v", sol.ReasoningEfforts)
	}
	if !reflect.DeepEqual(got.ModelCatalog.Models[1].ReasoningEfforts, []string{"medium", "high", "ultra"}) {
		t.Fatalf("second model reasoningEfforts dropped or wrong: %#v", got.ModelCatalog.Models[1].ReasoningEfforts)
	}
}

// TestRuntimeDetectionDecodesCatalogError proves the discovery-failed state
// (capable daemon whose probe failed) round-trips as a first-class value.
func TestRuntimeDetectionDecodesCatalogError(t *testing.T) {
	wire := []byte(`{"kind":"codex","available":true,"modelCatalog":{"models":[],"error":"probe timed out"}}`)
	var got RuntimeDetection
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ModelCatalog == nil {
		t.Fatal("modelCatalog dropped")
	}
	if got.ModelCatalog.Error != "probe timed out" {
		t.Fatalf("catalog error dropped or wrong: %q", got.ModelCatalog.Error)
	}
	if len(got.ModelCatalog.Models) != 0 {
		t.Fatalf("expected empty models, got %d", len(got.ModelCatalog.Models))
	}
}

// denizRealDaemonStatusArtifact is the byte-exact current-main RED-control that
// @Deniz captured from the real installed Codex detector and sent through a live
// backend at PATCH /daemon/status (task #3 thread, 2026-07-26). On current main
// the backend answered 200 but dropped modelCatalog entirely. This test proves
// the landed mirror decodes the same bytes populated through the real request
// type — the lane-local anti-silent-drop proof against real producer bytes.
// (Cross-lane producer/consumer compatibility remains task #5's live-stack gate.)
const denizRealDaemonStatusArtifact = `{"version":"qa-current-main","os":"linux","arch":"arm64","runtimes":[{"kind":"codex","available":true,"version":"WARNING: failed to clean up stale arg0 temp dirs: Permission denied (os error 13)\ncodex-cli 0.144.5","path":"/home/ubuntu/.nvm/versions/node/v22.22.3/bin/codex","modelCatalog":{"models":[{"model":"gpt-5.6-sol","displayName":"GPT-5.6-Sol","isDefault":true,"reasoningEfforts":["low","medium","high","xhigh","max","ultra"],"defaultReasoningEffort":"low"},{"model":"gpt-5.6-terra","displayName":"GPT-5.6-Terra","isDefault":false,"reasoningEfforts":["low","medium","high","xhigh","max","ultra"],"defaultReasoningEffort":"medium"},{"model":"gpt-5.6-luna","displayName":"GPT-5.6-Luna","isDefault":false,"reasoningEfforts":["low","medium","high","xhigh","max"],"defaultReasoningEffort":"medium"},{"model":"gpt-5.5","displayName":"GPT-5.5","isDefault":false,"reasoningEfforts":["low","medium","high","xhigh"],"defaultReasoningEffort":"medium"},{"model":"gpt-5.4","displayName":"GPT-5.4","isDefault":false,"reasoningEfforts":["low","medium","high","xhigh"],"defaultReasoningEffort":"medium"},{"model":"gpt-5.4-mini","displayName":"GPT-5.4-Mini","isDefault":false,"reasoningEfforts":["low","medium","high","xhigh"],"defaultReasoningEffort":"medium"},{"model":"gpt-5.3-codex-spark","displayName":"GPT-5.3-Codex-Spark","isDefault":false,"reasoningEfforts":["low","medium","high","xhigh"],"defaultReasoningEffort":"high"}]}}]}`

const denizRealArtifactSHA256 = "389516adcb0ae35357dff373b19ca06a2820945d6dc0fc2eba745c1de9e2c7c0"

func TestRealProducerArtifactDecodesModelCatalog(t *testing.T) {
	raw := []byte(denizRealDaemonStatusArtifact)
	if got := hex.EncodeToString(sha256Sum(raw)); got != denizRealArtifactSHA256 {
		t.Fatalf("embedded artifact drifted from Deniz's captured control bytes:\n got %s\nwant %s", got, denizRealArtifactSHA256)
	}

	var req UpdateDaemonStatusRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode real daemon status: %v", err)
	}
	if len(req.Runtimes) != 1 {
		t.Fatalf("expected 1 runtime, got %d", len(req.Runtimes))
	}
	catalog := req.Runtimes[0].ModelCatalog
	if catalog == nil {
		t.Fatal("modelCatalog silently dropped from the real producer payload — mirror casing regressed")
	}
	if len(catalog.Models) != 7 {
		t.Fatalf("expected 7 models from the real detector, got %d", len(catalog.Models))
	}

	var defaults []RuntimeModel
	for _, m := range catalog.Models {
		if m.IsDefault {
			defaults = append(defaults, m)
		}
	}
	if len(defaults) != 1 || defaults[0].Model != "gpt-5.6-sol" || defaults[0].DefaultReasoningEffort != "low" {
		t.Fatalf("expected exactly one default gpt-5.6-sol/low, got %#v", defaults)
	}
	// Per-model efforts must survive intact: sol carries "ultra", gpt-5.5 does not.
	if !reasoningEffortSupported(defaults[0].ReasoningEfforts, "ultra") {
		t.Fatalf("sol reasoningEfforts dropped ultra: %#v", defaults[0].ReasoningEfforts)
	}

	// The real catalog must drive the create-path rules identically to fixtures.
	daemon := &Daemon{ID: "real-artifact", Runtimes: req.Runtimes}
	kind, _ := normalizeAgentRuntimeKind("codex")
	if err := validateAgentModelProfile(daemon, kind, "gpt-5.6-sol", "ultra"); err != nil {
		t.Fatalf("real default model + supported effort should validate, got %v", err)
	}
	if err := validateAgentModelProfile(daemon, kind, "gpt-5.5", "ultra"); err == nil {
		t.Fatal("gpt-5.5 does not list ultra; expected per-model rejection")
	}
	if err := validateAgentModelProfile(daemon, kind, "", "low"); err != nil {
		t.Fatalf("partial inheritance against the real single default should validate, got %v", err)
	}
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func TestValidateAgentModelProfile(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		catalog *RuntimeModelCatalog
		model   string
		effort  string
		wantErr []string // substrings; empty => expect success
	}{
		// Inheritance is always allowed, independent of catalog presence.
		{name: "full inheritance with catalog", kind: "codex", catalog: standardModelCatalog()},
		{name: "full inheritance nil catalog (old daemon)", kind: "codex", catalog: nil},
		{name: "full inheritance empty catalog", kind: "codex", catalog: &RuntimeModelCatalog{}},
		{name: "full inheritance catalog error", kind: "codex", catalog: &RuntimeModelCatalog{Error: "probe timed out"}},

		// Explicit model (+/- effort).
		{name: "explicit model and effort valid", kind: "codex", catalog: standardModelCatalog(), model: "gpt-5.6-sol", effort: "high"},
		{name: "explicit model empty effort", kind: "codex", catalog: standardModelCatalog(), model: "gpt-5.6-luna"},
		{name: "explicit model effort valid per-model only", kind: "codex", catalog: standardModelCatalog(), model: "gpt-5.6-luna", effort: "ultra"},
		{name: "explicit model unknown", kind: "codex", catalog: standardModelCatalog(), model: "gpt-5.6-nope", wantErr: []string{"model \"gpt-5.6-nope\" is not available"}},
		{name: "explicit effort not for this model", kind: "codex", catalog: standardModelCatalog(), model: "gpt-5.6-sol", effort: "ultra", wantErr: []string{"reasoning effort \"ultra\" is not available for model \"gpt-5.6-sol\""}},

		// Partial inheritance: model="" + effort validated against single default,
		// persisted unchanged by the caller.
		{name: "partial inheritance effort valid for default", kind: "codex", catalog: standardModelCatalog(), effort: "medium"},
		{name: "partial inheritance effort invalid for default", kind: "codex", catalog: standardModelCatalog(), effort: "ultra", wantErr: []string{"reasoning effort \"ultra\" is not available for the default model"}},

		// Three provider-agnostic catalog-state errors on any explicit choice.
		{name: "explicit against nil catalog", kind: "codex", catalog: nil, model: "gpt-5.6-sol", wantErr: []string{"has not reported a model catalog"}},
		{name: "explicit against catalog error", kind: "codex", catalog: &RuntimeModelCatalog{Error: "probe timed out"}, model: "gpt-5.6-sol", wantErr: []string{"model catalog discovery failed"}},
		{name: "explicit against empty catalog", kind: "codex", catalog: &RuntimeModelCatalog{Models: []RuntimeModel{}}, effort: "medium", wantErr: []string{"no models available"}},

		// Ambiguous / missing default only matters for partial inheritance.
		{
			name: "partial inheritance ambiguous default", kind: "codex",
			catalog: &RuntimeModelCatalog{Models: []RuntimeModel{
				{Model: "a", IsDefault: true, ReasoningEfforts: []string{"low"}},
				{Model: "b", IsDefault: true, ReasoningEfforts: []string{"low"}},
			}},
			effort: "low", wantErr: []string{"more than one default model"},
		},
		{
			name: "partial inheritance no default", kind: "codex",
			catalog: &RuntimeModelCatalog{Models: []RuntimeModel{
				{Model: "a", ReasoningEfforts: []string{"low"}},
			}},
			effort: "low", wantErr: []string{"no default model"},
		},

		// Capability-over-provider: a non-codex runtime with a catalog validates
		// identically (proves selection is by kind, not a hardcoded "codex").
		{name: "generic runtime valid", kind: "claude-code", catalog: standardModelCatalog(), model: "gpt-5.6-sol", effort: "high"},
		{name: "generic runtime invalid model", kind: "claude-code", catalog: standardModelCatalog(), model: "gpt-5.6-nope", wantErr: []string{"is not available"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := daemonWithRuntime(tc.kind, tc.catalog)
			kind, err := normalizeAgentRuntimeKind(tc.kind)
			if err != nil {
				t.Fatalf("normalize kind: %v", err)
			}
			err = validateAgentModelProfile(daemon, kind, tc.model, tc.effort)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %v, got nil", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing substring %q", err.Error(), want)
				}
			}
		})
	}
}

// TestModelProfileErrorsAreProviderAgnostic locks the three catalog-state error
// strings so they never leak a provider/kind name — capability is catalog
// presence, not a provider switch.
func TestModelProfileErrorsAreProviderAgnostic(t *testing.T) {
	kind, _ := normalizeAgentRuntimeKind("codex")
	checks := []struct {
		name    string
		catalog *RuntimeModelCatalog
	}{
		{"absent", nil},
		{"error", &RuntimeModelCatalog{Error: "boom"}},
		{"empty", &RuntimeModelCatalog{Models: []RuntimeModel{}}},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			err := validateAgentModelProfile(daemonWithRuntime("codex", c.catalog), kind, "gpt-5.6-sol", "high")
			if err == nil {
				t.Fatal("expected error")
			}
			for _, leak := range []string{"codex", "claude", "gpt-5.6-sol"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("catalog-state error leaked provider/model token %q: %q", leak, err.Error())
				}
			}
		})
	}
}

// TestPostgresCreateAgentModelProfile exercises the full column plumbing end to
// end: the catalog is marshaled into the daemon's runtime_detections, read back
// inside CreateAgent, validated, and the persisted model/effort round-trips
// through the INSERT and scan paths. A passing valid-model case is itself the
// end-to-end anti-silent-drop proof — if the catalog were dropped on the DB
// round-trip, the valid model would be rejected as unavailable.
func TestPostgresCreateAgentModelProfile(t *testing.T) {
	cases := []struct {
		name       string
		runtimes   []RuntimeDetection
		model      string
		effort     string
		wantErr    []string
		wantModel  string
		wantEffort string
	}{
		{
			name:       "explicit model and effort persist",
			runtimes:   []RuntimeDetection{{Kind: "codex", Available: true, ModelCatalog: standardModelCatalog()}},
			model:      "gpt-5.6-luna",
			effort:     "ultra",
			wantModel:  "gpt-5.6-luna",
			wantEffort: "ultra",
		},
		{
			name:       "partial inheritance persists unchanged",
			runtimes:   []RuntimeDetection{{Kind: "codex", Available: true, ModelCatalog: standardModelCatalog()}},
			model:      "",
			effort:     "medium",
			wantModel:  "",
			wantEffort: "medium",
		},
		{
			name:     "full inheritance persists empty",
			runtimes: []RuntimeDetection{{Kind: "codex", Available: true, ModelCatalog: standardModelCatalog()}},
		},
		{
			name:     "full inheritance survives old daemon with no catalog",
			runtimes: []RuntimeDetection{{Kind: "codex", Available: true}},
		},
		{
			name:     "invalid model rejected without persisting",
			runtimes: []RuntimeDetection{{Kind: "codex", Available: true, ModelCatalog: standardModelCatalog()}},
			model:    "gpt-5.6-nope",
			wantErr:  []string{"is not available"},
		},
		{
			name:     "explicit choice against old daemon rejected",
			runtimes: []RuntimeDetection{{Kind: "codex", Available: true}},
			model:    "gpt-5.6-sol",
			wantErr:  []string{"has not reported a model catalog"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newPostgresTestDatabase(t)
			db := database.DB
			store := newPostgresTestWorkspaceStore(t, database)
			daemonID := seedStoreDaemonRuntime(t, store, "daemon_model_test", tc.runtimes...)

			agent, err := store.CreateAgent(CreateAgentRequest{
				DaemonID:        daemonID,
				Handle:          "model-agent",
				Name:            "Model Agent",
				Role:            "Exercises model profile validation",
				Kind:            "codex",
				Model:           tc.model,
				ReasoningEffort: tc.effort,
			}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})

			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected error, got agent %#v", agent)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q missing %q", err.Error(), want)
					}
				}
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM agents WHERE workspace_id = $1`, store.workspaceID).Scan(&count); err != nil {
					t.Fatalf("count agents: %v", err)
				}
				if count != 0 {
					t.Fatalf("failed create must not persist rows, got %d", count)
				}
				return
			}

			if err != nil {
				t.Fatalf("create agent: %v", err)
			}
			if agent.Model != tc.wantModel || agent.ReasoningEffort != tc.wantEffort {
				t.Fatalf("in-memory agent model/effort = %q/%q, want %q/%q", agent.Model, agent.ReasoningEffort, tc.wantModel, tc.wantEffort)
			}
			// Round-trip through the scan path to prove the columns persist.
			reloaded, err := getAgentPostgres(store.db, store.workspaceID, agent.ID)
			if err != nil {
				t.Fatalf("reload agent: %v", err)
			}
			if reloaded.Model != tc.wantModel || reloaded.ReasoningEffort != tc.wantEffort {
				t.Fatalf("reloaded model/effort = %q/%q, want %q/%q", reloaded.Model, reloaded.ReasoningEffort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// TestAgentModelSurfaceIsCreateOnly guards the contract that model selection is a
// create-time surface only: CreateAgentRequest carries the fields; the update and
// run request surfaces must not, so a model can never be silently mutated post-create.
func TestAgentModelSurfaceIsCreateOnly(t *testing.T) {
	hasField := func(v any, name string) bool {
		_, ok := reflect.TypeOf(v).FieldByName(name)
		return ok
	}
	for _, name := range []string{"Model", "ReasoningEffort"} {
		if !hasField(CreateAgentRequest{}, name) {
			t.Fatalf("CreateAgentRequest must expose %s", name)
		}
		if hasField(UpdateAgentRequest{}, name) {
			t.Fatalf("UpdateAgentRequest must not expose %s (create-only surface)", name)
		}
		if hasField(AgentRun{}, name) {
			t.Fatalf("AgentRun must not expose %s (create-only surface)", name)
		}
	}
}
