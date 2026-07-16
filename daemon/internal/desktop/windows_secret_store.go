//go:build windows

package desktop

import (
	"errors"
	"math"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type dpapiProtector struct{}

func NewWindowsSecretStore(dataDir string) (SecretStore, error) {
	if err := requireAbsolute("data", dataDir); err != nil {
		return nil, err
	}
	return newFileSecretStore(filepath.Join(dataDir, protectedSecretsName), dpapiProtector{})
}

func (dpapiProtector) Protect(secret []byte) ([]byte, error) {
	input, err := dataBlob(secret)
	if err != nil {
		return nil, err
	}
	description, err := windows.UTF16PtrFromString("Codesk desktop credential")
	if err != nil {
		return nil, err
	}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, description, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer freeDataBlob(&output)
	return copyDataBlob(output)
}

func (dpapiProtector) Unprotect(protected []byte) ([]byte, error) {
	input, err := dataBlob(protected)
	if err != nil {
		return nil, err
	}
	var description *uint16
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, &description, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	if description != nil {
		_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(description))))
	}
	defer freeDataBlob(&output)
	return copyDataBlob(output)
}

func dataBlob(data []byte) (windows.DataBlob, error) {
	if len(data) == 0 || uint64(len(data)) > math.MaxUint32 {
		return windows.DataBlob{}, errors.New("desktop: invalid DPAPI input size")
	}
	return windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}, nil
}

func copyDataBlob(blob windows.DataBlob) ([]byte, error) {
	if blob.Data == nil || blob.Size == 0 {
		return nil, errors.New("desktop: DPAPI returned empty data")
	}
	return append([]byte(nil), unsafe.Slice(blob.Data, int(blob.Size))...), nil
}

func freeDataBlob(blob *windows.DataBlob) {
	if blob == nil || blob.Data == nil {
		return
	}
	_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(blob.Data))))
	blob.Data = nil
	blob.Size = 0
}
