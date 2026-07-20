//go:build darwin && cgo

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"notty/daemon/internal/buildinfo"
	"notty/daemon/internal/desktop"
	"notty/daemon/internal/desktopapp"
	"notty/daemon/internal/desktopstate"
	"notty/daemon/internal/macosapp"
)

var (
	codeskOrigin  = "https://app.getcodesk.com"
	backendOrigin  = "https://api.getcodesk.com"

	//go:embed assets/codesk-tray-template.png
	codeskTemplateIcon []byte
)

func main() {
	handled, err := desktop.RunDarwinProcessWatchdog(os.Args[1:])
	if handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := runDesktop(); err != nil {
		desktop.ShowDarwinFatalError(err.Error() + "\n\nSee ~/Library/Logs/Codesk for details.")
		os.Exit(1)
	}
}

func runDesktop() error {
	bundle, err := macosapp.ResolveCurrent()
	if err != nil {
		return err
	}
	dirs, err := desktop.DefaultDirs()
	if err != nil {
		return err
	}
	if err := dirs.Validate(); err != nil {
		return err
	}
	for _, directory := range []string{dirs.Data, dirs.Logs, dirs.Cache} {
		if err := ensureDarwinPrivateDirectory(directory); err != nil {
			return err
		}
	}

	instanceLock, err := desktop.NewDarwinInstanceLock(filepath.Join(dirs.Data, "Locks", "desktop.lock"))
	if err != nil {
		return err
	}
	acquired, err := instanceLock.Acquire()
	if err != nil {
		return fmt.Errorf("acquire single-instance lock: %w", err)
	}
	if !acquired {
		return nil
	}
	defer instanceLock.Release()
	if err := desktop.ContainDarwinProcessTree(bundle.Executable); err != nil {
		return err
	}
	if err := bundle.ConfigureHelperPath(); err != nil {
		return err
	}

	logFile, err := openDarwinPrivateLog(filepath.Join(dirs.Logs, "codesk-desktop.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(logFile, "codesk-desktop: ", log.Ldate|log.Ltime|log.LUTC)
	log.SetOutput(logFile)
	log.SetPrefix("codesk-daemon: ")
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)

	configStore, err := desktopstate.NewFileConfigurationStore(dirs.Data)
	if err != nil {
		return err
	}
	opener, err := desktop.NewDarwinWorkspaceOpener(dirs.Logs)
	if err != nil {
		return err
	}
	app, err := desktopapp.New(desktopapp.Options{
		Dirs:          dirs,
		CodeskOrigin:  codeskOrigin,
		BackendOrigin: backendOrigin,
		Version:       buildinfo.Version,
		ConfigStore:   configStore,
		Secrets:       desktop.NewDarwinKeychainSecretStore(),
		LoginItem:     desktop.NewDarwinLoginItem(),
		Opener:        opener,
		Logger:        logger,
	})
	if err != nil {
		return err
	}

	parent, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := app.Start(parent); err != nil {
		return err
	}
	defer app.Shutdown()
	return runNativeTray(app.Context(), nativeTrayOptions{
		Initial:      app.Menu(),
		Updates:      app.Updates(),
		Actions:      app.Actions(),
		TemplateIcon: codeskTemplateIcon,
		ReportError:  func(err error) { logger.Printf("tray update: %v", err) },
	})
}

func ensureDarwinPrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create desktop directory %q: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect desktop directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("desktop directory %q is not a real directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect desktop directory %q: %w", path, err)
	}
	return nil
}

func openDarwinPrivateLog(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_APPEND|unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open desktop log: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap desktop log descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect desktop log: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("desktop log is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("protect desktop log: %w", err)
	}
	return file, nil
}
