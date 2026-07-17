package desktop

import (
	"errors"
	"fmt"

	"notty/daemon/internal/desktop/handoff"
	"notty/daemon/internal/desktopstate"
)

// CommitConnectionMetadata completes the durable half of a browser handoff.
// Connect has already protected the new token before this function is called.
// If the metadata write fails, the previous token is restored so an old
// configuration can never intentionally be paired with the new credential.
func CommitConnectionMetadata(
	configStore desktopstate.ConfigurationStore,
	secrets desktopstate.SecretStore,
	previousToken []byte,
	payload handoff.Payload,
) (desktopstate.Configuration, error) {
	if configStore == nil || secrets == nil {
		return desktopstate.Configuration{}, errors.New("desktop: connection stores are required")
	}
	config := desktopstate.ConfigurationFromPayload(payload)
	if err := configStore.Save(config); err != nil {
		rollbackErr := restoreConnectionToken(secrets, previousToken)
		if rollbackErr == nil {
			return desktopstate.Configuration{}, fmt.Errorf("desktop: persist connection metadata: %w", err)
		}

		// A failed credential rollback must not leave durable metadata that can
		// be interpreted with an unknown token on the next launch.
		deleteErr := configStore.Delete()
		return desktopstate.Configuration{}, errors.Join(
			fmt.Errorf("desktop: persist connection metadata: %w", err),
			fmt.Errorf("desktop: restore previous credential: %w", rollbackErr),
			wrapConnectionCleanupError(deleteErr),
		)
	}
	return config, nil
}

func restoreConnectionToken(secrets desktopstate.SecretStore, previousToken []byte) error {
	if len(previousToken) == 0 {
		return secrets.Delete(desktopstate.SecretKeyDaemonToken)
	}
	restoredToken := append([]byte(nil), previousToken...)
	defer clear(restoredToken)
	return secrets.Save(desktopstate.SecretKeyDaemonToken, restoredToken)
}

func wrapConnectionCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("desktop: invalidate connection metadata: %w", err)
}
