package syncer

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"notty/internal/yproto"
)

type documentSync struct {
	cfg       Config
	cache     *documentCache
	markDirty func(string)

	mu       sync.Mutex
	document *document
}

func newDocumentSync(cfg Config, cache *documentCache, document *document, markDirty func(string)) *documentSync {
	return &documentSync{
		cfg:       cfg,
		cache:     cache,
		document:  cloneSyncDocument(document),
		markDirty: markDirty,
	}
}

func cloneSyncDocument(document *document) *document {
	if document == nil {
		return nil
	}
	clone := *document
	return &clone
}

func (d *documentSync) update(document *document) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.document = cloneSyncDocument(document)
}

func (d *documentSync) currentDocument() *document {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneSyncDocument(d.document)
}

func (d *documentSync) run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.runOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("document sync error doc=%s err=%v", d.documentID(), err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (d *documentSync) documentID() string {
	if document := d.currentDocument(); document != nil {
		return document.ID
	}
	return ""
}

func (d *documentSync) runOnce(ctx context.Context) error {
	document := d.currentDocument()
	if document == nil || document.ID == "" {
		return nil
	}
	paceDocumentConnect()
	clientID := nextAwarenessClientID()
	query := url.Values{
		"client_id":  {fmt.Sprintf("%d", clientID)},
		"actor_id":   {d.cfg.AgentID},
		"actor_type": {"daemon"},
	}
	conn, _, err := dialWorkspaceWebsocket(ctx, d.cfg, "/ws/documents/"+document.ID, query, "")
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.BinaryMessage, d.initialSyncStep(document)); err != nil {
		return err
	}
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		if err := d.handleMessage(payload); err != nil {
			log.Printf("document sync message error doc=%s err=%v", document.ID, err)
		}
	}
}

func (d *documentSync) initialSyncStep(document *document) []byte {
	if d.cache != nil {
		if stateVector := d.cache.localStateVector(document.ID); len(stateVector) > 0 {
			return yproto.BuildSyncStep1FromStateVector(stateVector)
		}
	}
	return yproto.BuildSyncStep1FromStateVector(nil)
}

func (d *documentSync) handleMessage(payload []byte) error {
	topLevel, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		return err
	}
	if topLevel != yproto.MessageSync {
		return nil
	}
	syncType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		return err
	}
	document := d.currentDocument()
	if document == nil || document.ID == "" {
		return nil
	}
	switch syncType {
	case yproto.SyncStep1:
		return nil
	case yproto.SyncStep2, yproto.SyncUpdate:
		if len(data) == 0 {
			return nil
		}
		if d.cache == nil {
			return nil
		}
		appended, err := d.cache.appendPendingRemoteUpdate(document.ID, document.Path, data)
		if err != nil {
			return err
		}
		if appended && d.markDirty != nil {
			d.markDirty(document.ID)
		}
	}
	return nil
}

func (s *Service) reconcileDocumentSyncs(ctx context.Context, documents []*document) error {
	desired := map[string]*document{}
	for _, document := range documents {
		if document == nil || document.ID == "" || isIgnoredDocumentPath(document.Path) {
			continue
		}
		desired[document.ID] = document
	}

	s.mu.Lock()
	for documentID, document := range desired {
		if managed := s.documentSyncs[documentID]; managed != nil {
			managed.sync.update(document)
			continue
		}
		syncCtx, cancel := context.WithCancel(ctx)
		sync := newDocumentSync(s.cfg, s.docCache, document, s.markDocumentDirty)
		s.documentSyncs[documentID] = &managedDocumentSync{sync: sync, cancel: cancel}
		go sync.run(syncCtx)
	}
	staleIDs := make([]string, 0)
	for documentID := range s.documentSyncs {
		if _, ok := desired[documentID]; !ok {
			staleIDs = append(staleIDs, documentID)
		}
	}
	sort.Strings(staleIDs)
	stale := make([]*managedDocumentSync, 0, len(staleIDs))
	for _, documentID := range staleIDs {
		stale = append(stale, s.documentSyncs[documentID])
		delete(s.documentSyncs, documentID)
	}
	s.mu.Unlock()

	for _, managed := range stale {
		managed.cancel()
	}
	return nil
}

func (s *Service) closeDocumentSyncs() {
	s.mu.Lock()
	syncs := make([]*managedDocumentSync, 0, len(s.documentSyncs))
	for _, sync := range s.documentSyncs {
		syncs = append(syncs, sync)
	}
	s.documentSyncs = map[string]*managedDocumentSync{}
	s.mu.Unlock()
	for _, sync := range syncs {
		sync.cancel()
	}
}
