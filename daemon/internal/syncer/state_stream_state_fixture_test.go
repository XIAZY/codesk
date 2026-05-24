package syncer

import (
	"context"
	"errors"

	crdt "notty/internal/ycrdt"
)

func (s *WorkspaceStateDB) persistLatestStreamDocFixture(ctx context.Context, streamID string, doc *crdt.Doc, materializedTextSHA256 string) (int64, error) {
	if doc == nil {
		return 0, errors.New("stream doc is required")
	}
	if err := s.EnsureLocalStream(ctx, streamID, "unknown"); err != nil {
		return 0, err
	}
	stateUpdate := doc.EncodeStateAsUpdate()
	stateVector, err := doc.StateVectorV1()
	if err != nil {
		return 0, err
	}
	stateID, err := s.InsertStreamState(ctx, streamID, stateUpdate, stateVector, materializedTextSHA256)
	if err != nil {
		return 0, err
	}
	if err := s.UpdateLatestStreamState(ctx, streamID, stateID, stateVector); err != nil {
		return 0, err
	}
	return stateID, nil
}
