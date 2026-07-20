package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScript invokes a shell script in the scripts/ directory with the given
// arguments and returns its combined output and error.
func runScript(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeVersionFile creates a VERSION file with the given contents and returns
// its path.
func writeVersionFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	return p
}

// resolveReleaseVersion runs the tag-vs-file resolver against an explicit VERSION
// file and returns trimmed stdout plus whether it exited zero.
func resolveReleaseVersion(t *testing.T, tag, versionFile string) (string, bool) {
	t.Helper()
	out, err := runScript(t, "./normalize-release-version.sh", tag, versionFile)
	return strings.TrimSpace(out), err == nil
}

// TestNormalizeReleaseVersionAcceptsMatchingTag pins that when the desktop-v tag
// matches a canonical VERSION file, the resolver emits exactly that version. This
// is the value that must flow to MSI ProductVersion, macOS metadata, manifest,
// and the baked daemon version.
func TestNormalizeReleaseVersionAcceptsMatchingTag(t *testing.T) {
	cases := map[string]string{
		"0.0.1":         "desktop-v0.0.1",
		"0.0.0":         "desktop-v0.0.0",
		"1.2.3":         "desktop-v1.2.3",
		"255.255.65535": "desktop-v255.255.65535",
	}
	for fileVer, tag := range cases {
		vf := writeVersionFile(t, fileVer+"\n")
		got, ok := resolveReleaseVersion(t, tag, vf)
		if !ok {
			t.Errorf("tag %q file %q: expected acceptance, got failure: %s", tag, fileVer, got)
			continue
		}
		if got != fileVer {
			t.Errorf("tag %q file %q: expected %q, got %q", tag, fileVer, fileVer, got)
		}
	}
}

// TestNormalizeReleaseVersionRejectsTagFileMismatch is the core new guard: a tag
// whose version disagrees with the VERSION file must fail closed, so a release
// can never publish a version that differs from the baked/packaged one.
func TestNormalizeReleaseVersionRejectsTagFileMismatch(t *testing.T) {
	vf := writeVersionFile(t, "0.0.1\n")
	for _, tag := range []string{"desktop-v0.0.2", "desktop-v1.0.1", "desktop-v0.1.0"} {
		if got, ok := resolveReleaseVersion(t, tag, vf); ok {
			t.Errorf("tag %q vs file 0.0.1: expected mismatch rejection, got accepted %q", tag, got)
		}
	}
}

// TestNormalizeReleaseVersionRejectsBadTagShape rejects tags that are not a
// desktop-v prefix over the exact file version.
func TestNormalizeReleaseVersionRejectsBadTagShape(t *testing.T) {
	vf := writeVersionFile(t, "1.2.3\n")
	for _, tag := range []string{
		"v1.2.3",                  // wrong prefix
		"1.2.3",                   // no prefix
		"desktop-vdesktop-v1.2.3", // doubled prefix -> remainder != file
		"",                        // empty
		"desktop-v1.2.3-rc1",      // prerelease tag -> remainder != file
	} {
		if got, ok := resolveReleaseVersion(t, tag, vf); ok {
			t.Errorf("tag %q: expected rejection, got accepted %q", tag, got)
		}
	}
}

// TestNormalizeReleaseVersionRejectsNonCanonicalFile guards the VERSION file
// itself: even if the tag matches, a non-canonical or MSI-out-of-range file must
// fail closed rather than propagate a bad version.
func TestNormalizeReleaseVersionRejectsNonCanonicalFile(t *testing.T) {
	bad := map[string]string{
		"1.2":         "desktop-v1.2",         // too few components
		"1.2.3.4":     "desktop-v1.2.3.4",     // too many components
		"1.2.x":       "desktop-v1.2.x",       // non-numeric
		"1.02.3":      "desktop-v1.02.3",      // leading zero
		"256.0.0":     "desktop-v256.0.0",     // major out of MSI range
		"1.256.0":     "desktop-v1.256.0",     // minor out of MSI range
		"0.0.65536":   "desktop-v0.0.65536",   // patch out of MSI range
		"1.2.3-rc1":   "desktop-v1.2.3-rc1",   // prerelease in file
		"1.2.3+build": "desktop-v1.2.3+build", // build metadata in file
	}
	for fileVer, tag := range bad {
		vf := writeVersionFile(t, fileVer+"\n")
		if got, ok := resolveReleaseVersion(t, tag, vf); ok {
			t.Errorf("file %q: expected non-canonical rejection, got accepted %q", fileVer, got)
		}
	}
}

// TestNormalizeReleaseVersionRejectsMissingOrEmptyFile fails closed when the
// VERSION file is absent or blank.
func TestNormalizeReleaseVersionRejectsMissingOrEmptyFile(t *testing.T) {
	if _, ok := resolveReleaseVersion(t, "desktop-v0.0.1", filepath.Join(t.TempDir(), "nope", "VERSION")); ok {
		t.Errorf("missing VERSION file: expected rejection")
	}
	if _, ok := resolveReleaseVersion(t, "desktop-v0.0.1", writeVersionFile(t, "\n")); ok {
		t.Errorf("empty VERSION file: expected rejection")
	}
}

// TestNormalizeReleaseVersionTrimsFileWhitespace pins that a trailing newline or
// surrounding whitespace in the VERSION file is tolerated (the file commonly
// ends in a newline).
func TestNormalizeReleaseVersionTrimsFileWhitespace(t *testing.T) {
	if got, ok := resolveReleaseVersion(t, "desktop-v0.0.1", writeVersionFile(t, "  0.0.1  \n")); !ok || got != "0.0.1" {
		t.Fatalf("whitespace file: expected 0.0.1 accepted, got %q ok=%v", got, ok)
	}
}
