package notty

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

type Server struct {
	cfg              Config
	db               *Database
	rooms            *DocumentRooms
	emailSender      EmailSender
	upgrader         websocket.Upgrader
	mu               sync.Mutex
	workspaceStores  map[string]*Store
	workspaceBrokers map[string]*Broker
}

func NewServer(cfg Config, database *Database) *Server {
	return &Server{
		cfg:         cfg,
		db:          database,
		rooms:       NewDocumentRooms(),
		emailSender: emailSenderFromConfig(cfg),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		workspaceStores:  map[string]*Store{},
		workspaceBrokers: map[string]*Broker{},
	}
}

func (s *Server) sqlDB() *sql.DB {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.DB
}

func (s *Server) workspaceStore(workspaceID string) (*Store, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, errors.New("workspace id is required")
	}
	s.mu.Lock()
	if store := s.workspaceStores[workspaceID]; store != nil {
		s.mu.Unlock()
		return store, nil
	}
	s.mu.Unlock()
	if s.db == nil || s.db.DB == nil {
		return nil, errors.New("database is required")
	}
	workspace, err := getWorkspace(s.db.DB, workspaceID)
	if err != nil {
		return nil, err
	}
	store, err := NewWorkspaceStore(s.db, workspace.ID, workspace.Name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if existing := s.workspaceStores[workspaceID]; existing != nil {
		s.mu.Unlock()
		return existing, nil
	}
	s.workspaceStores[workspaceID] = store
	s.mu.Unlock()
	return store, nil
}

func (s *Server) workspaceBroker(workspaceID string) *Broker {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil
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
	return nil
}

func (s *Server) requestBroker(r *http.Request) *Broker {
	if broker, ok := requestBrokerFromContext(r.Context()); ok {
		return broker
	}
	return nil
}

func (s *Server) requestWorkspaceID(r *http.Request) string {
	if id := workspaceIDFromContext(r.Context()); id != "" {
		return id
	}
	return ""
}
