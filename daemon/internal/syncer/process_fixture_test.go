package syncer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"notty/daemon/internal/buildinfo"
)

const fakeProcessModeEnv = "NOTTY_SYNCER_FAKE_PROCESS"

const (
	fakeProcessCodexWithoutAppServer = "codex-without-app-server"
	fakeProcessCodexDetectAvailable  = "codex-detect-available"
	fakeProcessCodexFinalStderr      = "codex-final-stderr"
	fakeProcessCodexLifecycleFlood   = "codex-lifecycle-flood"
	fakeProcessCodexPersistent       = "codex-persistent"
	fakeProcessClaude                = "claude"
	fakeProcessManagedChildIO        = "managed-child-io"
	fakeProcessVersionProbe          = "version-probe"
)

// TestMain lets subprocess tests reuse this already-native test executable
// instead of writing host-specific shell wrappers.
func TestMain(m *testing.M) {
	switch os.Getenv(fakeProcessModeEnv) {
	case fakeProcessCodexWithoutAppServer:
		os.Exit(runFakeCodexWithoutAppServer(os.Args[1:]))
	case fakeProcessCodexDetectAvailable:
		os.Exit(runFakeCodexDetectAvailable(os.Args[1:]))
	case fakeProcessCodexFinalStderr:
		os.Exit(runFakeCodexFinalStderr())
	case fakeProcessCodexLifecycleFlood:
		os.Exit(runFakeCodexLifecycleFlood())
	case fakeProcessCodexPersistent:
		os.Exit(runFakeCodexPersistent(os.Args[1:]))
	case fakeProcessClaude:
		os.Exit(runFakeClaude(os.Args[1:]))
	case fakeProcessManagedChildIO:
		os.Exit(runFakeManagedChildIO(os.Args[1:]))
	case fakeProcessVersionProbe:
		os.Exit(runFakeVersionProbe(os.Args[1:]))
	default:
		buildinfo.Version = "test-version"
		os.Exit(m.Run())
	}
}

func runFakeVersionProbe(args []string) int {
	if len(args) == 2 && args[0] == "app-server" && args[1] == "--help" {
		return 0
	}
	if len(args) != 1 || args[0] != "--version" {
		return 2
	}
	fmt.Fprint(os.Stdout, os.Getenv("FAKE_VERSION_STDOUT"))
	fmt.Fprint(os.Stderr, os.Getenv("FAKE_VERSION_STDERR"))
	return 0
}

func fakeVersionProbeCommand(t *testing.T, stdout string, stderr string) string {
	t.Helper()
	command := fakeProcessCommand(t, fakeProcessVersionProbe)
	t.Setenv("FAKE_VERSION_STDOUT", stdout)
	t.Setenv("FAKE_VERSION_STDERR", stderr)
	return command
}

func runFakeManagedChildIO(args []string) int {
	fmt.Fprintf(os.Stdout, "stdout:%s", strings.Join(args, "|"))
	fmt.Fprintf(os.Stderr, "stderr:%s", strings.Join(args, "|"))
	return 23
}

func runFakeCodexPersistent(args []string) int {
	if len(args) != 1 || args[0] != "app-server" {
		return 2
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		switch request.Method {
		case "initialize":
			if os.Getenv("FAKE_CODEX_SILENT_EXIT") == "1" {
				return 1
			}
			if os.Getenv("FAKE_CODEX_BLOCK_INITIALIZE") == "1" {
				continue
			}
			if os.Getenv("FAKE_CODEX_FAIL_INITIALIZE") == "1" {
				if err := encoder.Encode(map[string]any{
					"id":    request.ID,
					"error": map[string]any{"code": -1, "message": "fake initialize failure"},
				}); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
				continue
			}
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case "initialized":
		case "thread/start":
			if err := encoder.Encode(map[string]any{
				"id":     request.ID,
				"result": map[string]any{"thread": map[string]string{"id": "thread_persistent"}},
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case "thread/resume":
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case "turn/start":
			if err := encoder.Encode(map[string]any{
				"id":     request.ID,
				"result": map[string]any{"turn": map[string]string{"id": "turn_after_cancel"}},
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		default:
			if len(request.ID) == 0 {
				continue
			}
			if err := encoder.Encode(map[string]any{
				"id":    request.ID,
				"error": map[string]any{"code": -2, "message": "unsupported fake method"},
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func fakeProcessCommand(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv(fakeProcessModeEnv, mode)
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return path
}

func runFakeCodexWithoutAppServer(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex 0.1.0")
		return 0
	}
	return 2
}

func runFakeCodexDetectAvailable(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex 0.144.5")
		return 0
	}
	if len(args) == 2 && args[0] == "app-server" && args[1] == "--help" {
		return 0
	}
	return 2
}

func runFakeCodexLifecycleFlood() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		switch request.Method {
		case "initialize":
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case "initialized":
			for i := 0; i < 128; i++ {
				if err := encoder.Encode(map[string]any{
					"method": "item/agentMessage/delta",
					"params": map[string]any{"index": i},
				}); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
			}
			if err := encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{}}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			time.Sleep(time.Second)
			return 0
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runFakeCodexFinalStderr() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		switch request.Method {
		case "initialize":
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case "initialized":
			for i := 0; i < 3000; i++ {
				fmt.Fprintf(os.Stderr, "stderr noise %d\n", i)
			}
			fmt.Fprintln(os.Stderr, "FINAL-STDERR-SENTINEL")
			return 7
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runFakeClaude(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("9.9.9 (Claude Code)")
		return 0
	}
	if len(args) == 1 && args[0] == "--help" {
		if path := os.Getenv("FAKE_CLAUDE_HELP_FAIL_ONCE_FILE"); path != "" {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err := os.WriteFile(path, []byte("failed\n"), 0o644); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
				return 1
			}
		}
		if os.Getenv("FAKE_CLAUDE_HELP_DRIFT") == "1" {
			fmt.Println("--effort <level>  Choose effort (low, medium, high)")
			return 0
		}
		fmt.Println("--model <model>  Model alias (fable, opus, sonnet)")
		fmt.Println("--effort <level>  Choose effort (low, medium, high, xhigh, max)")
		fmt.Println("--fallback-model <model>  Fallback model")
		return 0
	}
	if path := os.Getenv("FAKE_CLAUDE_ENV_FILE"); path != "" {
		contents := strings.Join(os.Environ(), "\n") + "\n"
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if path := os.Getenv("FAKE_CLAUDE_ARGS_FILE"); path != "" {
		if err := appendFakeProcessLines(path, args...); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	var sessionID string
	var resumeID string
	for i, arg := range args {
		if i == 0 {
			continue
		}
		switch args[i-1] {
		case "--session-id":
			sessionID = arg
		case "--resume":
			resumeID = arg
		}
	}
	if resumeID != "" {
		if os.Getenv("FAKE_CLAUDE_FAIL_RESUME") == "1" {
			fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", resumeID)
			return 1
		}
		sessionID = resumeID
	}

	// Startup diagnostics some fatal reasons print before any handshake, one per
	// stream, then exit 1 — used to prove the daemon preserves whichever stream
	// carries the provider's reason.
	if diag := os.Getenv("FAKE_CLAUDE_STARTUP_STDOUT_DIAG"); diag != "" {
		fmt.Fprintln(os.Stdout, diag)
		return 1
	}
	if diag := os.Getenv("FAKE_CLAUDE_STARTUP_STDERR_DIAG"); diag != "" {
		fmt.Fprintln(os.Stderr, diag)
		return 1
	}
	if os.Getenv("FAKE_CLAUDE_STARTUP_EMIT_RESULT_THEN_EXIT") == "1" {
		// Emit one turn-end (result) frame then exit, during startup. With the
		// daemon's events channel pre-filled, readLoop parks in emitLifecycle on
		// this frame — exercising the bounded stdout-drain fence at startup exit.
		fmt.Fprintln(os.Stdout, `{"type":"result","subtype":"success"}`)
		return 0
	}

	holdTurn := os.Getenv("FAKE_CLAUDE_HOLD_TURN") == "1"
	ioPath := os.Getenv("FAKE_CLAUDE_IO_FILE")
	initialized := false
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if ioPath != "" {
			if err := appendFakeProcessLines(ioPath, line); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		if strings.Contains(line, "control_request") {
			if err := encoder.Encode(map[string]any{
				"type":     "control_response",
				"response": map[string]any{"subtype": "success"},
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			if holdTurn && strings.Contains(line, "interrupt") {
				if err := encodeFakeClaudeResult(encoder, sessionID, false); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
			}
			continue
		}
		if !initialized {
			initialized = true
			if err := encoder.Encode(map[string]any{
				"type":       "system",
				"subtype":    "init",
				"session_id": sessionID,
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		if strings.Contains(line, "fail this turn") {
			if err := encoder.Encode(map[string]any{
				"type":       "result",
				"subtype":    "error_during_execution",
				"is_error":   true,
				"errors":     []string{"boom"},
				"session_id": sessionID,
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			continue
		}
		if err := encoder.Encode(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}},
			},
			"session_id": sessionID,
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !holdTurn {
			if err := encodeFakeClaudeResult(encoder, sessionID, false); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func encodeFakeClaudeResult(encoder *json.Encoder, sessionID string, isError bool) error {
	return encoder.Encode(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   isError,
		"session_id": sessionID,
	})
}

func appendFakeProcessLines(path string, lines ...string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(file, line); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}
