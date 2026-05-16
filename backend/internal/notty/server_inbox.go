package notty

import "net/http"

func (s *Server) publishAgentInboxChanges(r *http.Request) {
	publishAgentInboxChanges(s.requestStore(r), s.requestBroker(r))
}

func publishAgentInboxChanges(store *Store, broker *Broker) {
	if store == nil || broker == nil {
		return
	}
	for _, change := range store.DrainAgentInboxChanges() {
		broker.Publish(EventEnvelope{Type: "agent.inbox.changed", Data: change})
	}
}
