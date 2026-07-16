package desktop

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type prefixProtector struct{}

func (prefixProtector) Protect(secret []byte) ([]byte, error) {
	return append([]byte("protected:"), secret...), nil
}

func (prefixProtector) Unprotect(protected []byte) ([]byte, error) {
	if !bytes.HasPrefix(protected, []byte("protected:")) {
		return nil, errors.New("invalid protected data")
	}
	return append([]byte(nil), protected[len("protected:"):]...), nil
}

func TestFileSecretStoreRoundTripNeverWritesPlaintext(t *testing.T) {
	root := filepath.Join(t.TempDir(), protectedSecretsName)
	store, err := newFileSecretStore(root, prefixProtector{})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("opaque-daemon-token")
	if err := store.Save(SecretKeyDaemonToken, secret); err != nil {
		t.Fatal(err)
	}
	path, err := store.pathForKey(SecretKeyDaemonToken)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(onDisk, secret) || !bytes.HasPrefix(onDisk, []byte("protected:")) {
		t.Fatalf("on-disk secret = %q, want protected bytes", onDisk)
	}
	loaded, err := store.Load(SecretKeyDaemonToken)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, secret) {
		t.Fatalf("Load() = %q, want %q", loaded, secret)
	}
}

func TestFileSecretStoreHashesArbitraryLogicalKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), protectedSecretsName)
	store, err := newFileSecretStore(root, prefixProtector{})
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.pathForKey(`..\..\escape/secret`)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(relative, "..") || filepath.Dir(relative) != "." || !strings.HasSuffix(relative, protectedFileSuffix) {
		t.Fatalf("pathForKey() = %q escaped root %q", path, root)
	}
}

func TestFileSecretStoreRejectsPlaintextProtector(t *testing.T) {
	root := filepath.Join(t.TempDir(), protectedSecretsName)
	store, err := newFileSecretStore(root, identityProtector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("key", []byte("secret")); err == nil {
		t.Fatal("Save() accepted plaintext protector output")
	}
}

type identityProtector struct{}

func (identityProtector) Protect(secret []byte) ([]byte, error) {
	return append([]byte(nil), secret...), nil
}

func (identityProtector) Unprotect(secret []byte) ([]byte, error) {
	return append([]byte(nil), secret...), nil
}

func TestFileSecretStoreDeleteIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), protectedSecretsName)
	store, err := newFileSecretStore(root, prefixProtector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("key", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("key"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("key"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() error = %v, want os.ErrNotExist", err)
	}
}

func TestFileSecretStoreRejectsInvalidInputs(t *testing.T) {
	root := filepath.Join(t.TempDir(), protectedSecretsName)
	store, err := newFileSecretStore(root, prefixProtector{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", " key", "key\n"} {
		if err := store.Save(key, []byte("secret")); err == nil {
			t.Fatalf("Save() accepted key %q", key)
		}
	}
	if err := store.Save("key", nil); err == nil {
		t.Fatal("Save() accepted empty secret")
	}
}
