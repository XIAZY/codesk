//go:build windows

package desktopstate_test

import "notty/daemon/internal/desktopstate"

type protectedFingerprintStore interface {
	ProtectedFingerprint(string) (desktopstate.Fingerprint, error)
}

var (
	_ desktopstate.SecretStore  = (*desktopstate.WindowsSecretStore)(nil)
	_ protectedFingerprintStore = (*desktopstate.WindowsSecretStore)(nil)
)
