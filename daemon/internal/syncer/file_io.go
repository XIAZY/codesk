package syncer

import (
	"io"
	"os"
	"path/filepath"
)

func replaceFileAtomically(path string, content string, mode os.FileMode) error {
	return replaceFileAtomicallyWith(path, content, mode, (*os.File).Sync, commitReplacement)
}

func replaceFileAtomicallyWith(
	path string,
	content string,
	mode os.FileMode,
	syncStaged func(*os.File) error,
	commit func(stagedPath, targetPath string) error,
) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.WriteString(tmp, content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	// The namespace commit must never publish bytes that are still only buffered.
	if err := syncStaged(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return commit(tmpPath, path)
}

func readFileObservation(path string) ([]byte, error) {
	file, err := openFileObservation(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
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
