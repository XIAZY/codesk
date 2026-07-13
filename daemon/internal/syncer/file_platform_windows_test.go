//go:build windows

package syncer

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestFileRenameInfoIncludesTrailingNUL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replacement.md")
	buffer, err := makeFileRenameInfo(path)
	if err != nil {
		t.Fatalf("make rename info: %v", err)
	}
	wantName, err := windows.UTF16FromString(path)
	if err != nil {
		t.Fatalf("encode path: %v", err)
	}
	var header fileRenameInfo
	nameOffset := int(unsafe.Offsetof(header.fileName))
	if len(buffer) != nameOffset+len(wantName)*2 {
		t.Fatalf("rename buffer does not include UTF-16 terminator: got %d bytes want %d", len(buffer), nameOffset+len(wantName)*2)
	}
	info := (*fileRenameInfo)(unsafe.Pointer(&buffer[0]))
	if info.flags != windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS {
		t.Fatalf("unexpected rename flags: %#x", info.flags)
	}
	if info.fileNameLength != uint32((len(wantName)-1)*2) {
		t.Fatalf("filename length includes terminator: got %d want %d", info.fileNameLength, (len(wantName)-1)*2)
	}
	gotName := unsafe.Slice(&info.fileName[0], len(wantName))
	if !slices.Equal(gotName, wantName) || gotName[len(gotName)-1] != 0 {
		t.Fatalf("rename filename is not NUL-terminated: got %v want %v", gotName, wantName)
	}
}

func TestFileIdentityFromWindowsInfoRejectsZeroFileIndex(t *testing.T) {
	zero := fileIdentityFromWindowsInfo(windows.ByHandleFileInformation{VolumeSerialNumber: 42})
	if zero.valid {
		t.Fatalf("zero file index should be invalid: %+v", zero)
	}

	identity := fileIdentityFromWindowsInfo(windows.ByHandleFileInformation{
		VolumeSerialNumber: 42,
		FileIndexHigh:      0x01234567,
		FileIndexLow:       0x89abcdef,
	})
	if !identity.valid || identity.dev != 42 || identity.ino != 0x0123456789abcdef {
		t.Fatalf("unexpected Windows identity mapping: %+v", identity)
	}
}

func TestFileIdentitySurvivesRenameOnWindows(t *testing.T) {
	dir := t.TempDir()
	before := filepath.Join(dir, "before.md")
	after := filepath.Join(dir, "after.md")
	if err := os.WriteFile(before, []byte("content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	beforeIdentity := fileIdentityForPath(before)
	if err := os.Rename(before, after); err != nil {
		t.Fatalf("rename file: %v", err)
	}
	afterIdentity := fileIdentityForPath(after)
	if !sameFileIdentity(beforeIdentity, afterIdentity) {
		t.Fatalf("rename changed file identity: before=%+v after=%+v", beforeIdentity, afterIdentity)
	}
}

func TestDirectoryIdentitySurvivesRenameOnWindows(t *testing.T) {
	root := t.TempDir()
	before := filepath.Join(root, "before")
	after := filepath.Join(root, "after")
	if err := os.Mkdir(before, 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	beforeIdentity := fileIdentityForPath(before)
	if err := os.Rename(before, after); err != nil {
		t.Fatalf("rename directory: %v", err)
	}
	afterIdentity := fileIdentityForPath(after)
	if !sameFileIdentity(beforeIdentity, afterIdentity) {
		t.Fatalf("rename changed directory identity: before=%+v after=%+v", beforeIdentity, afterIdentity)
	}
}

func TestFileIdentityChangesAfterDeleteAndRecreateOnWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	firstIdentity := fileIdentityForPath(path)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove first file after all handles closed: %v", err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("write replacement file: %v", err)
	}
	secondIdentity := fileIdentityForPath(path)
	if !firstIdentity.valid || !secondIdentity.valid {
		t.Fatalf("expected valid identities: first=%+v second=%+v", firstIdentity, secondIdentity)
	}
	if sameFileIdentity(firstIdentity, secondIdentity) {
		t.Fatalf("delete/recreate reused stable identity: first=%+v second=%+v", firstIdentity, secondIdentity)
	}
}

func TestFileIdentityFollowsDistinctReplacementObjectOnWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	replacement := filepath.Join(dir, "replacement.md")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	if err := os.WriteFile(replacement, []byte("second"), 0o644); err != nil {
		t.Fatalf("write replacement file: %v", err)
	}
	firstIdentity := fileIdentityForPath(path)
	replacementIdentity := fileIdentityForPath(replacement)
	if !firstIdentity.valid || !replacementIdentity.valid || sameFileIdentity(firstIdentity, replacementIdentity) {
		t.Fatalf("expected two distinct live identities: first=%+v replacement=%+v", firstIdentity, replacementIdentity)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove first file: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("move replacement to original path: %v", err)
	}
	pathIdentity := fileIdentityForPath(path)
	if !sameFileIdentity(pathIdentity, replacementIdentity) || sameFileIdentity(pathIdentity, firstIdentity) {
		t.Fatalf("path did not follow replacement identity: first=%+v replacement=%+v path=%+v", firstIdentity, replacementIdentity, pathIdentity)
	}
}

func TestReplaceFileAtomicallyLeavesDestinationCompleteWhenDeleteSharingIsDenied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	openFile, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open destination without delete sharing: %v", err)
	}
	defer openFile.Close()

	if err := replaceFileAtomically(path, "after", 0o644); err == nil {
		t.Fatal("replacement should fail while destination denies delete sharing")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read destination after failed replacement: %v", err)
	}
	if string(content) != "before" {
		t.Fatalf("failed replacement changed destination: got %q want %q", content, "before")
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".doc.md.*.tmp"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("failed replacement left staging files: %v", matches)
	}
}

func TestAtomicReplacementSucceedsWhileDeleteSharedObservationIsOpenOnWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	observation, err := openFileObservation(path)
	if err != nil {
		t.Fatalf("open delete-shared observation: %v", err)
	}
	defer observation.Close()

	if err := replaceFileAtomically(path, "after", 0o644); err != nil {
		t.Fatalf("replace while observation handle is open: %v", err)
	}
	observed, err := io.ReadAll(observation)
	if err != nil {
		t.Fatalf("read old observation: %v", err)
	}
	if string(observed) != "before" {
		t.Fatalf("open observation changed during replacement: got %q want %q", observed, "before")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if string(current) != "after" {
		t.Fatalf("replacement content mismatch: got %q want %q", current, "after")
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".doc.md.*.tmp"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("replacement stranded staging files: %v", matches)
	}
}
