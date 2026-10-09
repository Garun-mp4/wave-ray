package vendor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultPollInterval = time.Minute
	MaxPollInterval     = time.Hour
	MaxPollPages        = 10
	MaxPollCursorBytes  = 512
	MaxPollBody         = MaxPayload
	maxPollErrorBackoff = 5 * time.Minute
)

var (
	ErrPollConfiguration = errors.New("invalid vendor poll configuration")
	ErrPollCredentials   = errors.New("vendor poll credentials are unavailable")
	ErrPollRequest       = errors.New("vendor poll request failed")
	ErrPollResponse      = errors.New("vendor poll response was rejected")
	ErrPollPagination    = errors.New("vendor poll pagination was rejected")
)

// PollSource contains only non-secret settings. Credentials are resolved from
// the named environment variable when a request is prepared and are never
// returned by Status or included in log messages.
type PollSource struct {
	Vendor       ID
	BaseURL      string
	Path         string
	TokenEnv     string
	AuthScheme   string
	Method       string
	Paginated    bool
	OptionalAuth bool
}

type PollConfig struct {
	Interval time.Duration
	Sources  []PollSource
}

type PollStatus struct {
	Vendor      ID         `json:"vendor"`
	Enabled     bool       `json:"enabled"`
	State       string     `json:"state"`
	LastPoll    *time.Time `json:"last_poll,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

type Poller struct {
	config PollConfig
	store  *Store
	getenv func(string) string
	client *http.Client
	logger *slog.Logger

	mu     sync.RWMutex
	status map[ID]PollStatus
}

// PollConfigFromEnv reads optional cloud-provider configuration. An
// integration is enabled only when its target identifiers/URL are supplied;
// its configured credential must then exist in the process environment.
func PollConfigFromEnv(getenv func(string) string) (PollConfig, error) {
	if getenv == nil {
		return PollConfig{}, fmt.Errorf("%w: environment reader is nil", ErrPollConfiguration)
	}
	interval := DefaultPollInterval
	if value := strings.TrimSpace(getenv("RUVIEW_VENDOR_POLL_SECS")); value != "" {
		seconds, err := strconv.ParseUint(value, 10, 32)
		if err != nil || seconds == 0 || time.Duration(seconds)*time.Second > MaxPollInterval {
			return PollConfig{}, fmt.Errorf("%w: RUVIEW_VENDOR_POLL_SECS must be 1..3600", ErrPollConfiguration)
		}
		interval = time.Duration(seconds) * time.Second
	}

	config := PollConfig{Interval: interval}
	add := func(source PollSource) error {
		if err := source.Validate(); err != nil {
			return err
		}
		config.Sources = append(config.Sources, source)
		return nil
	}
	optional := func(key, fallback string) string {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			return value
		}
		return fallback
	}
	requireToken := func(vendorID ID, envName string) error {
		if !validEnvName(envName) {
			return fmt.Errorf("%w: invalid credential environment variable for %s", ErrPollConfiguration, vendorID)
		}
		token := strings.TrimSpace(getenv(envName))
		if !validToken(token) {
			return fmt.Errorf("%w: set %s for %s", ErrPollCredentials, envName, vendorID)
		}
		return nil
	}

	mistSite := strings.TrimSpace(getenv("RUVIEW_MIST_SITE_ID"))
	mistRegion := strings.ToLower(optional("RUVIEW_MIST_REGION", "global_01"))
	if mistSite == "" && strings.TrimSpace(getenv("RUVIEW_MIST_REGION")) != "" {
		return PollConfig{}, fmt.Errorf("%w: RUVIEW_MIST_REGION requires RUVIEW_MIST_SITE_ID", ErrPollConfiguration)
	}
	if mistSite != "" {
		if !validateProviderID(mistSite) {
			return PollConfig{}, fmt.Errorf("%w: invalid RUVIEW_MIST_SITE_ID", ErrPollConfiguration)
		}
		tokenEnv := optional("RUVIEW_MIST_TOKEN_ENV", "MIST_API_TOKEN")
		if err := requireToken(Mist, tokenEnv); err != nil {
			return PollConfig{}, err
		}
		base, ok := mistBaseURL(mistRegion)
		if !ok {
			return PollConfig{}, fmt.Errorf("%w: unsupported Mist region %q", ErrPollConfiguration, mistRegion)
		}
		if err := add(PollSource{Vendor: Mist, BaseURL: base, Path: "/api/v1/sites/" + mistSite + "/stats/clients", TokenEnv: tokenEnv, AuthScheme: "Token", Method: http.MethodGet, Paginated: true}); err != nil {
			return PollConfig{}, err
		}
	}

	netgearLocation := strings.TrimSpace(getenv("RUVIEW_NETGEAR_LOCATION_ID"))
	netgearBase := strings.TrimSpace(getenv("RUVIEW_NETGEAR_BASE_URL"))
	netgearPath := strings.TrimSpace(getenv("RUVIEW_NETGEAR_CLIENTS_PATH"))
	if netgearLocation != "" || netgearBase != "" || netgearPath != "" || strings.TrimSpace(getenv("RUVIEW_NETGEAR_REGION")) != "" {
		if netgearLocation == "" || netgearBase == "" || netgearPath == "" {
			return PollConfig{}, fmt.Errorf("%w: NETGEAR Insight requires LOCATION_ID, BASE_URL and CLIENTS_PATH from the partner API contract", ErrPollConfiguration)
		}
		if !validateProviderID(netgearLocation) {
			return PollConfig{}, fmt.Errorf("%w: invalid RUVIEW_NETGEAR_LOCATION_ID", ErrPollConfiguration)
		}
		if strings.TrimSpace(getenv("RUVIEW_NETGEAR_REGION")) != "" {
			return PollConfig{}, fmt.Errorf("%w: NETGEAR region host is partner-specific; configure RUVIEW_NETGEAR_BASE_URL and RUVIEW_NETGEAR_CLIENTS_PATH", ErrPollConfiguration)
		}
		if !strings.Contains(netgearPath, "{location_id}") {
			return PollConfig{}, fmt.Errorf("%w: RUVIEW_NETGEAR_CLIENTS_PATH must include {location_id}", ErrPollConfiguration)
		}
		netgearPath = strings.ReplaceAll(netgearPath, "{location_id}", url.PathEscape(netgearLocation))
		tokenEnv := optional("RUVIEW_NETGEAR_TOKEN_ENV", "NETGEAR_INSIGHT_TOKEN")
		if err := requireToken(Netgear, tokenEnv); err != nil {
			return PollConfig{}, err
		}
		if err := add(PollSource{Vendor: Netgear, BaseURL: netgearBase, Path: netgearPath, TokenEnv: tokenEnv, Method: http.MethodGet, Paginated: true}); err != nil {
			return PollConfig{}, err
		}
	}

	originBase := strings.TrimSpace(getenv("RUVIEW_ORIGIN_AI_BASE_URL"))
	originPath := strings.TrimSpace(getenv("RUVIEW_ORIGIN_AI_EVENTS_PATH"))
	originTokenEnv := strings.TrimSpace(getenv("RUVIEW_ORIGIN_AI_TOKEN_ENV"))
	if originBase != "" || originPath != "" || originTokenEnv != "" {
		if originBase == "" || originPath == "" {
			return PollConfig{}, fmt.Errorf("%w: Origin AI requires BASE_URL and EVENTS_PATH", ErrPollConfiguration)
		}
		if originTokenEnv == "" {
			originTokenEnv = "ORIGIN_AI_TOKEN"
		}
		if err := requireToken(OriginAI, originTokenEnv); err != nil {
			return PollConfig{}, err
		}
		if err := add(PollSource{Vendor: OriginAI, BaseURL: originBase, Path: originPath, TokenEnv: originTokenEnv, Method: http.MethodGet}); err != nil {
			return PollConfig{}, err
		}
	}

	plumeBase := strings.TrimSpace(getenv("RUVIEW_PLUME_BASE_URL"))
	plumePath := strings.TrimSpace(getenv("RUVIEW_PLUME_EVENTS_PATH"))
	plumeTokenEnv := strings.TrimSpace(getenv("RUVIEW_PLUME_TOKEN_ENV"))
	if plumeBase != "" || plumePath != "" || plumeTokenEnv != "" {
		if plumeBase == "" || plumePath == "" {
			return PollConfig{}, fmt.Errorf("%w: OpenSync requires BASE_URL and EVENTS_PATH", ErrPollConfiguration)
		}
		if plumeTokenEnv != "" && !validEnvName(plumeTokenEnv) {
			return PollConfig{}, fmt.Errorf("%w: invalid OpenSync credential environment variable", ErrPollConfiguration)
		}
		if plumeTokenEnv != "" && !validToken(strings.TrimSpace(getenv(plumeTokenEnv))) {
			return PollConfig{}, fmt.Errorf("%w: set %s for OpenSync", ErrPollCredentials, plumeTokenEnv)
		}
		if err := add(PollSource{Vendor: Plume, BaseURL: plumeBase, Path: plumePath, TokenEnv: plumeTokenEnv, Method: http.MethodGet, OptionalAuth: true}); err != nil {
			return PollConfig{}, err
		}
	}
	return config, nil
}

func mistBaseURL(region string) (string, bool) {
	switch region {
	case "global", "global_01":
		return "https://api.mist.com", true
	case "europe", "eu", "emea_01":
		return "https://api.eu.mist.com", true
	case "global_02":
		return "https://api.gc1.mist.com", true
	case "global_03":
		return "https://api.ac2.mist.com", true
	case "global_04":
		return "https://api.gc2.mist.com", true
	case "global_05":
		return "https://api.gc4.mist.com", true
	case "emea_02":
		return "https://api.gc3.mist.com", true
	case "emea_03":
		return "https://api.ac6.mist.com", true
	case "emea_04":
		return "https://api.gc6.mist.com", true
	case "apac_01":
		return "https://api.ac5.mist.com", true
	case "apac_02":
		return "https://api.gc5.mist.com", true
	case "apac_03":
		return "https://api.gc7.mist.com", true
	default:
		return "", false
	}
}

func (s PollSource) Validate() error {
	if s.Vendor != Mist && s.Vendor != Netgear && s.Vendor != OriginAI && s.Vendor != Plume {
		return fmt.Errorf("%w: polling is not supported for %s", ErrPollConfiguration, s.Vendor)
	}
	if s.Method != http.MethodGet || !safePollPath(s.Path) || !safePollBaseURL(s.BaseURL) || (s.TokenEnv != "" && !validEnvName(s.TokenEnv)) {
		return fmt.Errorf("%w: unsafe URL or method for %s", ErrPollConfiguration, s.Vendor)
	}
	if s.AuthScheme != "" && s.AuthScheme != "Bearer" && s.AuthScheme != "Token" {
		return fmt.Errorf("%w: unsupported authorization scheme for %s", ErrPollConfiguration, s.Vendor)
	}
	if s.Vendor == Mist && s.AuthScheme != "Token" {
		return fmt.Errorf("%w: Mist API tokens must use the Token authorization scheme", ErrPollConfiguration)
	}
	if s.Paginated && s.Vendor != Mist && s.Vendor != Netgear {
		return fmt.Errorf("%w: cursor pagination is unsupported for %s", ErrPollConfiguration, s.Vendor)
	}
	if s.OptionalAuth && s.Vendor != Plume {
		return fmt.Errorf("%w: optional authentication is unsupported for %s", ErrPollConfiguration, s.Vendor)
	}
	if !s.OptionalAuth && !validEnvName(s.TokenEnv) {
		return fmt.Errorf("%w: missing credential reference for %s", ErrPollConfiguration, s.Vendor)
	}
	return nil
}

func safePollBaseURL(value string) bool {
	if len(value) == 0 || len(value) > 2048 || strings.ContainsAny(value, "\r\n#?") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	ip := net.ParseIP(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && ip.IsLoopback())
}

func safePollPath(value string) bool {
	if len(value) == 0 || len(value) > 2048 || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\r\n#?") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

func validEnvName(value string) bool {
	if len(value) == 0 || len(value) > 128 || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, char := range value {
		if !((char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}
	return true
}

func validToken(value string) bool {
	return len(value) > 0 && len(value) <= 4096 && !strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func validateProviderID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func NewPoller(config PollConfig, store *Store, getenv func(string) string, logger *slog.Logger) (*Poller, error) {
	if store == nil || getenv == nil || config.Interval <= 0 || config.Interval > MaxPollInterval {
		return nil, fmt.Errorf("%w: store, environment reader and a bounded interval are required", ErrPollConfiguration)
	}
	status := make(map[ID]PollStatus, len(config.Sources))
	seen := make(map[ID]struct{}, len(config.Sources))
	for _, source := range config.Sources {
		if err := source.Validate(); err != nil {
			return nil, err
		}
		if _, exists := seen[source.Vendor]; exists {
			return nil, fmt.Errorf("%w: duplicate source %s", ErrPollConfiguration, source.Vendor)
		}
		seen[source.Vendor] = struct{}{}
		status[source.Vendor] = PollStatus{Vendor: source.Vendor, Enabled: true, State: "waiting"}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{
		config: config, store: store, getenv: getenv,
		client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		logger: logger, status: status,
	}, nil
}

func (p *Poller) Status() []PollStatus {
	if p == nil {
		return []PollStatus{}
	}
	p.mu.RLock()
	result := make([]PollStatus, 0, len(p.status))
	for _, status := range p.status {
		result = append(result, status)
	}
	p.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].Vendor < result[j].Vendor })
	return result
}

// Run polls each configured provider independently. A failed source backs off
// without delaying healthy providers; context cancellation stops all requests.
func (p *Poller) Run(ctx context.Context) {
	if p == nil || len(p.config.Sources) == 0 {
		return
	}
	var workers sync.WaitGroup
	for _, source := range p.config.Sources {
		source := source
		workers.Add(1)
		go func() {
			defer workers.Done()
			p.runSource(ctx, source)
		}()
	}
	<-ctx.Done()
	workers.Wait()
}

func (p *Poller) runSource(ctx context.Context, source PollSource) {
	backoff := min(5*time.Second, p.config.Interval)
	for ctx.Err() == nil {
		err := p.PollOnce(ctx, source.Vendor)
		wait := p.config.Interval
		if err != nil {
			code := pollErrorCode(err)
			p.logger.Warn("vendor poll failed", "vendor", source.Vendor, "reason", code)
			wait = backoff
			backoff = min(backoff*2, maxPollErrorBackoff)
		} else {
			backoff = min(5*time.Second, p.config.Interval)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PollOnce runs one provider poll. It is exported to support deterministic
// integration tests and administrative diagnostics without exposing secrets.
func (p *Poller) PollOnce(ctx context.Context, id ID) error {
	if p == nil {
		return ErrPollConfiguration
	}
	var source *PollSource
	for i := range p.config.Sources {
		if p.config.Sources[i].Vendor == id {
			source = &p.config.Sources[i]
			break
		}
	}
	if source == nil {
		return ErrUnsupported
	}
	started := time.Now().UTC()
	p.mu.Lock()
	status := p.status[id]
	status.State = "polling"
	status.LastPoll = &started
	status.LastError = ""
	p.status[id] = status
	p.mu.Unlock()

	latest, hasSnapshot, err := p.pollSource(ctx, *source)
	if err == nil {
		if hasSnapshot {
			err = p.store.Put(latest)
		} else {
			p.store.Delete(id)
		}
	}
	finished := time.Now().UTC()
	p.mu.Lock()
	status = p.status[id]
	if err != nil {
		status.State = "error"
		status.LastError = pollErrorCode(err)
	} else {
		status.State = "ready"
		status.LastSuccess = &finished
		status.LastError = ""
	}
	p.status[id] = status
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (p *Poller) pollSource(ctx context.Context, source PollSource) (Event, bool, error) {
	token := ""
	if source.TokenEnv != "" {
		token = strings.TrimSpace(p.getenv(source.TokenEnv))
		if !validToken(token) {
			return Event{}, false, ErrPollCredentials
		}
	} else if !source.OptionalAuth {
		return Event{}, false, ErrPollCredentials
	}

	all := make([]Event, 0, 64)
	cursor := ""
	seenCursors := make(map[string]struct{}, MaxPollPages)
	for page := 0; page < MaxPollPages; page++ {
		payload, err := p.requestPage(ctx, source, token, cursor)
		if err != nil {
			return Event{}, false, err
		}
		var events []Event
		next := ""
		if source.Paginated {
			events, next, err = DecodePage(source.Vendor, payload)
		} else {
			events, err = Decode(source.Vendor, payload)
		}
		if err != nil {
			return Event{}, false, fmt.Errorf("%w: provider payload rejected", ErrPollResponse)
		}
		all = append(all, events...)
		if next == "" {
			break
		}
		if !source.Paginated || len(next) > MaxPollCursorBytes {
			return Event{}, false, ErrPollPagination
		}
		if _, exists := seenCursors[next]; exists {
			return Event{}, false, ErrPollPagination
		}
		seenCursors[next] = struct{}{}
		cursor = next
		if page == MaxPollPages-1 {
			return Event{}, false, ErrPollPagination
		}
	}
	if len(all) == 0 {
		return Event{}, false, nil
	}
	latest := all[0]
	for _, event := range all[1:] {
		if event.TimestampUS > latest.TimestampUS || (event.TimestampUS == latest.TimestampUS && event.Sequence > latest.Sequence) {
			latest = event
		}
	}
	return latest, true, nil
}

func (p *Poller) requestPage(ctx context.Context, source PollSource, token, cursor string) ([]byte, error) {
	endpoint := strings.TrimRight(source.BaseURL, "/") + source.Path
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, ErrPollConfiguration
	}
	if cursor != "" {
		query := u.Query()
		query.Set("cursor", cursor)
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, source.Method, u.String(), nil)
	if err != nil {
		return nil, ErrPollConfiguration
	}
	req.Header.Set("Accept", "application/json")
	if source.Vendor == Mist {
		req.Header.Set("Accept", "application/json, application/vnd.api+json")
	}
	if token != "" {
		scheme := source.AuthScheme
		if scheme == "" {
			scheme = "Bearer"
		}
		req.Header.Set("Authorization", scheme+" "+token)
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: transport error", ErrPollRequest)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, ErrPollResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxPollBody+1))
	if err != nil || len(body) == 0 || len(body) > MaxPollBody {
		return nil, ErrPollResponse
	}
	return body, nil
}

func pollErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrPollCredentials):
		return "credentials_unavailable"
	case errors.Is(err, ErrPollRequest):
		return "request_failed"
	case errors.Is(err, ErrPollPagination):
		return "pagination_rejected"
	case errors.Is(err, ErrPollResponse):
		return "response_rejected"
	default:
		return "provider_unavailable"
	}
}
