package syncer

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

var errProjectedFileDiverged = errors.New("projected file diverged from last materialized content")

func readFileLocked(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := lockFile(file, syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer unlockFile(file)
	return io.ReadAll(file)
}

func appendFileLocked(path string, line string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := lockFile(file, syscall.LOCK_EX); err != nil {
		return err
	}
	defer unlockFile(file)
	return writeFullString(file, line)
}

func appendOpenFileLocked(file *os.File, line string) error {
	if err := lockFile(file, syscall.LOCK_EX); err != nil {
		return err
	}
	defer unlockFile(file)
	return writeFullString(file, line)
}

func writeIfChanged(path string, content string) error {
	return writeFileLocked(path, content, projectedContentHash{}, false)
}

func writeProjectedFileLocked(path string, content string, expected projectedContentHash) error {
	return writeFileLocked(path, content, expected, true)
}

func writeFileLocked(path string, content string, expected projectedContentHash, requireExpectedMatch bool) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if errors.Is(err, os.ErrNotExist) {
		if requireExpectedMatch && isKnownProjectedHash(expected) && expected != projectedHashBytes(nil) {
			return errProjectedFileDiverged
		}
		file, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := lockFile(file, syscall.LOCK_EX); err != nil {
		return err
	}
	defer unlockFile(file)

	current, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	nextHash := projectedHashString(content)
	currentHash := projectedHashBytes(current)
	if currentHash == nextHash {
		return nil
	}
	if requireExpectedMatch && isKnownProjectedHash(expected) && currentHash != expected {
		return errProjectedFileDiverged
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return replaceFileAtomically(path, content, info.Mode().Perm())
}

func replaceFileAtomically(path string, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.WriteString(tmp, content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

func lockFile(file *os.File, op int) error {
	for {
		err := syscall.Flock(int(file.Fd()), op)
		if err == syscall.EINTR {
			continue
		}
		return err
	}
}

func unlockFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func writeFullString(file *os.File, content string) error {
	written, err := io.WriteString(file, content)
	if err != nil {
		return err
	}
	if written != len(content) {
		return io.ErrShortWrite
	}
	return nil
}

func isKnownProjectedHash(hash projectedContentHash) bool {
	return hash != (projectedContentHash{})
}
