package syncer

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	crdt "notty/internal/ycrdt"
)

var errInvalidRootPath = errors.New("invalid root path")

type rootMutationActor struct {
	ID   string
	Kind string
}

type RootTree struct {
	Entries map[string]rootEntry
}

type RootFile struct {
	EntryID           string
	ContentDocumentID string
	DesiredPath       string
}

func DecodeRootTree(doc *crdt.Doc) (RootTree, error) {
	entries := map[string]rootEntry{}
	if doc == nil {
		return RootTree{Entries: entries}, nil
	}
	root := doc.GetMap(rootMapName)
	if err := doc.Read(func(txn *crdt.Transaction) error {
		entriesMap, ok, err := root.GetMap(txn, rootEntriesMapName)
		if err != nil || !ok {
			return err
		}
		items, err := entriesMap.Entries(txn)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.ValueKind != crdt.YMapEntryMap || item.MapValue == nil {
				continue
			}
			entry, ok, err := decodeRootEntryMap(txn, item.Key, item.MapValue)
			if err != nil {
				return err
			}
			if ok {
				entries[entry.EntryID] = entry
			}
		}
		return nil
	}); err != nil {
		return RootTree{}, err
	}
	return RootTree{Entries: entries}, nil
}

func decodeRootEntries(doc *crdt.Doc) (map[string]rootEntry, error) {
	tree, err := DecodeRootTree(doc)
	if err != nil {
		return nil, err
	}
	return tree.Entries, nil
}

func UpsertRootFile(rootDoc *crdt.Doc, documentID, desiredPath string, actor rootMutationActor) ([]byte, error) {
	return UpsertRootFiles(rootDoc, []RootFile{{
		ContentDocumentID: documentID,
		DesiredPath:       desiredPath,
	}}, actor)
}

func UpsertRootFiles(rootDoc *crdt.Doc, files []RootFile, actor rootMutationActor) ([]byte, error) {
	if rootDoc == nil {
		return nil, errors.New("root document is required")
	}
	normalized := make([]RootFile, 0, len(files))
	for _, file := range files {
		documentID := strings.TrimSpace(file.ContentDocumentID)
		if documentID == "" {
			return nil, errors.New("document id is required")
		}
		path, err := normalizeVisibleRootPath(file.DesiredPath)
		if err != nil {
			return nil, err
		}
		entryID := strings.TrimSpace(file.EntryID)
		if entryID == "" {
			entryID = rootEntryIDForDocument(documentID)
		}
		normalized = append(normalized, RootFile{
			EntryID:           entryID,
			ContentDocumentID: documentID,
			DesiredPath:       path,
		})
	}
	root := rootDoc.GetMap(rootMapName)
	return rootDoc.Update(func(txn *crdt.Transaction) error {
		entriesMap, err := ensureRootEntriesMap(txn, root)
		if err != nil {
			return err
		}
		for _, file := range normalized {
			if err := setRootFileEntry(txn, entriesMap, rootEntry{
				EntryID:           file.EntryID,
				Kind:              rootEntryKindFile,
				ContentDocumentID: file.ContentDocumentID,
				Name:              file.DesiredPath,
				Deleted:           false,
			}); err != nil {
				return err
			}
		}
		return nil
	}, rootMutationOrigin(actor))
}

func TombstoneRootFile(rootDoc *crdt.Doc, documentID string, actor rootMutationActor) ([]byte, error) {
	if rootDoc == nil {
		return nil, errors.New("root document is required")
	}
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, errors.New("document id is required")
	}
	root := rootDoc.GetMap(rootMapName)
	return rootDoc.Update(func(txn *crdt.Transaction) error {
		entriesMap, err := ensureRootEntriesMap(txn, root)
		if err != nil {
			return err
		}
		entryID := rootEntryIDForDocument(documentID)
		entryMap, ok, err := entriesMap.GetMap(txn, entryID)
		if err != nil {
			return err
		}
		if !ok {
			entryMap, err = entriesMap.SetMap(txn, entryID)
			if err != nil {
				return err
			}
			if err := entryMap.SetString(txn, "kind", rootEntryKindFile); err != nil {
				return err
			}
			if err := entryMap.SetString(txn, "contentDocumentId", documentID); err != nil {
				return err
			}
		}
		return entryMap.SetString(txn, "deleted", rootDeletedTrue)
	}, rootMutationOrigin(actor))
}

func ResolveRootPath(tree RootTree, desiredPath string) (string, bool) {
	path, err := normalizeVisibleRootPath(desiredPath)
	if err != nil {
		return "", false
	}
	var match string
	for _, file := range ListVisibleRootFiles(tree) {
		if file.DesiredPath != path {
			continue
		}
		if match != "" {
			return "", false
		}
		match = file.ContentDocumentID
	}
	return match, match != ""
}

func ListVisibleRootFiles(tree RootTree) []RootFile {
	files := make([]RootFile, 0, len(tree.Entries))
	for _, entry := range tree.Entries {
		if entry.Deleted || entry.Kind != rootEntryKindFile || strings.TrimSpace(entry.ContentDocumentID) == "" {
			continue
		}
		path, err := normalizeVisibleRootPath(entry.desiredPath())
		if err != nil {
			continue
		}
		files = append(files, RootFile{
			EntryID:           entry.EntryID,
			ContentDocumentID: entry.ContentDocumentID,
			DesiredPath:       path,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].DesiredPath != files[j].DesiredPath {
			return files[i].DesiredPath < files[j].DesiredPath
		}
		if files[i].ContentDocumentID != files[j].ContentDocumentID {
			return files[i].ContentDocumentID < files[j].ContentDocumentID
		}
		return files[i].EntryID < files[j].EntryID
	})
	return files
}

func ensureRootEntriesMap(txn *crdt.Transaction, root *crdt.YMap) (*crdt.YMap, error) {
	entriesMap, ok, err := root.GetMap(txn, rootEntriesMapName)
	if err != nil {
		return nil, err
	}
	if ok {
		return entriesMap, nil
	}
	return root.SetMap(txn, rootEntriesMapName)
}

func normalizeVisibleRootPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("%w: path is required", errInvalidRootPath)
	}
	slashed := strings.ReplaceAll(trimmed, "\\", "/")
	if strings.HasPrefix(slashed, "/") {
		return "", fmt.Errorf("%w: absolute path %q", errInvalidRootPath, path)
	}
	clean := normalizeRootPath(slashed)
	if clean == "" || clean == "." {
		return "", fmt.Errorf("%w: path is required", errInvalidRootPath)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: path escapes workspace", errInvalidRootPath)
	}
	if isIgnoredDocumentPath(clean) {
		return "", fmt.Errorf("%w: ignored path %q", errInvalidRootPath, clean)
	}
	return clean, nil
}

func rootMutationOrigin(actor rootMutationActor) string {
	kind := firstNonEmptyText(actor.Kind, "daemon")
	id := strings.TrimSpace(actor.ID)
	if id == "" {
		return kind + "-root-namespace"
	}
	return kind + ":" + id + "-root-namespace"
}
