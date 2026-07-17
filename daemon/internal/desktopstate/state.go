// Package desktopstate owns the durable, token-safe desktop configuration and
// credential contracts shared by product composition roots and native
// acceptance adapters. Platform credential mechanics remain build-tagged.
package desktopstate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const SecretKeyDaemonToken = "codesk:daemon-token"

type SecretStore interface {
	Save(key string, secret []byte) error
	Load(key string) ([]byte, error)
	Delete(key string) error
}

// Fingerprint identifies exact persisted bytes without exposing their path or
// content. An absent file is represented by the zero value.
type Fingerprint struct {
	Present bool   `json:"present"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

func fingerprint(data []byte) Fingerprint {
	digest := sha256.Sum256(data)
	return Fingerprint{Present: true, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}
}

func readStateFile(path, label string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("desktop: inspect %s failed", label)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("desktop: %s is not a regular file", label)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("desktop: open %s failed", label)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("desktop: %s changed while opening", label)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("desktop: read %s failed", label)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("desktop: %s is too large", label)
	}
	return data, nil
}

func RequireAbsolute(name, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("desktop: %s directory is empty", name)
	}
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("desktop: %s directory %q has surrounding whitespace", name, path)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("desktop: %s directory %q is not absolute", name, path)
	}
	if path != filepath.Clean(path) {
		return fmt.Errorf("desktop: %s directory %q is not clean (use %q)", name, path, filepath.Clean(path))
	}
	return nil
}
