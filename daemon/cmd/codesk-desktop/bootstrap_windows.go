//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
)

func prependExecutableDirectoryToPath(executable string) error {
	directory := filepath.Dir(executable)
	if !filepath.IsAbs(directory) {
		return errors.New("codesk desktop: executable directory is not absolute")
	}
	pathValue := os.Getenv("PATH")
	if pathValue == "" {
		return os.Setenv("PATH", directory)
	}
	return os.Setenv("PATH", directory+string(os.PathListSeparator)+pathValue)
}
