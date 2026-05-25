package syncer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	threadIntentPending = "pending"
	threadIntentReady   = "ready"
	threadIntentFailed  = "failed"
)

type threadOutboxIntent struct {
	IntentID       string                      `json:"intentId"`
	IdempotencyKey string                      `json:"idempotencyKey"`
	DocumentID     string                      `json:"documentId"`
	DocumentPath   string                      `json:"documentPath"`
	ActorID        string                      `json:"actorId"`
	ActorType      string                      `json:"actorType"`
	RunID          string                      `json:"runId,omitempty"`
	Request        createThreadPayload         `json:"request"`
	Resolved       *backendCreateThreadPayload `json:"resolved,omitempty"`
	Status         string                      `json:"status"`
	Attempts       int                         `json:"attempts,omitempty"`
	NextAttemptAt  time.Time                   `json:"nextAttemptAt,omitempty"`
	LastAttemptAt  time.Time                   `json:"lastAttemptAt,omitempty"`
	LastError      string                      `json:"lastError,omitempty"`
	LastStatusCode int                         `json:"lastStatusCode,omitempty"`
	CreatedAt      time.Time                   `json:"createdAt"`
	UpdatedAt      time.Time                   `json:"updatedAt"`
}

func (c *documentCache) appendThreadIntent(documentID string, intent threadOutboxIntent) error {
	if c == nil || documentID == "" {
		return nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return c.writeThreadIntentLocked(documentID, intent)
}

func (c *documentCache) loadThreadIntentsLocked(_ *documentCacheEntry, documentID string) ([]threadOutboxIntent, error) {
	if c == nil || documentID == "" {
		return nil, nil
	}
	return c.loadThreadIntentsFromDir(c.threadOutboxDir(documentID))
}

func (c *documentCache) pendingThreadIntentCountLocked(entry *documentCacheEntry, documentID string) (int, error) {
	intents, err := c.loadThreadIntentsLocked(entry, documentID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, intent := range intents {
		if intent.Status == threadIntentPending {
			count++
		}
	}
	return count, nil
}

func (c *documentCache) materializableThreadIntentCountLocked(entry *documentCacheEntry, documentID string) (int, error) {
	intents, err := c.loadThreadIntentsLocked(entry, documentID)
	if err != nil {
		return 0, err
	}
	stateExists := false
	stateChecked := false
	count := 0
	for _, intent := range intents {
		if intent.Status != threadIntentPending {
			continue
		}
		if threadIntentHasRelativeAnchors(intent.Request) {
			count++
			continue
		}
		if !stateChecked {
			if _, err := os.Stat(c.statePath(documentID)); err == nil {
				stateExists = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
			stateChecked = true
		}
		if stateExists {
			count++
		}
	}
	return count, nil
}

func (c *documentCache) loadDueReadyThreadIntents(now time.Time) ([]threadOutboxIntent, error) {
	if c == nil || strings.TrimSpace(c.root) == "" {
		return nil, nil
	}
	docDirs, err := os.ReadDir(c.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var due []threadOutboxIntent
	for _, docDir := range docDirs {
		if !docDir.IsDir() {
			continue
		}
		intents, err := c.loadThreadIntentsFromDir(filepath.Join(c.root, docDir.Name(), "thread_outbox"))
		if err != nil {
			return nil, err
		}
		for _, intent := range intents {
			if intent.Status != threadIntentReady || intent.Resolved == nil {
				continue
			}
			if !intent.NextAttemptAt.IsZero() && now.Before(intent.NextAttemptAt) {
				continue
			}
			due = append(due, intent)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].CreatedAt.Equal(due[j].CreatedAt) {
			return due[i].IntentID < due[j].IntentID
		}
		return due[i].CreatedAt.Before(due[j].CreatedAt)
	})
	return due, nil
}

func (c *documentCache) nextThreadIntentAttemptAt(now time.Time) (time.Time, bool, error) {
	if c == nil || strings.TrimSpace(c.root) == "" {
		return time.Time{}, false, nil
	}
	docDirs, err := os.ReadDir(c.root)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	var next time.Time
	for _, docDir := range docDirs {
		if !docDir.IsDir() {
			continue
		}
		intents, err := c.loadThreadIntentsFromDir(filepath.Join(c.root, docDir.Name(), "thread_outbox"))
		if err != nil {
			return time.Time{}, false, err
		}
		for _, intent := range intents {
			if intent.Status != threadIntentReady || intent.Resolved == nil {
				continue
			}
			candidate := intent.NextAttemptAt
			if candidate.IsZero() || candidate.Before(now) {
				candidate = now
			}
			if next.IsZero() || candidate.Before(next) {
				next = candidate
			}
		}
	}
	if next.IsZero() {
		return time.Time{}, false, nil
	}
	return next, true, nil
}

func (c *documentCache) updateThreadIntent(intent threadOutboxIntent) error {
	if c == nil || intent.DocumentID == "" {
		return nil
	}
	entry := c.entryFor(intent.DocumentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return c.writeThreadIntentLocked(intent.DocumentID, intent)
}

func (c *documentCache) deleteThreadIntent(intent threadOutboxIntent) error {
	if c == nil || intent.DocumentID == "" || intent.IntentID == "" {
		return nil
	}
	entry := c.entryFor(intent.DocumentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err := os.Remove(c.threadIntentPath(intent.DocumentID, intent.IntentID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (c *documentCache) loadThreadIntentsFromDir(dir string) ([]threadOutboxIntent, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	intents := make([]threadOutboxIntent, 0, len(files))
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		var intent threadOutboxIntent
		if err := json.Unmarshal(payload, &intent); err != nil {
			return nil, err
		}
		if intent.IntentID == "" || intent.DocumentID == "" {
			continue
		}
		intents = append(intents, intent)
	}
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].CreatedAt.Equal(intents[j].CreatedAt) {
			return intents[i].IntentID < intents[j].IntentID
		}
		return intents[i].CreatedAt.Before(intents[j].CreatedAt)
	})
	return intents, nil
}

func (c *documentCache) writeThreadIntentLocked(documentID string, intent threadOutboxIntent) error {
	if c == nil || documentID == "" || intent.IntentID == "" {
		return nil
	}
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	intent.UpdatedAt = now
	if intent.Status == "" {
		intent.Status = threadIntentPending
	}
	if intent.DocumentID == "" {
		intent.DocumentID = documentID
	}
	if err := os.MkdirAll(c.threadOutboxDir(documentID), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	path := c.threadIntentPath(documentID, intent.IntentID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *documentCache) threadOutboxDir(documentID string) string {
	return filepath.Join(c.documentDir(documentID), "thread_outbox")
}

func (c *documentCache) threadIntentPath(documentID, intentID string) string {
	return filepath.Join(c.threadOutboxDir(documentID), safeDocumentCacheName(intentID)+".json")
}
