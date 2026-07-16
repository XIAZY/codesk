//go:build windows

package desktopsetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	setupRunKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	setupRunValueName = "Codesk"
	setupUninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\Codesk`
)

var (
	advapi32RegSetValueEx = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegSetValueExW")
	advapi32RegFlushKey   = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegFlushKey")
)

func captureRegistrationState(paths windowsPaths) (registrationState, error) {
	run, err := readStringRegistryValue(registry.CURRENT_USER, setupRunKey, setupRunValueName)
	if err != nil {
		return registrationState{}, err
	}
	shortcut, err := captureRegistrationFile(paths.Shortcut)
	if err != nil {
		return registrationState{}, fmt.Errorf("desktop setup: inspect Start Menu shortcut: %w", err)
	}
	uninstall, err := captureUninstallRegistration()
	if err != nil {
		return registrationState{}, err
	}
	return registrationState{Run: run, Shortcut: shortcut, Uninstall: uninstall}, nil
}

func registerInstallation(ctx context.Context, paths windowsPaths, version, uninstaller string, loginEnabled bool) error {
	if err := setLoginRegistration(paths.Desktop, loginEnabled); err != nil {
		return err
	}
	if err := writeUninstallRegistration(paths, version, uninstaller); err != nil {
		return err
	}
	if err := createStartMenuShortcut(ctx, paths); err != nil {
		return fmt.Errorf("desktop setup: create Start Menu shortcut: %w", err)
	}
	return nil
}

func removeInstallationRegistration(paths windowsPaths) error {
	return errors.Join(
		removeLoginRegistration(),
		removeStartMenuShortcut(paths.Shortcut),
		removeUninstallRegistration(),
	)
}

func restoreRegistrationState(_ context.Context, paths windowsPaths, state registrationState) error {
	var restoreErrors []error
	if err := restoreStringRegistryValue(registry.CURRENT_USER, setupRunKey, setupRunValueName, state.Run); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	if err := restoreRegistrationFile(paths.Shortcut, state.Shortcut); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	if err := restoreUninstallRegistration(state.Uninstall); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	return errors.Join(restoreErrors...)
}

func setLoginRegistration(executable string, enabled bool) error {
	if !enabled {
		return removeLoginRegistration()
	}
	if err := validateWindowsPath(executable); err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, setupRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("desktop setup: open login registry key: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue(setupRunValueName, quoteExecutable(executable)); err != nil {
		return fmt.Errorf("desktop setup: write login registration: %w", err)
	}
	return flushRegistryKey(key, "login registration")
}

func removeLoginRegistration() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, setupRunKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("desktop setup: open login registry key: %w", err)
	}
	defer key.Close()
	if err := key.DeleteValue(setupRunValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("desktop setup: remove login registration: %w", err)
	}
	return flushRegistryKey(key, "login registration")
}

func writeUninstallRegistration(paths windowsPaths, version, uninstaller string) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	if err := validateWindowsPath(uninstaller); err != nil {
		return err
	}
	estimatedSize, err := installedSizeKB(paths.InstallRoot)
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, setupUninstallKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("desktop setup: open uninstall registry key: %w", err)
	}
	defer key.Close()
	stringsToWrite := map[string]string{
		"DisplayName":          "Codesk",
		"DisplayVersion":       version,
		"Publisher":            "Codesk",
		"DisplayIcon":          paths.Icon,
		"InstallLocation":      paths.InstallRoot,
		"UninstallString":      quoteExecutable(uninstaller) + " --uninstall",
		"QuietUninstallString": quoteExecutable(uninstaller) + " --uninstall --quiet",
	}
	for _, name := range uninstallStringValues {
		if err := key.SetStringValue(name, stringsToWrite[name]); err != nil {
			return fmt.Errorf("desktop setup: write uninstall value %q: %w", name, err)
		}
	}
	for name, value := range map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": estimatedSize} {
		if err := key.SetDWordValue(name, value); err != nil {
			return fmt.Errorf("desktop setup: write uninstall value %q: %w", name, err)
		}
	}
	return flushRegistryKey(key, "uninstall registration")
}

func removeUninstallRegistration() error {
	if err := registry.DeleteKey(registry.CURRENT_USER, setupUninstallKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("desktop setup: remove uninstall registration: %w", err)
	}
	return flushRegistryPath(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall`, "uninstall registration parent")
}

func captureUninstallRegistration() (uninstallRegistrationState, error) {
	return captureRawRegistryKey(registry.CURRENT_USER, setupUninstallKey)
}

func captureRawRegistryKey(root registry.Key, path string) (uninstallRegistrationState, error) {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if errors.Is(err, registry.ErrNotExist) {
		return uninstallRegistrationState{}, nil
	}
	if err != nil {
		return uninstallRegistrationState{}, fmt.Errorf("desktop setup: open registration key: %w", err)
	}
	defer key.Close()
	subkeys, err := key.ReadSubKeyNames(-1)
	if err != nil {
		return uninstallRegistrationState{}, fmt.Errorf("desktop setup: enumerate registration subkeys: %w", err)
	}
	if len(subkeys) != 0 {
		return uninstallRegistrationState{}, errors.New("desktop setup: registration key contains subkeys")
	}
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return uninstallRegistrationState{}, fmt.Errorf("desktop setup: enumerate registration values: %w", err)
	}
	sort.Strings(names)
	state := uninstallRegistrationState{Existed: true, Values: make([]registryValueState, 0, len(names))}
	total := 0
	for _, name := range names {
		value, err := readRawRegistryValue(key, name)
		if err != nil {
			return uninstallRegistrationState{}, err
		}
		total += len(name) + len(value.Data)
		if total > maximumRegistrationBlobBytes {
			return uninstallRegistrationState{}, errors.New("desktop setup: uninstall registration is too large to preserve")
		}
		state.Values = append(state.Values, value)
	}
	return state, nil
}

func restoreUninstallRegistration(state uninstallRegistrationState) error {
	return restoreRawRegistryKey(
		registry.CURRENT_USER,
		setupUninstallKey,
		`Software\Microsoft\Windows\CurrentVersion\Uninstall`,
		state,
	)
}

func restoreRawRegistryKey(root registry.Key, path, parent string, state uninstallRegistrationState) error {
	if err := registry.DeleteKey(root, path); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("desktop setup: clear registration key for restore: %w", err)
	}
	if !state.Existed {
		return flushRegistryPath(root, parent, "registration parent")
	}
	key, _, err := registry.CreateKey(root, path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("desktop setup: open registration key for restore: %w", err)
	}
	defer key.Close()
	for _, value := range state.Values {
		if err := setRawRegistryValue(key, value); err != nil {
			return err
		}
	}
	return flushRegistryKey(key, "restored registration key")
}

func readStringRegistryValue(root registry.Key, path, name string) (stringValueState, error) {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return stringValueState{}, nil
	}
	if err != nil {
		return stringValueState{}, fmt.Errorf("desktop setup: open registry key %q: %w", path, err)
	}
	defer key.Close()
	return readStringValue(key, name)
}

func readStringValue(key registry.Key, name string) (stringValueState, error) {
	value, typ, err := key.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return stringValueState{}, nil
	}
	if err != nil {
		return stringValueState{}, fmt.Errorf("desktop setup: read registry value %q: %w", name, err)
	}
	return stringValueState{Present: true, Value: value, Type: typ}, nil
}

func restoreStringRegistryValue(root registry.Key, path, name string, state stringValueState) error {
	if !state.Present {
		key, err := registry.OpenKey(root, path, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("desktop setup: open registry key %q for restore: %w", path, err)
		}
		defer key.Close()
		if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("desktop setup: remove new registry value %q: %w", name, err)
		}
		return flushRegistryKey(key, "restored registry value")
	}
	key, _, err := registry.CreateKey(root, path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("desktop setup: open registry key %q for restore: %w", path, err)
	}
	defer key.Close()
	if err := restoreStringValue(key, name, state); err != nil {
		return err
	}
	return flushRegistryKey(key, "restored registry value")
}

func restoreStringValue(key registry.Key, name string, state stringValueState) error {
	if !state.Present {
		if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("desktop setup: remove new registry value %q: %w", name, err)
		}
		return nil
	}
	var err error
	switch state.Type {
	case registry.SZ:
		err = key.SetStringValue(name, state.Value)
	case registry.EXPAND_SZ:
		err = key.SetExpandStringValue(name, state.Value)
	default:
		return fmt.Errorf("desktop setup: cannot restore registry value %q with type %d", name, state.Type)
	}
	if err != nil {
		return fmt.Errorf("desktop setup: restore registry value %q: %w", name, err)
	}
	return nil
}

func captureRegistrationFile(path string) (fileValueState, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileValueState{}, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if os.IsNotExist(err) {
		return fileValueState{}, nil
	}
	if err != nil {
		return fileValueState{}, err
	}
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		_ = windows.CloseHandle(handle)
		return fileValueState{}, err
	}
	if handleInfo.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(handle)
		return fileValueState{}, errors.New("path is not a bounded real file")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return fileValueState{}, errors.New("could not open registration file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileValueState{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumRegistrationBlobBytes {
		return fileValueState{}, errors.New("path is not a bounded real file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumRegistrationBlobBytes+1))
	if err != nil {
		return fileValueState{}, err
	}
	if int64(len(data)) != info.Size() {
		return fileValueState{}, errors.New("registration file changed while reading")
	}
	return fileValueState{Present: true, Data: data}, nil
}

func restoreRegistrationFile(path string, state fileValueState) (returnErr error) {
	if !state.Present {
		return removeStartMenuShortcut(path)
	}
	if err := validateRegistrationState(registrationState{Shortcut: state}); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := ensureRealDirectory(parent); err != nil {
		return fmt.Errorf("desktop setup: prepare Start Menu directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("desktop setup: Start Menu shortcut path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: inspect Start Menu shortcut: %w", err)
	}
	temporary, err := os.CreateTemp(parent, ".codesk-shortcut-restore-*")
	if err != nil {
		return fmt.Errorf("desktop setup: create restored Start Menu shortcut: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(state.Data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := movePathDurably(temporaryPath, path, true); err != nil {
		return fmt.Errorf("desktop setup: restore Start Menu shortcut: %w", err)
	}
	return nil
}

func removeStartMenuShortcut(path string) error {
	if err := validateWindowsPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("desktop setup: inspect Start Menu shortcut: %w", err)
	}
	if info.IsDir() {
		return errors.New("desktop setup: Start Menu shortcut path is a directory")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("desktop setup: remove Start Menu shortcut: %w", err)
	}
	return nil
}

func readRawRegistryValue(key registry.Key, name string) (registryValueState, error) {
	size, valueType, err := key.GetValue(name, nil)
	if err != nil {
		return registryValueState{}, fmt.Errorf("desktop setup: read uninstall value %q: %w", name, err)
	}
	if size < 0 || size > maximumRegistrationBlobBytes {
		return registryValueState{}, fmt.Errorf("desktop setup: uninstall value %q is too large", name)
	}
	data := make([]byte, size)
	actualSize, actualType, err := key.GetValue(name, data)
	if err != nil {
		return registryValueState{}, fmt.Errorf("desktop setup: read uninstall value %q: %w", name, err)
	}
	if actualSize != size || actualType != valueType {
		return registryValueState{}, fmt.Errorf("desktop setup: uninstall value %q changed while reading", name)
	}
	return registryValueState{Name: name, Type: valueType, Data: data}, nil
}

func setRawRegistryValue(key registry.Key, value registryValueState) error {
	name, err := windows.UTF16PtrFromString(value.Name)
	if err != nil {
		return fmt.Errorf("desktop setup: restore uninstall value name: %w", err)
	}
	var data uintptr
	if len(value.Data) != 0 {
		data = uintptr(unsafe.Pointer(&value.Data[0]))
	}
	result, _, _ := advapi32RegSetValueEx.Call(
		uintptr(key),
		uintptr(unsafe.Pointer(name)),
		0,
		uintptr(value.Type),
		data,
		uintptr(len(value.Data)),
	)
	if result != 0 {
		return fmt.Errorf("desktop setup: restore uninstall value %q: %w", value.Name, syscall.Errno(result))
	}
	return nil
}

func flushRegistryPath(root registry.Key, path, description string) error {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("desktop setup: open %s for flush: %w", description, err)
	}
	defer key.Close()
	return flushRegistryKey(key, description)
}

func flushRegistryKey(key registry.Key, description string) error {
	result, _, _ := advapi32RegFlushKey.Call(uintptr(key))
	if result != 0 {
		return fmt.Errorf("desktop setup: flush %s: %w", description, syscall.Errno(result))
	}
	return nil
}

func installedSizeKB(root string) (uint32, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("desktop setup: installed path %q is a symbolic link", path)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return fmt.Errorf("desktop setup: installed path %q is not a regular file", path)
		}
		total += uint64(info.Size())
		if total > uint64(math.MaxUint32)*1024 {
			return errors.New("desktop setup: installed size exceeds registry limit")
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("desktop setup: calculate installed size: %w", err)
	}
	return uint32((total + 1023) / 1024), nil
}

func quoteExecutable(path string) string {
	return `"` + strings.TrimSpace(path) + `"`
}
