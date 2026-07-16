package desktopsetup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryTransactionFreshInstall(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, staging, "new")

	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "new")
	if err := transaction.Commit(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Complete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup remains after completion: %v", err)
	}
}

func TestDirectoryTransactionUpgradeRollbackRestoresOldVersion(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, install, "old")
	writeTransactionFile(t, staging, "new")

	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "new")
	assertTransactionFile(t, backup, "old")
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "old")
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup remains after rollback: %v", err)
	}
	if err := transaction.Forget(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryTransactionRollbackRecoversIntermediateBackupMove(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, install, "old")
	writeTransactionFile(t, staging, "new")

	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	// Model a move that changed the filesystem before its caller observed an
	// error. Rollback must inspect topology rather than trust the phase flag.
	if err := movePathDurably(install, backup, false); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "old")
	assertMissingPath(t, backup)
	if err := transaction.Forget(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryTransactionUpgradeCompletionRemovesBackup(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, install, "old")
	writeTransactionFile(t, staging, "new")

	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Complete(); err == nil {
		t.Fatal("Complete() accepted an uncommitted transaction")
	}
	if err := transaction.Commit(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Complete(); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "new")
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup remains after completion: %v", err)
	}
}

func TestDirectoryTransactionCommitErrorAfterProofPublicationStaysForwardRecoverable(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, install, "old")
	writeTransactionFile(t, staging, "new")

	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	prior := testRegistrationState()
	if err := transaction.Prepare(prior); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err != nil {
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
		t.Fatal("transaction allowed rollback across a published commit proof")
	}
	var restored int
	if err := recoverInstallState(install, func(state registrationState) error {
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
	assertTransactionFile(t, install, "new")
	assertMissingPath(t, backup)
	assertMissingPath(t, transaction.recordPath)
	assertMissingPath(t, transaction.commitPath)
}

func TestDirectoryTransactionRejectsUnsafeTopology(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if _, err := newDirectoryTransaction(
		filepath.Join(root, "Codesk"),
		filepath.Join(outside, "stage"),
		filepath.Join(root, "backup"),
	); err == nil {
		t.Fatal("newDirectoryTransaction() accepted paths on different parents")
	}

	staging, backup := testTransactionPaths(root)
	if err := os.WriteFile(staging, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	transaction, err := newDirectoryTransaction(filepath.Join(root, "Codesk"), staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err == nil {
		t.Fatal("Swap() accepted a non-directory staging path")
	}

	symlinkRoot := t.TempDir()
	symlinkTarget := t.TempDir()
	symlinkStage, symlinkBackup := testTransactionPaths(symlinkRoot)
	if err := os.Symlink(symlinkTarget, symlinkStage); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	transaction, err = newDirectoryTransaction(
		filepath.Join(symlinkRoot, "Codesk"),
		symlinkStage,
		symlinkBackup,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(testRegistrationState()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err == nil {
		t.Fatal("Swap() accepted a staging symlink")
	}
}

func TestValidateRegistrationStateRequiresBoundedExactSnapshot(t *testing.T) {
	valid := testRegistrationState()
	if err := validateRegistrationState(valid); err != nil {
		t.Fatalf("valid registration state rejected: %v", err)
	}

	for _, test := range []struct {
		name  string
		state registrationState
	}{
		{
			name:  "absent shortcut with bytes",
			state: registrationState{Shortcut: fileValueState{Data: []byte("partial")}},
		},
		{
			name:  "unsupported Run value type",
			state: registrationState{Run: stringValueState{Present: true, Value: `"C:\\Codesk.exe"`, Type: 4}},
		},
		{
			name:  "Run value with embedded NUL",
			state: registrationState{Run: stringValueState{Present: true, Value: "Codesk\x00extra", Type: registryStringType}},
		},
		{
			name:  "oversized shortcut",
			state: registrationState{Shortcut: fileValueState{Present: true, Data: make([]byte, maximumRegistrationBlobBytes+1)}},
		},
		{
			name:  "absent key with values",
			state: registrationState{Uninstall: uninstallRegistrationState{Values: []registryValueState{{Name: "orphan"}}}},
		},
		{
			name: "unsorted key values",
			state: registrationState{Uninstall: uninstallRegistrationState{
				Existed: true,
				Values:  []registryValueState{{Name: "z"}, {Name: "a"}},
			}},
		},
		{
			name: "duplicate key values",
			state: registrationState{Uninstall: uninstallRegistrationState{
				Existed: true,
				Values:  []registryValueState{{Name: "same"}, {Name: "same"}},
			}},
		},
		{
			name: "case folded duplicate key values",
			state: registrationState{Uninstall: uninstallRegistrationState{
				Existed: true,
				Values:  []registryValueState{{Name: "DisplayName"}, {Name: "displayname"}},
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRegistrationState(test.state); err == nil {
				t.Fatal("validateRegistrationState() accepted an invalid snapshot")
			}
		})
	}
}

func testTransactionPaths(root string) (string, string) {
	return filepath.Join(root, ".Codesk-stage-"+strings.Repeat("1", 32)),
		filepath.Join(root, ".Codesk-backup-"+strings.Repeat("2", 32))
}

func testRegistrationState() registrationState {
	return registrationState{
		Run:      stringValueState{Present: true, Value: `"C:\\Codesk.exe"`, Type: 1},
		Shortcut: fileValueState{Present: true, Data: []byte("exact shortcut bytes")},
		Uninstall: uninstallRegistrationState{
			Existed: true,
			Values: []registryValueState{
				{Name: "DisplayName", Type: 1, Data: []byte{'C', 0, 'o', 0, 'd', 0, 'e', 0, 's', 0, 'k', 0, 0, 0}},
				{Name: "EstimatedSize", Type: 4, Data: []byte{42, 0, 0, 0}},
			},
		},
	}
}

func writeTransactionFile(t *testing.T, root, value string) {
	t.Helper()
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "version"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertTransactionFile(t *testing.T, root, expected string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "version"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("version = %q, want %q", data, expected)
	}
}
