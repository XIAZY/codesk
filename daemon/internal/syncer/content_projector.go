package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	crdt "notty/internal/ycrdt"
)

var ErrUnknownProjectedBase = errors.New("unknown projected content base")

type ContentProjector struct {
	State        *WorkspaceStateDB
	FS           *WorkspaceFS
	StreamID     string
	ActorID      string
	ActorType    string
	Capabilities ScanCapabilities
	MaxReadBytes int64
}

func (p ContentProjector) CaptureLocal(ctx context.Context, doc *crdt.Doc) ([]StreamMutation, error) {
	if p.State == nil {
		return nil, errors.New("state db is required")
	}
	if p.FS == nil {
		return nil, errors.New("workspace fs is required")
	}
	if strings.TrimSpace(p.StreamID) == "" {
		return nil, errors.New("stream id is required")
	}
	blocking, err := p.State.HasBlockingFSJob(ctx, p.StreamID)
	if err != nil || blocking {
		return nil, err
	}
	projection, err := p.State.GetContentProjection(ctx, p.StreamID)
	if err != nil || projection == nil {
		return nil, err
	}
	if !projection.ProjectedStateID.Valid {
		return nil, nil
	}
	stat, err := p.FS.Stat(ctx, projection.MaterializedPath)
	if err != nil {
		return nil, err
	}
	if !stat.Exists {
		return nil, nil
	}
	if !projection.Dirty && projection.Stat.StatValid && SameStatTuple(projection.Stat, stat, p.Capabilities) {
		return nil, nil
	}
	read, ok, err := p.FS.ReadBytesStable(ctx, projection.MaterializedPath, StableReadOptions{
		Capabilities: p.Capabilities,
		MaxBytes:     p.MaxReadBytes,
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	localHash := contentSHA256(read.Bytes)
	if localHash == projection.ProjectedHash {
		projection.Stat = read.FinalStat
		projection.Dirty = false
		if err := p.State.UpsertContentProjection(ctx, *projection); err != nil {
			return nil, err
		}
		return nil, nil
	}
	base, err := p.State.LoadStreamState(ctx, projection.ProjectedStateID.Int64)
	if err != nil {
		return nil, err
	}
	localContent := string(read.Bytes)
	baseContent, err := ContentFromStateUpdate(base.StateUpdate)
	if err != nil {
		return nil, err
	}
	replace := computeReplace(baseContent, localContent)
	if replace.End > replace.Start {
		stream, streamErr := p.State.GetStream(ctx, p.StreamID)
		if streamErr != nil {
			return nil, streamErr
		}
		remoteAdvanced := stream.LatestStateID.Valid &&
			projection.ProjectedStateID.Valid &&
			stream.LatestStateID.Int64 != projection.ProjectedStateID.Int64
		if projection.Dirty && remoteAdvanced {
			return nil, nil
		}
		if !projection.Dirty {
			projection.Stat = read.FinalStat
			projection.Dirty = true
			if err := p.State.UpsertContentProjection(ctx, *projection); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}
	update, err := BuildContentPatchUpdate(base.StateUpdate, baseContent, replace)
	if err != nil {
		return nil, err
	}
	projection.ProjectedHash = localHash
	projection.Stat = read.FinalStat
	projection.Dirty = true
	if err := p.State.UpsertContentProjection(ctx, *projection); err != nil {
		return nil, err
	}
	return []StreamMutation{{
		StreamID:    p.StreamID,
		KindHint:    "content",
		MutationKey: "content:edit:" + p.StreamID + ":" + localHash,
		UpdateBytes: update,
		ActorID:     firstNonEmptyText(p.ActorID, "daemon"),
		ActorType:   firstNonEmptyText(p.ActorType, "daemon"),
		Reason:      "content-local-edit",
	}}, nil
}

func BuildContentPatchUpdate(projectedState []byte, baseContent string, replace replaceOp) ([]byte, error) {
	doc := crdt.New()
	defer doc.Close()
	if len(projectedState) > 0 {
		if err := crdt.ApplyUpdateV1(doc, projectedState, "projected-base"); err != nil {
			return nil, err
		}
	}
	if replace.Start < 0 || replace.End < replace.Start || replace.End > len(baseContent) {
		return nil, fmt.Errorf("invalid content replace range")
	}
	start := utf16Length(baseContent[:replace.Start])
	deleteLength := utf16Length(baseContent[replace.Start:replace.End])
	text := doc.GetText("content")
	update, err := doc.Update(func(txn *crdt.Transaction) error {
		if deleteLength > 0 {
			if err := text.DeleteRange(txn, start, deleteLength); err != nil {
				return err
			}
		}
		if replace.Text != "" {
			if err := text.InsertValue(txn, start, replace.Text); err != nil {
				return err
			}
		}
		return nil
	}, "content-local-edit")
	if err != nil {
		return nil, err
	}
	if len(update) == 0 {
		return nil, fmt.Errorf("content update is empty")
	}
	return update, nil
}

func (p ContentProjector) PlanApplyMerged(ctx context.Context, doc *crdt.Doc, stateID int64) error {
	if p.State == nil {
		return errors.New("state db is required")
	}
	if strings.TrimSpace(p.StreamID) == "" {
		return errors.New("stream id is required")
	}
	if doc == nil {
		return errors.New("stream doc is required")
	}
	projection, err := p.State.GetContentProjection(ctx, p.StreamID)
	if err != nil || projection == nil {
		return err
	}
	content := []byte(doc.GetText("content").ToString())
	targetHash := contentSHA256(content)
	if projection.ProjectedStateID.Valid && projection.ProjectedStateID.Int64 == stateID && projection.ProjectedHash == targetHash {
		return nil
	}
	targetPath := normalizeStateRelPath(projection.MaterializedPath)
	if targetPath == "" {
		return nil
	}
	_, err = p.State.InsertFSJob(ctx, FSJob{
		JobKey:        "content:write:" + p.StreamID + ":" + strconv.FormatInt(stateID, 10) + ":" + hashKey(projection.ProjectedHash),
		Kind:          "write-content",
		StreamID:      p.StreamID,
		EntryID:       projection.EntryID,
		TargetPath:    targetPath,
		ExpectedHash:  projection.ProjectedHash,
		TargetHash:    targetHash,
		TargetStateID: sql.NullInt64{Int64: stateID, Valid: true},
	})
	return err
}

func BuildContentReplaceUpdate(projectedState []byte, content string) ([]byte, error) {
	doc := crdt.New()
	defer doc.Close()
	if len(projectedState) > 0 {
		if err := crdt.ApplyUpdateV1(doc, projectedState, "projected-base"); err != nil {
			return nil, err
		}
	}
	text := doc.GetText("content")
	update, err := doc.Update(func(txn *crdt.Transaction) error {
		if length := text.LenInTxn(txn); length > 0 {
			if err := text.DeleteRange(txn, 0, length); err != nil {
				return err
			}
		}
		if content != "" {
			if err := text.InsertValue(txn, 0, content); err != nil {
				return err
			}
		}
		return nil
	}, "content-local-edit")
	if err != nil {
		return nil, err
	}
	if len(update) == 0 {
		return nil, fmt.Errorf("content update is empty")
	}
	return update, nil
}

func ContentFromStateUpdate(projectedState []byte) (string, error) {
	doc := crdt.New()
	defer doc.Close()
	if len(projectedState) > 0 {
		if err := crdt.ApplyUpdateV1(doc, projectedState, "projected-base"); err != nil {
			return "", err
		}
	}
	return doc.GetText("content").ToString(), nil
}
