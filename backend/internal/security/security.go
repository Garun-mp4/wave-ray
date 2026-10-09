package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	TicketTTL       = 30 * time.Second
	maxTickets      = 512
	maxPerPrincipal = 16
	maxAllowEntries = 4096
)

// UDPAllowlist restricts CSI UDP input by source address. Loopback is always
// accepted; an empty allowlist accepts all sources only after the bind policy
// has explicitly allowed that network exposure.
type UDPAllowlist struct {
	prefixes []netip.Prefix
	mu       sync.Mutex
	dropped  uint64
}

func NewUDPAllowlist(specs []string) (*UDPAllowlist, error) {
	allow := &UDPAllowlist{}
	for _, spec := range specs {
		for _, raw := range strings.Split(spec, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if len(allow.prefixes) >= maxAllowEntries {
				return nil, fmt.Errorf("UDP allowlist exceeds %d entries", maxAllowEntries)
			}
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				addr, addrErr := netip.ParseAddr(raw)
				if addrErr != nil {
					return nil, fmt.Errorf("invalid UDP allowlist entry %q", raw)
				}
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			}
			if prefix.Addr().Is4In6() {
				if prefix.Bits() < 96 {
					return nil, fmt.Errorf("IPv4-mapped UDP allowlist prefix %q must be /96 or narrower", raw)
				}
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			allow.prefixes = append(allow.prefixes, prefix.Masked())
		}
	}
	return allow, nil
}

func (a *UDPAllowlist) Active() bool { return a != nil && len(a.prefixes) > 0 }

func (a *UDPAllowlist) Admit(addr net.IP) bool {
	if addr == nil {
		a.recordDrop()
		return false
	}
	ip, ok := netip.AddrFromSlice(addr)
	if !ok {
		a.recordDrop()
		return false
	}
	return a.AdmitAddr(ip.Unmap())
}

func (a *UDPAllowlist) AdmitAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || a == nil || len(a.prefixes) == 0 {
		return true
	}
	for _, prefix := range a.prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	a.recordDrop()
	return false
}

func (a *UDPAllowlist) recordDrop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.dropped++
	a.mu.Unlock()
}

func (a *UDPAllowlist) Dropped() uint64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dropped
}

// HostAllowlist is a DNS-rebinding defense. Entries without a port match any
// port; entries with a port require an exact host:port match.
type HostAllowlist struct{ entries map[string]struct{} }

func NewHostAllowlist(extras []string) (*HostAllowlist, error) {
	entries := map[string]struct{}{
		"localhost": {}, "127.0.0.1": {}, "[::1]": {},
	}
	for _, item := range extras {
		for _, raw := range strings.Split(item, ",") {
			host := strings.ToLower(strings.TrimSpace(raw))
			if host == "" {
				continue
			}
			if len(host) > 255 || strings.ContainsAny(host, "\r\n\t /\\") {
				return nil, fmt.Errorf("invalid allowed Host entry %q", raw)
			}
			entries[host] = struct{}{}
		}
	}
	return &HostAllowlist{entries: entries}, nil
}

func (a *HostAllowlist) Allows(host string) bool {
	if a == nil || len(a.entries) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if _, ok := a.entries[host]; ok {
		return true
	}
	if bare, _, err := net.SplitHostPort(host); err == nil {
		if _, ok := a.entries[strings.ToLower(bare)]; ok {
			return true
		}
		if strings.Contains(bare, ":") {
			if _, ok := a.entries["["+strings.ToLower(bare)+"]"]; ok {
				return true
			}
		}
	}
	return false
}

func (a *HostAllowlist) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" {
			http.Error(w, "missing Host header", http.StatusBadRequest)
			return
		}
		if !a.Allows(r.Host) {
			http.Error(w, "Host header not in allowlist (DNS-rebinding defense)", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TicketGrant preserves authority granted to the authenticated principal.
// Static API tokens are legacy full-authority credentials and use nil scopes.
type TicketGrant struct {
	Scopes  *string `json:"scopes,omitempty"`
	Subject string  `json:"subject,omitempty"`
}

type ticketEntry struct {
	grant   TicketGrant
	expires time.Time
}

// TicketStore stores only ticket digests, so accidental diagnostics never
// expose a usable bearer credential. Tickets expire after 30 seconds and are
// consumed on the first upgrade attempt.
type TicketStore struct {
	mu      sync.Mutex
	entries map[[32]byte]ticketEntry
	now     func() time.Time
}

func NewTicketStore() *TicketStore {
	return &TicketStore{entries: make(map[[32]byte]ticketEntry), now: time.Now}
}

func (s *TicketStore) Issue(grant TicketGrant) (string, error) {
	if s == nil {
		return "", errors.New("ticket store is unavailable")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate WebSocket ticket: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if len(s.entries) >= maxTickets {
		return "", errors.New("WebSocket ticket capacity reached")
	}
	principalCount := 0
	for _, item := range s.entries {
		if item.grant.Subject == grant.Subject {
			principalCount++
		}
	}
	if principalCount >= maxPerPrincipal {
		return "", errors.New("WebSocket ticket principal capacity reached")
	}
	s.entries[digest] = ticketEntry{grant: grant, expires: s.now().Add(TicketTTL)}
	return token, nil
}

func (s *TicketStore) Consume(token string) (TicketGrant, bool) {
	if s == nil || len(token) != 64 {
		return TicketGrant{}, false
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[digest]
	delete(s.entries, digest)
	if !ok || !s.now().Before(entry.expires) {
		return TicketGrant{}, false
	}
	return entry.grant, true
}

func (s *TicketStore) Outstanding() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.entries)
}

func (s *TicketStore) pruneLocked() {
	now := s.now()
	for digest, entry := range s.entries {
		if !now.Before(entry.expires) {
			delete(s.entries, digest)
		}
	}
}

func RequiredScope(method, path string) string {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return "sensing:read"
	}
	safeMutations := map[string]struct{}{
		"/api/v1/ws-ticket":   {},
		"/api/v1/models/load": {}, "/api/v1/models/unload": {},
		"/api/v1/models/lora/activate": {}, "/api/v1/model/sona/activate": {},
		"/api/v1/adaptive/unload": {}, "/api/v1/calibration/start": {},
		"/api/v1/calibration/stop": {}, "/api/v1/calibration/bootstrap/promote": {},
		"/api/v1/calibration/cancel": {}, "/api/v1/pose/calibrate": {},
		"/api/v1/recording/start": {}, "/api/v1/recording/stop": {},
	}
	if _, ok := safeMutations[path]; ok || (strings.HasPrefix(path, "/api/v1/rf/vendors/") && strings.HasSuffix(path, "/events")) {
		return "sensing:read"
	}
	return "sensing:admin"
}

func EqualToken(expected, provided string) bool {
	return expected != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

func BearerToken(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

var biometricKeys = map[string]struct{}{
	"vital_signs": {}, "breathing_rate_bpm": {}, "heart_rate_bpm": {},
	"heartrate_bpm": {}, "heart_rate": {}, "hr_bpm": {}, "br_bpm": {},
	"respiration_bpm": {}, "breathing_rate": {}, "enhanced_breathing": {},
	"keypoints": {}, "pose_keypoints": {}, "pose": {}, "joints_m": {},
}

func RedactJSON(data []byte, maxBytes int) ([]byte, error) {
	if len(data) > maxBytes {
		return nil, errors.New("response exceeds privacy-filter size limit")
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	redactValue(value)
	return json.Marshal(value)
}

func redactValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, sensitive := biometricKeys[strings.ToLower(key)]; sensitive {
				delete(typed, key)
				continue
			}
			redactValue(child)
		}
	case []any:
		for _, child := range typed {
			redactValue(child)
		}
	}
}

func PrivacyMiddleware(enabled bool, maxBytes int, next http.Handler) http.Handler {
	if !enabled {
		return next
	}
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RuField events are already reduced to P2-safe data before signing.
		// Re-serializing their JSON here can alter signed numeric spellings.
		if r.URL.Path == "/api/field" || !strings.HasPrefix(r.URL.Path, "/api/v1/") ||
			strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
			strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
			strings.HasPrefix(r.URL.Path, "/api/v1/recording/download/") {
			next.ServeHTTP(w, r)
			return
		}
		buffer := &responseBuffer{header: make(http.Header), maxBytes: maxBytes}
		next.ServeHTTP(buffer, r)
		if buffer.overflow {
			writePrivacyFailure(w)
			return
		}
		if !strings.HasPrefix(strings.ToLower(buffer.header.Get("Content-Type")), "application/json") {
			copyResponse(w, buffer)
			return
		}
		body, err := RedactJSON(buffer.body, maxBytes)
		if err != nil {
			writePrivacyFailure(w)
			return
		}
		buffer.body = body
		buffer.header.Del("Content-Length")
		copyResponse(w, buffer)
	})
}

type responseBuffer struct {
	header   http.Header
	status   int
	body     []byte
	maxBytes int
	overflow bool
}

func (b *responseBuffer) Header() http.Header { return b.header }
func (b *responseBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *responseBuffer) Write(data []byte) (int, error) {
	if b.maxBytes > 0 && len(data) > b.maxBytes-len(b.body) {
		b.overflow = true
		return len(data), nil
	}
	b.body = append(b.body, data...)
	return len(data), nil
}

func writePrivacyFailure(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"code":"privacy_filter_failed","detail":"response withheld because privacy mode could not filter it"}`))
}

func copyResponse(w http.ResponseWriter, b *responseBuffer) {
	for key, values := range b.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.body)
}
