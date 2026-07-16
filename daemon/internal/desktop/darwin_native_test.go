//go:build darwin && cgo

package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinKeychainSecretStoreRoundTrip(t *testing.T) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	key := "codesk:test:" + hex.EncodeToString(suffix[:])
	store := NewDarwinKeychainSecretStore()
	defer store.Delete(key)
	secret := []byte("native-keychain-test-secret")
	if err := store.Save(key, secret); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := store.Load(key)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if string(loaded) != string(secret) {
		t.Fatalf("Load() = %q, want %q", loaded, secret)
	}
	if err := store.Delete(key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(key); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	if _, err := store.Load(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() after delete error = %v, want os.ErrNotExist", err)
	}
}

func TestDarwinNativeAdaptersRejectUnsafeInputsBeforeNativeCalls(t *testing.T) {
	store := NewDarwinKeychainSecretStore()
	if err := store.Save("invalid\nkey", []byte("secret")); err == nil {
		t.Fatal("Save() unexpectedly accepted a control character in the key")
	}
	if err := store.Save(SecretKeyDaemonToken, nil); err == nil {
		t.Fatal("Save() unexpectedly accepted an empty secret")
	}
	opener, err := NewDarwinWorkspaceOpener(filepath.Join(t.TempDir(), "Logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"file:///tmp", "https://token@example.com", "/tmp"} {
		if err := opener.Open(target); err == nil {
			t.Fatalf("Open(%q) unexpectedly succeeded", target)
		}
	}
}
