package ws

import "sync"

const clientQueueSize = 256

type client struct {
	queue   chan []byte
	browser bool
}

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
// shed their oldest queued frame instead of blocking CSI ingestion or growing
// memory without a bound.
type Hub struct {
	mu             sync.Mutex
	clients        map[uint64]client
	nextID         uint64
	browserClients int
	closed         bool
}

func NewHub() *Hub { return &Hub{clients: make(map[uint64]client)} }

func (h *Hub) Subscribe() *Subscription {
	return h.subscribe(false)
}

// SubscribeClient registers a browser-facing sensing or pose socket. Internal
// consumers such as the recorder use Subscribe and are excluded from stream
// client counts, matching the Rust server's WebSocket counter.
func (h *Hub) SubscribeClient() *Subscription {
	return h.subscribe(true)
}

func (h *Hub) subscribe(browser bool) *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		ch := make(chan []byte)
		close(ch)
		return &Subscription{C: ch, hub: h}
	}
	h.nextID++
	queue := make(chan []byte, clientQueueSize)
	h.clients[h.nextID] = client{queue: queue, browser: browser}
	if browser {
		h.browserClients++
	}
	return &Subscription{C: queue, id: h.nextID, hub: h}
}

func (h *Hub) Publish(frame []byte) (delivered, dropped int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	immutableFrame := append([]byte(nil), frame...)
	for _, subscriber := range h.clients {
		queue := subscriber.queue
		select {
		case queue <- immutableFrame:
			delivered++
		default:
			// Tokio broadcast keeps a bounded ring and reports Lagged to a slow
			// receiver. Drop its oldest queued frame and retain the newest one;
			// ingestion never blocks and healthy subscribers keep FIFO order.
			select {
			case <-queue:
				dropped++
			default:
			}
			select {
			case queue <- immutableFrame:
				delivered++
			default:
				// A receiver may race the eviction. This should be unreachable
				// because this hub is the only sender, but don't block ingestion.
				dropped++
			}
		}
	}
	return delivered, dropped
}

func (h *Hub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subscriber, ok := h.clients[id]; ok {
		close(subscriber.queue)
		if subscriber.browser {
			h.browserClients--
		}
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
	for id, subscriber := range h.clients {
		close(subscriber.queue)
		delete(h.clients, id)
	}
	h.browserClients = 0
}

func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// BrowserClientCount reports only connected browser-facing sensing/pose
// sockets. Recorders, introspection readers, and other internal consumers are
// deliberately excluded.
func (h *Hub) BrowserClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.browserClients
}
