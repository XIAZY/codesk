package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductEntrypointsRequireEmbeddedVersion(t *testing.T) {
	paths := map[string]string{
		"buildinfo":       "../daemon/internal/buildinfo/buildinfo.go",
		"agenttool":       "../daemon/cmd/agenttool/main.go",
		"daemon":          "../daemon/cmd/daemon/main.go",
		"desktop-windows": "../daemon/cmd/codesk-desktop/main_windows.go",
		"desktop-darwin":  "../daemon/cmd/codesk-desktop/main_darwin.go",
	}
	sources := make(map[string]string, len(paths))
	for name, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(data)
	}
	if err := checkEmbeddedVersionSource(sources); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name, file, old, replacement string
	}{
		{"development fallback restored", "buildinfo", "var Version string", `var Version = "dev"`},
		{"agent tool guard removed", "agenttool", "version, err := buildinfo.Require()", `version, err := buildinfo.Version, error(nil)`},
		{"daemon guard removed", "daemon", "version, err := buildinfo.Require()", `version, err := buildinfo.Version, error(nil)`},
		{"Windows desktop guard removed", "desktop-windows", "version, err := buildinfo.Require()", `version, err := buildinfo.Version, error(nil)`},
		{"macOS desktop guard removed", "desktop-darwin", "version, err := buildinfo.Require()", `version, err := buildinfo.Version, error(nil)`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(sources[mutation.file], mutation.old) != 1 {
				t.Fatalf("mutation source %q is not unique", mutation.old)
			}
			mutated := make(map[string]string, len(sources))
			for name, source := range sources {
				mutated[name] = source
			}
			mutated[mutation.file] = strings.Replace(mutated[mutation.file], mutation.old, mutation.replacement, 1)
			if err := checkEmbeddedVersionSource(mutated); err == nil {
				t.Fatal("embedded version mutation survived")
			}
		})
	}
}

func TestAgentToolRawBuildFailsClosedWithoutEmbeddedVersion(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is unavailable")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	rawBinary := filepath.Join(t.TempDir(), "notty-agent-tool")
	build := exec.Command("go", "build", "-o", rawBinary, "./daemon/cmd/agenttool")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("raw agent tool build failed: %v\n%s", err, output)
	}
	output, err := exec.Command(rawBinary, "--version").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "embedded build version is missing or invalid") {
		t.Fatalf("raw agent tool --version = %q, %v; want fail-closed embedded-version error", output, err)
	}

	reader := exec.Command("scripts/read-version.sh")
	reader.Dir = root
	versionBytes, err := reader.Output()
	if err != nil {
		t.Fatalf("read root VERSION: %v", err)
	}
	version := strings.TrimSpace(string(versionBytes))
	boundBinary := filepath.Join(t.TempDir(), "notty-agent-tool-bound")
	build = exec.Command("go", "build", "-ldflags", "-X notty/daemon/internal/buildinfo.Version="+version, "-o", boundBinary, "./daemon/cmd/agenttool")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("bound agent tool build failed: %v\n%s", err, output)
	}
	output, err = exec.Command(boundBinary, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != version {
		t.Fatalf("bound agent tool --version = %q, %v; want %q", output, err, version)
	}
}

func TestDaemonContainerBuildBindsRootVersion(t *testing.T) {
	data, err := os.ReadFile("../daemon/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if err := checkDaemonContainerVersionSource(source); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name, old, replacement string
	}{
		{"root version input removed", "COPY VERSION ./VERSION", "# VERSION input removed"},
		{"strict version reader removed", "COPY scripts/read-version.sh ./scripts/read-version.sh", "# strict reader removed"},
		{"one product binding removed", `-ldflags "-X notty/daemon/internal/buildinfo.Version=$version"`, ""},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := strings.Replace(source, mutation.old, mutation.replacement, 1)
			if mutated == source {
				t.Fatalf("mutation source %q was not found", mutation.old)
			}
			if err := checkDaemonContainerVersionSource(mutated); err == nil {
				t.Fatal("daemon container version mutation survived")
			}
		})
	}
}

func TestDaemonReportedVersionHasNoRuntimeOrInstallerOwner(t *testing.T) {
	paths := map[string]string{
		"config":          "../daemon/internal/syncer/config.go",
		"status":          "../daemon/internal/syncer/daemon_status.go",
		"desktop":         "../daemon/internal/desktop/desktop.go",
		"install-posix":   "../deploy/daemons/install.sh",
		"install-windows": "../deploy/daemons/install.ps1",
		"runner-windows":  "../deploy/daemons/run-windows.ps1",
	}
	sources := make(map[string]string, len(paths))
	for name, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(data)
	}
	if err := checkDaemonVersionOwnerSource(sources); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name, file, old, replacement string
	}{
		{"config field restored", "config", "DaemonToken        string", "DaemonToken        string\n\tDaemonVersion      string"},
		{"status dev fallback restored", "status", "Version:  version", `Version: "dev"`},
		{"POSIX installer environment override restored", "install-posix", `version="latest"`, `version="${NOTTY_DAEMON_VERSION:-latest}"`},
		{"Windows installer environment override restored", "install-windows", `$Version = "latest"`, `$Version = $env:NOTTY_DAEMON_VERSION`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(sources[mutation.file], mutation.old) != 1 {
				t.Fatalf("mutation source %q is not unique", mutation.old)
			}
			mutated := make(map[string]string, len(sources))
			for name, source := range sources {
				mutated[name] = source
			}
			mutated[mutation.file] = strings.Replace(mutated[mutation.file], mutation.old, mutation.replacement, 1)
			if err := checkDaemonVersionOwnerSource(mutated); err == nil {
				t.Fatal("alternate daemon version owner mutation survived")
			}
		})
	}
}

func checkEmbeddedVersionSource(sources map[string]string) error {
	buildinfo := sources["buildinfo"]
	for required, want := range map[string]int{
		"var Version string":                           1,
		`Version == "" || Version == "dev"`:            1,
		"func Require() (string, error)":               1,
		"return Version, nil":                          1,
		"embedded build version is missing or invalid": 1,
	} {
		if got := strings.Count(buildinfo, required); got != want {
			return fmt.Errorf("buildinfo source count for %q = %d, want %d", required, got, want)
		}
	}
	if strings.Contains(buildinfo, `var Version = "dev"`) {
		return fmt.Errorf("buildinfo restores a development fallback")
	}
	for _, name := range []string{"agenttool", "daemon", "desktop-windows", "desktop-darwin"} {
		if got := strings.Count(sources[name], "version, err := buildinfo.Require()"); got != 1 {
			return fmt.Errorf("%s embedded version guard count = %d, want 1", name, got)
		}
	}
	if strings.Count(sources["agenttool"], "fmt.Println(version)") != 1 ||
		strings.Count(sources["daemon"], `log.Printf("notty daemon version %s", version)`) != 1 ||
		strings.Count(sources["desktop-windows"], "Version:       version") != 1 ||
		strings.Count(sources["desktop-darwin"], "Version:       version") != 1 {
		return fmt.Errorf("a product entrypoint bypasses its required embedded version")
	}
	return nil
}

func checkDaemonContainerVersionSource(source string) error {
	for required, want := range map[string]int{
		"COPY VERSION ./VERSION":                                         1,
		"COPY scripts/read-version.sh ./scripts/read-version.sh":         1,
		`version="$(scripts/read-version.sh)"`:                           2,
		`-ldflags "-X notty/daemon/internal/buildinfo.Version=$version"`: 2,
		"-o /bin/notty-daemon ./daemon/cmd/daemon":                       1,
		"-o /bin/notty-agent-tool ./daemon/cmd/agenttool":                1,
	} {
		if got := strings.Count(source, required); got != want {
			return fmt.Errorf("daemon container source count for %q = %d, want %d", required, got, want)
		}
	}
	return nil
}

func checkDaemonVersionOwnerSource(sources map[string]string) error {
	if strings.Count(sources["status"], "version, err := buildinfo.Require()") != 1 ||
		strings.Count(sources["status"], "Version:  version") != 1 {
		return fmt.Errorf("daemon status does not report the required embedded version")
	}
	if strings.Count(sources["install-posix"], `version="latest"`) != 1 ||
		strings.Count(sources["install-windows"], `$Version = "latest"`) != 1 {
		return fmt.Errorf("daemon installers do not default artifact selection to latest")
	}
	for name, source := range sources {
		for _, forbidden := range []string{"DaemonVersion", "NOTTY_DAEMON_VERSION"} {
			if strings.Contains(source, forbidden) {
				return fmt.Errorf("%s restores alternate daemon version owner %q", name, forbidden)
			}
		}
	}
	return nil
}
