package notty

import (
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

type Server struct {
	cfg              Config
	store            *Store
	subscribers      *Broker
	rooms            *DocumentRooms
	upgrader         websocket.Upgrader
	mu               sync.Mutex
	workspaceStores  map[string]*Store
	workspaceBrokers map[string]*Broker
}

func NewServer(cfg Config, store *Store) *Server {
	return &Server{
		cfg:         cfg,
		store:       store,
		subscribers: NewBroker(),
		rooms:       NewDocumentRooms(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		workspaceStores:  map[string]*Store{},
		workspaceBrokers: map[string]*Broker{},
	}
}

func (s *Server) authEnabled() bool {
	return strings.TrimSpace(s.cfg.JWTSecret) != "" && s.store != nil && s.store.db != nil
}

func (s *Server) workspaceStore(workspaceID string) (*Store, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		workspaceID = s.store.Snapshot().WorkspaceID
	}
	if s.store != nil && s.store.Snapshot().WorkspaceID == workspaceID {
		return s.store, nil
	}
	s.mu.Lock()
	if store := s.workspaceStores[workspaceID]; store != nil {
		s.mu.Unlock()
		return store, nil
	}
	s.mu.Unlock()
	if s.store == nil || s.store.db == nil {
		return s.store, nil
	}
	workspace, err := getWorkspace(s.store.db, workspaceID)
	if err != nil {
		return nil, err
	}
	dataSource := s.cfg.DatabaseURL
	if dataSource == "" {
		dataSource = s.store.dataFile
	}
	store, err := NewStoreForWorkspace(dataSource, workspace.ID, workspace.Name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if existing := s.workspaceStores[workspaceID]; existing != nil {
		s.mu.Unlock()
		_ = store.Close()
		return existing, nil
	}
	s.workspaceStores[workspaceID] = store
	s.mu.Unlock()
	return store, nil
}

func (s *Server) workspaceBroker(workspaceID string) *Broker {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" || (s.store != nil && workspaceID == s.store.Snapshot().WorkspaceID) {
		return s.subscribers
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	broker := s.workspaceBrokers[workspaceID]
	if broker == nil {
		broker = NewBroker()
		s.workspaceBrokers[workspaceID] = broker
	}
	return broker
}

func (s *Server) requestStore(r *http.Request) *Store {
	if store, ok := requestStoreFromContext(r.Context()); ok {
		return store
	}
	return s.store
}

func (s *Server) requestBroker(r *http.Request) *Broker {
	if broker, ok := requestBrokerFromContext(r.Context()); ok {
		return broker
	}
	return s.subscribers
}

func (s *Server) requestWorkspaceID(r *http.Request) string {
	if id := workspaceIDFromContext(r.Context()); id != "" {
		return id
	}
	if s.store == nil {
		return ""
	}
	return s.store.Snapshot().WorkspaceID
}
