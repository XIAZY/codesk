package syncer

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The fake process fills the app-server event channel with telemetry, emits a
// real lifecycle notification, and exits. The lifecycle event must survive,
// and process exit must not close the channel until readLoop has delivered it.
func TestCodexAppServerExitDrainsGuaranteedLifecycleNotification(t *testing.T) {
	codexPath := fakeProcessCommand(t, fakeProcessCodexLifecycleFlood)
	client := newCodexAppServer(Config{CodexCommand: codexPath, DataDir: t.TempDir()}, t.TempDir(), "", "agent_codex")
	defer client.closeLog()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start app-server: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(client.events) < cap(client.events) {
		if time.Now().After(deadline) {
			t.Fatalf("app-server channel did not fill: len=%d cap=%d", len(client.events), cap(client.events))
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-client.done:
	case <-time.After(3 * time.Second):
		t.Fatal("fake app-server did not exit while lifecycle delivery was blocked")
	}

	telemetry := 0
	foundLifecycle := false
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				if !foundLifecycle {
					t.Fatalf("turn/completed was lost before process-exit channel close; telemetry=%d", telemetry)
				}
				return
			}
			if event.Method == "turn/completed" {
				foundLifecycle = true
			} else if event.Method == "item/agentMessage/delta" {
				telemetry++
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("events did not close after readLoop drained; telemetry=%d lifecycle=%v", telemetry, foundLifecycle)
		}
	}
}

func TestCodexAppServerFullChannelDropsTelemetryWithoutBlockingReadLoop(t *testing.T) {
	client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
	for i := 0; i < cap(client.events); i++ {
		client.events <- appServerEvent{Method: "filler"}
	}

	readDone := make(chan struct{})
	go func() {
		client.readLoop(strings.NewReader(`{"method":"item/agentMessage/delta","params":{"delta":"text"}}` + "\n"))
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("telemetry notification blocked on a full channel")
	}
	if got := len(client.events); got != cap(client.events) {
		t.Fatalf("telemetry should be dropped without changing the full channel: len=%d cap=%d", got, cap(client.events))
	}
}

func TestCodexAppServerFullChannelDropsNonIdleStatusTelemetry(t *testing.T) {
	for _, status := range []string{"working", "future-status"} {
		t.Run(status, func(t *testing.T) {
			client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
			for i := 0; i < cap(client.events); i++ {
				client.events <- appServerEvent{Method: "filler"}
			}

			readDone := make(chan struct{})
			go func() {
				line := `{"method":"thread/status/changed","params":{"status":{"type":"` + status + `"}}}`
				client.readLoop(strings.NewReader(line + "\n"))
				close(readDone)
			}()
			select {
			case <-readDone:
			case <-time.After(100 * time.Millisecond):
				_ = client.Stop()
				<-readDone
				t.Fatalf("non-idle status %q blocked despite being discarded by codexRuntimeEvent", status)
			}
			if got := len(client.events); got != cap(client.events) {
				t.Fatalf("non-idle status should be dropped without changing the full channel: len=%d cap=%d", got, cap(client.events))
			}
		})
	}
}

func TestCodexAppServerGuaranteesEveryMappedLifecycleMethod(t *testing.T) {
	tests := []struct {
		method string
		params string
	}{
		{method: "turn/started", params: `{"turn":{"id":"turn_1"}}`},
		{method: "turn/completed", params: `{}`},
		{method: "turn/failed", params: `{}`},
		{method: "thread/status/changed", params: `{"status":{"type":"idle"}}`},
	}

	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
			for i := 0; i < cap(client.events); i++ {
				client.events <- appServerEvent{Method: "filler"}
			}

			readDone := make(chan struct{})
			go func() {
				line := `{"method":"` + test.method + `","params":` + test.params + `}`
				client.readLoop(strings.NewReader(line + "\n"))
				close(readDone)
			}()
			select {
			case <-readDone:
				t.Fatalf("%s was dropped instead of blocking on the full channel", test.method)
			case <-time.After(100 * time.Millisecond):
			}

			for i := 0; i < cap(client.events); i++ {
				<-client.events
			}
			select {
			case event := <-client.events:
				if event.Method != test.method {
					t.Fatalf("lifecycle method = %q, want %q", event.Method, test.method)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s was not delivered after channel capacity became available", test.method)
			}
			select {
			case <-readDone:
			case <-time.After(2 * time.Second):
				t.Fatalf("readLoop did not return after delivering %s", test.method)
			}
		})
	}
}

func TestCodexAppServerStopWithoutCommandReleasesBlockedLifecycle(t *testing.T) {
	client := newCodexAppServer(Config{}, t.TempDir(), "", "agent_codex")
	for i := 0; i < cap(client.events); i++ {
		client.events <- appServerEvent{Method: "filler"}
	}

	readDone := make(chan struct{})
	go func() {
		client.readLoop(strings.NewReader(`{"method":"turn/failed","params":{}}` + "\n"))
		close(readDone)
	}()
	select {
	case <-readDone:
		t.Fatal("lifecycle notification returned instead of blocking on the full live channel")
	case <-time.After(100 * time.Millisecond):
	}

	if err := client.Stop(); err != nil {
		t.Fatalf("stop app-server without command: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not release the blocked lifecycle notification when cmd was nil")
	}
}
