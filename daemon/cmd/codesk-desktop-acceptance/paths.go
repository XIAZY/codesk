package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func samePath(left, right string) bool {
	left, _ = filepath.Abs(left)
	right, _ = filepath.Abs(right)
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func pathWithin(root, candidate string) (bool, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return false, fmt.Errorf("resolve reset root: %w", err)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return false, fmt.Errorf("resolve protected path: %w", err)
	}
	if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(candidate)) {
		return false, nil
	}
	root = strings.ToLower(filepath.Clean(root))
	candidate = strings.ToLower(filepath.Clean(candidate))
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, fmt.Errorf("compare reset and protected paths: %w", err)
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

func pathWithinResolved(root, candidate string) (bool, error) {
	resolvedRoot, err := resolveExistingPathPrefix(root)
	if err != nil {
		return false, fmt.Errorf("resolve physical reset root: %w", err)
	}
	resolvedCandidate, err := resolveExistingPathPrefix(candidate)
	if err != nil {
		return false, fmt.Errorf("resolve physical protected path: %w", err)
	}
	return pathWithin(resolvedRoot, resolvedCandidate)
}

func resolveExistingPathPrefix(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	existing := absolute
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(existing)
			if err != nil {
				return "", err
			}
			remainder, err := filepath.Rel(existing, absolute)
			if err != nil {
				return "", err
			}
			if remainder == "." {
				return filepath.Clean(resolved), nil
			}
			return filepath.Clean(filepath.Join(resolved, remainder)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing path prefix for %s", absolute)
		}
		existing = parent
	}
}
