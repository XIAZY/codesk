package desktop

import (
	"errors"
	"strings"
	"testing"

	"notty/daemon/internal/desktop/handoff"
	"notty/daemon/internal/desktopstate"
)

type connectionConfigStore struct {
	config    desktopstate.Configuration
	saveErr   error
	deleteErr error
	deleted   bool
}

func (s *connectionConfigStore) Load() (desktopstate.Configuration, error) {
	return s.config, nil
}

func (s *connectionConfigStore) Save(config desktopstate.Configuration) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.config = config
	return nil
}

func (s *connectionConfigStore) Delete() error {
	s.deleted = true
	return s.deleteErr
}

type connectionSecretStore struct {
	token     []byte
	saveErr   error
	deleteErr error
}

func (s *connectionSecretStore) Save(key string, secret []byte) error {
	if key != desktopstate.SecretKeyDaemonToken {
		return errors.New("unexpected secret key")
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	s.token = append([]byte(nil), secret...)
	return nil
}

func (s *connectionSecretStore) Load(string) ([]byte, error) {
	return append([]byte(nil), s.token...), nil
}

func (s *connectionSecretStore) Delete(key string) error {
	if key != desktopstate.SecretKeyDaemonToken {
		return errors.New("unexpected secret key")
	}
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.token = nil
	return nil
}

func connectionPayload() handoff.Payload {
	return handoff.Payload{
		DaemonID:      "daemon-new",
		WorkspaceID:   "workspace-new",
		WorkspaceName: "Workspace New",
		WorkspaceSlug: "workspace-new",
		WorkspaceURL:  "https://app.getcodesk.com/w/workspace-new",
	}
}

func TestCommitConnectionMetadataStoresValidatedConfiguration(t *testing.T) {
	store := &connectionConfigStore{}
	secrets := &connectionSecretStore{token: []byte("new-token")}

	config, err := CommitConnectionMetadata(store, secrets, []byte("old-token"), connectionPayload())
	if err != nil {
		t.Fatal(err)
	}
	if config != store.config {
		t.Fatalf("stored configuration = %#v, want %#v", store.config, config)
	}
	if got := string(secrets.token); got != "new-token" {
		t.Fatalf("stored token = %q, want new token", got)
	}
}

func TestCommitConnectionMetadataRestoresPreviousTokenAfterMetadataFailure(t *testing.T) {
	store := &connectionConfigStore{config: desktopstate.ConfigurationFromPayload(connectionPayload()), saveErr: errors.New("disk full")}
	secrets := &connectionSecretStore{token: []byte("new-token")}

	_, err := CommitConnectionMetadata(store, secrets, []byte("old-token"), connectionPayload())
	if err == nil {
		t.Fatal("CommitConnectionMetadata() succeeded despite metadata failure")
	}
	if got := string(secrets.token); got != "old-token" {
		t.Fatalf("stored token = %q, want restored old token", got)
	}
	if store.deleted {
		t.Fatal("metadata was deleted despite a successful credential rollback")
	}
}

func TestCommitConnectionMetadataDeletesNewTokenWhenPreviouslyUnconfigured(t *testing.T) {
	store := &connectionConfigStore{saveErr: errors.New("read only")}
	secrets := &connectionSecretStore{token: []byte("new-token")}

	_, err := CommitConnectionMetadata(store, secrets, nil, connectionPayload())
	if err == nil {
		t.Fatal("CommitConnectionMetadata() succeeded despite metadata failure")
	}
	if len(secrets.token) != 0 {
		t.Fatalf("stored token = %q, want deletion", secrets.token)
	}
}

func TestCommitConnectionMetadataInvalidatesMetadataWhenTokenRollbackFails(t *testing.T) {
	store := &connectionConfigStore{config: desktopstate.ConfigurationFromPayload(connectionPayload()), saveErr: errors.New("metadata failure")}
	secrets := &connectionSecretStore{token: []byte("new-token"), saveErr: errors.New("DPAPI failure")}

	_, err := CommitConnectionMetadata(store, secrets, []byte("old-token"), connectionPayload())
	if err == nil {
		t.Fatal("CommitConnectionMetadata() succeeded despite rollback failure")
	}
	if !store.deleted {
		t.Fatal("metadata was not invalidated after credential rollback failed")
	}
	if got := string(secrets.token); got != "new-token" {
		t.Fatalf("stored token = %q, want unchanged new token", got)
	}
}

func TestCommitConnectionMetadataDoesNotLeakTokensInErrors(t *testing.T) {
	store := &connectionConfigStore{saveErr: errors.New("metadata failure")}
	secrets := &connectionSecretStore{token: []byte("new-token"), saveErr: errors.New("secret failure")}

	_, err := CommitConnectionMetadata(store, secrets, []byte("old-token"), connectionPayload())
	if err == nil {
		t.Fatal("CommitConnectionMetadata() succeeded")
	}
	for _, secret := range []string{"new-token", "old-token"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q contains secret %q", err, secret)
		}
	}
}
