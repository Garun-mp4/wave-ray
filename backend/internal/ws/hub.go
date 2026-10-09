package ws

import "sync"

const clientQueueSize = 16

type Subscription struct {
	C      <-chan []byte
	id     uint64
	hub    *Hub
	closed sync.Once
}

func (s *Subscription) Close() {
	if s == nil || s.hub == nil {
		return
	}
	s.closed.Do(func() { s.hub.unsubscribe(s.id) })
}

// Hub publishes immutable JSON frames to bounded client queues. Slow clients
// are disconnected instead of growing memory use or blocking CSI ingestion.
type Hub struct {
	mu      sync.Mutex
	clients map[uint64]chan []byte
	nextID  uint64
	closed  bool
}

func NewHub() *Hub { return &Hub{clients: make(map[uint64]chan []byte)} }

func (h *Hub) Subscribe() *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		ch := make(chan []byte)
		close(ch)
		return &Subscription{C: ch, hub: h}
	}
	h.nextID++
	queue := make(chan []byte, clientQueueSize)
	h.clients[h.nextID] = queue
	return &Subscription{C: queue, id: h.nextID, hub: h}
}

func (h *Hub) Publish(frame []byte) (delivered, dropped int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, queue := range h.clients {
		copyFrame := append([]byte(nil), frame...)
		select {
		case queue <- copyFrame:
			delivered++
		default:
			close(queue)
			delete(h.clients, id)
			dropped++
		}
	}
	return delivered, dropped
}

func (h *Hub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if queue, ok := h.clients[id]; ok {
		close(queue)
		delete(h.clients, id)
	}
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, queue := range h.clients {
		close(queue)
		delete(h.clients, id)
	}
}

func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}
