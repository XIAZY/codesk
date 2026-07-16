//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildScriptRejectsVersionBeforeCreatingVersionPath(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot, err := filepath.Abs(filepath.Join(workingDirectory, "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	temporaryRoot := t.TempDir()
	dist := filepath.Join(temporaryRoot, "dist")
	escaped := filepath.Join(temporaryRoot, "escape-marker")
	command := exec.Command(
		"sh",
		filepath.Join(repositoryRoot, "scripts", "build-windows-desktop-release.sh"),
		"../../escape-marker",
		dist,
	)
	command.Dir = repositoryRoot
	command.Env = append(os.Environ(), "NOTTY_TEST_TMP_ROOT="+temporaryRoot)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("build script accepted a path-like release version")
	}
	if !strings.Contains(string(output), "invalid release version") {
		t.Fatalf("build script error = %s, want invalid release version", output)
	}
	for _, path := range []string{escaped, dist} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("invalid version created %s (stat error %v)", path, err)
		}
	}
}

func TestBuildScriptPinsInputsAndDisablesAmbientBuildMetadata(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot, err := filepath.Abs(filepath.Join(workingDirectory, "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "build-windows-desktop-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, required := range []string{
		"export GOENV=off",
		"export GOWORK=off",
		"export GOAMD64=v1",
		"export GOARM64=v8.0",
		"go_toolchain='go1.26.5'",
		"rustc 1.97.0 (2d8144b78 2026-07-07)",
		"cargo 1.97.0 (c980f4866 2026-06-30)",
		"export CARGO_TARGET_DIR=\"$tmp_dir/cargo-target\"",
		"export CARGO_ENCODED_RUSTFLAGS=",
		"export RUSTC=\"$(command -v rustc)\"",
		"--remap-path-prefix=$root_dir=.",
		"--remap-path-prefix=$tmp_dir=/build",
		"go_repro_ldflags='-buildid='",
		"go-winres@v0.3.1",
		"-buildvcs=false",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("build script is missing %q", required)
		}
	}
	if strings.Contains(script, `${RUSTFLAGS:-}`) {
		t.Fatal("build script inherits ambient RUSTFLAGS")
	}
	if strings.Contains(script, `RUSTFLAGS="$rust_flags"`) {
		t.Fatal("build script passes whitespace-split Rust flags")
	}
	if count := strings.Count(script, `-ldflags "$go_repro_ldflags`); count != 3 {
		t.Fatalf("published Go build commands using deterministic build IDs = %d, want 3", count)
	}
}
