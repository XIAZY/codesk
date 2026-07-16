//go:build windows

package desktopsetup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"notty/daemon/internal/winlock"
)

const maximumSetupExecutableBytes = 1 << 30

func Run(ctx context.Context, options Options) (returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateOptions(options); err != nil {
		return err
	}
	currentExecutable, err := currentExecutablePath()
	if err != nil {
		return err
	}
	paths, err := resolveWindowsPaths()
	if err != nil {
		return err
	}
	lock, err := acquireSetupLock(paths.SetupLock)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, lock.Close())
	}()

	desktopLock := winlock.New(paths.DesktopLock)
	acquired, err := desktopLock.Acquire()
	if err != nil {
		return fmt.Errorf("desktop setup: acquire desktop state lock: %w", err)
	}
	if !acquired {
		return errors.New("desktop setup: Codesk is running; quit Codesk and run setup again")
	}
	desktopLockHeld := true
	defer func() {
		if desktopLockHeld {
			returnErr = errors.Join(returnErr, desktopLock.Release())
		}
	}()
	if options.Uninstall {
		if windowsPathWithin(currentExecutable, paths.InstallRoot) {
			return errors.New("desktop setup: registered uninstaller must run outside the install directory")
		}
		if err := uninstall(ctx, paths); err != nil {
			return err
		}
		if windowsPathWithin(currentExecutable, paths.SetupRoot) {
			return scheduleSelfDelete(paths.SystemPowerShell, paths.SetupRoot)
		}
		return nil
	}
	if windowsPathWithin(currentExecutable, paths.InstallRoot) {
		return errors.New("desktop setup: setup must run outside the install directory")
	}
	payload, err := OpenPayload(currentExecutable)
	if err != nil {
		return err
	}
	if _, err := payload.Verify(options.Version, options.Arch); err != nil {
		return err
	}
	if err := install(ctx, paths, currentExecutable, payload, options); err != nil {
		return err
	}
	if options.NoLaunch {
		return nil
	}
	if err := desktopLock.Release(); err != nil {
		return fmt.Errorf("desktop setup: release desktop state lock: %w", err)
	}
	desktopLockHeld = false
	if err := startDetached(paths.Desktop); err != nil {
		return fmt.Errorf("desktop setup: launch Codesk: %w", err)
	}
	return nil
}

func install(
	ctx context.Context,
	paths windowsPaths,
	currentExecutable string,
	payload *Payload,
	options Options,
) (returnErr error) {
	if err := recoverInstallState(paths.InstallRoot, func(state registrationState) error {
		return restoreRegistrationState(ctx, paths, state)
	}); err != nil {
		return err
	}
	priorRegistration, err := captureRegistrationState(paths)
	if err != nil {
		return err
	}
	uninstaller, createdUninstaller, err := prepareInstalledUninstaller(currentExecutable, paths, options.Version)
	if err != nil {
		return err
	}
	keepUninstaller := !createdUninstaller
	defer func() {
		if keepUninstaller {
			return
		}
		removeErr := os.Remove(uninstaller)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			removeErr = os.Remove(filepath.Dir(uninstaller))
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
		}
		returnErr = errors.Join(returnErr, removeErr)
	}()

	stagingPath, err := newSiblingPath(paths.InstallRoot, "stage")
	if err != nil {
		return err
	}
	backupPath, err := newSiblingPath(paths.InstallRoot, "backup")
	if err != nil {
		return err
	}
	if err := payload.Extract(stagingPath); err != nil {
		return err
	}
	stagingActive := true
	defer func() {
		if stagingActive {
			returnErr = errors.Join(returnErr, removeRealDirectoryIfPresent(stagingPath))
		}
	}()
	if err := os.Remove(filepath.Join(stagingPath, "payload.json")); err != nil {
		return fmt.Errorf("desktop setup: remove extracted payload manifest: %w", err)
	}
	transaction, err := newDirectoryTransaction(paths.InstallRoot, stagingPath, backupPath)
	if err != nil {
		return err
	}
	if err := transaction.Prepare(priorRegistration); err != nil {
		return err
	}
	uncommitted := true
	defer func() {
		if !uncommitted || returnErr == nil {
			return
		}
		if !transaction.rollbackAllowed() {
			return
		}
		rollbackErr := transaction.Rollback()
		restoreErr := restoreRegistrationState(ctx, paths, priorRegistration)
		var forgetErr error
		if rollbackErr == nil && restoreErr == nil {
			forgetErr = transaction.Forget()
		}
		returnErr = errors.Join(returnErr, rollbackErr, restoreErr, forgetErr)
	}()
	if err := removeLoginRegistration(); err != nil {
		return err
	}
	if err := cleanupLegacyLaunchers(ctx, paths); err != nil {
		return fmt.Errorf("desktop setup: remove legacy launchers: %w", err)
	}
	if err := stopProcessesAtPaths(ctx, paths.LegacyDaemon); err != nil {
		return err
	}

	if err := transaction.Swap(); err != nil {
		return err
	}
	stagingActive = false

	loginEnabled := !transaction.hadInstall || priorRegistration.Run.Present
	if err := registerInstallation(ctx, paths, options.Version, uninstaller, loginEnabled); err != nil {
		return fmt.Errorf("desktop setup: register installed version: %w", err)
	}
	committedRegistration, err := captureRegistrationState(paths)
	if err != nil {
		return fmt.Errorf("desktop setup: capture committed registration: %w", err)
	}
	if err := transaction.Commit(committedRegistration); err != nil {
		if !transaction.rollbackAllowed() {
			keepUninstaller = true
			uncommitted = false
		}
		return err
	}
	keepUninstaller = true
	uncommitted = false
	if err := transaction.Complete(); err != nil {
		return err
	}
	return nil
}

func uninstall(ctx context.Context, paths windowsPaths) (returnErr error) {
	if err := recoverInstallState(paths.InstallRoot, func(state registrationState) error {
		return restoreRegistrationState(ctx, paths, state)
	}); err != nil {
		return err
	}
	priorRegistration, err := captureRegistrationState(paths)
	if err != nil {
		return err
	}
	if err := cleanupLegacyLaunchers(ctx, paths); err != nil {
		return fmt.Errorf("desktop setup: remove legacy launchers: %w", err)
	}
	if err := stopProcessesAtPaths(ctx, paths.LegacyDaemon); err != nil {
		return err
	}
	tombstone, err := newSiblingPath(paths.InstallRoot, "remove")
	if err != nil {
		return err
	}
	transaction, err := newUninstallTransaction(paths.InstallRoot, tombstone)
	if err != nil {
		return err
	}
	if err := transaction.Prepare(priorRegistration); err != nil {
		return err
	}
	uncommitted := true
	defer func() {
		if !uncommitted || returnErr == nil {
			return
		}
		if !transaction.rollbackAllowed() {
			return
		}
		rollbackErr := transaction.Rollback()
		restoreErr := restoreRegistrationState(ctx, paths, priorRegistration)
		var forgetErr error
		if rollbackErr == nil && restoreErr == nil {
			forgetErr = transaction.Forget()
		}
		returnErr = errors.Join(returnErr, rollbackErr, restoreErr, forgetErr)
	}()
	if err := transaction.Tombstone(); err != nil {
		return err
	}
	if err := removeInstallationRegistration(paths); err != nil {
		return err
	}
	committedRegistration, err := captureRegistrationState(paths)
	if err != nil {
		return fmt.Errorf("desktop setup: verify removed registration: %w", err)
	}
	if committedRegistration.Run.Present || committedRegistration.Shortcut.Present || committedRegistration.Uninstall.Existed {
		return errors.New("desktop setup: installation registration remains after removal")
	}
	if err := transaction.Commit(committedRegistration); err != nil {
		if !transaction.rollbackAllowed() {
			uncommitted = false
		}
		return err
	}
	uncommitted = false
	if err := transaction.Complete(); err != nil {
		return err
	}
	return nil
}

func prepareInstalledUninstaller(currentExecutable string, paths windowsPaths, version string) (string, bool, error) {
	uninstaller, err := installedUninstallerPath(paths, version)
	if err != nil {
		return "", false, err
	}
	parent := filepath.Dir(uninstaller)
	parentExists, err := realDirectoryPresent(parent)
	if err != nil {
		return "", false, fmt.Errorf("desktop setup: inspect uninstaller directory: %w", err)
	}
	if !parentExists {
		if err := ensureRealDirectory(parent); err != nil {
			return "", false, fmt.Errorf("desktop setup: create uninstaller directory: %w", err)
		}
	}
	if _, err := os.Lstat(uninstaller); errors.Is(err, os.ErrNotExist) {
		if err := copyRegularFile(currentExecutable, uninstaller, 0o700); err != nil {
			if !parentExists {
				_ = os.Remove(parent)
			}
			return "", false, fmt.Errorf("desktop setup: install uninstaller: %w", err)
		}
		return uninstaller, true, nil
	} else if err != nil {
		return "", false, fmt.Errorf("desktop setup: inspect installed uninstaller: %w", err)
	}
	equal, err := regularFilesEqual(currentExecutable, uninstaller)
	if err != nil {
		return "", false, fmt.Errorf("desktop setup: verify installed uninstaller: %w", err)
	}
	if !equal {
		return "", false, errors.New("desktop setup: installed uninstaller does not match this release")
	}
	return uninstaller, false, nil
}

func regularFilesEqual(firstPath, secondPath string) (bool, error) {
	firstInfo, err := os.Lstat(firstPath)
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Lstat(secondPath)
	if err != nil {
		return false, err
	}
	if !firstInfo.Mode().IsRegular() || !secondInfo.Mode().IsRegular() ||
		firstInfo.Size() <= 0 || firstInfo.Size() > maximumSetupExecutableBytes ||
		firstInfo.Size() != secondInfo.Size() {
		return false, nil
	}
	if os.SameFile(firstInfo, secondInfo) {
		return true, nil
	}
	firstHash, err := hashRegularFile(firstPath, firstInfo)
	if err != nil {
		return false, err
	}
	secondHash, err := hashRegularFile(secondPath, secondInfo)
	if err != nil {
		return false, err
	}
	return firstHash == secondHash, nil
}

func hashRegularFile(path string, expected os.FileInfo) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	if !info.Mode().IsRegular() || info.Size() != expected.Size() || !os.SameFile(info, expected) {
		return [sha256.Size]byte{}, errors.New("file changed before hashing")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximumSetupExecutableBytes+1))
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	if written != expected.Size() {
		return [sha256.Size]byte{}, errors.New("file changed while hashing")
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func currentExecutablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("desktop setup: resolve current executable: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("desktop setup: resolve absolute current executable: %w", err)
	}
	path = filepath.Clean(path)
	if err := validateWindowsPath(path); err != nil {
		return "", err
	}
	return path, nil
}

func windowsPathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func removeRealDirectoryIfPresent(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return removeRealDirectory(path)
}

func copyRegularFile(sourcePath, destinationPath string, mode os.FileMode) (returnErr error) {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumSetupExecutableBytes {
		return errors.New("source is not a bounded regular file")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	openedInfo, err := source.Stat()
	if err != nil {
		return err
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() || !os.SameFile(info, openedInfo) {
		return errors.New("source changed before copying")
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = destination.Close()
		}
		if returnErr != nil {
			_ = os.Remove(destinationPath)
		}
	}()
	written, err := io.Copy(destination, io.LimitReader(source, maximumSetupExecutableBytes+1))
	if err != nil {
		return err
	}
	if written != info.Size() {
		return errors.New("source changed while copying")
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	closed = true
	return nil
}
