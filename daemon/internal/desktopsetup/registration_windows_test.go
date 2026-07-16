//go:build windows

package desktopsetup

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

func TestRawRegistryStateRoundTripPreservesEveryValueTypeAndByte(t *testing.T) {
	id, err := newTransactionID()
	if err != nil {
		t.Fatal(err)
	}
	parent := `Software\Codesk\DesktopSetupTests`
	path := parent + `\` + id
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
		_ = registry.DeleteKey(registry.CURRENT_USER, parent)
	})

	want := uninstallRegistrationState{
		Existed: true,
		Values: []registryValueState{
			{Name: "", Type: registry.SZ, Data: registryStringBytes("default")},
			{Name: "Binary", Type: registry.BINARY, Data: []byte{0, 1, 2, 0xff}},
			{Name: "DWORD", Type: registry.DWORD, Data: []byte{42, 0, 0, 0}},
			{Name: "Expand", Type: registry.EXPAND_SZ, Data: registryStringBytes(`%LOCALAPPDATA%\Codesk`)},
			{Name: "Multi", Type: registry.MULTI_SZ, Data: registryStringBytes("one\x00two\x00")},
		},
	}
	if err := restoreRawRegistryKey(registry.CURRENT_USER, path, parent, want); err != nil {
		t.Fatal(err)
	}
	captured, err := captureRawRegistryKey(registry.CURRENT_USER, path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(captured, want) {
		t.Fatalf("captured state = %#v, want %#v", captured, want)
	}

	if err := restoreRawRegistryKey(registry.CURRENT_USER, path, parent, uninstallRegistrationState{}); err != nil {
		t.Fatal(err)
	}
	absent, err := captureRawRegistryKey(registry.CURRENT_USER, path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(absent, uninstallRegistrationState{}) {
		t.Fatalf("absent state = %#v", absent)
	}
	if err := restoreRawRegistryKey(registry.CURRENT_USER, path, parent, want); err != nil {
		t.Fatal(err)
	}
	restored, err := captureRawRegistryKey(registry.CURRENT_USER, path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, want) {
		t.Fatalf("restored state = %#v, want %#v", restored, want)
	}
}

func TestCaptureRawRegistryKeyRejectsSubkeys(t *testing.T) {
	id, err := newTransactionID()
	if err != nil {
		t.Fatal(err)
	}
	parent := `Software\Codesk\DesktopSetupTests`
	path := parent + `\` + id
	child := path + `\child`
	key, _, err := registry.CreateKey(registry.CURRENT_USER, child, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, child)
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
		_ = registry.DeleteKey(registry.CURRENT_USER, parent)
	})
	if _, err := captureRawRegistryKey(registry.CURRENT_USER, path); err == nil {
		t.Fatal("captureRawRegistryKey() accepted a registration key with subkeys")
	}
}

func TestCaptureRegistrationFileRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.lnk")
	link := filepath.Join(directory, "Codesk.lnk")
	if err := os.WriteFile(target, []byte("shortcut"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := captureRegistrationFile(link); err == nil {
		t.Fatal("captureRegistrationFile() followed a symlink")
	}
}

func registryStringBytes(value string) []byte {
	units := utf16.Encode([]rune(value))
	units = append(units, 0)
	data := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(data[index*2:], unit)
	}
	return data
}
