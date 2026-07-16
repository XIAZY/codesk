package desktopsetup

import (
	"errors"
	"fmt"
	"path/filepath"
)

func recoverUninstallState(installPath string, restoreRegistration restoreRegistrationFunc) error {
	parent := filepath.Dir(installPath)
	base := filepath.Base(installPath)
	tombstones, err := transactionDirectories(parent, "."+base+"-remove-", "uninstall tombstone")
	if err != nil {
		return err
	}
	installExists, err := realDirectoryPresent(installPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect install directory for uninstall recovery: %w", err)
	}
	recordPath := uninstallRecordPath(installPath)
	commitPath := uninstallCommitPath(installPath)
	recordExists, err := regularFilePresent(recordPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect uninstall record: %w", err)
	}
	commitExists, err := regularFilePresent(commitPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect uninstall commit: %w", err)
	}
	if !recordExists {
		if len(tombstones) != 0 {
			return errors.New("desktop setup: uninstall tombstone without a record requires manual recovery")
		}
		if commitExists {
			if installExists {
				return errors.New("desktop setup: orphaned uninstall commit conflicts with an installed version")
			}
			commit, err := readTransactionCommit(commitPath)
			if err != nil {
				return fmt.Errorf("desktop setup: read orphaned uninstall commit: %w", err)
			}
			if restoreRegistration == nil {
				return errors.New("desktop setup: orphaned uninstall commit requires registration recovery")
			}
			if err := restoreRegistration(commit.Registration); err != nil {
				return fmt.Errorf("desktop setup: finish orphaned uninstall registration: %w", err)
			}
			if err := removeRegularFile(commitPath); err != nil {
				return fmt.Errorf("desktop setup: remove orphaned uninstall commit: %w", err)
			}
		}
		return nil
	}

	record, err := readUninstallRecord(recordPath, base)
	if err != nil {
		return err
	}
	expectedTombstone := filepath.Join(parent, record.Tombstone)
	if err := requireOnlyExpectedTransactionPaths(tombstones, expectedTombstone, "uninstall tombstone"); err != nil {
		return err
	}
	tombstoneExists := pathListed(tombstones, expectedTombstone)

	var commit transactionCommit
	committed := false
	if commitExists {
		commit, err = readTransactionCommit(commitPath)
		if err != nil {
			return fmt.Errorf("desktop setup: read uninstall commit: %w", err)
		}
		if commit.ID != record.ID {
			return errors.New("desktop setup: uninstall commit does not match its record")
		}
		committed = true
	}
	if committed {
		if installExists {
			return errors.New("desktop setup: committed uninstall has an installed version")
		}
		if restoreRegistration == nil {
			return errors.New("desktop setup: committed uninstall requires registration recovery")
		}
		if err := restoreRegistration(commit.Registration); err != nil {
			return fmt.Errorf("desktop setup: finish committed uninstall registration: %w", err)
		}
		if tombstoneExists {
			if err := removeRealDirectory(expectedTombstone); err != nil {
				return fmt.Errorf("desktop setup: finish committed uninstall: %w", err)
			}
		}
		if err := removeRegularFile(recordPath); err != nil {
			return fmt.Errorf("desktop setup: remove completed uninstall record: %w", err)
		}
		if err := removeRegularFile(commitPath); err != nil {
			return fmt.Errorf("desktop setup: remove completed uninstall commit: %w", err)
		}
		return nil
	}

	if record.HadInstall {
		switch {
		case installExists && tombstoneExists:
			return errors.New("desktop setup: uncommitted uninstall has two installed versions")
		case !installExists && !tombstoneExists:
			return errors.New("desktop setup: uncommitted uninstall lost its installed version")
		case tombstoneExists:
			if err := movePathDurably(expectedTombstone, installPath, false); err != nil {
				return fmt.Errorf("desktop setup: restore interrupted uninstall: %w", err)
			}
		}
	} else if installExists || tombstoneExists {
		return errors.New("desktop setup: uninstall of an absent version has unexpected files")
	}
	if restoreRegistration == nil {
		return errors.New("desktop setup: interrupted uninstall requires registration recovery")
	}
	if err := restoreRegistration(record.Registration); err != nil {
		return fmt.Errorf("desktop setup: restore interrupted uninstall registration: %w", err)
	}
	if err := removeRegularFile(recordPath); err != nil {
		return fmt.Errorf("desktop setup: remove recovered uninstall record: %w", err)
	}
	return nil
}

func readUninstallRecord(path, installBase string) (uninstallRecord, error) {
	var record uninstallRecord
	if err := readStrictJSON(path, &record); err != nil {
		return uninstallRecord{}, fmt.Errorf("desktop setup: read uninstall record: %w", err)
	}
	if record.Version != transactionRecordVersion || !validTransactionID(record.ID) ||
		!validTransactionSiblingName(record.Tombstone, installBase, "remove") {
		return uninstallRecord{}, errors.New("desktop setup: invalid uninstall record")
	}
	if err := validateRegistrationState(record.Registration); err != nil {
		return uninstallRecord{}, err
	}
	return record, nil
}
