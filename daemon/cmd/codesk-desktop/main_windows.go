//go:build windows

package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"

	"golang.org/x/sys/windows"

	"notty/daemon/internal/buildinfo"
	"notty/daemon/internal/desktop"
	"notty/daemon/internal/desktopapp"
	"notty/daemon/internal/desktopstate"
)

const (
	loginItemName = "Codesk"
)

var (
	codeskOrigin  = "https://app.getcodesk.com"
	backendOrigin = "https://api.getcodesk.com"

	//go:embed assets/codesk.ico
	codeskIcon []byte
)

func main() {
	if err := runDesktop(); err != nil {
		showFatalError(err)
		os.Exit(1)
	}
}

func runDesktop() error {
	dirs, err := desktop.DefaultDirs()
	if err != nil {
		return err
	}
	if err := dirs.Validate(); err != nil {
		return err
	}
	instanceLock := desktop.NewWindowsInstanceLock(filepath.Join(dirs.Data, "Locks", "desktop.lock"))
	acquired, err := instanceLock.Acquire()
	if err != nil {
		return fmt.Errorf("acquire single-instance lock: %w", err)
	}
	if !acquired {
		return nil
	}
	defer instanceLock.Release()
	if err := desktop.ContainWindowsProcessTree(); err != nil {
		return err
	}

	if err := os.MkdirAll(dirs.Logs, 0o700); err != nil {
		return fmt.Errorf("create logs directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(dirs.Logs, "codesk-desktop.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open desktop log: %w", err)
	}
	defer logFile.Close()
	logger := log.New(logFile, "codesk-desktop: ", log.Ldate|log.Ltime|log.LUTC)

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve desktop executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve absolute desktop executable: %w", err)
	}
	if err := prependExecutableDirectoryToPath(executable); err != nil {
		return err
	}
	configStore, err := desktopstate.NewFileConfigurationStore(dirs.Data)
	if err != nil {
		return err
	}
	secrets, err := desktopstate.NewWindowsSecretStore(dirs.Data)
	if err != nil {
		return err
	}
	loginItem, err := desktop.NewWindowsLoginItem(loginItemName, executable)
	if err != nil {
		return err
	}

	app, err := desktopapp.New(desktopapp.Options{
		Dirs:          dirs,
		CodeskOrigin:  codeskOrigin,
		BackendOrigin: backendOrigin,
		Version:       buildinfo.Version,
		ConfigStore:   configStore,
		Secrets:       secrets,
		LoginItem:     loginItem,
		Opener:        desktop.NewWindowsShellOpener(),
		Logger:        logger,
	})
	if err != nil {
		return err
	}

	parent, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()
	if err := app.Start(parent); err != nil {
		return err
	}
	err = runNativeTray(app.Context(), nativeTrayOptions{
		Initial:     app.Menu(),
		Updates:     app.Updates(),
		Actions:     app.Actions(),
		Icon:        codeskIcon,
		ReportError: func(err error) { logger.Printf("tray update: %v", err) },
	})
	app.Shutdown()
	return err
}

func showFatalError(err error) {
	if err == nil {
		return
	}
	title, _ := windows.UTF16PtrFromString("Codesk")
	message, conversionErr := windows.UTF16PtrFromString("Codesk could not start.\n\n" + err.Error() + "\n\nSee the Codesk Logs folder under your current-user Local AppData directory for details.")
	if conversionErr != nil {
		return
	}
	_, _ = windows.MessageBox(0, message, title, windows.MB_OK|windows.MB_ICONERROR)
}
