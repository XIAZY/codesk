package desktopsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type uninstallRecord struct {
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	Tombstone    string            `json:"tombstone"`
	HadInstall   bool              `json:"hadInstall"`
	Registration registrationState `json:"registration"`
}

type uninstallTransaction struct {
	installPath          string
	tombstonePath        string
	recordPath           string
	commitPath           string
	id                   string
	hadInstall           bool
	prepared             bool
	moved                bool
	committed            bool
	commitStateUncertain bool
	writeState           func(string, any) error
}

func newUninstallTransaction(installPath, tombstonePath string) (*uninstallTransaction, error) {
	if installPath == "" || !filepath.IsAbs(installPath) || installPath != filepath.Clean(installPath) ||
		tombstonePath == "" || !filepath.IsAbs(tombstonePath) || tombstonePath != filepath.Clean(tombstonePath) {
		return nil, errors.New("desktop setup: uninstall transaction paths must be absolute and clean")
	}
	if filepath.Dir(installPath) != filepath.Dir(tombstonePath) || installPath == tombstonePath ||
		!validTransactionSiblingName(filepath.Base(tombstonePath), filepath.Base(installPath), "remove") {
		return nil, errors.New("desktop setup: invalid uninstall tombstone path")
	}
	return &uninstallTransaction{
		installPath:   installPath,
		tombstonePath: tombstonePath,
		recordPath:    uninstallRecordPath(installPath),
		commitPath:    uninstallCommitPath(installPath),
		writeState:    writeExclusiveJSON,
	}, nil
}

func (t *uninstallTransaction) Prepare(registration registrationState) error {
	if t == nil || t.prepared || t.moved || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: invalid uninstall transaction state")
	}
	if err := validateRegistrationState(registration); err != nil {
		return err
	}
	id, err := newTransactionID()
	if err != nil {
		return err
	}
	record := uninstallRecord{
		Version:      transactionRecordVersion,
		ID:           id,
		Tombstone:    filepath.Base(t.tombstonePath),
		Registration: registration,
	}
	if _, err := os.Lstat(t.installPath); err == nil {
		if err := requireDirectory(t.installPath); err != nil {
			return fmt.Errorf("desktop setup: invalid install directory: %w", err)
		}
		record.HadInstall = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: inspect install directory: %w", err)
	}
	if err := t.writeState(t.recordPath, record); err != nil {
		return fmt.Errorf("desktop setup: persist uninstall record: %w", err)
	}
	t.id = id
	t.hadInstall = record.HadInstall
	t.prepared = true
	return nil
}

func (t *uninstallTransaction) Tombstone() error {
	if t == nil || !t.prepared || t.moved || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: invalid uninstall transaction state")
	}
	if _, err := os.Lstat(t.tombstonePath); err == nil {
		return errors.New("desktop setup: uninstall tombstone already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: inspect uninstall tombstone: %w", err)
	}
	if t.hadInstall {
		if err := requireDirectory(t.installPath); err != nil {
			return fmt.Errorf("desktop setup: invalid install directory: %w", err)
		}
		if err := movePathDurably(t.installPath, t.tombstonePath, false); err != nil {
			return fmt.Errorf("desktop setup: tombstone install directory: %w", err)
		}
	} else if _, err := os.Lstat(t.installPath); err == nil {
		return errors.New("desktop setup: install directory appeared after uninstall preparation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: inspect install directory: %w", err)
	}
	t.moved = true
	return nil
}

func (t *uninstallTransaction) Rollback() error {
	if t == nil || !t.prepared || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot roll back an inactive uninstall")
	}
	installExists, err := realDirectoryPresent(t.installPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect install directory during uninstall rollback: %w", err)
	}
	tombstoneExists, err := realDirectoryPresent(t.tombstonePath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect tombstone during uninstall rollback: %w", err)
	}
	if !t.hadInstall {
		if installExists || tombstoneExists {
			return errors.New("desktop setup: absent-install rollback has unexpected files")
		}
		t.moved = false
		return nil
	}
	if installExists && tombstoneExists {
		return errors.New("desktop setup: uninstall rollback has two installed versions")
	}
	if tombstoneExists {
		if err := movePathDurably(t.tombstonePath, t.installPath, false); err != nil {
			return fmt.Errorf("desktop setup: restore uninstalled version: %w", err)
		}
	} else if t.moved || !installExists {
		return errors.New("desktop setup: uninstall rollback lost the installed version")
	}
	t.moved = false
	return nil
}

func (t *uninstallTransaction) Forget() error {
	if t == nil || !t.prepared || t.moved || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot forget an active uninstall")
	}
	if err := removeRegularFile(t.recordPath); err != nil {
		return fmt.Errorf("desktop setup: remove rolled-back uninstall record: %w", err)
	}
	t.prepared = false
	return nil
}

func (t *uninstallTransaction) Commit(registration registrationState) error {
	if t == nil || !t.prepared || !t.moved || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot commit an inactive uninstall")
	}
	if err := validateRegistrationState(registration); err != nil {
		return err
	}
	commit := transactionCommit{
		Version:      transactionRecordVersion,
		ID:           t.id,
		Registration: registration,
	}
	if err := t.writeState(t.commitPath, commit); err != nil {
		present, matches, observationErr := observeCommitPublication(t.commitPath, commit)
		if observationErr != nil {
			t.commitStateUncertain = true
			return errors.Join(
				fmt.Errorf("desktop setup: persist uninstall commit: %w", err),
				fmt.Errorf("desktop setup: inspect uninstall commit after publication failure: %w", observationErr),
			)
		}
		if !present {
			return fmt.Errorf("desktop setup: persist uninstall commit: %w", err)
		}
		t.commitStateUncertain = true
		if matches {
			t.committed = true
			return fmt.Errorf("desktop setup: uninstall commit publication reported failure after publishing the expected proof: %w", err)
		}
		return fmt.Errorf("desktop setup: uninstall commit publication left an unexpected proof: %w", err)
	}
	t.committed = true
	return nil
}

func (t *uninstallTransaction) Complete() error {
	if t == nil || !t.prepared || !t.moved || !t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot complete an uncommitted uninstall")
	}
	if t.hadInstall {
		if err := removeRealDirectory(t.tombstonePath); err != nil {
			return fmt.Errorf("desktop setup: remove uninstalled version: %w", err)
		}
	}
	if err := removeRegularFile(t.recordPath); err != nil {
		return fmt.Errorf("desktop setup: remove completed uninstall record: %w", err)
	}
	if err := removeRegularFile(t.commitPath); err != nil {
		return fmt.Errorf("desktop setup: remove uninstall commit: %w", err)
	}
	t.prepared = false
	t.moved = false
	t.committed = false
	return nil
}

func (t *uninstallTransaction) rollbackAllowed() bool {
	return t != nil && !t.committed && !t.commitStateUncertain
}

func uninstallRecordPath(installPath string) string {
	return filepath.Join(filepath.Dir(installPath), "."+filepath.Base(installPath)+"-uninstall.json")
}

func uninstallCommitPath(installPath string) string {
	return filepath.Join(filepath.Dir(installPath), "."+filepath.Base(installPath)+"-uninstall.commit")
}
