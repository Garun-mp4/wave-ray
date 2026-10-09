package edgeregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	MaxPayloadBytes = 8 << 20
	fetchTimeout    = 10 * time.Second
)

type Response struct {
	FetchedAt      uint64          `json:"fetched_at"`
	TTLSeconds     uint64          `json:"ttl_seconds"`
	Stale          bool            `json:"stale"`
	UpstreamURL    string          `json:"upstream_url"`
	UpstreamSHA256 string          `json:"upstream_sha256"`
	Registry       json.RawMessage `json:"registry"`
}

type Fetcher interface {
	Fetch(context.Context, string) ([]byte, error)
}

type cachedEntry struct {
	payload       json.RawMessage
	fetchedAt     time.Time
	fetchedAtUnix uint64
	sha256        string
}

// Registry fetches the public cog catalog on demand and keeps a bounded,
// in-memory TTL cache. A failed refresh serves the last valid payload as stale.
type Registry struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	cached    *cachedEntry
	ttl       time.Duration
	upstream  string
	fetcher   Fetcher
}

func New(upstream string, ttl time.Duration) *Registry {
	client := &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many upstream redirects")
			}
			return nil
		},
	}
	return NewWithFetcher(upstream, ttl, httpFetcher{client: client})
}

func NewWithFetcher(upstream string, ttl time.Duration, fetcher Fetcher) *Registry {
	if fetcher == nil {
		fetcher = httpFetcher{client: &http.Client{Timeout: fetchTimeout}}
	}
	return &Registry{upstream: upstream, ttl: ttl, fetcher: fetcher}
}

func (r *Registry) Get(ctx context.Context, forceRefresh bool) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !forceRefresh {
		if entry := r.freshSnapshot(); entry != nil {
			return r.responseFrom(entry, false), nil
		}
	}

	// Serialize refreshes so an expired cache cannot fan out one upstream
	// request per concurrent API client.
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if !forceRefresh {
		if entry := r.freshSnapshot(); entry != nil {
			return r.responseFrom(entry, false), nil
		}
	}

	payload, err := r.fetcher.Fetch(ctx, r.upstream)
	if err == nil && len(payload) > MaxPayloadBytes {
		err = fmt.Errorf("registry response exceeds %d bytes", MaxPayloadBytes)
	}
	if err == nil && !json.Valid(payload) {
		err = errors.New("registry response is not valid JSON")
	}
	if err != nil {
		if entry := r.anySnapshot(); entry != nil {
			return r.responseFrom(entry, true), nil
		}
		return Response{}, err
	}

	now := time.Now()
	unix := now.Unix()
	if unix < 0 {
		unix = 0
	}
	digest := sha256.Sum256(payload)
	entry := &cachedEntry{
		payload: append(json.RawMessage(nil), payload...), fetchedAt: now,
		fetchedAtUnix: uint64(unix), sha256: hex.EncodeToString(digest[:]),
	}
	r.mu.Lock()
	r.cached = entry
	r.mu.Unlock()
	return r.responseFrom(entry, false), nil
}

func (r *Registry) freshSnapshot() *cachedEntry {
	entry := r.anySnapshot()
	if entry == nil || time.Since(entry.fetchedAt) >= r.ttl {
		return nil
	}
	return entry
}

func (r *Registry) anySnapshot() *cachedEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cached == nil {
		return nil
	}
	entry := *r.cached
	entry.payload = append(json.RawMessage(nil), r.cached.payload...)
	return &entry
}

func (r *Registry) responseFrom(entry *cachedEntry, stale bool) Response {
	return Response{
		FetchedAt: entry.fetchedAtUnix, TTLSeconds: uint64(max(0, r.ttl.Seconds())),
		Stale: stale, UpstreamURL: r.upstream,
		UpstreamSHA256: entry.sha256, Registry: append(json.RawMessage(nil), entry.payload...),
	}
}

type httpFetcher struct {
	client *http.Client
}

func (f httpFetcher) Fetch(ctx context.Context, upstream string) ([]byte, error) {
	client := f.client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		return nil, fmt.Errorf("create registry request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch registry: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("registry returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxPayloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read registry response: %w", err)
	}
	if len(body) > MaxPayloadBytes {
		return nil, fmt.Errorf("registry response exceeds %d bytes", MaxPayloadBytes)
	}
	return body, nil
}

var _ Fetcher = httpFetcher{}

func ForceRefreshRequested(value string) bool {
	switch strings.TrimSpace(value) {
	case "1", "true":
		return true
	default:
		return false
	}
}
