package security

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestUDPAllowlist(t *testing.T) {
	allow, err := NewUDPAllowlist([]string{"192.168.4.0/24, 2001:db8::/32", "10.0.0.8"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"192.168.4.20", "2001:db8::42", "10.0.0.8", "127.0.0.1", "::1"} {
		addr := netip.MustParseAddr(ip)
		if !allow.AdmitAddr(addr) {
			t.Errorf("%s should be admitted", ip)
		}
	}
	if allow.AdmitAddr(netip.MustParseAddr("192.168.5.1")) || allow.Dropped() != 1 {
		t.Fatal("out-of-prefix source should be rejected and counted")
	}
}

func TestUDPAllowlistIPv4MappedPrefix(t *testing.T) {
	allow, err := NewUDPAllowlist([]string{"::ffff:192.168.4.0/120"})
	if err != nil {
		t.Fatal(err)
	}
	if !allow.AdmitAddr(netip.MustParseAddr("192.168.4.42")) || allow.AdmitAddr(netip.MustParseAddr("192.168.5.1")) {
		t.Fatal("IPv4-mapped prefix did not normalize to the expected IPv4 network")
	}
	if _, err := NewUDPAllowlist([]string{"::ffff:192.168.4.0/80"}); err == nil {
		t.Fatal("ambiguous IPv4-mapped prefix broader than /96 was accepted")
	}
}

func TestHostAllowlistAllowsConfiguredHostWithPort(t *testing.T) {
	allow, err := NewHostAllowlist([]string{"sensor.local:8080"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"sensor.local:8080", "localhost:9000", "[::1]:9000"} {
		if !allow.Allows(host) {
			t.Errorf("%s should be allowed", host)
		}
	}
	if allow.Allows("evil.example:8080") {
		t.Fatal("foreign host allowed")
	}
}

func TestTicketSingleUseAndExpiry(t *testing.T) {
	store := NewTicketStore()
	token, err := store.Issue(TicketGrant{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Consume(token); !ok {
		t.Fatal("first consume failed")
	}
	if _, ok := store.Consume(token); ok {
		t.Fatal("replayed ticket accepted")
	}

	expired := NewTicketStore()
	now := time.Now()
	expired.now = func() time.Time { return now }
	token, err = expired.Issue(TicketGrant{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(TicketTTL + time.Second)
	if _, ok := expired.Consume(token); ok {
		t.Fatal("expired ticket accepted")
	}
}

func TestRequiredScopeFailsClosedForUnknownMutation(t *testing.T) {
	if got := RequiredScope("POST", "/api/v1/new-dangerous-route"); got != "sensing:admin" {
		t.Fatalf("got %q", got)
	}
	if got := RequiredScope("POST", "/api/v1/recording/start"); got != "sensing:read" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactJSONRecursively(t *testing.T) {
	got, err := RedactJSON([]byte(`{"vital_signs":{"heart_rate_bpm":70},"persons":[{"keypoints":[1],"zone":"a"}],"presence":true}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, secret := range []string{"vital_signs", "heart_rate_bpm", "keypoints"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%q leaked in %s", secret, text)
		}
	}
	if !strings.Contains(text, `"presence":true`) || !strings.Contains(text, `"zone":"a"`) {
		t.Fatalf("non-biometric fields lost: %s", text)
	}
}

func TestRedactJSONNormalizesBiometricKeyCase(t *testing.T) {
	got, err := RedactJSON([]byte(`{"Heart_Rate":72,"Respiration_BPM":14,"Presence":true}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(strings.ToLower(text), "heart_rate") || strings.Contains(strings.ToLower(text), "respiration_bpm") {
		t.Fatalf("case-variant biometric fields leaked: %s", text)
	}
	if !strings.Contains(text, `"Presence":true`) {
		t.Fatalf("non-sensitive fields should be preserved: %s", text)
	}
}

func TestPrivacyMiddlewareBoundsBufferedResponse(t *testing.T) {
	handler := PrivacyMiddleware(true, 8, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"long":"this is too large"}`))
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/test", nil))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "privacy_filter_failed") {
		t.Fatalf("oversized response was not withheld: status=%d body=%s", response.Code, response.Body.String())
	}
}
