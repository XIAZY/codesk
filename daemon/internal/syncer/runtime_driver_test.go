package syncer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type retryCatalogDriver struct {
	mu       sync.Mutex
	catalogs []*RuntimeModelCatalog
	calls    int
	called   chan struct{}
}

func (d *retryCatalogDriver) Kind() RuntimeKind {
	return RuntimeCodex
}

func (d *retryCatalogDriver) Detect(context.Context) RuntimeDetection {
	return RuntimeDetection{Kind: RuntimeCodex, Available: true}
}

func (d *retryCatalogDriver) Spawn(context.Context, RuntimeSpawnSpec) (RuntimeProcess, error) {
	return nil, nil
}

func (d *retryCatalogDriver) detectModelCatalog(context.Context) *RuntimeModelCatalog {
	d.mu.Lock()
	index := d.calls
	d.calls++
	catalog := d.catalogs[len(d.catalogs)-1]
	if index < len(d.catalogs) {
		catalog = d.catalogs[index]
	}
	d.mu.Unlock()
	if d.called != nil {
		d.called <- struct{}{}
	}
	return catalog
}

func (d *retryCatalogDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func TestRuntimeRegistryFailedCatalogRecoveryBacksOffUntilSuccess(t *testing.T) {
	recoveredCatalog := &RuntimeModelCatalog{Models: []RuntimeModel{{
		Model:                  "gpt-5.6-sol",
		DisplayName:            "GPT-5.6-Sol",
		IsDefault:              true,
		ReasoningEfforts:       []string{"low"},
		DefaultReasoningEffort: "low",
	}}}
	driver := &retryCatalogDriver{
		catalogs: []*RuntimeModelCatalog{
			{Models: []RuntimeModel{}, Error: "model catalog unavailable"},
			{Models: []RuntimeModel{}, Error: "model catalog unavailable"},
			recoveredCatalog,
		},
	}
	registry := newRuntimeRegistry(driver)
	registry.detections[RuntimeCodex] = RuntimeDetection{
		Kind:         RuntimeCodex,
		Available:    true,
		Version:      "codex-cli 1.0.0",
		Path:         "/usr/bin/codex",
		ModelCatalog: &RuntimeModelCatalog{Models: []RuntimeModel{}, Error: "model catalog unavailable"},
	}

	now := time.Unix(1_700_000_000, 0)
	registry.refreshFailedModelCatalogs(context.Background(), now)
	registry.refreshFailedModelCatalogs(context.Background(), now.Add(10*time.Second))
	registry.refreshFailedModelCatalogs(context.Background(), now.Add(failedCatalogRetryInterval-time.Nanosecond))
	if got := driver.callCount(); got != 1 {
		t.Fatalf("persistent catalog retries before cooldown = %d, want 1", got)
	}
	registry.refreshFailedModelCatalogs(context.Background(), now.Add(failedCatalogRetryInterval))
	registry.refreshFailedModelCatalogs(context.Background(), now.Add(failedCatalogRetryInterval+10*time.Second))
	if got := driver.callCount(); got != 2 {
		t.Fatalf("persistent catalog retries after cooldown = %d, want 2", got)
	}
	registry.refreshFailedModelCatalogs(context.Background(), now.Add(2*failedCatalogRetryInterval-time.Nanosecond))
	if got := driver.callCount(); got != 2 {
		t.Fatalf("persistent catalog retries before second cooldown = %d, want 2", got)
	}
	registry.refreshFailedModelCatalogs(context.Background(), now.Add(2*failedCatalogRetryInterval))
	registry.refreshFailedModelCatalogs(context.Background(), now.Add(3*failedCatalogRetryInterval))
	if got := driver.callCount(); got != 3 {
		t.Fatalf("catalog retries after recovery = %d, want 3", got)
	}
	detection := registry.cachedDetections()[0]
	if !detection.Available || detection.Version != "codex-cli 1.0.0" || detection.Path != "/usr/bin/codex" {
		t.Fatalf("catalog retry changed runtime availability metadata: %#v", detection)
	}
	if detection.ModelCatalog == nil || detection.ModelCatalog.Error != "" || len(detection.ModelCatalog.Models) != 1 {
		t.Fatalf("catalog did not recover after bounded retries: %#v", detection.ModelCatalog)
	}
}

func TestDaemonStatusHeartbeatRecoversFailedCatalogOnce(t *testing.T) {
	driver := &retryCatalogDriver{
		catalogs: []*RuntimeModelCatalog{
			{Models: []RuntimeModel{{
				Model:                  "gpt-5.6-sol",
				DisplayName:            "GPT-5.6-Sol",
				IsDefault:              true,
				ReasoningEfforts:       []string{"low"},
				DefaultReasoningEffort: "low",
			}}},
		},
		called: make(chan struct{}, 1),
	}
	registry := newRuntimeRegistry(driver)
	registry.detections[RuntimeCodex] = RuntimeDetection{
		Kind:         RuntimeCodex,
		Available:    true,
		Version:      "codex-cli 1.0.0",
		Path:         "/usr/bin/codex",
		ModelCatalog: &RuntimeModelCatalog{Models: []RuntimeModel{}, Error: "model catalog unavailable"},
	}

	payloads := make(chan daemonStatusUpdate, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload daemonStatusUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode daemon status: %v", err)
		}
		payloads <- payload
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	cfg := Config{
		BackendURL:  server.URL,
		WorkspaceID: "workspace:test",
		DaemonToken: "daemon_token",
	}
	service := &Service{
		cfg:          cfg,
		client:       server.Client(),
		daemonStatus: newDaemonStatusReporter(cfg, server.Client()),
		runtimes:     registry,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ticks := make(chan time.Time, 1)
	go func() {
		defer close(done)
		service.runDaemonStatusHeartbeat(ctx, ticks)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	ticks <- time.Now()
	first := receiveDaemonStatus(t, payloads)
	if got := first.Runtimes[0].ModelCatalog.Error; got != "model catalog unavailable" {
		t.Fatalf("first heartbeat catalog error = %q, want cached failure", got)
	}
	select {
	case <-driver.called:
	case <-time.After(time.Second):
		t.Fatal("failed catalog was not retried after heartbeat publication")
	}

	ticks <- time.Now()
	second := receiveDaemonStatus(t, payloads)
	if second.Runtimes[0].ModelCatalog == nil || len(second.Runtimes[0].ModelCatalog.Models) != 1 {
		t.Fatalf("second heartbeat did not publish recovered catalog: %#v", second.Runtimes)
	}
	if got := driver.callCount(); got != 1 {
		t.Fatalf("recovered catalog retries = %d, want exactly 1", got)
	}
}

func receiveDaemonStatus(t *testing.T, payloads <-chan daemonStatusUpdate) daemonStatusUpdate {
	t.Helper()
	select {
	case payload := <-payloads:
		if len(payload.Runtimes) != 1 {
			t.Fatalf("daemon status runtimes = %#v, want one", payload.Runtimes)
		}
		return payload
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for daemon status")
		return daemonStatusUpdate{}
	}
}
