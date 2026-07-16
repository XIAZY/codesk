package desktopsetup

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type restoreRegistrationFunc func(registrationState) error

func recoverInstallState(installPath string, restoreRegistration restoreRegistrationFunc) error {
	if installPath == "" || !filepath.IsAbs(installPath) || installPath != filepath.Clean(installPath) {
		return errors.New("desktop setup: invalid install path")
	}
	parent := filepath.Dir(installPath)
	if err := ensureRealDirectory(parent); err != nil {
		return fmt.Errorf("desktop setup: prepare install parent: %w", err)
	}
	if err := removeTransactionPublishingFiles(installPath); err != nil {
		return err
	}
	if err := recoverUninstallState(installPath, restoreRegistration); err != nil {
		return err
	}

	base := filepath.Base(installPath)
	staging, err := transactionDirectories(parent, "."+base+"-stage-", "staging")
	if err != nil {
		return err
	}
	backups, err := transactionDirectories(parent, "."+base+"-backup-", "backup")
	if err != nil {
		return err
	}
	installExists, err := realDirectoryPresent(installPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect install directory: %w", err)
	}

	recordPath := transactionRecordPath(installPath)
	commitPath := transactionCommitPath(installPath)
	recordExists, err := regularFilePresent(recordPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect transaction record: %w", err)
	}
	commitExists, err := regularFilePresent(commitPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect transaction commit: %w", err)
	}
	if !recordExists {
		return recoverWithoutRecord(installPath, installExists, staging, backups, commitPath, commitExists, restoreRegistration)
	}

	record, err := readTransactionRecord(recordPath, base)
	if err != nil {
		return err
	}
	expectedStage := filepath.Join(parent, record.Staging)
	expectedBackup := filepath.Join(parent, record.Backup)
	if err := requireOnlyExpectedTransactionPaths(staging, expectedStage, "staging"); err != nil {
		return err
	}
	if err := requireOnlyExpectedTransactionPaths(backups, expectedBackup, "backup"); err != nil {
		return err
	}
	stageExists := pathListed(staging, expectedStage)
	backupExists := pathListed(backups, expectedBackup)

	var commit transactionCommit
	committed := false
	if commitExists {
		commit, err = readTransactionCommit(commitPath)
		if err != nil {
			return err
		}
		if commit.ID != record.ID {
			return errors.New("desktop setup: transaction commit does not match its record")
		}
		committed = true
	}
	if committed {
		if !installExists {
			return errors.New("desktop setup: committed transaction has no installed version")
		}
		if restoreRegistration == nil {
			return errors.New("desktop setup: committed transaction requires registration recovery")
		}
		if err := restoreRegistration(commit.Registration); err != nil {
			return fmt.Errorf("desktop setup: finish committed registration: %w", err)
		}
		if stageExists {
			if err := removeRealDirectory(expectedStage); err != nil {
				return fmt.Errorf("desktop setup: remove committed staging directory: %w", err)
			}
		}
		if backupExists {
			if err := removeRealDirectory(expectedBackup); err != nil {
				return fmt.Errorf("desktop setup: remove committed backup: %w", err)
			}
		}
		if err := removeRegularFile(recordPath); err != nil {
			return fmt.Errorf("desktop setup: remove completed transaction record: %w", err)
		}
		if err := removeRegularFile(commitPath); err != nil {
			return fmt.Errorf("desktop setup: remove completed transaction commit: %w", err)
		}
		return nil
	}

	if record.HadInstall {
		if backupExists {
			if installExists {
				if err := removeRealDirectory(installPath); err != nil {
					return fmt.Errorf("desktop setup: remove uncommitted installed version: %w", err)
				}
			}
			if err := movePathDurably(expectedBackup, installPath, false); err != nil {
				return fmt.Errorf("desktop setup: restore uncommitted installed version: %w", err)
			}
			installExists = true
		} else if !installExists {
			return errors.New("desktop setup: uncommitted upgrade lost both installed versions")
		}
	} else {
		if backupExists {
			return errors.New("desktop setup: fresh install transaction has an unexpected backup")
		}
		if installExists {
			if err := removeRealDirectory(installPath); err != nil {
				return fmt.Errorf("desktop setup: remove uncommitted fresh install: %w", err)
			}
		}
	}
	if stageExists {
		if err := removeRealDirectory(expectedStage); err != nil {
			return fmt.Errorf("desktop setup: remove uncommitted staging directory: %w", err)
		}
	}
	if restoreRegistration == nil {
		return errors.New("desktop setup: interrupted transaction requires registration recovery")
	}
	if err := restoreRegistration(record.Registration); err != nil {
		return fmt.Errorf("desktop setup: restore interrupted registration: %w", err)
	}
	if err := removeRegularFile(recordPath); err != nil {
		return fmt.Errorf("desktop setup: remove recovered transaction record: %w", err)
	}
	return nil
}

func recoverWithoutRecord(
	installPath string,
	installExists bool,
	staging []string,
	backups []string,
	commitPath string,
	commitExists bool,
	restoreRegistration restoreRegistrationFunc,
) error {
	if commitExists {
		if !installExists || len(staging) != 0 || len(backups) != 0 {
			return errors.New("desktop setup: orphaned transaction commit has ambiguous install state")
		}
		commit, err := readTransactionCommit(commitPath)
		if err != nil {
			return err
		}
		if restoreRegistration == nil {
			return errors.New("desktop setup: orphaned transaction commit requires registration recovery")
		}
		if err := restoreRegistration(commit.Registration); err != nil {
			return fmt.Errorf("desktop setup: finish orphaned transaction registration: %w", err)
		}
		if err := removeRegularFile(commitPath); err != nil {
			return fmt.Errorf("desktop setup: remove orphaned transaction commit: %w", err)
		}
		return nil
	}
	for _, path := range staging {
		if err := removeRealDirectory(path); err != nil {
			return fmt.Errorf("desktop setup: remove stale staging directory: %w", err)
		}
	}
	if len(backups) > 1 || installExists && len(backups) != 0 {
		return errors.New("desktop setup: interrupted install without a transaction record requires manual recovery")
	}
	if !installExists && len(backups) == 1 {
		if err := movePathDurably(backups[0], installPath, false); err != nil {
			return fmt.Errorf("desktop setup: restore interrupted install: %w", err)
		}
	}
	return nil
}

func removeTransactionPublishingFiles(installPath string) error {
	paths := []string{
		transactionRecordPath(installPath),
		transactionCommitPath(installPath),
		uninstallRecordPath(installPath),
		uninstallCommitPath(installPath),
	}
	for _, path := range paths {
		if err := removeRegularFile(transactionPublishingPath(path)); err != nil {
			return fmt.Errorf("desktop setup: remove incomplete transaction publication: %w", err)
		}
	}
	return nil
}

func readTransactionRecord(path, installBase string) (transactionRecord, error) {
	var record transactionRecord
	if err := readStrictJSON(path, &record); err != nil {
		return transactionRecord{}, fmt.Errorf("desktop setup: read transaction record: %w", err)
	}
	if record.Version != transactionRecordVersion || !validTransactionID(record.ID) ||
		!validTransactionSiblingName(record.Staging, installBase, "stage") ||
		!validTransactionSiblingName(record.Backup, installBase, "backup") {
		return transactionRecord{}, errors.New("desktop setup: invalid transaction record")
	}
	if err := validateRegistrationState(record.Registration); err != nil {
		return transactionRecord{}, err
	}
	return record, nil
}

func readTransactionCommit(path string) (transactionCommit, error) {
	var commit transactionCommit
	if err := readStrictJSON(path, &commit); err != nil {
		return transactionCommit{}, fmt.Errorf("desktop setup: read transaction commit: %w", err)
	}
	if commit.Version != transactionRecordVersion || !validTransactionID(commit.ID) {
		return transactionCommit{}, errors.New("desktop setup: invalid transaction commit")
	}
	if err := validateRegistrationState(commit.Registration); err != nil {
		return transactionCommit{}, err
	}
	return commit, nil
}

func readStrictJSON(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumTransactionBytes {
		return errors.New("invalid transaction state file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumTransactionBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != info.Size() {
		return errors.New("transaction state changed while reading")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailer any
	if err := decoder.Decode(&trailer); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("transaction state contains multiple JSON values")
		}
		return err
	}
	return nil
}

func transactionDirectories(parent, prefix, kind string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(parent, prefix+"*"))
	if err != nil {
		return nil, fmt.Errorf("desktop setup: find %s directories: %w", kind, err)
	}
	for _, path := range paths {
		if err := requireDirectory(path); err != nil {
			return nil, fmt.Errorf("desktop setup: invalid %s directory %q: %w", kind, path, err)
		}
	}
	return paths, nil
}

func requireOnlyExpectedTransactionPaths(paths []string, expected, kind string) error {
	for _, path := range paths {
		if path != expected {
			return fmt.Errorf("desktop setup: unexpected %s directory %q requires manual recovery", kind, path)
		}
	}
	return nil
}

func pathListed(paths []string, expected string) bool {
	return len(paths) == 1 && paths[0] == expected
}

func realDirectoryPresent(path string) (bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := requireDirectory(path); err != nil {
		return false, err
	}
	return true, nil
}

func regularFilePresent(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("path is not a real file")
	}
	return true, nil
}

func newSiblingPath(installPath, purpose string) (string, error) {
	if installPath == "" || !filepath.IsAbs(installPath) || installPath != filepath.Clean(installPath) {
		return "", errors.New("desktop setup: invalid install path")
	}
	if purpose != "stage" && purpose != "backup" && purpose != "remove" {
		return "", errors.New("desktop setup: invalid sibling path purpose")
	}
	parent := filepath.Dir(installPath)
	base := filepath.Base(installPath)
	for attempt := 0; attempt < 16; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("desktop setup: generate transaction path: %w", err)
		}
		path := filepath.Join(parent, "."+base+"-"+purpose+"-"+hex.EncodeToString(random[:]))
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", fmt.Errorf("desktop setup: inspect transaction path: %w", err)
		}
	}
	return "", errors.New("desktop setup: could not allocate a transaction path")
}

func ensureRealDirectory(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return requireDirectory(path)
}

func removeRealDirectory(path string) error {
	if err := requireDirectory(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}
