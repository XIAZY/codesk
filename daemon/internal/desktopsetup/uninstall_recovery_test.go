package desktopsetup

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecoverUninstallStateRollsBackEveryUncommittedPhase(t *testing.T) {
	for _, test := range []struct {
		name      string
		tombstone bool
	}{
		{name: "before tombstone"},
		{name: "after tombstone", tombstone: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			install := filepath.Join(root, "Codesk")
			tombstone := testUninstallTombstone(root, "6")
			writeTransactionFile(t, install, "old")
			prior := testRegistrationState()
			transaction, err := newUninstallTransaction(install, tombstone)
			if err != nil {
				t.Fatal(err)
			}
			if err := transaction.Prepare(prior); err != nil {
				t.Fatal(err)
			}
			if test.tombstone {
				if err := transaction.Tombstone(); err != nil {
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
			assertMissingPath(t, tombstone)
			assertMissingPath(t, transaction.recordPath)
			if !reflect.DeepEqual(restored, []registrationState{prior}) {
				t.Fatalf("restored registration = %#v, want %#v", restored, prior)
			}
		})
	}
}

func TestRecoverUninstallStateFinishesCommittedRemoval(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "7")
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
	if err := transaction.Commit(registrationState{}); err != nil {
		t.Fatal(err)
	}

	var restored []registrationState
	if err := recoverInstallState(install, func(state registrationState) error {
		restored = append(restored, state)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, []registrationState{{}}) {
		t.Fatalf("restored committed registration = %#v, want empty registration", restored)
	}
	assertMissingPath(t, install)
	assertMissingPath(t, tombstone)
	assertMissingPath(t, transaction.recordPath)
	assertMissingPath(t, transaction.commitPath)
}

func TestRecoverUninstallStateConvergesAcrossCommittedCleanupCrashPoints(t *testing.T) {
	for _, test := range []struct {
		name            string
		removeTombstone bool
		removeRecord    bool
		removeCommit    bool
		wantRestores    int
	}{
		{name: "after proof before cleanup", wantRestores: 1},
		{name: "after tombstone removal", removeTombstone: true, wantRestores: 1},
		{name: "after record removal", removeTombstone: true, removeRecord: true, wantRestores: 1},
		{name: "after all cleanup", removeTombstone: true, removeRecord: true, removeCommit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			install := filepath.Join(root, "Codesk")
			tombstone := testUninstallTombstone(root, "d")
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
			if err := transaction.Commit(registrationState{}); err != nil {
				t.Fatal(err)
			}
			if test.removeTombstone {
				if err := removeRealDirectory(tombstone); err != nil {
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
				if !reflect.DeepEqual(state, registrationState{}) {
					t.Fatalf("registration = %#v, want empty", state)
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
			assertMissingPath(t, install)
			assertMissingPath(t, tombstone)
			assertMissingPath(t, transaction.recordPath)
			assertMissingPath(t, transaction.commitPath)
		})
	}
}

func TestRecoverUninstallStateRetainsCommitUntilRegistrationRepairSucceeds(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "b")
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
	if err := transaction.Commit(registrationState{}); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("registry unavailable")
	if err := recoverInstallState(install, func(registrationState) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("recoverInstallState() error = %v, want %v", err, wantErr)
	}
	assertTransactionFile(t, tombstone, "old")
	if err := recoverInstallState(install, func(state registrationState) error {
		if !reflect.DeepEqual(state, registrationState{}) {
			t.Fatalf("registration = %#v, want empty", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertMissingPath(t, tombstone)
	assertMissingPath(t, transaction.recordPath)
	assertMissingPath(t, transaction.commitPath)
}

func TestRecoverUninstallStateReplaysOrphanCommitUntilRegistrationRepairSucceeds(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "c")
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
	if err := transaction.Commit(registrationState{}); err != nil {
		t.Fatal(err)
	}
	if err := removeRealDirectory(tombstone); err != nil {
		t.Fatal(err)
	}
	if err := removeRegularFile(transaction.recordPath); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("registry unavailable")
	if err := recoverInstallState(install, func(registrationState) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("recoverInstallState() error = %v, want %v", err, wantErr)
	}
	restores := 0
	if err := recoverInstallState(install, func(state registrationState) error {
		restores++
		if !reflect.DeepEqual(state, registrationState{}) {
			t.Fatalf("registration = %#v, want empty", state)
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
	assertMissingPath(t, transaction.commitPath)
}

func TestRecoverUninstallStateRestoresSnapshotAfterPartialRemoval(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "a")
	writeTransactionFile(t, install, "old")
	prior := testRegistrationState()
	transaction, err := newUninstallTransaction(install, tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Prepare(prior); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Tombstone(); err != nil {
		t.Fatal(err)
	}

	current := registrationState{}
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

func TestRecoverUninstallStateRetainsProofUntilRegistrationRestores(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	tombstone := testUninstallTombstone(root, "8")
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
	wantErr := errors.New("registry unavailable")
	if err := recoverInstallState(install, func(registrationState) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("recoverInstallState() error = %v, want %v", err, wantErr)
	}
	assertTransactionFile(t, install, "old")
	if err := recoverInstallState(install, func(registrationState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertMissingPath(t, transaction.recordPath)
}

func TestRecoverUninstallStateRejectsUnprovenTombstones(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "Codesk")
	writeTransactionFile(t, testUninstallTombstone(root, "9"), "old")
	if err := recoverInstallState(install, nil); err == nil || !strings.Contains(err.Error(), "manual") {
		t.Fatalf("recoverInstallState() error = %v, want manual recovery", err)
	}
}
