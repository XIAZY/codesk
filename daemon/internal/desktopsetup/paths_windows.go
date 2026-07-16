//go:build windows

package desktopsetup

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	desktopExecutableName   = "Codesk.exe"
	agentToolExecutableName = "notty-agent-tool.exe"
	iconFilename            = "codesk.ico"
	uninstallerFilename     = "Uninstall Codesk.exe"
	shortcutFilename        = "Codesk.lnk"
)

type windowsPaths struct {
	DataRoot         string
	SetupRoot        string
	SetupLock        string
	DesktopLock      string
	InstallParent    string
	InstallRoot      string
	Desktop          string
	AgentTool        string
	Icon             string
	Shortcut         string
	Startup          string
	LegacyDaemon     string
	SystemPowerShell string
}

func resolveWindowsPaths() (windowsPaths, error) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, fmt.Errorf("desktop setup: resolve local application data directory: %w", err)
	}
	userPrograms, err := windows.KnownFolderPath(windows.FOLDERID_UserProgramFiles, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, fmt.Errorf("desktop setup: resolve per-user programs directory: %w", err)
	}
	programs, err := windows.KnownFolderPath(windows.FOLDERID_Programs, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, fmt.Errorf("desktop setup: resolve Start Menu programs directory: %w", err)
	}
	startup, err := windows.KnownFolderPath(windows.FOLDERID_Startup, windows.KF_FLAG_CREATE)
	if err != nil {
		return windowsPaths{}, fmt.Errorf("desktop setup: resolve Startup directory: %w", err)
	}
	profile, err := windows.KnownFolderPath(windows.FOLDERID_Profile, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return windowsPaths{}, fmt.Errorf("desktop setup: resolve user profile: %w", err)
	}
	system, err := windows.KnownFolderPath(windows.FOLDERID_System, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return windowsPaths{}, fmt.Errorf("desktop setup: resolve Windows system directory: %w", err)
	}

	dataRoot := filepath.Join(localAppData, "Codesk")
	installRoot := filepath.Join(userPrograms, "Codesk")
	paths := windowsPaths{
		DataRoot:         dataRoot,
		SetupRoot:        filepath.Join(dataRoot, "Setup"),
		SetupLock:        filepath.Join(dataRoot, "Locks", "setup.lock"),
		DesktopLock:      filepath.Join(dataRoot, "Locks", "desktop.lock"),
		InstallParent:    userPrograms,
		InstallRoot:      installRoot,
		Desktop:          filepath.Join(installRoot, desktopExecutableName),
		AgentTool:        filepath.Join(installRoot, agentToolExecutableName),
		Icon:             filepath.Join(installRoot, iconFilename),
		Shortcut:         filepath.Join(programs, shortcutFilename),
		Startup:          startup,
		LegacyDaemon:     filepath.Join(profile, ".notty", "bin", "notty-daemon.exe"),
		SystemPowerShell: filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe"),
	}
	for _, path := range []string{
		paths.DataRoot, paths.SetupRoot, paths.SetupLock, paths.DesktopLock,
		paths.InstallParent, paths.InstallRoot, paths.Desktop, paths.AgentTool, paths.Icon,
		paths.Shortcut, paths.Startup, paths.LegacyDaemon, paths.SystemPowerShell,
	} {
		if err := validateWindowsPath(path); err != nil {
			return windowsPaths{}, err
		}
	}
	return paths, nil
}

func installedUninstallerPath(paths windowsPaths, version string) (string, error) {
	if err := validateVersion(version); err != nil {
		return "", err
	}
	path := filepath.Join(paths.SetupRoot, version, uninstallerFilename)
	if err := validateWindowsPath(path); err != nil {
		return "", err
	}
	return path, nil
}

func validateWindowsPath(path string) error {
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\x00\"") || !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return errors.New("desktop setup: resolved an invalid Windows path")
	}
	return nil
}
