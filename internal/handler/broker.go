package handler

import (
	"encoding/json"
	"sync"
)

type Broker struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

func NewBroker() *Broker {
	return &Broker{clients: make(map[chan []byte]struct{})}
}

func (b *Broker) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// TrySubscribe is Subscribe, but returns nil if max clients are already connected.
func (b *Broker) TrySubscribe(max int) chan []byte {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.clients) >= max {
		return nil
	}
	b.clients[ch] = struct{}{}
	return ch
}

func (b *Broker) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *Broker) Publish(v any) {
	data, _ := json.Marshal(v)
	b.mu.RLock()
	for ch := range b.clients {
		select {
		case ch <- data:
		default:
		}
	}
	b.mu.RUnlock()
}
