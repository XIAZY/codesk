package syncer

import (
	"io"
	"os"
	"path/filepath"
)

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
	defer os.Remove(tmpPath)
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
	return commitReplacement(tmpPath, path)
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
