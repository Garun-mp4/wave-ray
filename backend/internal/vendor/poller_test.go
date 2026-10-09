package vendor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPollConfigFromEnvRequiresCompleteCredentialedSources(t *testing.T) {
	values := map[string]string{
		"RUVIEW_MIST_SITE_ID":     "site_1",
		"RUVIEW_MIST_REGION":      "europe",
		"MIST_API_TOKEN":          "secret-token",
		"RUVIEW_VENDOR_POLL_SECS": "30",
	}
	config, err := PollConfigFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Interval != 30*time.Second || len(config.Sources) != 1 {
		t.Fatalf("unexpected config: %+v", config)
	}
	source := config.Sources[0]
	if source.Vendor != Mist || source.BaseURL != "https://api.eu.mist.com" || source.Path != "/api/v1/sites/site_1/stats/clients" || source.TokenEnv != "MIST_API_TOKEN" || source.AuthScheme != "Token" {
		t.Fatalf("unexpected source: %+v", source)
	}
	if strings.Contains(strings.Join([]string{source.BaseURL, source.Path, source.TokenEnv}, " "), "secret-token") {
		t.Fatal("credential value was stored in poll configuration")
	}

	values["RUVIEW_MIST_SITE_ID"] = "../secret"
	if _, err := PollConfigFromEnv(func(key string) string { return values[key] }); !errors.Is(err, ErrPollConfiguration) {
		t.Fatalf("path traversal site id error=%v", err)
	}

	values = map[string]string{"RUVIEW_ORIGIN_AI_BASE_URL": "https://partner.example", "RUVIEW_ORIGIN_AI_EVENTS_PATH": "/api/events"}
	if _, err := PollConfigFromEnv(func(key string) string { return values[key] }); !errors.Is(err, ErrPollCredentials) {
		t.Fatalf("missing Origin credential error=%v", err)
	}
	values = map[string]string{
		"RUVIEW_NETGEAR_LOCATION_ID":  "location-1",
		"NETGEAR_INSIGHT_TOKEN":       "partner-token",
		"RUVIEW_NETGEAR_BASE_URL":     "https://partner.example",
		"RUVIEW_NETGEAR_CLIENTS_PATH": "/api/locations/{location_id}/clients",
	}
	config, err = PollConfigFromEnv(func(key string) string { return values[key] })
	if err != nil || len(config.Sources) != 1 || config.Sources[0].Vendor != Netgear || config.Sources[0].Path != "/api/locations/location-1/clients" {
		t.Fatalf("partner-configured NETGEAR source=%+v error=%v", config, err)
	}
	values["RUVIEW_NETGEAR_BASE_URL"] = ""
	if _, err := PollConfigFromEnv(func(key string) string { return values[key] }); !errors.Is(err, ErrPollConfiguration) {
		t.Fatalf("NETGEAR endpoint without partner contract error=%v", err)
	}

	values = map[string]string{"RUVIEW_VENDOR_POLL_SECS": "3601"}
	if _, err := PollConfigFromEnv(func(key string) string { return values[key] }); !errors.Is(err, ErrPollConfiguration) {
		t.Fatalf("unbounded poll interval error=%v", err)
	}
	values = map[string]string{"RUVIEW_NETGEAR_TOKEN_ENV": "NETGEAR_INSIGHT_TOKEN"}
	config, err = PollConfigFromEnv(func(key string) string { return values[key] })
	if err != nil || len(config.Sources) != 0 {
		t.Fatalf("the Compose default NETGEAR credential reference must not enable polling by itself: sources=%+v error=%v", config.Sources, err)
	}
}

func TestPollerPaginatesAndStoresLatestEvent(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Token test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.api+json") {
			http.Error(w, "Mist media type missing", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"results":[{"id":"client-a","timestamp":1710000000,"rssi":-55}],"next_cursor":"page-2"}`))
			return
		}
		if r.URL.Query().Get("cursor") != "page-2" {
			http.Error(w, "wrong cursor", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"client-b","timestamp":1710000001,"rssi":-48}]}`))
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := PollConfig{Interval: time.Second, Sources: []PollSource{{
		Vendor: Mist, BaseURL: parsed.Scheme + "://" + parsed.Host, Path: "/api/v1/sites/site/stats/clients",
		TokenEnv: "MIST_TOKEN", AuthScheme: "Token", Method: http.MethodGet, Paginated: true,
	}}}
	store := NewStore()
	poller, err := NewPoller(config, store, func(key string) string {
		if key == "MIST_TOKEN" {
			return "test-token"
		}
		return ""
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := poller.PollOnce(ctx, Mist); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("request count=%d, want 2", requests)
	}
	snapshot, ok := store.Latest(Mist)
	if !ok || snapshot.TimestampUS != 1710000001000000 || snapshot.SourceID != "client-b" {
		t.Fatalf("latest event=%+v, found=%t", snapshot, ok)
	}
	statuses := poller.Status()
	if len(statuses) != 1 || statuses[0].State != "ready" || statuses[0].LastSuccess == nil || statuses[0].LastError != "" {
		t.Fatalf("poll status=%+v", statuses)
	}
}

func TestPollerDoesNotCommitPartialPageWhenLaterPageFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"results":[{"id":"client-a","timestamp":1710000000,"rssi":-55}],"next_cursor":"page-2"}`))
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	store := NewStore()
	poller, err := NewPoller(PollConfig{Interval: time.Second, Sources: []PollSource{{
		Vendor: Mist, BaseURL: parsed.Scheme + "://" + parsed.Host, Path: "/clients", TokenEnv: "TOKEN", AuthScheme: "Token", Method: http.MethodGet, Paginated: true,
	}}}, store, func(string) string { return "test-token" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := poller.PollOnce(context.Background(), Mist); !errors.Is(err, ErrPollResponse) {
		t.Fatalf("poll error=%v", err)
	}
	if _, ok := store.Latest(Mist); ok {
		t.Fatal("partial page was committed after later page failed")
	}
	if status := poller.Status()[0]; status.State != "error" || status.LastError != "response_rejected" {
		t.Fatalf("failure status=%+v", status)
	}
}

func TestPollerClearsStaleSnapshotAfterValidEmptyClientPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	if err := store.Put(Event{Vendor: Mist, Capability: RFTelemetry, TimestampUS: 1_710_000_000_000_000, SourceID: "old-client", Metrics: map[string]float64{"rssi_dbm": -55}}); err != nil {
		t.Fatal(err)
	}
	poller, err := NewPoller(PollConfig{Interval: time.Second, Sources: []PollSource{{
		Vendor: Mist, BaseURL: parsed.Scheme + "://" + parsed.Host, Path: "/clients", TokenEnv: "TOKEN", AuthScheme: "Token", Method: http.MethodGet, Paginated: true,
	}}}, store, func(string) string { return "test-token" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := poller.PollOnce(context.Background(), Mist); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Latest(Mist); ok {
		t.Fatal("a successful empty response left a stale client event active")
	}
	if status := poller.Status()[0]; status.State != "ready" || status.LastError != "" {
		t.Fatalf("empty client page should be a healthy poll: %+v", status)
	}
}

func TestPollSourceRejectsRedirectsAndUnsafeEndpoints(t *testing.T) {
	for _, source := range []PollSource{
		{Vendor: Mist, BaseURL: "https://user:pass@api.mist.com", Path: "/api", TokenEnv: "TOKEN", Method: http.MethodGet},
		{Vendor: Mist, BaseURL: "https://api.mist.com", Path: "//attacker.example/path", TokenEnv: "TOKEN", Method: http.MethodGet},
		{Vendor: Mist, BaseURL: "http://example.com", Path: "/api", TokenEnv: "TOKEN", Method: http.MethodGet},
		{Vendor: Mist, BaseURL: "https://api.mist.com", Path: "/api/../secrets", TokenEnv: "TOKEN", Method: http.MethodGet},
	} {
		if err := source.Validate(); !errors.Is(err, ErrPollConfiguration) {
			t.Errorf("source %+v error=%v, want configuration rejection", source, err)
		}
	}
}
