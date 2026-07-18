//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=13.0
#cgo LDFLAGS: -framework Cocoa -framework Security -framework ServiceManagement

#include <stdlib.h>
#include "darwin_native.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"notty/daemon/internal/desktopstate"
)

const (
	darwinKeychainService = "com.getcodesk.desktop"
	darwinMaximumSecret   = 64 << 10
)

type darwinKeychainSecretStore struct{}

// NewDarwinKeychainSecretStore returns the current-user Keychain adapter for
// the fixed Codesk desktop service identity.
func NewDarwinKeychainSecretStore() desktopstate.SecretStore {
	return darwinKeychainSecretStore{}
}

func (darwinKeychainSecretStore) Save(key string, secret []byte) error {
	if err := validateDarwinSecretKey(key); err != nil {
		return err
	}
	if len(secret) == 0 || len(secret) > darwinMaximumSecret {
		return errors.New("desktop: invalid Keychain secret size")
	}
	service := C.CString(darwinKeychainService)
	account := C.CString(key)
	secretCopy := C.CBytes(secret)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	defer C.codesk_secret_free(secretCopy, C.size_t(len(secret)))
	var nativeError *C.char
	result := C.codesk_keychain_save(service, account, secretCopy, C.size_t(len(secret)), &nativeError)
	return darwinNativeError("save Keychain credential", result, nativeError)
}

func (darwinKeychainSecretStore) Load(key string) ([]byte, error) {
	if err := validateDarwinSecretKey(key); err != nil {
		return nil, err
	}
	service := C.CString(darwinKeychainService)
	account := C.CString(key)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	var secret unsafe.Pointer
	var secretLength C.size_t
	var nativeError *C.char
	result := C.codesk_keychain_load(service, account, &secret, &secretLength, &nativeError)
	if result == C.CODESK_NATIVE_NOT_FOUND {
		if nativeError != nil {
			C.codesk_error_free(nativeError)
		}
		return nil, os.ErrNotExist
	}
	if err := darwinNativeError("load Keychain credential", result, nativeError); err != nil {
		return nil, err
	}
	if secret == nil || secretLength == 0 || uint64(secretLength) > darwinMaximumSecret {
		if secret != nil {
			C.codesk_secret_free(secret, secretLength)
		}
		return nil, errors.New("desktop: Keychain returned an invalid secret size")
	}
	defer C.codesk_secret_free(secret, secretLength)
	return C.GoBytes(secret, C.int(secretLength)), nil
}

func (darwinKeychainSecretStore) Delete(key string) error {
	if err := validateDarwinSecretKey(key); err != nil {
		return err
	}
	service := C.CString(darwinKeychainService)
	account := C.CString(key)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	var nativeError *C.char
	result := C.codesk_keychain_delete(service, account, &nativeError)
	return darwinNativeError("delete Keychain credential", result, nativeError)
}

func validateDarwinSecretKey(key string) error {
	if key == "" || len(key) > 256 || key != strings.TrimSpace(key) || !utf8.ValidString(key) {
		return errors.New("desktop: invalid Keychain key")
	}
	for _, character := range key {
		if unicode.IsControl(character) {
			return errors.New("desktop: invalid Keychain key")
		}
	}
	return nil
}

type darwinLoginItem struct{}

func NewDarwinLoginItem() LoginItem {
	return darwinLoginItem{}
}

func (darwinLoginItem) Enable() error {
	var nativeError *C.char
	result := C.codesk_login_item_enable(&nativeError)
	return darwinNativeError("enable launch at login", result, nativeError)
}

func (darwinLoginItem) Disable() error {
	var nativeError *C.char
	result := C.codesk_login_item_disable(&nativeError)
	return darwinNativeError("disable launch at login", result, nativeError)
}

func (darwinLoginItem) IsEnabled() (bool, error) {
	var enabled C.int
	var nativeError *C.char
	result := C.codesk_login_item_is_enabled(&enabled, &nativeError)
	if err := darwinNativeError("read launch at login", result, nativeError); err != nil {
		return false, err
	}
	return enabled == 1, nil
}

type darwinWorkspaceOpener struct {
	logsDir string
}

func NewDarwinWorkspaceOpener(logsDir string) (OpenURL, error) {
	if err := requireAbsolute("logs", logsDir); err != nil {
		return nil, err
	}
	return darwinWorkspaceOpener{logsDir: logsDir}, nil
}

func (o darwinWorkspaceOpener) Open(target string) error {
	isDirectory, err := validateOpenTarget(target, o.logsDir)
	if err != nil {
		return err
	}
	nativeTarget := C.CString(target)
	defer C.free(unsafe.Pointer(nativeTarget))
	var nativeError *C.char
	directory := C.int(0)
	if isDirectory {
		directory = 1
	}
	result := C.codesk_workspace_open(nativeTarget, directory, &nativeError)
	return darwinNativeError("open target", result, nativeError)
}

// ShowDarwinFatalError presents a modal Cocoa alert without changing the
// accessory application's Dock policy.
func ShowDarwinFatalError(message string) {
	message = strings.ReplaceAll(message, "\x00", "")
	if len(message) > 4096 {
		message = message[:4096]
		for len(message) > 0 && !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	nativeMessage := C.CString(message)
	defer C.free(unsafe.Pointer(nativeMessage))
	C.codesk_show_fatal_error(nativeMessage)
}

func darwinNativeError(operation string, result C.int, nativeError *C.char) error {
	if nativeError != nil {
		defer C.codesk_error_free(nativeError)
	}
	if result == C.CODESK_NATIVE_OK {
		return nil
	}
	detail := "native macOS operation failed"
	if nativeError != nil {
		if value := C.GoString(nativeError); value != "" {
			detail = value
		}
	}
	return fmt.Errorf("desktop: %s: %s", operation, detail)
}
