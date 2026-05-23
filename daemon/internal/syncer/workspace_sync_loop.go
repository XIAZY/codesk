package syncer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	crdt "notty/internal/ycrdt"
)

type StreamProjector interface {
	CaptureLocal(context.Context, *crdt.Doc) ([]StreamMutation, error)
	PlanApplyMerged(context.Context, *crdt.Doc, int64) error
}

type WorkspaceSyncLoop struct {
	State        *WorkspaceStateDB
	FS           *WorkspaceFS
	RootStreamID string
	ActorID      string
	ActorType    string
	Capabilities ScanCapabilities
	NewID        func(kind string, relPath string) string
	Queue        func(streamID string)

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

	projector := l.projectorFor(streamID)
	mutations, err := projector.CaptureLocal(ctx, doc)
	if err != nil {
		return err
	}
	for _, mutation := range mutations {
		row, err := l.State.UpsertOutbox(ctx, mutation)
		if err != nil {
			return err
		}
		if mutation.StreamID == streamID {
			ready, err := l.State.OutboxDependencyAcked(ctx, row.ID)
			if err != nil {
				return err
			}
			if ready && !row.LocalAppliedAt.Valid {
				if err := crdt.ApplyUpdateV1(doc, row.UpdateBytes, "local-outbox"); err != nil {
					return err
				}
				if err := l.State.MarkOutboxLocallyApplied(ctx, row.ID, time.Now()); err != nil {
					return err
				}
			}
		} else {
			l.queue(mutation.StreamID)
		}
	}
	localOutbox, err := l.State.ApplyReadyLocalOutbox(ctx, streamID, doc)
	if err != nil {
		return err
	}
	inbox, err := l.State.ApplyUnappliedInbox(ctx, streamID, doc)
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
		return nil
	}
	materializedHash := ""
	if streamID != l.RootStreamID {
		materializedHash = contentSHA256([]byte(doc.GetText("content").ToString()))
	}
	stateID, err := l.State.PersistLatestStreamDoc(ctx, streamID, doc, materializedHash)
	if err != nil {
		return err
	}
	if err := projector.PlanApplyMerged(ctx, doc, stateID); err != nil {
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

func (l *WorkspaceSyncLoop) queue(streamID string) {
	if l != nil && l.Queue != nil {
		l.Queue(streamID)
	}
}
