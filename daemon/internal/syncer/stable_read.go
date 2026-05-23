package syncer

import (
	"context"
	"errors"
	"io"
	"os"
)

var ErrFileTooLargeForSingleRead = errors.New("file is too large for one stable read")

type StableReadOptions struct {
	ExpectedStat *FileStat
	Capabilities ScanCapabilities
	MaxBytes     int64
}

type ReadBytesResult struct {
	Bytes     []byte
	OpenStat  FileStat
	FinalStat FileStat
}

func (fs *WorkspaceFS) ReadBytesStable(ctx context.Context, path string, opts StableReadOptions) (ReadBytesResult, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, err := fs.cleanPath(fs.Abs(path))
	if err != nil {
		return ReadBytesResult{}, false, err
	}

	var file *os.File
	var openStat FileStat
	err = fs.withFilesystemLockContext(ctx, "open-stable-read", path, "", func() error {
		current, err := fs.Stat(ctx, path)
		if err != nil || !current.Exists || current.Kind != FileKindFile {
			return err
		}
		if opts.ExpectedStat != nil && !SameCreateDiscoveryStat(*opts.ExpectedStat, current, opts.Capabilities) {
			return nil
		}
		opened, err := os.Open(path)
		if err != nil {
			return err
		}
		fdStat, err := fs.FStat(opened, path)
		if err != nil {
			_ = opened.Close()
			return err
		}
		if !SameOpenFileStat(current, fdStat, opts.Capabilities) {
			_ = opened.Close()
			return nil
		}
		file = opened
		openStat = fdStat
		return nil
	})
	if err != nil || file == nil {
		return ReadBytesResult{}, false, err
	}
	defer file.Close()

	var data []byte
	if opts.MaxBytes > 0 {
		limited := io.LimitReader(file, opts.MaxBytes+1)
		data, err = io.ReadAll(limited)
		if err != nil {
			return ReadBytesResult{}, false, err
		}
		if int64(len(data)) > opts.MaxBytes {
			return ReadBytesResult{}, false, ErrFileTooLargeForSingleRead
		}
	} else {
		data, err = io.ReadAll(file)
		if err != nil {
			return ReadBytesResult{}, false, err
		}
	}
	return fs.finishStableRead(ctx, path, file, data, openStat, opts)
}

func (fs *WorkspaceFS) finishStableRead(ctx context.Context, path string, file *os.File, data []byte, openStat FileStat, opts StableReadOptions) (ReadBytesResult, bool, error) {
	var finalStat FileStat
	ok := false
	err := fs.withFilesystemLockContext(ctx, "finish-stable-read", path, "", func() error {
		current, err := fs.Stat(ctx, path)
		if err != nil || !current.Exists || current.Kind != FileKindFile {
			return err
		}
		fdStat, err := fs.FStat(file, path)
		if err != nil {
			return err
		}
		if !SameOpenFileStat(openStat, fdStat, opts.Capabilities) {
			return nil
		}
		if !SameOpenFileStat(openStat, current, opts.Capabilities) {
			return nil
		}
		finalStat = current
		ok = true
		return nil
	})
	if err != nil || !ok {
		return ReadBytesResult{}, false, err
	}
	return ReadBytesResult{Bytes: data, OpenStat: openStat, FinalStat: finalStat}, true, nil
}

func (fs *WorkspaceFS) FStat(file *os.File, path string) (FileStat, error) {
	if file == nil {
		return FileStat{}, errors.New("file is required")
	}
	info, err := file.Stat()
	if err != nil {
		return FileStat{}, err
	}
	return fileStatFromInfo(path, info), nil
}

func SameCreateDiscoveryStat(expected FileStat, current FileStat, caps ScanCapabilities) bool {
	return SameOpenFileStat(expected, current, caps)
}

func SameOpenFileStat(opened FileStat, current FileStat, caps ScanCapabilities) bool {
	if !opened.StatValid || !current.StatValid {
		return false
	}
	if opened.Kind != current.Kind ||
		opened.SizeBytes != current.SizeBytes ||
		opened.Mode != current.Mode ||
		opened.MTimeNS != current.MTimeNS {
		return false
	}
	if caps.CTimeReliable && opened.CTimeNS != current.CTimeNS {
		return false
	}
	if caps.FileKeyReliable {
		return opened.FileKey != "" && current.FileKey != "" && opened.FileKey == current.FileKey
	}
	return true
}
