package ws

import "testing"

func TestHubPublishesAndUnsubscribes(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe()
	hub.Publish([]byte("frame"))
	if got := string(<-sub.C); got != "frame" {
		t.Fatalf("got %q", got)
	}
	sub.Close()
	if got := hub.ClientCount(); got != 0 {
		t.Fatalf("client count=%d", got)
	}
}

func TestHubDisconnectsSlowClientAndBoundsQueuedFrames(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe()
	for i := 0; i < clientQueueSize; i++ {
		hub.Publish([]byte("frame"))
	}
	delivered, dropped := hub.Publish([]byte("overflow"))
	if delivered != 0 || dropped != 1 || hub.ClientCount() != 0 {
		t.Fatalf("delivered=%d dropped=%d clients=%d", delivered, dropped, hub.ClientCount())
	}
	if _, ok := <-sub.C; !ok {
		t.Fatal("queued frames should drain before channel close")
	}
	for range sub.C {
	}
}
