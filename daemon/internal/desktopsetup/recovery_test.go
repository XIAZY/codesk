package desktopsetup

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecoverInstallStateRestoresLegacyInterruptedBackup(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	backup := filepath.Join(root, ".Codesk-backup-"+strings.Repeat("1", 32))
	stage := filepath.Join(root, ".Codesk-stage-"+strings.Repeat("2", 32))
	writeTransactionFile(t, backup, "old")
	writeTransactionFile(t, stage, "stale")

	if err := recoverInstallState(install, nil); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "old")
	assertMissingPath(t, backup)
	assertMissingPath(t, stage)
}

func TestRecoverInstallStateRollsBackUncommittedUpgrade(t *testing.T) {
	for _, test := range []struct {
		name string
		swap bool
	}{
		{name: "before swap"},
		{name: "after swap", swap: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			install := filepath.Join(root, "Codesk")
			staging, backup := testTransactionPaths(root)
			writeTransactionFile(t, install, "old")
			writeTransactionFile(t, staging, "new")
			priorRegistration := testRegistrationState()

			transaction, err := newDirectoryTransaction(install, staging, backup)
			if err != nil {
				t.Fatal(err)
			}
			if err := transaction.Prepare(priorRegistration); err != nil {
				t.Fatal(err)
			}
			if test.swap {
				if err := transaction.Swap(); err != nil {
					t.Fatal(err)
				}
			}

			var restored []registrationState
			if err := recoverInstallState(install, func(state registrationState) error {
				restored = append(restored, state)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			assertTransactionFile(t, install, "old")
			assertMissingPath(t, staging)
			assertMissingPath(t, backup)
			assertMissingPath(t, transaction.recordPath)
			if !reflect.DeepEqual(restored, []registrationState{priorRegistration}) {
				t.Fatalf("restored registration = %#v, want %#v", restored, priorRegistration)
			}
		})
	}
}

func TestRecoverInstallStateRollsBackUncommittedFreshInstall(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, staging, "new")
	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(registrationState{}); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err != nil {
		t.Fatal(err)
	}
	var restored bool
	if err := recoverInstallState(install, func(state registrationState) error {
		restored = true
		if !reflect.DeepEqual(state, registrationState{}) {
			t.Fatalf("registration = %#v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("registration snapshot was not restored")
	}
	assertMissingPath(t, install)
	assertMissingPath(t, transaction.recordPath)
}

func TestRecoverInstallStateRestoresSnapshotAfterPartialRegistration(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, install, "old")
	writeTransactionFile(t, staging, "new")
	prior := testRegistrationState()
	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(prior); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err != nil {
		t.Fatal(err)
	}

	current := registrationState{
		Run: stringValueState{Present: true, Value: `"C:\\partial\\Codesk.exe"`, Type: 1},
	}
	if err := recoverInstallState(install, func(state registrationState) error {
		current = state
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertTransactionFile(t, install, "old")
	if !reflect.DeepEqual(current, prior) {
		t.Fatalf("registration after recovery = %#v, want %#v", current, prior)
	}
}

func TestRecoverInstallStateKeepsCommittedUpgrade(t *testing.T) {
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
	committedRegistration := testRegistrationState()
	committedRegistration.Run.Value = `"C:\\new\\Codesk.exe"`
	if err := transaction.Commit(committedRegistration); err != nil {
		t.Fatal(err)
	}

	var restored []registrationState
	if err := recoverInstallState(install, func(state registrationState) error {
		restored = append(restored, state)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, []registrationState{committedRegistration}) {
		t.Fatalf("restored committed registration = %#v, want %#v", restored, committedRegistration)
	}
	assertTransactionFile(t, install, "new")
	assertMissingPath(t, backup)
	assertMissingPath(t, transaction.recordPath)
	assertMissingPath(t, transaction.commitPath)
}

func TestRecoverInstallStateConvergesAcrossCommittedCleanupCrashPoints(t *testing.T) {
	for _, test := range []struct {
		name         string
		removeBackup bool
		removeRecord bool
		removeCommit bool
		wantRestores int
	}{
		{name: "after proof before cleanup", wantRestores: 1},
		{name: "after backup removal", removeBackup: true, wantRestores: 1},
		{name: "after record removal", removeBackup: true, removeRecord: true, wantRestores: 1},
		{name: "after all cleanup", removeBackup: true, removeRecord: true, removeCommit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			committedRegistration := testRegistrationState()
			committedRegistration.Run.Value = `"C:\\new\\Codesk.exe"`
			if err := transaction.Commit(committedRegistration); err != nil {
				t.Fatal(err)
			}
			if test.removeBackup {
				if err := removeRealDirectory(backup); err != nil {
					t.Fatal(err)
				}
			}
			if test.removeRecord {
				if err := removeRegularFile(transaction.recordPath); err != nil {
					t.Fatal(err)
				}
			}
			if test.removeCommit {
				if err := removeRegularFile(transaction.commitPath); err != nil {
					t.Fatal(err)
				}
			}

			restores := 0
			restore := func(state registrationState) error {
				restores++
				if !reflect.DeepEqual(state, committedRegistration) {
					t.Fatalf("registration = %#v, want %#v", state, committedRegistration)
				}
				return nil
			}
			if err := recoverInstallState(install, restore); err != nil {
				t.Fatal(err)
			}
			if err := recoverInstallState(install, restore); err != nil {
				t.Fatal(err)
			}
			if restores != test.wantRestores {
				t.Fatalf("registration restore calls = %d, want %d", restores, test.wantRestores)
			}
			assertTransactionFile(t, install, "new")
			assertMissingPath(t, backup)
			assertMissingPath(t, transaction.recordPath)
			assertMissingPath(t, transaction.commitPath)
		})
	}
}

func TestRecoverInstallStateRetainsCommitUntilRegistrationRepairSucceeds(t *testing.T) {
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
	committedRegistration := testRegistrationState()
	committedRegistration.Run.Value = `"C:\\new\\Codesk.exe"`
	if err := transaction.Commit(committedRegistration); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("registry unavailable")
	if err := recoverInstallState(install, func(registrationState) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("recoverInstallState() error = %v, want %v", err, wantErr)
	}
	assertTransactionFile(t, install, "new")
	assertTransactionFile(t, backup, "old")
	if _, err := os.Stat(transaction.commitPath); err != nil {
		t.Fatalf("transaction commit was not retained: %v", err)
	}
	if err := recoverInstallState(install, func(state registrationState) error {
		if !reflect.DeepEqual(state, committedRegistration) {
			t.Fatalf("registration = %#v, want %#v", state, committedRegistration)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertMissingPath(t, backup)
	assertMissingPath(t, transaction.recordPath)
	assertMissingPath(t, transaction.commitPath)
}

func TestRecoverInstallStateReplaysOrphanCommitUntilRegistrationRepairSucceeds(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	staging, backup := testTransactionPaths(root)
	writeTransactionFile(t, staging, "new")
	transaction, err := newDirectoryTransaction(install, staging, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(registrationState{}); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Swap(); err != nil {
		t.Fatal(err)
	}
	committedRegistration := testRegistrationState()
	if err := transaction.Commit(committedRegistration); err != nil {
		t.Fatal(err)
	}
	if err := removeRegularFile(transaction.recordPath); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("registry unavailable")
	if err := recoverInstallState(install, func(registrationState) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("recoverInstallState() error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(transaction.commitPath); err != nil {
		t.Fatalf("orphaned commit was not retained: %v", err)
	}
	restores := 0
	if err := recoverInstallState(install, func(state registrationState) error {
		restores++
		if !reflect.DeepEqual(state, committedRegistration) {
			t.Fatalf("registration = %#v, want %#v", state, committedRegistration)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := recoverInstallState(install, func(registrationState) error {
		restores++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if restores != 1 {
		t.Fatalf("registration restore calls = %d, want 1", restores)
	}
	assertTransactionFile(t, install, "new")
	assertMissingPath(t, transaction.commitPath)
}

func TestRecoverInstallStateRemovesIncompleteStatePublication(t *testing.T) {
	install := filepath.Join(t.TempDir(), "Codesk")
	publishing := transactionPublishingPath(transactionCommitPath(install))
	if err := os.WriteFile(publishing, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverInstallState(install, nil); err != nil {
		t.Fatal(err)
	}
	assertMissingPath(t, publishing)
}

func TestRecoverInstallStateRetainsRecordUntilRegistrationRestoreSucceeds(t *testing.T) {
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

	wantErr := errors.New("registry unavailable")
	if err := recoverInstallState(install, func(registrationState) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("recoverInstallState() error = %v, want %v", err, wantErr)
	}
	assertTransactionFile(t, install, "old")
	if _, err := os.Stat(transaction.recordPath); err != nil {
		t.Fatalf("transaction record was not retained: %v", err)
	}
	if err := recoverInstallState(install, func(registrationState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertMissingPath(t, transaction.recordPath)
}

func TestRecoverInstallStateRejectsAmbiguousOrUnsafeState(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	writeTransactionFile(t, filepath.Join(root, ".Codesk-backup-"+strings.Repeat("1", 32)), "one")
	writeTransactionFile(t, filepath.Join(root, ".Codesk-backup-"+strings.Repeat("2", 32)), "two")
	if err := recoverInstallState(install, nil); err == nil || !strings.Contains(err.Error(), "manual") {
		t.Fatalf("recoverInstallState() error = %v, want ambiguous-backup failure", err)
	}

	unsafeRoot := t.TempDir()
	unsafeInstall := filepath.Join(unsafeRoot, "Codesk")
	if err := os.WriteFile(filepath.Join(unsafeRoot, ".Codesk-stage-file"), []byte("unsafe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverInstallState(unsafeInstall, nil); err == nil {
		t.Fatal("recoverInstallState() accepted a stale staging file")
	}

	symlinkRoot := t.TempDir()
	symlinkInstall := filepath.Join(symlinkRoot, "Codesk")
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(symlinkRoot, ".Codesk-backup-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := recoverInstallState(symlinkInstall, nil); err == nil {
		t.Fatal("recoverInstallState() accepted a backup symlink")
	}
}

func TestNewSiblingPathIsBoundedAndUnique(t *testing.T) {
	install := filepath.Join(t.TempDir(), "Codesk")
	first, err := newSiblingPath(install, "stage")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newSiblingPath(install, "stage")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || filepath.Dir(first) != filepath.Dir(install) || !validTransactionSiblingName(filepath.Base(first), "Codesk", "stage") {
		t.Fatalf("unexpected sibling paths %q and %q", first, second)
	}
	removePath, err := newSiblingPath(install, "remove")
	if err != nil || !validTransactionSiblingName(filepath.Base(removePath), "Codesk", "remove") {
		t.Fatalf("unexpected remove path %q: %v", removePath, err)
	}
	if _, err := newSiblingPath(install, "other"); err == nil {
		t.Fatal("newSiblingPath() accepted an unknown purpose")
	}
}

func assertMissingPath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("path %q remains: %v", path, err)
	}
}
