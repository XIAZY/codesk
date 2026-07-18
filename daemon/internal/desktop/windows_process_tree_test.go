//go:build windows

package desktop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	processTreeHelperMode = "CODESK_PROCESS_TREE_HELPER"
	processTreePIDFile    = "CODESK_PROCESS_TREE_PID_FILE"
)

func TestKillOnCloseJobHasRequiredLimit(t *testing.T) {
	job, err := newKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)

	var information windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if information.BasicLimitInformation.LimitFlags != windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
		t.Fatalf("desktop containment job limits = %#x, want only JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE", information.BasicLimitInformation.LimitFlags)
	}
}

func TestWindowsProcessTreeKillsDescendantWhenOwnerDies(t *testing.T) {
	if os.Getenv(processTreeHelperMode) != "" {
		runProcessTreeHelper(t)
		return
	}

	directory := t.TempDir()
	pidFile := filepath.Join(directory, "child.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(executable, "-test.run=^TestWindowsProcessTreeKillsDescendantWhenOwnerDies$")
	owner.Env = append(os.Environ(), processTreeHelperMode+"=owner", processTreePIDFile+"="+pidFile)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if owner.Process != nil {
			_ = owner.Process.Kill()
		}
		_ = owner.Wait()
	})

	pids := waitForHelperPIDs(t, pidFile)
	handles := make(map[string]windows.Handle, len(pids))
	for generation, pid := range pids {
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			t.Fatalf("open %s process %d: %v", generation, pid, err)
		}
		handles[generation] = handle
		t.Cleanup(func() {
			_ = windows.TerminateProcess(handle, 1)
			_ = windows.CloseHandle(handle)
		})
	}

	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("kill containment owner: %v", err)
	}
	if err := owner.Wait(); err == nil {
		t.Fatal("killed containment owner exited successfully")
	}
	owner.Process = nil

	for generation, handle := range handles {
		status, err := windows.WaitForSingleObject(handle, 10_000)
		if err != nil {
			t.Fatalf("wait for %s exit: %v", generation, err)
		}
		if status != windows.WAIT_OBJECT_0 {
			t.Fatalf("%s process %d survived owner death (wait status %#x)", generation, pids[generation], status)
		}
	}
}

func runProcessTreeHelper(t *testing.T) {
	mode := os.Getenv(processTreeHelperMode)
	switch mode {
	case "owner":
		if err := ContainWindowsProcessTree(); err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(executable, "-test.run=^TestWindowsProcessTreeKillsDescendantWhenOwnerDies$")
		child.Env = append(os.Environ(), processTreeHelperMode+"=child", processTreePIDFile+"="+os.Getenv(processTreePIDFile))
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "child":
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		grandchild := exec.Command(executable, "-test.run=^TestWindowsProcessTreeKillsDescendantWhenOwnerDies$")
		grandchild.Env = append(os.Environ(), processTreeHelperMode+"=grandchild")
		if err := grandchild.Start(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(struct {
			Child      string `json:"child"`
			Grandchild string `json:"grandchild"`
		}{Child: strconv.Itoa(os.Getpid()), Grandchild: strconv.Itoa(grandchild.Process.Pid)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(processTreePIDFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "grandchild":
		for {
			time.Sleep(time.Hour)
		}
	default:
		t.Fatalf("unknown process tree helper mode %q", mode)
	}
}

func waitForHelperPIDs(t *testing.T, path string) map[string]int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var record struct {
				Child      string `json:"child"`
				Grandchild string `json:"grandchild"`
			}
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatalf("decode helper pid: %v", err)
			}
			pids := map[string]int{}
			for generation, value := range map[string]string{"child": record.Child, "grandchild": record.Grandchild} {
				pid, err := strconv.Atoi(value)
				if err != nil || pid <= 0 {
					t.Fatalf("invalid %s pid %q", generation, value)
				}
				pids[generation] = pid
			}
			return pids
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read helper pid: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for containment descendant")
	return nil
}
