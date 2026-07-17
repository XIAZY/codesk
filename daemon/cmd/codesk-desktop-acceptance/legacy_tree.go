package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"notty/daemon/internal/desktopacceptance"
)

var (
	errUnsafeLegacyState   = errors.New("legacy CLI state contains an unsafe filesystem entry")
	errUnstableLegacyState = errors.New("legacy CLI state changed while it was fingerprinted")
)

type reparsePointCheck func(string) (bool, error)

func fingerprintLegacyTree(
	ctx context.Context,
	root string,
	isReparsePoint reparsePointCheck,
) (desktopacceptance.LegacyStateFingerprint, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return desktopacceptance.LegacyStateFingerprint{}, nil
	}
	if err != nil {
		return desktopacceptance.LegacyStateFingerprint{}, errUnstableLegacyState
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return desktopacceptance.LegacyStateFingerprint{}, errUnsafeLegacyState
	}
	if unsafe, err := unsafeTreeEntry(root, isReparsePoint); err != nil || unsafe {
		return desktopacceptance.LegacyStateFingerprint{}, errUnsafeLegacyState
	}

	value := desktopacceptance.LegacyStateFingerprint{Present: true}
	digest := sha256.New()
	_, _ = digest.Write([]byte("codesk-legacy-cli-state-v1\x00"))
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errUnstableLegacyState
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		info, err := entry.Info()
		if err != nil {
			return errUnstableLegacyState
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errUnsafeLegacyState
		}
		if unsafe, err := unsafeTreeEntry(path, isReparsePoint); err != nil || unsafe {
			return errUnsafeLegacyState
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) {
			return errUnsafeLegacyState
		}
		relative = filepath.ToSlash(relative)
		if info.IsDir() {
			writeTreeRecord(digest, 'd', relative, 0)
			if value.EntryCount == ^uint64(0) {
				return errUnsafeLegacyState
			}
			value.EntryCount++
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return errUnsafeLegacyState
		}
		file, err := os.Open(path)
		if err != nil {
			return errUnstableLegacyState
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = file.Close()
			return errUnstableLegacyState
		}
		writeTreeRecord(digest, 'f', relative, uint64(info.Size()))
		written, readErr := copyFingerprintBytes(ctx, digest, file)
		finished, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if readErr != nil || statErr != nil || closeErr != nil || written != uint64(info.Size()) ||
			!os.SameFile(opened, finished) || finished.Size() != info.Size() || !finished.ModTime().Equal(info.ModTime()) {
			return errUnstableLegacyState
		}
		if ^uint64(0)-value.ByteCount < written {
			return errUnsafeLegacyState
		}
		if value.EntryCount == ^uint64(0) {
			return errUnsafeLegacyState
		}
		value.EntryCount++
		value.ByteCount += written
		return nil
	})
	if err != nil {
		return desktopacceptance.LegacyStateFingerprint{}, err
	}
	value.DigestSHA256 = hex.EncodeToString(digest.Sum(nil))
	return value, nil
}

func unsafeTreeEntry(path string, check reparsePointCheck) (bool, error) {
	if check == nil {
		return false, nil
	}
	return check(path)
}

func writeTreeRecord(destination io.Writer, kind byte, relative string, size uint64) {
	var encoded [8]byte
	_, _ = destination.Write([]byte{kind})
	binary.BigEndian.PutUint64(encoded[:], uint64(len(relative)))
	_, _ = destination.Write(encoded[:])
	_, _ = destination.Write([]byte(relative))
	binary.BigEndian.PutUint64(encoded[:], size)
	_, _ = destination.Write(encoded[:])
}

func copyFingerprintBytes(ctx context.Context, destination io.Writer, source io.Reader) (uint64, error) {
	buffer := make([]byte, 64<<10)
	var total uint64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		count, err := source.Read(buffer)
		if count > 0 {
			_, _ = destination.Write(buffer[:count])
			if ^uint64(0)-total < uint64(count) {
				return total, errUnsafeLegacyState
			}
			total += uint64(count)
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
