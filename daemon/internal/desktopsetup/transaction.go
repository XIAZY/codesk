package desktopsetup

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	transactionRecordVersion = 2
	maximumTransactionBytes  = 1 << 20
	registryStringType       = 1
	registryExpandStringType = 2
)

type transactionRecord struct {
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	Staging      string            `json:"staging"`
	Backup       string            `json:"backup"`
	HadInstall   bool              `json:"hadInstall"`
	Registration registrationState `json:"registration"`
}

type transactionCommit struct {
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	Registration registrationState `json:"registration"`
}

type directoryTransaction struct {
	installPath          string
	stagingPath          string
	backupPath           string
	recordPath           string
	commitPath           string
	hadInstall           bool
	id                   string
	prepared             bool
	swapped              bool
	committed            bool
	commitStateUncertain bool
	writeState           func(string, any) error
}

func newDirectoryTransaction(installPath, stagingPath, backupPath string) (*directoryTransaction, error) {
	paths := []string{installPath, stagingPath, backupPath}
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) || path != filepath.Clean(path) {
			return nil, errors.New("desktop setup: transaction paths must be absolute and clean")
		}
	}
	parent := filepath.Dir(installPath)
	if filepath.Dir(stagingPath) != parent || filepath.Dir(backupPath) != parent {
		return nil, errors.New("desktop setup: transaction paths must share a parent directory")
	}
	if installPath == stagingPath || installPath == backupPath || stagingPath == backupPath {
		return nil, errors.New("desktop setup: transaction paths must be distinct")
	}
	base := filepath.Base(installPath)
	if !validTransactionSiblingName(filepath.Base(stagingPath), base, "stage") ||
		!validTransactionSiblingName(filepath.Base(backupPath), base, "backup") {
		return nil, errors.New("desktop setup: transaction paths have invalid names")
	}
	return &directoryTransaction{
		installPath: installPath,
		stagingPath: stagingPath,
		backupPath:  backupPath,
		recordPath:  transactionRecordPath(installPath),
		commitPath:  transactionCommitPath(installPath),
		writeState:  writeExclusiveJSON,
	}, nil
}

func (t *directoryTransaction) Prepare(registration registrationState) error {
	if t == nil || t.prepared || t.swapped || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: invalid directory transaction state")
	}
	if err := validateRegistrationState(registration); err != nil {
		return err
	}
	id, err := newTransactionID()
	if err != nil {
		return err
	}
	record := transactionRecord{
		Version:      transactionRecordVersion,
		ID:           id,
		Staging:      filepath.Base(t.stagingPath),
		Backup:       filepath.Base(t.backupPath),
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
		return fmt.Errorf("desktop setup: persist transaction record: %w", err)
	}
	t.id = id
	t.hadInstall = record.HadInstall
	t.prepared = true
	return nil
}

func (t *directoryTransaction) Swap() error {
	if t == nil || !t.prepared || t.swapped || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: invalid directory transaction state")
	}
	if err := requireDirectory(t.stagingPath); err != nil {
		return fmt.Errorf("desktop setup: invalid staging directory: %w", err)
	}
	if _, err := os.Lstat(t.backupPath); err == nil {
		return errors.New("desktop setup: backup path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: inspect backup path: %w", err)
	}

	if t.hadInstall {
		if err := requireDirectory(t.installPath); err != nil {
			return fmt.Errorf("desktop setup: invalid install directory: %w", err)
		}
		if err := movePathDurably(t.installPath, t.backupPath, false); err != nil {
			return fmt.Errorf("desktop setup: preserve installed version: %w", err)
		}
	} else if _, err := os.Lstat(t.installPath); err == nil {
		return errors.New("desktop setup: install directory appeared after transaction preparation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: inspect install directory: %w", err)
	}

	if err := movePathDurably(t.stagingPath, t.installPath, false); err != nil {
		var restoreErr error
		if t.hadInstall {
			restoreErr = movePathDurably(t.backupPath, t.installPath, false)
		}
		return errors.Join(
			fmt.Errorf("desktop setup: activate staged version: %w", err),
			wrapTransactionError("restore installed version", restoreErr),
		)
	}
	t.swapped = true
	return nil
}

func (t *directoryTransaction) Rollback() error {
	if t == nil || !t.prepared || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot roll back an inactive transaction")
	}
	installExists, err := realDirectoryPresent(t.installPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect installed version during rollback: %w", err)
	}
	backupExists, err := realDirectoryPresent(t.backupPath)
	if err != nil {
		return fmt.Errorf("desktop setup: inspect backup during rollback: %w", err)
	}

	if !t.hadInstall {
		if backupExists {
			return errors.New("desktop setup: fresh install rollback has an unexpected backup")
		}
		if installExists {
			if err := removeRealDirectory(t.installPath); err != nil {
				return fmt.Errorf("desktop setup: remove failed fresh install: %w", err)
			}
		}
		t.swapped = false
		return nil
	}

	if !backupExists {
		if t.swapped || !installExists {
			return errors.New("desktop setup: upgrade rollback lost the previous installed version")
		}
		return nil
	}
	if installExists {
		if err := removeRealDirectory(t.installPath); err != nil {
			return fmt.Errorf("desktop setup: remove failed version: %w", err)
		}
	}
	if err := movePathDurably(t.backupPath, t.installPath, false); err != nil {
		return fmt.Errorf("desktop setup: restore installed version: %w", err)
	}
	t.swapped = false
	return nil
}

func (t *directoryTransaction) Forget() error {
	if t == nil || !t.prepared || t.swapped || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot forget an active transaction")
	}
	if err := removeRegularFile(t.recordPath); err != nil {
		return fmt.Errorf("desktop setup: remove rolled-back transaction record: %w", err)
	}
	t.prepared = false
	return nil
}

func (t *directoryTransaction) Commit(registration registrationState) error {
	if t == nil || !t.prepared || !t.swapped || t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot commit an inactive transaction")
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
		return t.reconcileCommitPublication(commit, err)
	}
	t.committed = true
	return nil
}

func (t *directoryTransaction) Complete() error {
	if t == nil || !t.prepared || !t.swapped || !t.committed || t.commitStateUncertain {
		return errors.New("desktop setup: cannot complete an uncommitted transaction")
	}
	if t.hadInstall {
		if err := removeRealDirectory(t.backupPath); err != nil {
			return fmt.Errorf("desktop setup: remove previous version: %w", err)
		}
	}
	if err := removeRegularFile(t.recordPath); err != nil {
		return fmt.Errorf("desktop setup: remove completed transaction record: %w", err)
	}
	if err := removeRegularFile(t.commitPath); err != nil {
		return fmt.Errorf("desktop setup: remove transaction commit: %w", err)
	}
	t.prepared = false
	t.swapped = false
	t.committed = false
	return nil
}

func (t *directoryTransaction) rollbackAllowed() bool {
	return t != nil && !t.committed && !t.commitStateUncertain
}

func (t *directoryTransaction) reconcileCommitPublication(want transactionCommit, publicationErr error) error {
	present, matches, observationErr := observeCommitPublication(t.commitPath, want)
	if observationErr != nil {
		t.commitStateUncertain = true
		return errors.Join(
			fmt.Errorf("desktop setup: persist transaction commit: %w", publicationErr),
			fmt.Errorf("desktop setup: inspect transaction commit after publication failure: %w", observationErr),
		)
	}
	if !present {
		return fmt.Errorf("desktop setup: persist transaction commit: %w", publicationErr)
	}
	t.commitStateUncertain = true
	if matches {
		t.committed = true
		return fmt.Errorf("desktop setup: transaction commit publication reported failure after publishing the expected proof: %w", publicationErr)
	}
	return fmt.Errorf("desktop setup: transaction commit publication left an unexpected proof: %w", publicationErr)
}

func observeCommitPublication(path string, want transactionCommit) (bool, bool, error) {
	present, err := regularFilePresent(path)
	if err != nil || !present {
		return present, false, err
	}
	got, err := readTransactionCommit(path)
	if err != nil {
		return true, false, err
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return true, false, err
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		return true, false, err
	}
	return true, bytes.Equal(gotJSON, wantJSON), nil
}

func transactionRecordPath(installPath string) string {
	return filepath.Join(filepath.Dir(installPath), "."+filepath.Base(installPath)+"-transaction.json")
}

func transactionCommitPath(installPath string) string {
	return filepath.Join(filepath.Dir(installPath), "."+filepath.Base(installPath)+"-transaction.commit")
}

func validTransactionSiblingName(name, installBase, purpose string) bool {
	prefix := "." + installBase + "-" + purpose + "-"
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == name || len(suffix) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(suffix)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == suffix
}

func newTransactionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("desktop setup: generate transaction ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validTransactionID(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == value
}

func writeExclusiveJSON(path string, value any) (returnErr error) {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode transaction state: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maximumTransactionBytes {
		return errors.New("transaction state is too large")
	}
	publishingPath := transactionPublishingPath(path)
	file, err := os.OpenFile(publishingPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			if err := file.Close(); returnErr == nil && err != nil {
				returnErr = err
			}
		}
		if returnErr != nil {
			_ = os.Remove(publishingPath)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	if err := movePathDurably(publishingPath, path, false); err != nil {
		return err
	}
	return nil
}

func transactionPublishingPath(path string) string {
	return path + ".publishing"
}

func validateRegistrationState(state registrationState) error {
	if err := validateStringValueState(state.Run); err != nil {
		return fmt.Errorf("desktop setup: invalid saved login registration: %w", err)
	}
	if !state.Shortcut.Present {
		if len(state.Shortcut.Data) != 0 {
			return errors.New("desktop setup: absent saved shortcut has data")
		}
	} else if len(state.Shortcut.Data) == 0 || len(state.Shortcut.Data) > maximumRegistrationBlobBytes {
		return errors.New("desktop setup: invalid saved shortcut")
	}
	if !state.Uninstall.Existed {
		if len(state.Uninstall.Values) != 0 {
			return errors.New("desktop setup: invalid absent uninstall registration")
		}
		return nil
	}
	total := 0
	for index, value := range state.Uninstall.Values {
		if len(value.Name) > 16383 || strings.ContainsRune(value.Name, '\x00') {
			return errors.New("desktop setup: invalid saved uninstall value name")
		}
		if index > 0 && state.Uninstall.Values[index-1].Name >= value.Name {
			return errors.New("desktop setup: saved uninstall values are not unique and sorted")
		}
		for prior := 0; prior < index; prior++ {
			if strings.EqualFold(state.Uninstall.Values[prior].Name, value.Name) {
				return errors.New("desktop setup: saved uninstall values are not unique under Windows comparison")
			}
		}
		total += len(value.Name) + len(value.Data)
		if total > maximumRegistrationBlobBytes {
			return errors.New("desktop setup: saved uninstall registration is too large")
		}
	}
	return nil
}

func validateStringValueState(state stringValueState) error {
	if !state.Present {
		if state.Value != "" || state.Type != 0 {
			return errors.New("absent value has data")
		}
		return nil
	}
	if (state.Type != registryStringType && state.Type != registryExpandStringType) ||
		len(state.Value) > 32768 || strings.ContainsRune(state.Value, '\x00') {
		return errors.New("present value has invalid data")
	}
	return nil
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path is not a real directory")
	}
	return nil
}

func removeRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path is not a real file")
	}
	return os.Remove(path)
}

func wrapTransactionError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("desktop setup: %s: %w", operation, err)
}
