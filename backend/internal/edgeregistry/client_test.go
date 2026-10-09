package edgeregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
)

type fetchResult struct {
	body []byte
	err  error
}

type queueFetcher struct {
	mu      sync.Mutex
	results []fetchResult
	calls   int
	started chan struct{}
	release chan struct{}
}

func (f *queueFetcher) Fetch(ctx context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	if f.started != nil {
		close(f.started)
		f.started = nil
	}
	release := f.release
	if len(f.results) == 0 {
		f.mu.Unlock()
		return nil, errors.New("no queued response")
	}
	result := f.results[0]
	f.results = f.results[1:]
	f.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return result.body, result.err
}

func (f *queueFetcher) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestGetCachesAndForceRefreshes(t *testing.T) {
	first, second := []byte(`{"version":1}`), []byte(`{"version":2}`)
	fetcher := &queueFetcher{results: []fetchResult{{body: first}, {body: second}}}
	registry := NewWithFetcher("https://registry.example/catalog.json", time.Hour, fetcher)

	initial, err := registry.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Stale || string(initial.Registry) != string(first) || fetcher.CallCount() != 1 {
		t.Fatalf("unexpected first response: %+v calls=%d", initial, fetcher.CallCount())
	}
	digest := sha256.Sum256(first)
	if initial.UpstreamSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("wrong upstream digest: %q", initial.UpstreamSHA256)
	}

	cached, err := registry.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Stale || string(cached.Registry) != string(first) || fetcher.CallCount() != 1 {
		t.Fatalf("fresh cache was not used: %+v calls=%d", cached, fetcher.CallCount())
	}

	refreshed, err := registry.Get(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Stale || string(refreshed.Registry) != string(second) || fetcher.CallCount() != 2 {
		t.Fatalf("forced refresh did not bypass cache: %+v calls=%d", refreshed, fetcher.CallCount())
	}
}

func TestGetServesStalePayloadWhenRefreshFails(t *testing.T) {
	fetcher := &queueFetcher{results: []fetchResult{
		{body: []byte(`{"version":1}`)},
		{err: errors.New("private upstream detail")},
	}}
	registry := NewWithFetcher("https://registry.example/catalog.json", 0, fetcher)
	if _, err := registry.Get(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	response, err := registry.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Stale || string(response.Registry) != `{"version":1}` {
		t.Fatalf("expected stale cached registry: %+v", response)
	}
}

func TestGetRejectsInvalidOversizedAndUnavailableResponsesWithoutCache(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result fetchResult
	}{
		{name: "invalid json", result: fetchResult{body: []byte("{"), err: nil}},
		{name: "too large", result: fetchResult{body: make([]byte, MaxPayloadBytes+1)}},
		{name: "upstream error", result: fetchResult{err: errors.New("upstream unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewWithFetcher("https://registry.example/catalog.json", time.Hour, &queueFetcher{results: []fetchResult{tc.result}})
			if _, err := registry.Get(context.Background(), false); err == nil {
				t.Fatal("invalid upstream response succeeded without a cache")
			}
		})
	}
}

func TestConcurrentGetsShareOneRefresh(t *testing.T) {
	fetcher := &queueFetcher{
		results: []fetchResult{{body: []byte(`{"version":1}`)}},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	started := fetcher.started
	registry := NewWithFetcher("https://registry.example/catalog.json", time.Hour, fetcher)
	const callers = 8
	results := make(chan error, callers)
	for range callers {
		go func() {
			_, err := registry.Get(context.Background(), false)
			results <- err
		}()
	}
	<-started
	close(fetcher.release)
	for range callers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if fetcher.CallCount() != 1 {
		t.Fatalf("concurrent callers triggered %d upstream requests", fetcher.CallCount())
	}
}

func TestForceRefreshQuerySemantics(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"1", true}, {"true", true}, {"0", false}, {"TRUE", false}, {"yes", false}} {
		if got := ForceRefreshRequested(tc.value); got != tc.want {
			t.Errorf("ForceRefreshRequested(%q)=%v, want %v", tc.value, got, tc.want)
		}
	}
}
