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
	releaseLibraryData, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "lib", "desktop-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	releaseLibrary := string(releaseLibraryData)
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
		`. "$root_dir/scripts/lib/desktop-release.sh"`,
		`notty_desktop_release_source_revision "$root_dir"`,
		"--source-revision \"$source_revision\"",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("build script is missing %q", required)
		}
	}
	for _, required := range []string{
		`git -C "$notty_release_root" rev-parse --verify HEAD`,
		`git -C "$notty_release_root" status --porcelain=v1 --untracked-files=all`,
		"source checkout must have no tracked, staged, or untracked changes before building a release",
	} {
		if !strings.Contains(releaseLibrary, required) {
			t.Errorf("shared release library is missing %q", required)
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

func TestDesktopReleaseSourceRevisionRequiresCleanCheckout(t *testing.T) {
	repositoryRoot := t.TempDir()
	runGit := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repositoryRoot}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "--quiet")
	if err := os.WriteFile(filepath.Join(repositoryRoot, "tracked"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked")
	runGit("-c", "user.name=Codesk Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "fixture")
	wantRevision := runGit("rev-parse", "HEAD")

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	projectRoot, err := filepath.Abs(filepath.Join(workingDirectory, "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(projectRoot, "scripts", "lib", "desktop-release.sh")
	runHelper := func() ([]byte, error) {
		command := exec.Command("sh", "-c", `. "$1"; notty_desktop_release_source_revision "$2"`, "sh", helper, repositoryRoot)
		return command.CombinedOutput()
	}
	output, err := runHelper()
	if err != nil {
		t.Fatalf("clean helper: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != wantRevision {
		t.Fatalf("source revision = %q, want %q", got, wantRevision)
	}

	if err := os.WriteFile(filepath.Join(repositoryRoot, "untracked"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runHelper()
	if err == nil {
		t.Fatal("source revision helper accepted an untracked file")
	}
	if !strings.Contains(string(output), "source checkout must have no tracked, staged, or untracked changes") {
		t.Fatalf("dirty helper error = %s", output)
	}
}
