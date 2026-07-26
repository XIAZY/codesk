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

type asyncCatalogDriver struct {
	mu       sync.Mutex
	catalogs []*RuntimeModelCatalog
	calls    int
	called   chan struct{}
	entered  chan struct{}
	release  <-chan struct{}
}

func (d *asyncCatalogDriver) Kind() RuntimeKind {
	return RuntimeCodex
}

func (d *asyncCatalogDriver) Detect(context.Context) RuntimeDetection {
	return RuntimeDetection{
		Kind:      RuntimeCodex,
		Available: true,
		Version:   "codex-cli 1.0.0",
		Path:      "/usr/bin/codex",
	}
}

func (d *asyncCatalogDriver) Spawn(context.Context, RuntimeSpawnSpec) (RuntimeProcess, error) {
	return nil, nil
}

func (d *asyncCatalogDriver) detectModelCatalog(context.Context) *RuntimeModelCatalog {
	d.mu.Lock()
	index := d.calls
	d.calls++
	catalog := d.catalogs[len(d.catalogs)-1]
	if index < len(d.catalogs) {
		catalog = d.catalogs[index]
	}
	d.mu.Unlock()
	if d.entered != nil {
		d.entered <- struct{}{}
	}
	if d.release != nil {
		<-d.release
	}
	if d.called != nil {
		d.called <- struct{}{}
	}
	return catalog
}

func (d *asyncCatalogDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func TestRuntimeRegistryStartupDefersCatalogDiscovery(t *testing.T) {
	driver := &asyncCatalogDriver{
		catalogs: []*RuntimeModelCatalog{{Models: []RuntimeModel{{
			Model: "gpt-5.6-sol",
		}}}},
	}
	registry := newRuntimeRegistry(driver)

	detections := registry.DetectAll(context.Background())

	if got := driver.callCount(); got != 0 {
		t.Fatalf("startup catalog probes = %d, want 0", got)
	}
	if len(detections) != 1 {
		t.Fatalf("startup detections = %#v, want one", detections)
	}
	detection := detections[0]
	if !detection.Available || detection.Version != "codex-cli 1.0.0" || detection.Path != "/usr/bin/codex" {
		t.Fatalf("startup lost runtime metadata: %#v", detection)
	}
	if detection.ModelCatalog == nil || detection.ModelCatalog.Error != "" || len(detection.ModelCatalog.Models) != 0 {
		t.Fatalf("startup catalog = %#v, want explicit empty catalog", detection.ModelCatalog)
	}
}

func TestRuntimeRegistryCatalogDiscoveryBacksOffUntilSuccess(t *testing.T) {
	recoveredCatalog := &RuntimeModelCatalog{Models: []RuntimeModel{{
		Model:                  "gpt-5.6-sol",
		DisplayName:            "GPT-5.6-Sol",
		IsDefault:              true,
		ReasoningEfforts:       []string{"low"},
		DefaultReasoningEffort: "low",
	}}}
	driver := &asyncCatalogDriver{
		catalogs: []*RuntimeModelCatalog{
			{Models: []RuntimeModel{}, Error: "model catalog unavailable"},
			{Models: []RuntimeModel{}, Error: "model catalog unavailable"},
			recoveredCatalog,
		},
	}
	registry := newRuntimeRegistry(driver)
	registry.DetectAll(context.Background())

	now := time.Unix(1_700_000_000, 0)
	registry.discoverModelCatalogs(context.Background(), now)
	registry.discoverModelCatalogs(context.Background(), now.Add(10*time.Second))
	registry.discoverModelCatalogs(context.Background(), now.Add(failedCatalogRetryInterval-time.Nanosecond))
	if got := driver.callCount(); got != 1 {
		t.Fatalf("persistent catalog retries before cooldown = %d, want 1", got)
	}
	registry.discoverModelCatalogs(context.Background(), now.Add(failedCatalogRetryInterval))
	registry.discoverModelCatalogs(context.Background(), now.Add(failedCatalogRetryInterval+10*time.Second))
	if got := driver.callCount(); got != 2 {
		t.Fatalf("persistent catalog retries after cooldown = %d, want 2", got)
	}
	registry.discoverModelCatalogs(context.Background(), now.Add(2*failedCatalogRetryInterval-time.Nanosecond))
	if got := driver.callCount(); got != 2 {
		t.Fatalf("persistent catalog retries before second cooldown = %d, want 2", got)
	}
	registry.discoverModelCatalogs(context.Background(), now.Add(2*failedCatalogRetryInterval))
	registry.discoverModelCatalogs(context.Background(), now.Add(3*failedCatalogRetryInterval))
	if got := driver.callCount(); got != 3 {
		t.Fatalf("catalog retries after recovery = %d, want 3", got)
	}
	detection := registry.cachedDetections()[0]
	if !detection.Available || detection.Version != "codex-cli 1.0.0" || detection.Path != "/usr/bin/codex" {
		t.Fatalf("catalog discovery changed runtime metadata: %#v", detection)
	}
	if detection.ModelCatalog == nil || detection.ModelCatalog.Error != "" || len(detection.ModelCatalog.Models) != 1 {
		t.Fatalf("catalog did not recover after bounded retries: %#v", detection.ModelCatalog)
	}
}

func TestRuntimeRegistryCatalogDiscoveryDoesNotOverlapAndAcceptsEmptySuccess(t *testing.T) {
	release := make(chan struct{})
	driver := &asyncCatalogDriver{
		catalogs: []*RuntimeModelCatalog{{Models: []RuntimeModel{}}},
		entered:  make(chan struct{}, 1),
		release:  release,
	}
	registry := newRuntimeRegistry(driver)
	registry.DetectAll(context.Background())

	now := time.Unix(1_700_000_000, 0)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		registry.discoverModelCatalogs(context.Background(), now)
	}()
	select {
	case <-driver.entered:
	case <-time.After(time.Second):
		t.Fatal("first catalog discovery did not start")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		registry.discoverModelCatalogs(context.Background(), now)
	}()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("overlapping discovery blocked instead of observing in-flight ownership")
	}
	if got := driver.callCount(); got != 1 {
		t.Fatalf("overlapping catalog probes = %d, want 1", got)
	}

	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first catalog discovery did not finish")
	}
	registry.discoverModelCatalogs(context.Background(), now.Add(failedCatalogRetryInterval))
	if got := driver.callCount(); got != 1 {
		t.Fatalf("genuinely empty successful catalog probes = %d, want exactly 1", got)
	}
	catalog := registry.cachedDetections()[0].ModelCatalog
	if catalog == nil || catalog.Error != "" || len(catalog.Models) != 0 {
		t.Fatalf("empty successful catalog = %#v", catalog)
	}
}

func TestDaemonStatusHeartbeatPublishesEmptyCatalogBeforeDiscovery(t *testing.T) {
	driver := &asyncCatalogDriver{
		catalogs: []*RuntimeModelCatalog{{Models: []RuntimeModel{{
			Model:                  "gpt-5.6-sol",
			DisplayName:            "GPT-5.6-Sol",
			IsDefault:              true,
			ReasoningEfforts:       []string{"low"},
			DefaultReasoningEffort: "low",
		}}}},
		called: make(chan struct{}, 1),
	}
	registry := newRuntimeRegistry(driver)
	registry.DetectAll(context.Background())

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
		cfg:           cfg,
		client:        server.Client(),
		daemonStatus:  newDaemonStatusReporter(cfg, server.Client()),
		runtimes:      registry,
		refreshNeeded: make(chan struct{}, 1),
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
	firstCatalog := first.Runtimes[0].ModelCatalog
	if firstCatalog == nil || firstCatalog.Error != "" || len(firstCatalog.Models) != 0 {
		t.Fatalf("first heartbeat catalog = %#v, want explicit empty pre-discovery state", firstCatalog)
	}
	select {
	case <-driver.called:
	case <-time.After(time.Second):
		t.Fatal("first heartbeat did not own catalog discovery")
	}
	select {
	case <-service.refreshNeeded:
	case <-time.After(time.Second):
		t.Fatal("successful catalog discovery did not wake agent reconciliation")
	}

	ticks <- time.Now()
	second := receiveDaemonStatus(t, payloads)
	if second.Runtimes[0].ModelCatalog == nil || len(second.Runtimes[0].ModelCatalog.Models) != 1 {
		t.Fatalf("second heartbeat did not publish discovered catalog: %#v", second.Runtimes)
	}
	if got := driver.callCount(); got != 1 {
		t.Fatalf("successful catalog probes = %d, want exactly 1", got)
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
