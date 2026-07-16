//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"

	"golang.org/x/sys/windows"

	"notty/daemon/internal/desktopsetup"
)

var (
	setupVersion = "dev"
	setupArch    = "unknown"
)

func main() {
	os.Exit(runMain(os.Args[1:]))
}

func runMain(arguments []string) int {
	return desktopsetup.RunMain(arguments, setupVersion, setupArch, runSetup, func(kind desktopsetup.MessageKind, message string) {
		flags := uint32(windows.MB_OK | windows.MB_ICONINFORMATION)
		if kind == desktopsetup.ErrorMessage {
			flags = windows.MB_OK | windows.MB_ICONERROR
		}
		showSetupMessage(flags, message)
	})
}

func runSetup(options desktopsetup.Options) error {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_CREATE)
	if err != nil {
		return fmt.Errorf("resolve local application data directory: %w", err)
	}
	logs := filepath.Join(localAppData, "Codesk", "Logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return fmt.Errorf("create setup log directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logs, "codesk-setup.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open setup log: %w", err)
	}
	defer logFile.Close()
	logger := log.New(logFile, "codesk-setup: ", log.Ldate|log.Ltime|log.LUTC)
	logger.Printf("starting version=%s arch=%s uninstall=%t", options.Version, options.Arch, options.Uninstall)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := desktopsetup.Run(ctx, options); err != nil {
		logger.Printf("failed: %v", err)
		return err
	}
	logger.Print("completed")
	return nil
}

func showSetupMessage(flags uint32, message string) {
	title, titleErr := windows.UTF16PtrFromString("Codesk Setup")
	text, textErr := windows.UTF16PtrFromString(message)
	if titleErr != nil || textErr != nil {
		return
	}
	_, _ = windows.MessageBox(0, text, title, flags)
}
