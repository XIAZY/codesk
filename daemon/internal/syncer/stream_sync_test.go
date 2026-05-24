package syncer

import (
	"context"
	"testing"

	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

func TestStreamSyncInsertsInboxAndQueuesStream(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	update := BuildInitialContentUpdate([]byte("remote"))
	queued := []string{}
	sync := newStreamSync(Config{}, state, "doc_a", "content", func(streamID string) {
		queued = append(queued, streamID)
	})

	if err := sync.handleMessage(ctx, yproto.BuildSyncUpdate(update)); err != nil {
		t.Fatalf("handle stream update: %v", err)
	}
	inbox, err := state.UnappliedInbox(ctx, "doc_a", 10)
	if err != nil {
		t.Fatalf("unapplied inbox: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("expected one inbox row, got %#v", inbox)
	}
	if len(queued) != 1 || queued[0] != "doc_a" {
		t.Fatalf("expected stream queued once, got %#v", queued)
	}
	if err := sync.handleMessage(ctx, yproto.BuildSyncUpdate(update)); err != nil {
		t.Fatalf("handle duplicate stream update: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("duplicate update should not requeue, got %#v", queued)
	}
}

func TestStreamSyncInitialStepUsesLocalStateVector(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	doc := crdt.New(crdt.WithGUID("doc_a"))
	text := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "local", nil)
	}, "test")
	if _, err := state.persistLatestStreamDocFixture(ctx, "doc_a", doc, contentSHA256([]byte("local"))); err != nil {
		t.Fatalf("persist state: %v", err)
	}
	payload := newStreamSync(Config{}, state, "doc_a", "content", nil).initialSyncStep(ctx, "doc_a")
	topLevel, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode protocol: %v", err)
	}
	if topLevel != yproto.MessageSync {
		t.Fatalf("unexpected top level %d", topLevel)
	}
	syncType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	if syncType != yproto.SyncStep1 || len(data) == 0 {
		t.Fatalf("expected sync step 1 with local state vector, type=%d bytes=%d", syncType, len(data))
	}
}

func TestStreamSyncBuildsLocalUpdateForServerSyncStep1(t *testing.T) {
	ctx := context.Background()
	state, err := OpenWorkspaceStateDB(t.TempDir())
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	local := contentDoc(t, "doc_a", "local")
	defer local.Close()
	if _, err := state.persistLatestStreamDocFixture(ctx, "doc_a", local, contentSHA256([]byte("local"))); err != nil {
		t.Fatalf("persist local state: %v", err)
	}
	remote := crdt.New(crdt.WithGUID("doc_a"))
	defer remote.Close()
	stateVector, err := remote.StateVectorV1()
	if err != nil {
		t.Fatalf("remote state vector: %v", err)
	}

	update, err := newStreamSync(Config{}, state, "doc_a", "content", nil).localUpdateForStateVector(ctx, "doc_a", "content", stateVector)
	if err != nil {
		t.Fatalf("build local update: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected local update bytes")
	}
	if err := crdt.ApplyUpdateV1(remote, update, "local"); err != nil {
		t.Fatalf("apply local update: %v", err)
	}
	if got := remote.GetText("content").ToString(); got != "local" {
		t.Fatalf("unexpected synced content %q", got)
	}
}
