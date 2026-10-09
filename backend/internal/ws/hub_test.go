package ws

import (
	"strconv"
	"testing"
)

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

func TestHubDropsOldestFrameForSlowClientWithoutDisconnecting(t *testing.T) {
	hub := NewHub()
	sub := hub.SubscribeClient()
	for i := 1; i <= clientQueueSize; i++ {
		delivered, dropped := hub.Publish([]byte(strconv.Itoa(i)))
		if delivered != 1 || dropped != 0 {
			t.Fatalf("publish %d: delivered=%d dropped=%d", i, delivered, dropped)
		}
	}
	delivered, dropped := hub.Publish([]byte("257"))
	if delivered != 1 || dropped != 1 || hub.ClientCount() != 1 || hub.BrowserClientCount() != 1 {
		t.Fatalf("delivered=%d dropped=%d subscribers=%d browser clients=%d", delivered, dropped, hub.ClientCount(), hub.BrowserClientCount())
	}
	for expected := 2; expected <= clientQueueSize+1; expected++ {
		frame := <-sub.C
		current, err := strconv.Atoi(string(frame))
		if err != nil || current != expected {
			t.Fatalf("slow client frame sequence: expected=%d current=%q err=%v", expected, frame, err)
		}
	}
	if hub.ClientCount() != 1 || hub.BrowserClientCount() != 1 {
		t.Fatalf("slow client was disconnected: subscribers=%d browser clients=%d", hub.ClientCount(), hub.BrowserClientCount())
	}
	sub.Close()
}

func TestHubCountsBrowserClientsSeparatelyFromInternalSubscribers(t *testing.T) {
	hub := NewHub()
	internal := hub.Subscribe()
	client := hub.SubscribeClient()
	if hub.ClientCount() != 2 || hub.BrowserClientCount() != 1 {
		t.Fatalf("subscribers=%d browser clients=%d", hub.ClientCount(), hub.BrowserClientCount())
	}
	internal.Close()
	if hub.BrowserClientCount() != 1 {
		t.Fatalf("internal unsubscribe changed browser count to %d", hub.BrowserClientCount())
	}
	client.Close()
	if hub.BrowserClientCount() != 0 {
		t.Fatalf("browser count after close=%d", hub.BrowserClientCount())
	}
}
