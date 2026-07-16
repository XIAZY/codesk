package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSecretKeyBytes    = 512
	maxSecretBytes       = 1 << 20
	maxProtectedBytes    = 2 << 20
	protectedFileSuffix  = ".dpapi"
	protectedSecretsName = "Secrets"
)

type secretProtector interface {
	Protect([]byte) ([]byte, error)
	Unprotect([]byte) ([]byte, error)
}

type fileSecretStore struct {
	root      string
	protector secretProtector
}

func newFileSecretStore(root string, protector secretProtector) (*fileSecretStore, error) {
	if err := requireAbsolute("secrets", root); err != nil {
		return nil, err
	}
	if protector == nil {
		return nil, errors.New("desktop: secret protector is required")
	}
	return &fileSecretStore{root: root, protector: protector}, nil
}

func (s *fileSecretStore) Save(key string, secret []byte) error {
	path, err := s.pathForKey(key)
	if err != nil {
		return err
	}
	if len(secret) == 0 || len(secret) > maxSecretBytes {
		return errors.New("desktop: secret has invalid size")
	}
	protected, err := s.protector.Protect(secret)
	if err != nil {
		return errors.New("desktop: protect secret failed")
	}
	if len(protected) == 0 || len(protected) > maxProtectedBytes {
		return errors.New("desktop: protected secret has invalid size")
	}
	if bytes.Equal(protected, secret) {
		return errors.New("desktop: protector returned plaintext")
	}
	if err := writePrivateFileAtomically(path, protected); err != nil {
		return fmt.Errorf("desktop: persist protected secret: %w", err)
	}
	return nil
}

func (s *fileSecretStore) Load(key string) ([]byte, error) {
	path, err := s.pathForKey(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	protected, err := io.ReadAll(io.LimitReader(file, maxProtectedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("desktop: read protected secret: %w", err)
	}
	if len(protected) == 0 || len(protected) > maxProtectedBytes {
		return nil, errors.New("desktop: protected secret has invalid size")
	}
	secret, err := s.protector.Unprotect(protected)
	if err != nil {
		return nil, errors.New("desktop: unprotect secret failed")
	}
	if len(secret) == 0 || len(secret) > maxSecretBytes {
		return nil, errors.New("desktop: unprotected secret has invalid size")
	}
	return secret, nil
}

func (s *fileSecretStore) Delete(key string) error {
	path, err := s.pathForKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop: delete protected secret: %w", err)
	}
	return nil
}

func (s *fileSecretStore) pathForKey(key string) (string, error) {
	if s == nil || s.root == "" || s.protector == nil {
		return "", errors.New("desktop: secret store is not initialized")
	}
	if key == "" || len(key) > maxSecretKeyBytes || key != strings.TrimSpace(key) || !utf8.ValidString(key) {
		return "", errors.New("desktop: invalid secret key")
	}
	for _, char := range key {
		if unicode.IsControl(char) {
			return "", errors.New("desktop: invalid secret key")
		}
	}
	digest := sha256.Sum256([]byte(key))
	filename := hex.EncodeToString(digest[:]) + protectedFileSuffix
	return filepath.Join(s.root, filename), nil
}
