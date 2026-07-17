package desktopstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"notty/daemon/internal/desktop/handoff"
	"notty/daemon/internal/desktopurl"
)

const (
	desktopConfigFilename = "desktop.json"
	maxDesktopConfigBytes = 16 << 10
)

type Configuration struct {
	DaemonID      string `json:"daemon_id"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	WorkspaceSlug string `json:"workspace_slug"`
	WorkspaceURL  string `json:"workspace_url"`
}

func ConfigurationFromPayload(payload handoff.Payload) Configuration {
	return Configuration{
		DaemonID:      payload.DaemonID,
		WorkspaceID:   payload.WorkspaceID,
		WorkspaceName: payload.WorkspaceName,
		WorkspaceSlug: payload.WorkspaceSlug,
		WorkspaceURL:  payload.WorkspaceURL,
	}
}

func (c Configuration) Validate() error {
	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"daemon ID", c.DaemonID, 128},
		{"workspace ID", c.WorkspaceID, 128},
		{"workspace name", c.WorkspaceName, 256},
		{"workspace slug", c.WorkspaceSlug, 128},
	}
	for _, field := range fields {
		if !validConfigurationText(field.value, field.limit) {
			return fmt.Errorf("desktop: invalid %s", field.name)
		}
	}

	if !desktopurl.Valid(c.WorkspaceURL) {
		return errors.New("desktop: invalid workspace URL")
	}
	return nil
}

func validConfigurationText(value string, limit int) bool {
	if value == "" || len(value) > limit || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

type ConfigurationStore interface {
	Load() (Configuration, error)
	Save(Configuration) error
	Delete() error
}

type FileConfigurationStore struct {
	path string
}

func NewFileConfigurationStore(dataDir string) (*FileConfigurationStore, error) {
	if err := RequireAbsolute("data", dataDir); err != nil {
		return nil, err
	}
	return &FileConfigurationStore{path: filepath.Join(dataDir, desktopConfigFilename)}, nil
}

func (s *FileConfigurationStore) Load() (Configuration, error) {
	config, _, err := s.read()
	return config, err
}

func (s *FileConfigurationStore) read() (Configuration, []byte, error) {
	if s == nil || s.path == "" {
		return Configuration{}, nil, errors.New("desktop: configuration store is not initialized")
	}
	data, err := readStateFile(s.path, "configuration", maxDesktopConfigBytes)
	if err != nil {
		return Configuration{}, nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Configuration
	if err := decoder.Decode(&config); err != nil {
		return Configuration{}, nil, fmt.Errorf("desktop: decode configuration: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Configuration{}, nil, err
	}
	if err := config.Validate(); err != nil {
		return Configuration{}, nil, err
	}
	return config, data, nil
}

func (s *FileConfigurationStore) Fingerprint() (Fingerprint, error) {
	_, data, err := s.read()
	if errors.Is(err, os.ErrNotExist) {
		return Fingerprint{}, nil
	}
	if err != nil {
		return Fingerprint{}, errors.New("desktop: configuration fingerprint unavailable")
	}
	return fingerprint(data), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("desktop: configuration contains multiple JSON values")
		}
		return fmt.Errorf("desktop: decode configuration trailer: %w", err)
	}
	return nil
}

func (s *FileConfigurationStore) Save(config Configuration) error {
	if s == nil || s.path == "" {
		return errors.New("desktop: configuration store is not initialized")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("desktop: encode configuration: %w", err)
	}
	data = append(data, '\n')
	return writePrivateFileAtomically(s.path, data)
}

func (s *FileConfigurationStore) Delete() error {
	if s == nil || s.path == "" {
		return errors.New("desktop: configuration store is not initialized")
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop: delete configuration: %w", err)
	}
	return nil
}

func writePrivateFileAtomically(path string, data []byte) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("desktop: create private directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".codesk-write-*")
	if err != nil {
		return fmt.Errorf("desktop: create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); returnErr == nil && closeErr != nil {
				returnErr = fmt.Errorf("desktop: close temporary file: %w", closeErr)
			}
		}
		if removeErr := os.Remove(temporaryPath); returnErr == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = fmt.Errorf("desktop: remove temporary file: %w", removeErr)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("desktop: protect temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("desktop: write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("desktop: sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("desktop: close temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("desktop: replace private file: %w", err)
	}
	return nil
}
