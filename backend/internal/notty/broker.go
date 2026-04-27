package notty

import "sync"

type Broker struct {
	mu          sync.RWMutex
	subscribers map[chan EventEnvelope]struct{}
}

func NewBroker() *Broker {
	return &Broker{subscribers: map[chan EventEnvelope]struct{}{}}
}

func (b *Broker) Subscribe() (chan EventEnvelope, func()) {
	channel := make(chan EventEnvelope, 32)
	b.mu.Lock()
	b.subscribers[channel] = struct{}{}
	b.mu.Unlock()

	return channel, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subscribers, channel)
		close(channel)
	}
}

func (b *Broker) Publish(event EventEnvelope) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for channel := range b.subscribers {
		select {
		case channel <- event:
		default:
		}
	}
}
