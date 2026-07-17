package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"notty/daemon/internal/desktop/handoff"
	"notty/daemon/internal/desktopstate"
)

// Connect opens a browser to the Codesk web app's desktop connect page, waits
// for the one-shot credential handoff, and persists the daemon token in the
// SecretStore before returning. If persistence fails the handoff is not
// reported as successful — the caller never observes a connected-but-unpersisted
// state.
func Connect(ctx context.Context, codeskOrigin string, secrets desktopstate.SecretStore, opener OpenURL) (handoff.Payload, error) {
	session, err := handoff.NewSession(codeskOrigin)
	if err != nil {
		return handoff.Payload{}, err
	}
	defer session.Close()

	connectURL := codeskOrigin + "/desktop/connect?callback=" + url.QueryEscape(session.CallbackURL())
	if err := opener.Open(connectURL); err != nil {
		return handoff.Payload{}, fmt.Errorf("desktop connect: open browser: %w", err)
	}

	payload, err := session.Wait(ctx)
	if err != nil {
		return handoff.Payload{}, err
	}

	token := []byte(payload.Token())
	defer clear(token)
	if err := secrets.Save(desktopstate.SecretKeyDaemonToken, token); err != nil {
		return handoff.Payload{}, errors.New("desktop connect: persist token failed")
	}

	return payload, nil
}
