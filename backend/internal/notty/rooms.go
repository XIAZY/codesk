package notty

import (
	"sync"
	"sync/atomic"

	"notty/internal/yproto"
)

type DocumentRooms struct {
	mu    sync.Mutex
	rooms map[string]*DocumentRoom
}

type DocumentRoom struct {
	mu        sync.Mutex
	conns     map[*DocumentConn]struct{}
	awareness map[uint64]yproto.AwarenessState
}

type DocumentConn struct {
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
}

func NewDocumentRooms() *DocumentRooms {
	return &DocumentRooms{rooms: map[string]*DocumentRoom{}}
}

func newDocumentConn(sendBuffer int) *DocumentConn {
	return &DocumentConn{
		send: make(chan []byte, sendBuffer),
		done: make(chan struct{}),
	}
}

func (c *DocumentConn) Close() {
	c.closed.Store(true)
	if c.done == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

func (c *DocumentConn) Done() <-chan struct{} {
	return c.done
}

func (c *DocumentConn) Enqueue(payload []byte) bool {
	if c == nil || c.send == nil || c.closed.Load() {
		return false
	}
	if c.done == nil {
		c.send <- payload
		return true
	}
	select {
	case <-c.done:
		return false
	case c.send <- payload:
		return true
	}
}

func (c *DocumentConn) TryEnqueue(payload []byte) bool {
	if c == nil || c.send == nil || c.closed.Load() {
		return false
	}
	if c.done == nil {
		select {
		case c.send <- payload:
			return true
		default:
			return false
		}
	}
	select {
	case <-c.done:
		return false
	case c.send <- payload:
		return true
	default:
		return false
	}
}

func (r *DocumentRooms) ForDocument(documentID string) *DocumentRoom {
	r.mu.Lock()
	defer r.mu.Unlock()
	if room, ok := r.rooms[documentID]; ok {
		return room
	}
	room := &DocumentRoom{
		conns:     map[*DocumentConn]struct{}{},
		awareness: map[uint64]yproto.AwarenessState{},
	}
	r.rooms[documentID] = room
	return room
}

func (r *DocumentRoom) Add(conn *DocumentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[conn] = struct{}{}
}

func (r *DocumentRoom) Remove(conn *DocumentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, conn)
}

func (r *DocumentRoom) BroadcastBestEffort(payload []byte, skip *DocumentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for conn := range r.conns {
		if conn == skip {
			continue
		}
		conn.TryEnqueue(payload)
	}
}

func (r *DocumentRoom) BroadcastSyncUpdate(payload []byte, skip *DocumentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for conn := range r.conns {
		if conn == skip {
			continue
		}
		if !conn.TryEnqueue(payload) {
			conn.Close()
		}
	}
}

func (r *DocumentRoom) SnapshotAwareness() map[uint64]yproto.AwarenessState {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[uint64]yproto.AwarenessState, len(r.awareness))
	for clientID, state := range r.awareness {
		result[clientID] = state
	}
	return result
}

func (r *DocumentRoom) ApplyAwareness(update map[uint64]yproto.AwarenessState) []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := make([]uint64, 0, len(update))
	for clientID, next := range update {
		current, ok := r.awareness[clientID]
		if !ok || current.Clock < next.Clock || (current.Clock == next.Clock && string(next.State) == "null") {
			if string(next.State) == "null" {
				delete(r.awareness, clientID)
			} else {
				r.awareness[clientID] = next
			}
			changed = append(changed, clientID)
		}
	}
	return changed
}

func (r *DocumentRoom) RemoveAwareness(clientID uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.awareness[clientID]; !ok {
		return false
	}
	delete(r.awareness, clientID)
	return true
}
