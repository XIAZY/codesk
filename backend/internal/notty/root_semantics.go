package notty

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	crdt "notty/internal/ycrdt"
)

const rootEntryKindFile = "file"

type rootFileCommandEntry struct {
	EntryID           string
	ContentDocumentID string
	DesiredPath       string
	Deleted           bool
}

type rootCommandLocation struct {
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
}

func rootFileEntryForCommand(doc *crdt.Doc, entryID, contentDocumentID, expectedDesiredPath string) (rootFileCommandEntry, error) {
	if doc == nil {
		return rootFileCommandEntry{}, errors.New("root document is required")
	}
	entryID = strings.TrimSpace(entryID)
	if entryID == "" {
		return rootFileCommandEntry{}, errors.New("root entry id is required")
	}
	contentDocumentID = strings.TrimSpace(contentDocumentID)
	if contentDocumentID == "" {
		return rootFileCommandEntry{}, errors.New("content document id is required")
	}
	expectedDesiredPath, err := normalizeRootCommandPath(expectedDesiredPath)
	if err != nil {
		return rootFileCommandEntry{}, err
	}

	root := doc.GetMap(rootMapName)
	var entry rootFileCommandEntry
	err = doc.Read(func(txn *crdt.Transaction) error {
		entryMap, err := rootCommandEntryMap(txn, root, entryID)
		if err != nil {
			return err
		}
		entry, err = decodeRootFileCommandEntry(txn, entryID, entryMap)
		return err
	})
	if err != nil {
		return rootFileCommandEntry{}, err
	}
	if entry.ContentDocumentID != contentDocumentID {
		return rootFileCommandEntry{}, fmt.Errorf(
			"root entry %q names content document %q, want %q",
			entryID,
			entry.ContentDocumentID,
			contentDocumentID,
		)
	}
	if entry.DesiredPath != expectedDesiredPath {
		return rootFileCommandEntry{}, fmt.Errorf(
			"root entry %q path is %q, want %q",
			entryID,
			entry.DesiredPath,
			expectedDesiredPath,
		)
	}
	return entry, nil
}

func setRootFileDeleted(doc *crdt.Doc, expected rootFileCommandEntry, deleted bool) ([]byte, bool, error) {
	if doc == nil {
		return nil, false, errors.New("root document is required")
	}
	current, err := rootFileEntryForCommand(
		doc,
		expected.EntryID,
		expected.ContentDocumentID,
		expected.DesiredPath,
	)
	if err != nil {
		return nil, false, err
	}
	if current.Deleted == deleted {
		return nil, false, nil
	}

	root := doc.GetMap(rootMapName)
	update, err := doc.Update(func(txn *crdt.Transaction) error {
		entryMap, err := rootCommandEntryMap(txn, root, current.EntryID)
		if err != nil {
			return err
		}
		entry, err := decodeRootFileCommandEntry(txn, current.EntryID, entryMap)
		if err != nil {
			return err
		}
		if entry.ContentDocumentID != current.ContentDocumentID || entry.DesiredPath != current.DesiredPath || entry.Deleted != current.Deleted {
			return errors.New("root entry changed during semantic mutation")
		}
		value := "false"
		if deleted {
			value = "true"
		}
		return entryMap.SetString(txn, "deleted", value)
	}, "backend-root-semantic-command")
	if err != nil {
		return nil, false, err
	}
	return update, true, nil
}

func rootPathClaimedByOtherActiveEntry(doc *crdt.Doc, excludedEntryID, desiredPath string) (bool, error) {
	if doc == nil {
		return false, errors.New("root document is required")
	}
	desiredPath, err := normalizeRootCommandPath(desiredPath)
	if err != nil {
		return false, err
	}
	excludedEntryID = strings.TrimSpace(excludedEntryID)
	root := doc.GetMap(rootMapName)
	claimed := false
	err = doc.Read(func(txn *crdt.Transaction) error {
		entries, ok, err := root.GetMap(txn, rootEntriesMapName)
		if err != nil || !ok {
			return err
		}
		items, err := entries.Entries(txn)
		if err != nil {
			return err
		}
		for _, item := range items {
			if strings.TrimSpace(item.Key) == excludedEntryID || item.ValueKind != crdt.YMapEntryMap || item.MapValue == nil {
				continue
			}
			entry, err := decodeRootFileCommandEntry(txn, item.Key, item.MapValue)
			if err != nil {
				return err
			}
			if !entry.Deleted && entry.DesiredPath == desiredPath {
				claimed = true
				return nil
			}
		}
		return nil
	})
	return claimed, err
}

func rootCommandEntryMap(txn *crdt.Transaction, root *crdt.YMap, entryID string) (*crdt.YMap, error) {
	entries, ok, err := root.GetMap(txn, rootEntriesMapName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("root entries map is missing")
	}
	entryMap, ok, err := entries.GetMap(txn, entryID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("root entry %q is missing", entryID)
	}
	return entryMap, nil
}

func decodeRootFileCommandEntry(txn *crdt.Transaction, entryID string, entryMap *crdt.YMap) (rootFileCommandEntry, error) {
	if txn == nil || entryMap == nil {
		return rootFileCommandEntry{}, errors.New("root entry is required")
	}
	kind, ok, err := entryMap.GetString(txn, "kind")
	if err != nil {
		return rootFileCommandEntry{}, err
	}
	if !ok || strings.TrimSpace(kind) == "" {
		kind = rootEntryKindFile
	}
	if strings.TrimSpace(kind) != rootEntryKindFile {
		return rootFileCommandEntry{}, fmt.Errorf("root entry %q is not a file", entryID)
	}
	contentDocumentID, ok, err := entryMap.GetString(txn, "contentDocumentId")
	if err != nil {
		return rootFileCommandEntry{}, err
	}
	contentDocumentID = strings.TrimSpace(contentDocumentID)
	if !ok || contentDocumentID == "" {
		return rootFileCommandEntry{}, fmt.Errorf("root entry %q has no content document", entryID)
	}
	locJSON, ok, err := entryMap.GetString(txn, "loc")
	if err != nil {
		return rootFileCommandEntry{}, err
	}
	if !ok || strings.TrimSpace(locJSON) == "" {
		return rootFileCommandEntry{}, fmt.Errorf("root entry %q has no path", entryID)
	}
	var loc rootCommandLocation
	if err := json.Unmarshal([]byte(locJSON), &loc); err != nil {
		return rootFileCommandEntry{}, fmt.Errorf("decode root entry %q path: %w", entryID, err)
	}
	path := strings.TrimSpace(loc.Name)
	if parent := strings.TrimSpace(loc.ParentID); parent != "" {
		path = parent + "/" + path
	}
	path, err = normalizeRootCommandPath(path)
	if err != nil {
		return rootFileCommandEntry{}, fmt.Errorf("root entry %q: %w", entryID, err)
	}
	deletedValue, ok, err := entryMap.GetString(txn, "deleted")
	if err != nil {
		return rootFileCommandEntry{}, err
	}
	return rootFileCommandEntry{
		EntryID:           strings.TrimSpace(entryID),
		ContentDocumentID: contentDocumentID,
		DesiredPath:       path,
		Deleted:           ok && strings.EqualFold(strings.TrimSpace(deletedValue), "true"),
	}, nil
}

func normalizeRootCommandPath(path string) (string, error) {
	path = strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	normalized, err := normalizeDocumentPath(path)
	if err != nil {
		return "", fmt.Errorf("visible root path must stay within workspace: %w", err)
	}
	for _, segment := range strings.Split(normalized, "/") {
		if strings.HasPrefix(segment, ".") {
			return "", fmt.Errorf("visible root path %q contains a hidden segment", normalized)
		}
	}
	return normalized, nil
}
