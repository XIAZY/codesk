package syncer

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type streamProjection struct {
	cfg          Config
	client       *http.Client
	rootDir      string
	actorID      string
	actorType    string
	rootStreamID string

	state      *WorkspaceStateDB
	fs         *WorkspaceFS
	loop       *WorkspaceSyncLoop
	sender     *StreamSender
	streamSync map[string]*managedStreamSync

	mu sync.Mutex
}

func newStreamProjection(ctx context.Context, cfg Config, client *http.Client, rootDir string, actorID string, actorType string, rootStreamID string) (*streamProjection, error) {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		rootDir = cfg.WorkspaceDir
	}
	actorID = firstNonEmptyText(actorID, cfg.AgentID, "daemon")
	actorType = firstNonEmptyText(actorType, "daemon")
	localCfg := cfg
	localCfg.WorkspaceDir = rootDir
	localCfg.AgentID = actorID
	state, err := OpenWorkspaceStateDB(rootDir)
	if err != nil {
		return nil, err
	}
	fs := NewWorkspaceFS(rootDir)
	fs.State = state
	scanState, err := state.InitializeScanCapabilities(ctx, fs)
	if err != nil {
		_ = state.Close()
		return nil, err
	}
	projection := &streamProjection{
		cfg:          localCfg,
		client:       client,
		rootDir:      rootDir,
		actorID:      actorID,
		actorType:    actorType,
		rootStreamID: strings.TrimSpace(rootStreamID),
		state:        state,
		fs:           fs,
		streamSync:   map[string]*managedStreamSync{},
	}
	projection.loop = &WorkspaceSyncLoop{
		State:        state,
		FS:           fs,
		RootStreamID: projection.rootStreamID,
		ActorID:      actorID,
		ActorType:    actorType,
		Capabilities: scanState.Capabilities(),
		Queue:        projection.Mark,
	}
	projection.sender = &StreamSender{
		State: state,
		Transport: HTTPStreamTransport{
			Config: localCfg,
			Client: client,
		},
		OnAck: projection.Mark,
	}
	return projection, nil
}

func (p *streamProjection) Run(ctx context.Context) {
	if p == nil || p.loop == nil || p.rootStreamID == "" {
		return
	}
	p.ensureManagedStreamSync(ctx, p.rootStreamID, "root")
	p.Mark(p.rootStreamID)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.Close()
			return
		case <-ticker.C:
			if err := p.Reconcile(ctx); err != nil && ctx.Err() == nil {
				log.Printf("stream projection reconcile error root=%s actor=%s err=%v", p.rootDir, p.actorID, err)
			}
		}
	}
}

func (p *streamProjection) Reconcile(ctx context.Context) error {
	if p == nil || p.loop == nil || p.sender == nil || p.rootStreamID == "" {
		return nil
	}
	if state := p.loop.State; state != nil {
		inserted, err := state.MaybeInsertPeriodicFullScanHint(ctx, PeriodicFullScanInterval)
		if err != nil {
			return err
		}
		if inserted {
			p.loop.queue(p.rootStreamID)
		}
	}
	if err := p.loop.ReconcileOne(ctx, p.rootStreamID); err != nil {
		return err
	}
	if p.state != nil {
		streamIDs, err := p.state.ListContentProjectionStreamIDs(ctx, 256)
		if err != nil {
			return err
		}
		for _, streamID := range streamIDs {
			if strings.TrimSpace(streamID) == "" || streamID == p.rootStreamID {
				continue
			}
			if err := p.loop.ReconcileOne(ctx, streamID); err != nil {
				return err
			}
		}
	}
	return p.sender.SendPending(ctx)
}

func (p *streamProjection) Mark(streamID string) {
	streamID = strings.TrimSpace(streamID)
	if p == nil || streamID == "" {
		return
	}
	go func() {
		ctx := context.Background()
		if streamID != p.rootStreamID {
			p.ensureManagedStreamSync(ctx, streamID, "content")
		}
		if p.loop != nil {
			if err := p.loop.ReconcileOne(ctx, streamID); err != nil {
				log.Printf("stream projection reconcile error root=%s stream=%s err=%v", p.rootDir, streamID, err)
				return
			}
		}
		if p.sender != nil {
			if err := p.sender.SendPending(ctx); err != nil {
				log.Printf("stream projection send error root=%s stream=%s err=%v", p.rootDir, streamID, err)
			}
		}
	}()
}

func (p *streamProjection) EnsureDocumentStreams(ctx context.Context, workspace *workspaceResponse) {
	if p == nil || workspace == nil {
		return
	}
	for _, document := range workspace.Documents {
		if document == nil || strings.TrimSpace(document.ID) == "" || isIgnoredDocumentPath(document.Path) {
			continue
		}
		p.ensureManagedStreamSync(ctx, document.ID, "content")
	}
}

func (p *streamProjection) ensureManagedStreamSync(ctx context.Context, streamID string, kind string) {
	streamID = strings.TrimSpace(streamID)
	if p == nil || streamID == "" {
		return
	}
	kind = firstNonEmptyText(kind, "unknown")
	p.mu.Lock()
	if p.streamSync == nil {
		p.streamSync = map[string]*managedStreamSync{}
	}
	if managed := p.streamSync[streamID]; managed != nil {
		managed.sync.update(streamID, kind)
		p.mu.Unlock()
		return
	}
	syncCtx, cancel := context.WithCancel(ctx)
	sync := newStreamSync(p.cfg, p.state, streamID, kind, p.Mark)
	p.streamSync[streamID] = &managedStreamSync{sync: sync, cancel: cancel}
	p.mu.Unlock()
	go sync.run(syncCtx)
}

func (p *streamProjection) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	syncs := make([]*managedStreamSync, 0, len(p.streamSync))
	for _, managed := range p.streamSync {
		syncs = append(syncs, managed)
	}
	p.streamSync = map[string]*managedStreamSync{}
	state := p.state
	p.state = nil
	p.mu.Unlock()
	for _, managed := range syncs {
		managed.cancel()
	}
	if state != nil {
		_ = state.Close()
	}
}
