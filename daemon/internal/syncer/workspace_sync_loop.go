package syncer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"notty/internal/rootmanifest"
	crdt "notty/internal/ycrdt"
)

type StreamProjector interface {
	CaptureLocal(context.Context, *crdt.Doc) ([]StreamMutation, error)
	PlanApplyMerged(context.Context, *crdt.Doc, int64) error
}

type WorkspaceSyncLoop struct {
	State          *WorkspaceStateDB
	FS             *WorkspaceFS
	RootStreamID   string
	ActorID        string
	ActorType      string
	ProjectionMode ProjectionMode
	Capabilities   ScanCapabilities
	NewID          func(kind string, relPath string) string
	Queue          func(streamID string)

	mu sync.Mutex
}

func (l *WorkspaceSyncLoop) ReconcileOne(ctx context.Context, streamID string) error {
	if l == nil || l.State == nil {
		return errors.New("state db is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.FS == nil {
		return errors.New("workspace fs is required")
	}
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return errors.New("stream id is required")
	}
	kind := "content"
	if streamID == l.RootStreamID {
		kind = "root"
	}
	doc, stream, err := l.State.LoadLatestStreamDoc(ctx, streamID, kind)
	if err != nil {
		return err
	}
	defer doc.Close()
	if streamID != l.RootStreamID {
		live, known, err := l.contentStreamLiveInRoot(ctx, streamID)
		if err != nil {
			return err
		}
		if known && !live {
			return l.State.DropPendingOutboxForStream(ctx, streamID, "root-stream-not-live", time.Now())
		}
	}

	projector := l.projectorFor(streamID)
	mutations, err := projector.CaptureLocal(ctx, doc)
	if err != nil {
		return err
	}
	for _, mutation := range mutations {
		_, err := l.State.UpsertOutbox(ctx, mutation)
		if err != nil {
			return err
		}
		if mutation.StreamID != streamID {
			l.queue(mutation.StreamID)
		}
	}
	localOutbox, err := l.State.ReadyLocalOutbox(ctx, streamID, 100)
	if err != nil {
		return err
	}
	inbox, err := l.State.UnappliedInbox(ctx, streamID, 100)
	if err != nil {
		return err
	}
	if streamID != l.RootStreamID && !stream.LatestStateID.Valid && len(localOutbox) == 0 && len(inbox) == 0 {
		return nil
	}
	if streamID != l.RootStreamID &&
		!stream.ProjectedStateID.Valid &&
		len(localOutbox) == 0 &&
		doc.GetText("content").ToString() == "" {
		if len(inbox) == 0 {
			return nil
		}
		empty, err := queuedContentWouldRemainEmpty(doc, inbox)
		if err != nil {
			return err
		}
		if empty {
			return nil
		}
	}
	result, err := l.State.ApplyStreamQueueAtomically(ctx, streamID, kind, doc, "")
	if err != nil {
		return err
	}
	if err := projector.PlanApplyMerged(ctx, doc, result.StateID); err != nil {
		return err
	}
	if streamID == l.RootStreamID {
		more, err := (PendingContentCreateProcessor{
			State:        l.State,
			FS:           l.FS,
			Capabilities: l.Capabilities,
			ActorID:      firstNonEmptyText(l.ActorID, "daemon"),
			ActorType:    firstNonEmptyText(l.ActorType, "daemon"),
			Queue:        l.queue,
		}).Process(ctx, PendingCreateLimits{
			MaxRows:  MaxPendingContentCreatesPerCycle,
			MaxBytes: MaxPendingContentCreateBytesPerCycle,
		})
		if err != nil {
			l.queue(l.RootStreamID)
			return err
		}
		if more {
			l.queue(l.RootStreamID)
		}
	}
	if err := l.State.RunPendingFSJobs(ctx, l.FS); err != nil {
		l.queue(streamID)
		if errors.Is(err, ErrDivergedWorkingCopy) || errors.Is(err, ErrPathCollision) {
			return nil
		}
		return err
	}
	if streamID != l.RootStreamID {
		stream, err := l.State.GetStream(ctx, streamID)
		if err == nil && stream.ProjectedStateID.Valid {
			_ = l.State.TryCompletePendingContentCreate(ctx, streamID, stream.ProjectedStateID.Int64)
		}
	}
	return nil
}

func (l *WorkspaceSyncLoop) projectorFor(streamID string) StreamProjector {
	if streamID == l.RootStreamID {
		return RootManifestProjector{
			State:        l.State,
			FS:           l.FS,
			RootStreamID: l.RootStreamID,
			ActorID:      l.ActorID,
			ActorType:    l.ActorType,
			Mode:         l.ProjectionMode,
			Capabilities: l.Capabilities,
			NewID:        l.NewID,
			Queue:        l.queue,
		}
	}
	return ContentProjector{
		State:        l.State,
		FS:           l.FS,
		StreamID:     streamID,
		ActorID:      l.ActorID,
		ActorType:    l.ActorType,
		Capabilities: l.Capabilities,
		MaxReadBytes: MaxSinglePendingCreateBytes,
	}
}

func (l *WorkspaceSyncLoop) contentStreamLiveInRoot(ctx context.Context, streamID string) (bool, bool, error) {
	if l == nil || l.State == nil || strings.TrimSpace(l.RootStreamID) == "" || strings.TrimSpace(streamID) == "" {
		return false, false, nil
	}
	rootDoc, rootStream, err := l.State.LoadLatestStreamDoc(ctx, l.RootStreamID, "root")
	if err != nil {
		return false, false, err
	}
	defer rootDoc.Close()
	if !rootStream.LatestStateID.Valid {
		return false, false, nil
	}
	manifest, err := rootmanifest.Read(rootDoc)
	if err != nil {
		return false, true, err
	}
	for _, entry := range manifest.EntriesByID {
		if entry.Kind == rootmanifest.EntryKindFile &&
			entry.Tombstone == nil &&
			strings.TrimSpace(entry.ContentStreamID) == streamID {
			return true, true, nil
		}
	}
	return false, true, nil
}

func (l *WorkspaceSyncLoop) queue(streamID string) {
	if l != nil && l.Queue != nil {
		l.Queue(streamID)
	}
}

func queuedContentWouldRemainEmpty(base *crdt.Doc, inbox []StreamInboxRow) (bool, error) {
	if base == nil {
		return false, errors.New("stream doc is required")
	}
	preview := crdt.New()
	defer preview.Close()
	if err := crdt.ApplyUpdateV1(preview, base.EncodeStateAsUpdate(), "preview-base"); err != nil {
		return false, err
	}
	for _, row := range inbox {
		if err := crdt.ApplyUpdateV1(preview, row.UpdateBytes, "preview-inbox"); err != nil {
			return false, err
		}
	}
	return preview.GetText("content").ToString() == "", nil
}
