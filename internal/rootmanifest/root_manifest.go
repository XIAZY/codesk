package rootmanifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	crdt "notty/internal/ycrdt"
)

const (
	TextName    = "rootManifestJSON"
	MapName     = "entriesById"
	RootEntryID = "root"

	EntryKindDir  = "dir"
	EntryKindFile = "file"
)

type Manifest struct {
	EntriesByID map[string]Entry `json:"entriesById"`
}

type Entry struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Loc             *Location  `json:"loc"`
	ContentStreamID string     `json:"contentStreamId,omitempty"`
	Tombstone       *Tombstone `json:"tombstone,omitempty"`
	CreatedBy       string     `json:"createdBy,omitempty"`
	UpdatedBy       string     `json:"updatedBy,omitempty"`
	CreatedAt       string     `json:"createdAt,omitempty"`
	UpdatedAt       string     `json:"updatedAt,omitempty"`
}

type Location struct {
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
	NormName string `json:"normName"`
}

type Tombstone struct {
	ActorID   string `json:"actorId"`
	ActorType string `json:"actorType"`
	At        string `json:"at"`
}

type Intent struct {
	Type      string
	Entry     Entry
	EntryID   string
	Loc       *Location
	Tombstone *Tombstone
}

type Projection struct {
	EntryPath   map[string]string
	DesiredPath map[string]string
	Orphaned    map[string]bool
}

type siblingKey struct {
	parentID string
	normName string
}

type projectionEntry struct {
	entry    Entry
	depth    int
	orphaned bool
}

func New() Manifest {
	return Manifest{EntriesByID: map[string]Entry{
		RootEntryID: {
			ID:   RootEntryID,
			Kind: EntryKindDir,
			Loc:  nil,
		},
	}}
}

func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func NewLocation(parentID string, name string) *Location {
	return &Location{
		ParentID: strings.TrimSpace(parentID),
		Name:     strings.TrimSpace(name),
		NormName: NormalizeName(name),
	}
}

func Read(doc *crdt.Doc) (Manifest, error) {
	if doc == nil {
		return Manifest{}, errors.New("root manifest doc is required")
	}
	if rawMap, err := doc.GetMap(MapName).JSON(); err == nil && strings.TrimSpace(rawMap) != "" && strings.TrimSpace(rawMap) != "{}" {
		entries := map[string]Entry{}
		if err := json.Unmarshal([]byte(rawMap), &entries); err != nil {
			return Manifest{}, err
		}
		return Manifest{EntriesByID: entries}, nil
	}
	raw := strings.TrimSpace(doc.GetText(TextName).ToString())
	if raw == "" {
		return New(), nil
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.EntriesByID == nil {
		manifest.EntriesByID = map[string]Entry{}
	}
	return manifest, nil
}

func ApplyIntents(doc *crdt.Doc, intents []Intent) ([]byte, error) {
	if doc == nil {
		return nil, errors.New("root manifest doc is required")
	}
	previous, err := Read(doc)
	if err != nil {
		return nil, err
	}
	next := clone(previous)
	if next.EntriesByID == nil {
		next.EntriesByID = map[string]Entry{}
	}
	for _, intent := range intents {
		switch intent.Type {
		case "create", "create-file", "create-dir":
			entry := intent.Entry
			if entry.ID == "" {
				entry.ID = intent.EntryID
			}
			if entry.ID == "" {
				return nil, errors.New("root create intent requires entry id")
			}
			next.EntriesByID[entry.ID] = entry
		case "loc":
			entryID := strings.TrimSpace(intent.EntryID)
			entry, ok := next.EntriesByID[entryID]
			if !ok {
				return nil, fmt.Errorf("root loc intent references missing entry %q", entryID)
			}
			if intent.Loc == nil {
				return nil, errors.New("root loc intent requires location")
			}
			loc := *intent.Loc
			entry.Loc = &loc
			next.EntriesByID[entryID] = entry
		case "tombstone":
			entryID := strings.TrimSpace(intent.EntryID)
			entry, ok := next.EntriesByID[entryID]
			if !ok {
				return nil, fmt.Errorf("root tombstone intent references missing entry %q", entryID)
			}
			if intent.Tombstone == nil {
				return nil, errors.New("root tombstone intent requires tombstone")
			}
			tombstone := *intent.Tombstone
			entry.Tombstone = &tombstone
			next.EntriesByID[entryID] = entry
		default:
			return nil, fmt.Errorf("unknown root intent type %q", intent.Type)
		}
	}
	if err := Validate(previous, next); err != nil {
		return nil, err
	}
	changed := map[string]struct{}{RootEntryID: {}}
	for _, intent := range intents {
		entryID := strings.TrimSpace(intent.EntryID)
		if entryID == "" {
			entryID = strings.TrimSpace(intent.Entry.ID)
		}
		if entryID != "" {
			changed[entryID] = struct{}{}
		}
	}
	text := doc.GetText(TextName)
	entries := doc.GetMap(MapName)
	rawMap, _ := entries.JSON()
	writeAll := strings.TrimSpace(rawMap) == "" || strings.TrimSpace(rawMap) == "{}"
	ids := make([]string, 0, len(changed))
	if writeAll {
		ids = sortedEntryIDs(next)
	} else {
		for id := range changed {
			if _, ok := next.EntriesByID[id]; ok {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	}
	return doc.Update(func(txn *crdt.Transaction) error {
		for _, id := range ids {
			payload, err := json.Marshal(next.EntriesByID[id])
			if err != nil {
				return err
			}
			if err := entries.InsertJSON(txn, id, string(payload)); err != nil {
				return err
			}
		}
		if length := text.LenInTxn(txn); length > 0 {
			if err := text.DeleteRange(txn, 0, length); err != nil {
				return err
			}
		}
		return nil
	}, "root-manifest")
}

func Validate(previous Manifest, next Manifest) error {
	if next.EntriesByID == nil {
		return errors.New("entriesById is required")
	}
	root, ok := next.EntriesByID[RootEntryID]
	if !ok {
		return errors.New("root entry is required")
	}
	if root.ID != RootEntryID {
		return errors.New("root entry id must match root key")
	}
	if root.Kind != EntryKindDir {
		return errors.New("root entry must be a directory")
	}
	if root.Loc != nil {
		return errors.New("root entry location must be null")
	}
	if root.Tombstone != nil {
		return errors.New("root entry cannot be tombstoned")
	}
	for key, entry := range next.EntriesByID {
		if strings.TrimSpace(key) == "" {
			return errors.New("entry key cannot be empty")
		}
		if key != entry.ID {
			return fmt.Errorf("entry key %q does not match entry id %q", key, entry.ID)
		}
		switch entry.Kind {
		case EntryKindDir:
			if entry.ContentStreamID != "" {
				return fmt.Errorf("directory entry %q cannot have contentStreamId", entry.ID)
			}
		case EntryKindFile:
			if strings.TrimSpace(entry.ContentStreamID) == "" {
				return fmt.Errorf("file entry %q requires contentStreamId", entry.ID)
			}
		default:
			return fmt.Errorf("entry %q has invalid kind %q", entry.ID, entry.Kind)
		}
		if entry.ID == RootEntryID {
			continue
		}
		if err := validateLocation(entry.ID, entry.Loc); err != nil {
			return err
		}
	}
	for id, before := range previous.EntriesByID {
		after, ok := next.EntriesByID[id]
		if !ok {
			continue
		}
		if before.Kind != "" && before.Kind != after.Kind {
			return fmt.Errorf("entry %q kind cannot change", id)
		}
		if before.Kind == EntryKindFile && before.ContentStreamID != "" && before.ContentStreamID != after.ContentStreamID {
			return fmt.Errorf("entry %q contentStreamId cannot change", id)
		}
		if before.Tombstone != nil && after.Tombstone == nil {
			return fmt.Errorf("entry %q tombstone cannot be removed", id)
		}
	}
	return nil
}

func Resolve(manifest Manifest) Projection {
	if manifest.EntriesByID == nil {
		manifest.EntriesByID = map[string]Entry{}
	}
	live := map[string]Entry{}
	for id, entry := range manifest.EntriesByID {
		if entry.Tombstone == nil {
			live[id] = entry
		}
	}

	reachable := map[string]projectionEntry{}
	orphaned := map[string]projectionEntry{}
	memo := map[string]bool{}
	visiting := map[string]bool{}
	var isReachable func(string) bool
	isReachable = func(id string) bool {
		if value, ok := memo[id]; ok {
			return value
		}
		entry, ok := live[id]
		if !ok {
			memo[id] = false
			return false
		}
		if id == RootEntryID {
			memo[id] = entry.Kind == EntryKindDir && entry.Loc == nil
			return memo[id]
		}
		if entry.Loc == nil || entry.Loc.ParentID == "" {
			memo[id] = false
			return false
		}
		if visiting[id] {
			memo[id] = false
			return false
		}
		visiting[id] = true
		parentEntry, parentLive := live[entry.Loc.ParentID]
		ok = parentLive && parentEntry.Kind == EntryKindDir && isReachable(entry.Loc.ParentID)
		visiting[id] = false
		memo[id] = ok
		return ok
	}

	for id, entry := range live {
		if id == RootEntryID {
			continue
		}
		depth := entryDepth(live, id)
		if isReachable(id) {
			reachable[id] = projectionEntry{entry: entry, depth: depth}
		} else {
			orphaned[id] = projectionEntry{entry: entry, depth: depth, orphaned: true}
		}
	}

	groups := map[siblingKey][]Entry{}
	for _, item := range reachable {
		loc := item.entry.Loc
		key := siblingKey{parentID: loc.ParentID, normName: loc.NormName}
		groups[key] = append(groups[key], item.entry)
	}
	for key := range groups {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool {
			return group[i].ID < group[j].ID
		})
		groups[key] = group
	}

	result := Projection{
		EntryPath:   map[string]string{},
		DesiredPath: map[string]string{},
		Orphaned:    map[string]bool{},
	}
	taken := map[string]string{}
	for _, item := range sortedProjectionEntries(reachable) {
		entry := item.entry
		loc := entry.Loc
		parentPath := result.EntryPath[loc.ParentID]
		desired := joinPath(parentPath, loc.Name)
		group := groups[siblingKey{parentID: loc.ParentID, normName: loc.NormName}]
		materialized := desired
		if indexInEntryGroup(group, entry.ID) > 0 {
			materialized = ConflictPath(desired, entry.ID)
		}
		materialized = FirstFreePath(materialized, entry.ID, taken)
		result.DesiredPath[entry.ID] = desired
		result.EntryPath[entry.ID] = materialized
		taken[materialized] = entry.ID
	}
	for _, item := range sortedProjectionEntries(orphaned) {
		entry := item.entry
		name := entry.ID
		if entry.Loc != nil && strings.TrimSpace(entry.Loc.Name) != "" {
			name = entry.Loc.Name
		}
		desired := joinPath("Recovered/orphans", entry.ID, name)
		materialized := FirstFreePath(desired, entry.ID, taken)
		result.DesiredPath[entry.ID] = desired
		result.EntryPath[entry.ID] = materialized
		result.Orphaned[entry.ID] = true
		taken[materialized] = entry.ID
	}
	return result
}

func ConflictPath(filePath string, entryID string) string {
	dir, base := path.Split(filePath)
	base = strings.TrimSuffix(base, "/")
	stem, ext := splitStemExt(base)
	return path.Join(strings.TrimSuffix(dir, "/"), stem+" (conflict "+ShortID(entryID)+")"+ext)
}

func FirstFreePath(candidate string, entryID string, taken map[string]string) string {
	if taken == nil {
		return candidate
	}
	if owner := taken[candidate]; owner == "" || owner == entryID {
		return candidate
	}
	for index := 2; ; index++ {
		next := conflictPathWithOrdinal(candidate, entryID, index)
		if owner := taken[next]; owner == "" || owner == entryID {
			return next
		}
	}
}

func ShortID(entryID string) string {
	entryID = strings.TrimSpace(entryID)
	if len(entryID) <= 12 {
		return entryID
	}
	return entryID[:12]
}

func Clone(manifest Manifest) Manifest {
	return clone(manifest)
}

func validateLocation(entryID string, loc *Location) error {
	if loc == nil {
		return fmt.Errorf("entry %q requires location", entryID)
	}
	if strings.TrimSpace(loc.ParentID) == "" {
		return fmt.Errorf("entry %q requires location parentId", entryID)
	}
	name := strings.TrimSpace(loc.Name)
	if name == "" {
		return fmt.Errorf("entry %q location name is required", entryID)
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("entry %q location name must be a single path segment", entryID)
	}
	if name == "." || name == ".." || name == ".notty" {
		return fmt.Errorf("entry %q location name %q is reserved", entryID, name)
	}
	if loc.NormName != NormalizeName(loc.Name) {
		return fmt.Errorf("entry %q location normName %q does not match normalized name %q", entryID, loc.NormName, NormalizeName(loc.Name))
	}
	return nil
}

func clone(manifest Manifest) Manifest {
	next := Manifest{EntriesByID: map[string]Entry{}}
	for id, entry := range manifest.EntriesByID {
		cloned := entry
		if entry.Loc != nil {
			loc := *entry.Loc
			cloned.Loc = &loc
		}
		if entry.Tombstone != nil {
			tombstone := *entry.Tombstone
			cloned.Tombstone = &tombstone
		}
		next.EntriesByID[id] = cloned
	}
	return next
}

func entryDepth(live map[string]Entry, id string) int {
	depth := 0
	seen := map[string]bool{}
	for {
		entry, ok := live[id]
		if !ok || entry.Loc == nil || entry.ID == RootEntryID || seen[id] {
			return depth
		}
		seen[id] = true
		depth++
		id = entry.Loc.ParentID
	}
}

func sortedProjectionEntries(items map[string]projectionEntry) []projectionEntry {
	result := make([]projectionEntry, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].depth != result[j].depth {
			return result[i].depth < result[j].depth
		}
		return result[i].entry.ID < result[j].entry.ID
	})
	return result
}

func sortedEntryIDs(manifest Manifest) []string {
	ids := make([]string, 0, len(manifest.EntriesByID))
	for id := range manifest.EntriesByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func indexInEntryGroup(group []Entry, entryID string) int {
	for index, entry := range group {
		if entry.ID == entryID {
			return index
		}
	}
	return -1
}

func joinPath(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, "/")
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	return path.Join(cleaned...)
}

func splitStemExt(base string) (string, string) {
	ext := path.Ext(base)
	if ext == "" || ext == base {
		return base, ""
	}
	return strings.TrimSuffix(base, ext), ext
}

func conflictPathWithOrdinal(filePath string, entryID string, ordinal int) string {
	dir, base := path.Split(filePath)
	base = strings.TrimSuffix(base, "/")
	stem, ext := splitStemExt(base)
	return path.Join(strings.TrimSuffix(dir, "/"), fmt.Sprintf("%s (conflict %s %d)%s", stem, ShortID(entryID), ordinal, ext))
}
