package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

type FileKind string

const (
	FileKindFile        FileKind = "file"
	FileKindDir         FileKind = "dir"
	FileKindSymlink     FileKind = "symlink"
	FileKindUnsupported FileKind = "unsupported"
)

type FileStat struct {
	Path      string
	Kind      FileKind
	Exists    bool
	FileKey   string
	SizeBytes int64
	Mode      uint32
	MTimeNS   int64
	CTimeNS   int64
	StatValid bool
}

type ScanCapabilities struct {
	DirectoryMTimeReliable bool
	FileKeyReliable        bool
	CTimeReliable          bool
}

func SameStatTuple(cached FileStat, current FileStat, caps ScanCapabilities) bool {
	if !cached.StatValid || !current.StatValid {
		return false
	}
	if cached.Kind != current.Kind ||
		cached.SizeBytes != current.SizeBytes ||
		cached.Mode != current.Mode ||
		cached.MTimeNS != current.MTimeNS {
		return false
	}
	if caps.CTimeReliable && cached.CTimeNS != current.CTimeNS {
		return false
	}
	if !caps.FileKeyReliable {
		return false
	}
	if cached.FileKey == "" || current.FileKey == "" || cached.FileKey != current.FileKey {
		return false
	}
	return true
}

func (fs *WorkspaceFS) Abs(path string) string {
	if fs == nil || fs.Root == "" {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return filepath.Clean(path)
	}
	if filepath.IsAbs(path) {
		cleaned, err := fs.cleanPath(path)
		if err != nil {
			return filepath.Clean(path)
		}
		return cleaned
	}
	return filepath.Join(fs.Root, filepath.FromSlash(path))
}

func (fs *WorkspaceFS) Stat(ctx context.Context, path string) (FileStat, error) {
	_ = ctx
	abs := fs.Abs(path)
	cleaned, err := fs.cleanPath(abs)
	if err != nil {
		return FileStat{}, err
	}
	info, err := os.Lstat(cleaned)
	if os.IsNotExist(err) {
		return FileStat{Path: cleaned, Exists: false}, nil
	}
	if err != nil {
		return FileStat{}, &FSError{Op: "stat", Path: cleaned, Err: err}
	}
	return fileStatFromInfo(cleaned, info), nil
}

func (fs *WorkspaceFS) FileKeyAbs(abs string) (string, bool) {
	info, err := os.Stat(abs)
	if err != nil {
		return "", false
	}
	return fileKeyFromInfo(info)
}

func (fs *WorkspaceFS) TestFileKeyReliability(ctx context.Context) bool {
	_ = ctx
	dir := fs.Abs(".notty/tmp/filekey-test")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	a2 := filepath.Join(dir, "a-renamed")
	if err := os.WriteFile(a, []byte("a"), 0o600); err != nil {
		return false
	}
	if err := os.WriteFile(b, []byte("b"), 0o600); err != nil {
		return false
	}
	ka1, oka1 := fs.FileKeyAbs(a)
	kb1, okb1 := fs.FileKeyAbs(b)
	if !oka1 || !okb1 || ka1 == "" || kb1 == "" || ka1 == kb1 {
		return false
	}
	time.Sleep(10 * time.Millisecond)
	ka2, oka2 := fs.FileKeyAbs(a)
	kb2, okb2 := fs.FileKeyAbs(b)
	if !oka2 || !okb2 || ka1 != ka2 || kb1 != kb2 {
		return false
	}
	if err := os.Rename(a, a2); err != nil {
		return false
	}
	ka3, oka3 := fs.FileKeyAbs(a2)
	if !oka3 || ka3 != ka1 {
		return false
	}
	c := filepath.Join(dir, "c")
	if err := os.WriteFile(c, []byte("a"), 0o600); err != nil {
		return false
	}
	kc, okc := fs.FileKeyAbs(c)
	if !okc || kc == "" || kc == ka1 || kc == kb1 {
		return false
	}
	return true
}

func (fs *WorkspaceFS) TestDirectoryMTimeReliability(ctx context.Context) bool {
	_ = ctx
	dir := fs.Abs(".notty/tmp/mtime-test")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	before, ok := statDirMTimeNS(dir)
	if !ok {
		return false
	}
	time.Sleep(10 * time.Millisecond)
	temp := filepath.Join(dir, "temp")
	if err := os.WriteFile(temp, []byte("x"), 0o600); err != nil {
		return false
	}
	afterCreate, ok := statDirMTimeNS(dir)
	if !ok {
		return false
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.Remove(temp); err != nil {
		return false
	}
	afterRemove, ok := statDirMTimeNS(dir)
	if !ok {
		return false
	}
	return afterCreate != before && afterRemove != afterCreate
}

func (fs *WorkspaceFS) TestCTimeReliability(ctx context.Context) bool {
	_ = ctx
	dir := fs.Abs(".notty/tmp/ctime-test")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		return false
	}
	before, ok := statCTimeNS(path)
	if !ok || before == 0 {
		return false
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.Chmod(path, 0o640); err != nil {
		return false
	}
	after, ok := statCTimeNS(path)
	if !ok {
		return false
	}
	return after != before
}

func fileStatFromInfo(path string, info os.FileInfo) FileStat {
	kind := FileKindUnsupported
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		kind = FileKindFile
	case mode.IsDir():
		kind = FileKindDir
	case mode&os.ModeSymlink != 0:
		kind = FileKindSymlink
	}
	key, _ := fileKeyFromInfo(info)
	return FileStat{
		Path:      path,
		Kind:      kind,
		Exists:    true,
		FileKey:   key,
		SizeBytes: info.Size(),
		Mode:      uint32(mode.Perm()),
		MTimeNS:   info.ModTime().UnixNano(),
		CTimeNS:   ctimeNSFromInfo(info),
		StatValid: true,
	}
}

func fileKeyFromInfo(info os.FileInfo) (string, bool) {
	if info == nil {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	if runtime.GOOS == "windows" {
		return "", false
	}
	return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino)), true
}

func ctimeNSFromInfo(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ctim.Nano()
}

func statDirMTimeNS(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return 0, false
	}
	return info.ModTime().UnixNano(), true
}

func statCTimeNS(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	ctime := ctimeNSFromInfo(info)
	return ctime, ctime != 0
}
