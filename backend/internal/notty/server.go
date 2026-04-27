package notty

import (
	"net/http"

	"github.com/gorilla/websocket"
)

type Server struct {
	cfg         Config
	store       *Store
	subscribers *Broker
	rooms       *DocumentRooms
	upgrader    websocket.Upgrader
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
	}
}
