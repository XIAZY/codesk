package desktopacceptance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, fmt.Errorf("%s is not a bounded regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maximum)
	}
	return data, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file for hashing: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashBytes(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func decodeCanonicalJSON(data []byte, destination any, label string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	var trailer any
	if err := decoder.Decode(&trailer); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s contains multiple JSON values", label)
		}
		return fmt.Errorf("decode %s trailer: %w", label, err)
	}
	canonical, err := json.MarshalIndent(destination, "", "  ")
	if err != nil {
		return fmt.Errorf("encode canonical %s: %w", label, err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return fmt.Errorf("%s is not canonical JSON", label)
	}
	return nil
}
