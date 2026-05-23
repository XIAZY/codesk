package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ScanBudget struct {
	MaxPaths     int
	MaxDirs      int
	MaxDuration  time.Duration
	MaxHashBytes int64
}

type ScanOptions struct {
	Hints        []ScanHint
	StatOnly     bool
	UseDirCache  bool
	Capabilities ScanCapabilities
	Budget       ScanBudget
	CursorPath   string
}

type WorkspaceScan struct {
	Files      map[string]FileSnapshot
	Dirs       map[string]FileStat
	Missing    map[string]struct{}
	Incomplete bool
	CursorPath string
}

var errScanBudgetExceeded = errors.New("scan budget exceeded")

func (fs *WorkspaceFS) Scan(ctx context.Context, opts ScanOptions) (WorkspaceScan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !opts.Capabilities.DirectoryMTimeReliable {
		opts.UseDirCache = false
	}
	opts.CursorPath = normalizeStateRelPath(opts.CursorPath)
	if hasFullHint(opts.Hints) || opts.CursorPath != "" || len(opts.Hints) == 0 {
		return fs.budgetedFullStatScan(ctx, opts)
	}

	scan := newWorkspaceScan()
	meter := newScanBudgetMeter(opts.Budget)
	for _, hint := range coalesceScanHints(opts.Hints) {
		if err := ctx.Err(); err != nil {
			return scan, err
		}
		hint.Path = normalizeStateRelPath(hint.Path)
		if isIgnoredStatePath(hint.Path) {
			continue
		}
		switch hint.Kind {
		case ScanHintPath:
			snap, stat, err := fs.statPathSnapshot(ctx, hint.Path, opts, meter)
			if err != nil {
				return scan, err
			}
			if stat.Exists {
				if stat.Kind == FileKindDir {
					scan.Dirs[hint.Path] = stat
				} else {
					scan.Files[hint.Path] = snap
				}
			} else {
				scan.Missing[hint.Path] = struct{}{}
			}
			if meter.exceeded() {
				scan.Incomplete = true
				scan.CursorPath = hint.Path
				return scan, nil
			}
		case ScanHintDir:
			partial, err := fs.scanDirectoryWithCache(ctx, hint.Path, opts, meter)
			if err != nil {
				return scan, err
			}
			scan.Merge(partial)
			if partial.Incomplete {
				scan.Incomplete = true
				scan.CursorPath = partial.CursorPath
				return scan, nil
			}
			if meter.exceeded() {
				scan.Incomplete = true
				scan.CursorPath = hint.Path
				return scan, nil
			}
		}
	}
	return scan, nil
}

func (s *WorkspaceScan) Merge(other WorkspaceScan) {
	if s == nil {
		return
	}
	s.ensure()
	for path, snap := range other.Files {
		s.Files[path] = snap
	}
	for path, stat := range other.Dirs {
		s.Dirs[path] = stat
	}
	for path := range other.Missing {
		s.Missing[path] = struct{}{}
	}
	if other.Incomplete {
		s.Incomplete = true
		s.CursorPath = other.CursorPath
	}
}

func (s *WorkspaceScan) ensure() {
	if s.Files == nil {
		s.Files = map[string]FileSnapshot{}
	}
	if s.Dirs == nil {
		s.Dirs = map[string]FileStat{}
	}
	if s.Missing == nil {
		s.Missing = map[string]struct{}{}
	}
}

func newWorkspaceScan() WorkspaceScan {
	return WorkspaceScan{
		Files:   map[string]FileSnapshot{},
		Dirs:    map[string]FileStat{},
		Missing: map[string]struct{}{},
	}
}

func (fs *WorkspaceFS) budgetedFullStatScan(ctx context.Context, opts ScanOptions) (WorkspaceScan, error) {
	scan := newWorkspaceScan()
	meter := newScanBudgetMeter(opts.Budget)
	root := fs.Abs("")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := fs.scanRelPath(path)
		if rel == "" {
			return nil
		}
		if isIgnoredStatePath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipScanCursor(rel, entry.IsDir(), opts.CursorPath) {
			if entry.IsDir() && !cursorMayContainRemainingPath(rel, opts.CursorPath) {
				return filepath.SkipDir
			}
			return nil
		}
		snap, stat, err := fs.statPathSnapshot(ctx, rel, opts, meter)
		if err != nil {
			return err
		}
		if !stat.Exists {
			scan.Missing[rel] = struct{}{}
			return nil
		}
		if stat.Kind == FileKindDir {
			scan.Dirs[rel] = stat
			if meter.noteDir() {
				scan.Incomplete = true
				scan.CursorPath = rel
				return errScanBudgetExceeded
			}
		} else {
			scan.Files[rel] = snap
		}
		if meter.exceeded() {
			scan.Incomplete = true
			scan.CursorPath = rel
			return errScanBudgetExceeded
		}
		return nil
	})
	if errors.Is(err, errScanBudgetExceeded) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	return scan, nil
}

func (fs *WorkspaceFS) scanDirectoryWithCache(ctx context.Context, dir string, opts ScanOptions, meter *scanBudgetMeter) (WorkspaceScan, error) {
	scan := newWorkspaceScan()
	dir = normalizeStateRelPath(dir)
	if isIgnoredStatePath(dir) {
		return scan, nil
	}
	current, err := fs.statForScan(ctx, dir, opts.Capabilities)
	if err != nil {
		return scan, err
	}
	if !current.Exists {
		scan.Missing[dir] = struct{}{}
		return scan, nil
	}
	if current.Kind != FileKindDir {
		snap, _, err := fs.statPathSnapshot(ctx, dir, opts, meter)
		if err != nil {
			return scan, err
		}
		scan.Files[dir] = snap
		return scan, nil
	}
	if meter.noteDir() {
		scan.Incomplete = true
		scan.CursorPath = dir
		return scan, nil
	}

	children, fromCache, err := fs.directoryChildren(ctx, dir, current, opts)
	if err != nil {
		return scan, err
	}
	for _, childName := range children {
		if err := ctx.Err(); err != nil {
			return scan, err
		}
		childRel := joinScanRel(dir, childName)
		if isIgnoredStatePath(childRel) {
			continue
		}
		snap, stat, err := fs.statPathSnapshot(ctx, childRel, opts, meter)
		if err != nil {
			return scan, err
		}
		if !stat.Exists {
			if fromCache {
				scan.Missing[childRel] = struct{}{}
			}
			continue
		}
		if stat.Kind == FileKindDir {
			scan.Dirs[childRel] = stat
		} else {
			scan.Files[childRel] = snap
		}
		if meter.exceeded() {
			scan.Incomplete = true
			scan.CursorPath = childRel
			return scan, nil
		}
	}
	return scan, nil
}

func (fs *WorkspaceFS) directoryChildren(ctx context.Context, dir string, current FileStat, opts ScanOptions) ([]string, bool, error) {
	if opts.UseDirCache && fs.State != nil {
		cached, ok, err := fs.State.LoadDirectoryScanCache(ctx, dir)
		if err != nil {
			return nil, false, err
		}
		if ok && directoryCacheValidFor(cached, current, opts.Capabilities) {
			children := append([]string(nil), cached.Children...)
			sort.Strings(children)
			return children, true, nil
		}
	}

	entries, err := os.ReadDir(fs.Abs(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &FSError{Op: "scan-dir", Path: fs.Abs(dir), Err: err}
	}
	children := make([]string, 0, len(entries))
	for _, entry := range entries {
		childRel := joinScanRel(dir, entry.Name())
		if isIgnoredStatePath(childRel) {
			continue
		}
		children = append(children, entry.Name())
	}
	sort.Strings(children)
	if fs.State != nil && opts.UseDirCache {
		if err := fs.State.StoreDirectoryScanCache(ctx, dir, current.MTimeNS, current.CTimeNS, children); err != nil {
			return nil, false, err
		}
	}
	return children, false, nil
}

func (fs *WorkspaceFS) statPathSnapshot(ctx context.Context, rel string, opts ScanOptions, meter *scanBudgetMeter) (FileSnapshot, FileStat, error) {
	stat, err := fs.statForScan(ctx, rel, opts.Capabilities)
	if err != nil {
		return FileSnapshot{}, FileStat{}, err
	}
	if meter.notePath() {
		return FileSnapshot{Path: rel, Exists: stat.Exists, Stat: stat}, stat, nil
	}
	snap := FileSnapshot{Path: rel, Exists: stat.Exists, Stat: stat}
	if !stat.Exists || stat.Kind != FileKindFile || opts.StatOnly || opts.Budget.MaxHashBytes <= 0 {
		return snap, stat, nil
	}
	remaining := meter.remainingHashBytes()
	if remaining <= 0 || stat.SizeBytes > remaining {
		return snap, stat, nil
	}
	read, ok, err := fs.ReadBytesStable(ctx, rel, StableReadOptions{
		ExpectedStat: &stat,
		Capabilities: opts.Capabilities,
		MaxBytes:     remaining,
	})
	if errors.Is(err, ErrFileTooLargeForSingleRead) {
		return snap, stat, nil
	}
	if err != nil {
		return FileSnapshot{}, FileStat{}, err
	}
	if ok {
		meter.noteHashBytes(int64(len(read.Bytes)))
		snap.Bytes = read.Bytes
		snap.Hash = projectedHashBytes(read.Bytes)
		snap.Stat = read.FinalStat
		stat = read.FinalStat
	}
	return snap, stat, nil
}

func (fs *WorkspaceFS) statForScan(ctx context.Context, rel string, caps ScanCapabilities) (FileStat, error) {
	stat, err := fs.Stat(ctx, rel)
	if err != nil {
		return FileStat{}, err
	}
	stat.Path = normalizeStateRelPath(rel)
	if !caps.FileKeyReliable {
		stat.FileKey = ""
	}
	return stat, nil
}

func hasFullHint(hints []ScanHint) bool {
	for _, hint := range hints {
		if hint.Kind == ScanHintFull {
			return true
		}
	}
	return false
}

func coalesceScanHints(hints []ScanHint) []ScanHint {
	seen := map[string]struct{}{}
	out := make([]ScanHint, 0, len(hints))
	for _, hint := range hints {
		if hint.Kind == ScanHintFull {
			return []ScanHint{{Kind: ScanHintFull}}
		}
		hint.Path = normalizeStateRelPath(hint.Path)
		if hint.Kind == "" || isIgnoredStatePath(hint.Path) {
			continue
		}
		key := string(hint.Kind) + "\x00" + hint.Path
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, hint)
	}
	return out
}

func directoryCacheValidFor(cached DirectoryScanCacheEntry, current FileStat, caps ScanCapabilities) bool {
	if !current.Exists || current.Kind != FileKindDir || !current.StatValid {
		return false
	}
	if cached.MTimeNS != current.MTimeNS {
		return false
	}
	if caps.CTimeReliable && cached.CTimeNS != current.CTimeNS {
		return false
	}
	return true
}

func (fs *WorkspaceFS) scanRelPath(abs string) string {
	if fs == nil || fs.Root == "" {
		return normalizeStateRelPath(abs)
	}
	rel, err := filepath.Rel(fs.Root, abs)
	if err != nil {
		return normalizeStateRelPath(abs)
	}
	return normalizeStateRelPath(rel)
}

func joinScanRel(parent string, child string) string {
	parent = normalizeStateRelPath(parent)
	child = normalizeStateRelPath(child)
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	return parent + "/" + child
}

func shouldSkipScanCursor(rel string, isDir bool, cursor string) bool {
	cursor = normalizeStateRelPath(cursor)
	if cursor == "" {
		return false
	}
	if rel > cursor {
		return false
	}
	if isDir && (rel == cursor || strings.HasPrefix(cursor, rel+"/")) {
		return false
	}
	return true
}

func cursorMayContainRemainingPath(rel string, cursor string) bool {
	rel = normalizeStateRelPath(rel)
	cursor = normalizeStateRelPath(cursor)
	return rel == cursor || strings.HasPrefix(cursor, rel+"/")
}

type scanBudgetMeter struct {
	budget    ScanBudget
	startedAt time.Time
	paths     int
	dirs      int
	hashBytes int64
}

func newScanBudgetMeter(budget ScanBudget) *scanBudgetMeter {
	return &scanBudgetMeter{budget: budget, startedAt: time.Now()}
}

func (m *scanBudgetMeter) notePath() bool {
	if m == nil {
		return false
	}
	m.paths++
	return m.exceeded()
}

func (m *scanBudgetMeter) noteDir() bool {
	if m == nil {
		return false
	}
	m.dirs++
	return m.exceeded()
}

func (m *scanBudgetMeter) noteHashBytes(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.hashBytes += n
}

func (m *scanBudgetMeter) remainingHashBytes() int64 {
	if m == nil || m.budget.MaxHashBytes <= 0 {
		return 0
	}
	return m.budget.MaxHashBytes - m.hashBytes
}

func (m *scanBudgetMeter) exceeded() bool {
	if m == nil {
		return false
	}
	if m.budget.MaxPaths > 0 && m.paths >= m.budget.MaxPaths {
		return true
	}
	if m.budget.MaxDirs > 0 && m.dirs >= m.budget.MaxDirs {
		return true
	}
	if m.budget.MaxDuration > 0 && time.Since(m.startedAt) >= m.budget.MaxDuration {
		return true
	}
	return false
}
