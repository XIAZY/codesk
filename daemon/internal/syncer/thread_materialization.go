package syncer

import (
	"context"
	"fmt"
	"strings"

	crdt "notty/internal/ycrdt"
)

func (s *workspaceRuntime) materializeThreadIntentsLocked(ctx context.Context, cache *documentCache, entry *documentCacheEntry, documentID, documentPath string, baseDoc *crdt.Doc, contentKnown bool) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if cache == nil || baseDoc == nil {
		return 0, nil
	}
	intents, err := cache.loadThreadIntentsLocked(entry, documentID)
	if err != nil {
		return 0, err
	}
	ready := 0
	text := baseDoc.GetText("content")
	content := text.ToString()
	for _, intent := range intents {
		if intent.Status != threadIntentPending {
			continue
		}
		if !contentKnown && !threadIntentHasRelativeAnchors(intent.Request) {
			continue
		}
		payload, materializeErr := materializeThreadIntentPayload(intent, documentID, documentPath, text, content)
		if materializeErr != nil {
			intent.Status = threadIntentFailed
			intent.LastError = materializeErr.Error()
			intent.Resolved = nil
			if err := cache.writeThreadIntentLocked(documentID, intent); err != nil {
				return ready, err
			}
			continue
		}
		intent.Status = threadIntentReady
		intent.LastError = ""
		intent.LastStatusCode = 0
		intent.Resolved = &payload
		if err := cache.writeThreadIntentLocked(documentID, intent); err != nil {
			return ready, err
		}
		ready++
	}
	return ready, nil
}

func threadIntentHasRelativeAnchors(payload createThreadPayload) bool {
	return strings.TrimSpace(payload.RelativeStart) != "" || strings.TrimSpace(payload.RelativeEnd) != ""
}

func materializeThreadIntentPayload(intent threadOutboxIntent, documentID, documentPath string, text *crdt.YText, content string) (backendCreateThreadPayload, error) {
	payload := intent.Request
	result := backendCreateThreadPayload{
		DocumentID:        documentID,
		ClientOperationID: intent.IdempotencyKey,
		Title:             payload.Title,
		Body:              payload.Body,
		Kind:              "text-range",
		Excerpt:           payload.Excerpt,
	}
	if result.DocumentID == "" {
		result.DocumentID = payload.DocumentID
	}
	if strings.TrimSpace(payload.Body) == "" {
		return backendCreateThreadPayload{}, fmt.Errorf("thread body is required")
	}
	if strings.TrimSpace(payload.RelativeStart) != "" || strings.TrimSpace(payload.RelativeEnd) != "" {
		if strings.TrimSpace(payload.RelativeStart) == "" || strings.TrimSpace(payload.RelativeEnd) == "" {
			return backendCreateThreadPayload{}, fmt.Errorf("relativeStart and relativeEnd must be provided together")
		}
		result.RelativeStart = payload.RelativeStart
		result.RelativeEnd = payload.RelativeEnd
		return result, nil
	}
	if text == nil {
		return backendCreateThreadPayload{}, fmt.Errorf("document %s is not available in daemon CRDT cache yet", documentPath)
	}
	target, err := resolveThreadTarget(content, payload)
	if err != nil {
		return backendCreateThreadPayload{}, err
	}
	if target.start < 0 || target.end < target.start || target.end > text.Len() {
		return backendCreateThreadPayload{}, fmt.Errorf("resolved thread range [%d,%d] is outside document length %d", target.start, target.end, text.Len())
	}
	result.RelativeStart = encodeRelativeAnchor(text, target.start)
	result.RelativeEnd = encodeRelativeAnchor(text, target.end)
	result.Excerpt = target.excerpt
	return result, nil
}
