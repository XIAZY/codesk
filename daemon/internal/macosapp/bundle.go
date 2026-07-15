// Package macosapp resolves and validates the installed Codesk application
// bundle without depending on macOS APIs. Keeping this boundary portable lets
// release tooling and host tests enforce the same runtime helper layout.
package macosapp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"notty/daemon/internal/macosuser"
)

const (
	AppName        = "Codesk.app"
	ExecutableName = "Codesk"
	AgentToolName  = "notty-agent-tool"
)

// Bundle is the canonical installed layout rooted at Codesk.app.
type Bundle struct {
	Root       string
	Contents   string
	MacOS      string
	Helpers    string
	Resources  string
	Executable string
	AgentTool  string
	InfoPlist  string
}

// ResolveCurrent resolves the executable of the running process as a Codesk
// application bundle.
func ResolveCurrent() (Bundle, error) {
	executable, err := os.Executable()
	if err != nil {
		return Bundle{}, fmt.Errorf("codesk macOS bundle: resolve executable: %w", err)
	}
	return Resolve(executable)
}

// Resolve validates the exact Codesk.app/Contents layout and returns canonical
// paths. Required bundle entries may not be symlinks.
func Resolve(executable string) (Bundle, error) {
	if executable == "" || executable != strings.TrimSpace(executable) || strings.ContainsRune(executable, '\x00') {
		return Bundle{}, errors.New("codesk macOS bundle: invalid executable path")
	}
	if !filepath.IsAbs(executable) || executable != filepath.Clean(executable) {
		return Bundle{}, errors.New("codesk macOS bundle: executable path must be absolute and clean")
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return Bundle{}, fmt.Errorf("codesk macOS bundle: resolve executable symlinks: %w", err)
	}
	resolvedExecutable = filepath.Clean(resolvedExecutable)

	macOSDir := filepath.Dir(resolvedExecutable)
	contentsDir := filepath.Dir(macOSDir)
	rootDir := filepath.Dir(contentsDir)
	if filepath.Base(resolvedExecutable) != ExecutableName || filepath.Base(macOSDir) != "MacOS" ||
		filepath.Base(contentsDir) != "Contents" || filepath.Base(rootDir) != AppName {
		return Bundle{}, fmt.Errorf("codesk macOS bundle: executable is not %s/Contents/MacOS/%s", AppName, ExecutableName)
	}

	bundle := Bundle{
		Root:       rootDir,
		Contents:   contentsDir,
		MacOS:      macOSDir,
		Helpers:    filepath.Join(contentsDir, "Helpers"),
		Resources:  filepath.Join(contentsDir, "Resources"),
		Executable: resolvedExecutable,
		AgentTool:  filepath.Join(contentsDir, "Helpers", AgentToolName),
		InfoPlist:  filepath.Join(contentsDir, "Info.plist"),
	}
	for _, directory := range []string{bundle.Root, bundle.Contents, bundle.MacOS, bundle.Helpers, bundle.Resources} {
		if err := requireDirectory(directory); err != nil {
			return Bundle{}, err
		}
	}
	if err := requireExecutable(bundle.Executable); err != nil {
		return Bundle{}, err
	}
	if err := requireExecutable(bundle.AgentTool); err != nil {
		return Bundle{}, err
	}
	if err := requireRegularFile(bundle.InfoPlist); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("codesk macOS bundle: inspect directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("codesk macOS bundle: %q is not a real directory", path)
	}
	return nil
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("codesk macOS bundle: inspect file %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("codesk macOS bundle: %q is not a regular file", path)
	}
	return nil
}

func requireExecutable(path string) error {
	if err := requireRegularFile(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("codesk macOS bundle: inspect executable %q: %w", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("codesk macOS bundle: %q is not executable", path)
	}
	return nil
}

// ConfigureHelperPath installs a bounded PATH for Finder and login-item
// launches, then proves that shell resolution selects the signed bundled agent
// tool. Ambient shell entries and the legacy ~/.notty tree are never inherited.
func (b Bundle) ConfigureHelperPath() error {
	if err := requireExecutable(b.AgentTool); err != nil {
		return err
	}
	home, err := macosuser.HomeDir()
	if err != nil {
		return fmt.Errorf("codesk macOS bundle: resolve home directory: %w", err)
	}
	pathValue, err := boundedToolPath(b.Helpers, home)
	if err != nil {
		return err
	}
	if err := os.Setenv("PATH", pathValue); err != nil {
		return fmt.Errorf("codesk macOS bundle: set helper PATH: %w", err)
	}
	resolved, err := exec.LookPath(AgentToolName)
	if err != nil {
		return fmt.Errorf("codesk macOS bundle: resolve bundled agent tool: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("codesk macOS bundle: resolve agent tool path: %w", err)
	}
	if filepath.Clean(resolved) != b.AgentTool {
		return fmt.Errorf("codesk macOS bundle: agent tool resolved to %q, want %q", resolved, b.AgentTool)
	}
	return nil
}

func boundedToolPath(helperDirectory, homeDirectory string) (string, error) {
	for _, input := range []struct {
		name      string
		directory string
	}{
		{name: "helper", directory: helperDirectory},
		{name: "home", directory: homeDirectory},
	} {
		name, directory := input.name, input.directory
		if !filepath.IsAbs(directory) || directory != filepath.Clean(directory) || strings.ContainsRune(directory, '\x00') ||
			strings.ContainsRune(directory, os.PathListSeparator) {
			return "", fmt.Errorf("codesk macOS bundle: %s directory must be an absolute PATH-safe path", name)
		}
	}
	candidates := []string{
		helperDirectory,
		filepath.Join(homeDirectory, ".local", "bin"),
		filepath.Join(homeDirectory, ".npm-global", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}
	entries := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for index, candidate := range candidates {
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		info, err := os.Stat(candidate)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && index != 0 {
				continue
			}
			return "", fmt.Errorf("codesk macOS bundle: inspect tool directory %q: %w", candidate, err)
		}
		if !info.IsDir() {
			if index == 0 {
				return "", fmt.Errorf("codesk macOS bundle: helper path %q is not a directory", candidate)
			}
			continue
		}
		entries = append(entries, candidate)
	}
	return strings.Join(entries, string(os.PathListSeparator)), nil
}
