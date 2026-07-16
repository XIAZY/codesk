package desktopsetup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallTransactionRollbackRestoresInstalledVersion(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "3")
	writeTransactionFile(t, install, "old")

	transaction, err := newUninstallTransaction(install, tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Tombstone(); err != nil {
		t.Fatal(err)
	}
	assertMissingPath(t, install)
	assertTransactionFile(t, tombstone, "old")
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Forget(); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "old")
	assertMissingPath(t, tombstone)
	assertMissingPath(t, transaction.recordPath)
}

func TestUninstallTransactionRollbackRecoversIntermediateTombstoneMove(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "e")
	writeTransactionFile(t, install, "old")

	transaction, err := newUninstallTransaction(install, tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	// Model a move that changed the filesystem before its caller observed an
	// error. Rollback must recover the authoritative tombstone.
	if err := movePathDurably(install, tombstone, false); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "old")
	assertMissingPath(t, tombstone)
	if err := transaction.Forget(); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallTransactionCommitRemovesInstalledVersion(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "4")
	writeTransactionFile(t, install, "old")

	transaction, err := newUninstallTransaction(install, tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Tombstone(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Complete(); err == nil {
		t.Fatal("Complete() accepted an uncommitted uninstall")
	}
	if err := transaction.Commit(registrationState{}); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Complete(); err != nil {
		t.Fatal(err)
	}
	assertMissingPath(t, install)
	assertMissingPath(t, tombstone)
	assertMissingPath(t, transaction.recordPath)
	assertMissingPath(t, transaction.commitPath)
}

func TestUninstallTransactionCommitErrorAfterProofPublicationStaysForwardRecoverable(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "f")
	writeTransactionFile(t, install, "old")

	transaction, err := newUninstallTransaction(install, tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Tombstone(); err != nil {
		t.Fatal(err)
	}
	desired := registrationState{}
	transaction.writeState = func(path string, value any) error {
		if err := writeExclusiveJSON(path, value); err != nil {
			return err
		}
		return errors.New("injected failure after proof publication")
	}
	if err := transaction.Commit(desired); err == nil {
		t.Fatal("Commit() ignored a post-publication failure")
	}
	if transaction.rollbackAllowed() {
		t.Fatal("uninstall allowed rollback across a published commit proof")
	}
	var restored int
	if err := recoverUninstallState(install, func(state registrationState) error {
		restored++
		if state.Run.Present || state.Shortcut.Present || state.Uninstall.Existed {
			return errors.New("recovery did not use the committed empty registration state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("registration restore count = %d, want 1", restored)
	}
	assertMissingPath(t, install)
	assertMissingPath(t, tombstone)
	assertMissingPath(t, transaction.recordPath)
	assertMissingPath(t, transaction.commitPath)
}

func TestUninstallTransactionAbsentInstallCommitsRegistrationRemoval(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "5")
	transaction, err := newUninstallTransaction(install, tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(registrationState{}); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Tombstone(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(registrationState{}); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Complete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(install); !os.IsNotExist(err) {
		t.Fatalf("absent install appeared: %v", err)
	}
}

func testUninstallTombstone(root, digit string) string {
	return filepath.Join(root, ".Codesk-remove-"+strings.Repeat(digit, 32))
}
