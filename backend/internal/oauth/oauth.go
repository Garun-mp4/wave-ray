// Package oauth implements RuView's Cognitum access-token verification and
// server-side browser sign-in. It intentionally uses only the Go standard
// library so runtime dependencies remain inside the backend container.
package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultClientID   = "ruview"
	readScope         = "sensing:read"
	accessTokenType   = "access"
	clockLeeway       = 30 * time.Second
	jwksTTL           = 5 * time.Minute
	fetchMinInterval  = 30 * time.Second
	fetchTimeout      = 3 * time.Second
	transactionTTL    = 10 * time.Minute
	sessionTTL        = time.Hour
	maxDocumentBytes  = 1 << 20
	maxTokenBytes     = 16 << 10
	txnCookieName     = "ruview_oauth_txn"
	sessionCookieName = "ruview_session"
)

var (
	ErrMalformedToken  = errors.New("malformed OAuth access token")
	ErrWrongAlgorithm  = errors.New("OAuth token algorithm is not ES256")
	ErrBadSignature    = errors.New("OAuth token signature is invalid")
	ErrExpiredToken    = errors.New("OAuth token is expired or not yet valid")
	ErrWrongTokenType  = errors.New("OAuth token is not an interactive access token")
	ErrWrongAudience   = errors.New("OAuth token was minted for an unapproved client")
	ErrLongLivedToken  = errors.New("long-lived setup and workload tokens are not accepted")
	ErrMissingAccount  = errors.New("OAuth token has no account_id")
	ErrMissingScope    = errors.New("OAuth token does not carry the required scope")
	ErrUnknownKey      = errors.New("OAuth token signing key is not available")
	ErrJWKSUnavailable = errors.New("OAuth verification keys are unavailable")
	ErrInvalidTxn      = errors.New("OAuth sign-in transaction is missing or expired")
	ErrStateMismatch   = errors.New("OAuth state did not match the sign-in transaction")
)

// Config is supplied from the process configuration. SessionSecret is an
// operator-provided secret when set; otherwise it is generated and persisted
// under DataDir with owner-only permissions.
type Config struct {
	Issuer           string
	JWKSURL          string
	AllowedClientIDs []string
	PublicBaseURL    string
	SessionSecret    string
	DataDir          string
	HTTPClient       *http.Client
	Now              func() time.Time
}

type Service struct {
	issuer         string
	clientID       string
	allowedIDs     map[string]struct{}
	allowAnyClient bool
	redirect       string
	secret         []byte
	keys           *keyCache
	client         *http.Client
	now            func() time.Time
}

// Principal is constructed only after ES256 signature and claim validation.
type Principal struct {
	Subject   string
	AccountID string
	ClientID  string
	TokenID   string
	Scopes    []string
	ExpiresAt time.Time
}

func (p Principal) HasScope(want string) bool {
	for _, scope := range p.Scopes {
		if scope == want {
			return true
		}
	}
	return false
}

// BrowserSession is a short-lived signed assertion. The access token itself is
// never stored in the browser cookie.
type BrowserSession struct {
	Subject   string `json:"subject"`
	AccountID string `json:"account_id"`
	Scope     string `json:"scope"`
	ExpiresAt int64  `json:"exp"`
	AuthTime  int64  `json:"auth_time"`
}

func (s BrowserSession) HasScope(want string) bool {
	for _, scope := range strings.Fields(s.Scope) {
		if scope == want {
			return true
		}
	}
	return false
}

// New validates OAuth URLs, loads a persistent cookie-signing secret, and
// fetches JWKS once before returning. A configured OAuth server therefore
// fails during startup if it cannot establish a verification key set.
func New(ctx context.Context, cfg Config) (*Service, error) {
	issuer, err := validateBaseURL(cfg.Issuer, "OAuth issuer")
	if err != nil {
		return nil, err
	}
	if err := requireSecureUpstream(issuer, "OAuth issuer"); err != nil {
		return nil, err
	}
	publicBase := strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	if publicBase == "" {
		publicBase = "http://127.0.0.1:8080"
	}
	publicBase, err = validateBaseURL(publicBase, "OAuth public base URL")
	if err != nil {
		return nil, err
	}
	jwksURL := strings.TrimSpace(cfg.JWKSURL)
	if jwksURL == "" {
		jwksURL = issuer + "/.well-known/jwks.json"
	}
	jwksURL, err = validateBaseURL(jwksURL, "OAuth JWKS URL")
	if err != nil {
		return nil, err
	}
	if err := requireSecureUpstream(jwksURL, "OAuth JWKS URL"); err != nil {
		return nil, err
	}

	secret, err := loadSessionSecret(cfg.DataDir, cfg.SessionSecret)
	if err != nil {
		return nil, fmt.Errorf("initialize OAuth session secret: %w", err)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: fetchTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	ids := cfg.AllowedClientIDs
	if len(ids) == 0 {
		ids = []string{defaultClientID}
	}
	allowAny := false
	allowed := make(map[string]struct{}, len(ids))
	clientID := defaultClientID
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "*" {
			allowAny = true
			break
		}
		if id != "" {
			if len(allowed) == 0 {
				clientID = id
			}
			allowed[id] = struct{}{}
		}
	}
	if !allowAny && len(allowed) == 0 {
		allowed[defaultClientID] = struct{}{}
	}
	if allowAny {
		clientID = defaultClientID
	}
	var now func() time.Time
	if cfg.Now == nil {
		now = time.Now
	} else {
		now = cfg.Now
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cache := newKeyCache(jwksURL, client, now)
	if err := cache.warm(ctx); err != nil {
		return nil, fmt.Errorf("warm OAuth JWKS cache: %w", err)
	}
	return &Service{
		issuer: issuer, clientID: clientID, allowedIDs: allowed, allowAnyClient: allowAny,
		redirect: publicBase + "/oauth/callback", secret: secret,
		keys: cache, client: client, now: now,
	}, nil
}

func validateBaseURL(value, name string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("%s must be an absolute HTTP(S) URL without credentials, query, or fragment", name)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func requireSecureUpstream(value, name string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid %s", name)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("%s must use HTTPS unless it is a loopback development endpoint", name)
}

func loadSessionSecret(dataDir, configured string) ([]byte, error) {
	if strings.TrimSpace(configured) != "" {
		secret := []byte(configured)
		if len(secret) < 32 {
			return nil, errors.New("RUVIEW_SESSION_SECRET must contain at least 32 bytes")
		}
		return secret, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory is required when RUVIEW_SESSION_SECRET is unset")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "session-secret")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4096 {
			return nil, errors.New("session-secret must be a regular file smaller than 4 KiB")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("restrict session-secret permissions: %w", err)
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		secret := []byte(strings.TrimSpace(string(value)))
		if len(secret) < 32 {
			return nil, errors.New("persisted session-secret must contain at least 32 bytes")
		}
		return secret, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate session secret: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(random)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadSessionSecret(dataDir, "")
	}
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(f, value+"\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return []byte(value), nil
}

func (s *Service) Issuer() string          { return s.issuer }
func (s *Service) PrimaryClientID() string { return s.clientID }
func (s *Service) RedirectURI() string     { return s.redirect }
func (s *Service) Configured() bool        { return s != nil && len(s.secret) >= 32 }

// VerifyAccessToken verifies signature, interactive token type, client
// audience, expiry, revocation-incompatible credential flags, account
// attribution, and the route-required exact scope.
func (s *Service) VerifyAccessToken(ctx context.Context, token, requiredScope string) (Principal, error) {
	if s == nil || len(token) == 0 || len(token) > maxTokenBytes {
		return Principal{}, ErrMalformedToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Principal{}, ErrMalformedToken
	}
	headerBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return Principal{}, ErrMalformedToken
	}
	var header struct {
		Algorithm     string   `json:"alg"`
		KeyID         string   `json:"kid"`
		Critical      []string `json:"crit"`
		Base64Payload *bool    `json:"b64"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Principal{}, ErrMalformedToken
	}
	if header.Algorithm != "ES256" {
		return Principal{}, ErrWrongAlgorithm
	}
	if len(header.Critical) != 0 || (header.Base64Payload != nil && !*header.Base64Payload) {
		return Principal{}, ErrMalformedToken
	}
	if header.KeyID == "" || len(header.KeyID) > 256 {
		return Principal{}, ErrMalformedToken
	}
	key, err := s.keys.key(ctx, header.KeyID)
	if err != nil {
		return Principal{}, err
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		return Principal{}, ErrMalformedToken
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	sigS := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(key, hash[:], r, sigS) {
		return Principal{}, ErrBadSignature
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(payload) > maxDocumentBytes {
		return Principal{}, ErrMalformedToken
	}
	var claims accessClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Principal{}, ErrMalformedToken
	}
	if claims.ExpiresAt == 0 {
		return Principal{}, ErrMalformedToken
	}
	now := s.now()
	if now.After(time.Unix(claims.ExpiresAt, 0).Add(clockLeeway)) || (claims.NotBefore != nil && now.Add(clockLeeway).Before(time.Unix(*claims.NotBefore, 0))) {
		return Principal{}, ErrExpiredToken
	}
	if claims.Type == nil || *claims.Type != accessTokenType {
		return Principal{}, ErrWrongTokenType
	}
	if !s.allowAnyClient {
		if _, ok := s.allowedIDs[claims.ClientID]; !ok {
			return Principal{}, ErrWrongAudience
		}
	}
	if claims.Setup || claims.Workload {
		return Principal{}, ErrLongLivedToken
	}
	if claims.Subject == "" || claims.AccountID == nil || strings.TrimSpace(*claims.AccountID) == "" {
		return Principal{}, ErrMissingAccount
	}
	scopes := strings.Fields(claims.Scope)
	principal := Principal{
		Subject: claims.Subject, AccountID: *claims.AccountID,
		ClientID: claims.ClientID, TokenID: claims.TokenID,
		Scopes: scopes, ExpiresAt: time.Unix(claims.ExpiresAt, 0),
	}
	if !principal.HasScope(requiredScope) {
		return Principal{}, fmt.Errorf("%w: %s", ErrMissingScope, requiredScope)
	}
	return principal, nil
}

type accessClaims struct {
	Type      *string `json:"typ"`
	Subject   string  `json:"sub"`
	AccountID *string `json:"account_id"`
	ClientID  string  `json:"client_id"`
	TokenID   string  `json:"jti"`
	Scope     string  `json:"scope"`
	ExpiresAt int64   `json:"exp"`
	NotBefore *int64  `json:"nbf"`
	Setup     bool    `json:"setup"`
	Workload  bool    `json:"workload"`
}

type jwkDocument struct {
	Keys []jwk `json:"keys"`
}
type jwk struct {
	KeyType string `json:"kty"`
	Curve   string `json:"crv"`
	KeyID   string `json:"kid"`
	X       string `json:"x"`
	Y       string `json:"y"`
}

type keyCache struct {
	url         string
	client      *http.Client
	now         func() time.Time
	mu          sync.Mutex
	keys        map[string]*ecdsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	lastForced  time.Time
}

func newKeyCache(endpoint string, client *http.Client, now func() time.Time) *keyCache {
	return &keyCache{url: endpoint, client: client, now: now, keys: make(map[string]*ecdsa.PublicKey)}
}

func (c *keyCache) warm(ctx context.Context) error {
	keys, err := c.fetch(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.keys, c.fetchedAt = keys, c.now()
	c.mu.Unlock()
	return nil
}

func (c *keyCache) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	now := c.now()
	c.mu.Lock()
	fresh := !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < jwksTTL
	cached, exists := c.keys[kid]
	if fresh && exists {
		c.mu.Unlock()
		return cached, nil
	}
	haveAny := len(c.keys) > 0
	mayForce := c.lastForced.IsZero() || now.Sub(c.lastForced) >= fetchMinInterval
	mayAttempt := c.lastAttempt.IsZero() || now.Sub(c.lastAttempt) >= fetchMinInterval
	if fresh && !mayForce {
		c.mu.Unlock()
		return nil, ErrUnknownKey
	}
	if !fresh && !mayAttempt {
		c.mu.Unlock()
		if exists {
			return cached, nil
		}
		if haveAny {
			return nil, ErrUnknownKey
		}
		return nil, ErrJWKSUnavailable
	}
	if fresh {
		c.lastForced = now
	}
	c.lastAttempt = now
	c.mu.Unlock()

	fetched, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.keys, c.fetchedAt = fetched, c.now()
	} else if !haveAny {
		return nil, fmt.Errorf("%w: %v", ErrJWKSUnavailable, err)
	}
	if key := c.keys[kid]; key != nil {
		return key, nil
	}
	return nil, ErrUnknownKey
}

func (c *keyCache) fetch(ctx context.Context) (map[string]*ecdsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	if len(body) > maxDocumentBytes {
		return nil, errors.New("JWKS response exceeds 1 MiB")
	}
	return parseJWKS(body)
}

func parseJWKS(body []byte) (map[string]*ecdsa.PublicKey, error) {
	var document jwkDocument
	if len(body) > maxDocumentBytes {
		return nil, errors.New("JWKS response exceeds 1 MiB")
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("malformed JWKS: %w", err)
	}
	keys := make(map[string]*ecdsa.PublicKey)
	for _, item := range document.Keys {
		if item.KeyType != "EC" || item.Curve != "P-256" || item.KeyID == "" || len(item.KeyID) > 256 {
			continue
		}
		x, xErr := base64.RawURLEncoding.Strict().DecodeString(item.X)
		y, yErr := base64.RawURLEncoding.Strict().DecodeString(item.Y)
		if xErr != nil || yErr != nil || len(x) != 32 || len(y) != 32 {
			continue
		}
		publicKey := &ecdsa.PublicKey{Curve: ellipticP256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !publicKey.Curve.IsOnCurve(publicKey.X, publicKey.Y) {
			continue
		}
		keys[item.KeyID] = publicKey
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS contains no usable ES256 keys")
	}
	return keys, nil
}

// ellipticP256 is kept behind a helper so the parsing and verification paths
// share the exact standard-library curve instance.
func ellipticP256() elliptic.Curve { return elliptic.P256() }

type transaction struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Expires  int64  `json:"exp"`
}

// Begin returns the authorization URL and the signed, short-lived transaction
// cookie. PKCE S256 keeps the verifier server-side throughout the redirect.
func (s *Service) Begin(secure bool) (string, string, error) {
	stateBytes := make([]byte, 32)
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", err
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	data, err := json.Marshal(transaction{State: state, Verifier: verifier, Expires: s.now().Add(transactionTTL).Unix()})
	if err != nil {
		return "", "", err
	}
	authorize, err := url.Parse(s.issuer + "/oauth/authorize")
	if err != nil {
		return "", "", err
	}
	query := authorize.Query()
	query.Set("response_type", "code")
	query.Set("client_id", s.clientID)
	query.Set("redirect_uri", s.redirect)
	query.Set("scope", readScope)
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	authorize.RawQuery = query.Encode()
	return authorize.String(), setCookie(txnCookieName, s.sign(data), int(transactionTTL.Seconds()), secure), nil
}

func (s *Service) VerifierForCallback(cookieHeader, state string) (string, error) {
	data, ok := s.unsignUnambiguous(cookieHeader, txnCookieName)
	if !ok {
		return "", ErrInvalidTxn
	}
	var txn transaction
	if err := json.Unmarshal(data, &txn); err != nil || txn.State == "" || txn.Verifier == "" || txn.Expires < s.now().Unix() {
		return "", ErrInvalidTxn
	}
	if subtle.ConstantTimeCompare([]byte(txn.State), []byte(state)) != 1 {
		return "", ErrStateMismatch
	}
	return txn.Verifier, nil
}

func (s *Service) ExchangeCode(ctx context.Context, code, verifier string) (string, error) {
	if code == "" || verifier == "" || len(code) > 4096 {
		return "", errors.New("invalid OAuth callback parameters")
	}
	endpoint := s.issuer + "/oauth/token"
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {verifier}, "client_id": {s.clientID},
		"redirect_uri": {s.redirect},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("OAuth token exchange failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDocumentBytes+1))
	if err != nil {
		return "", fmt.Errorf("read OAuth token response: %w", err)
	}
	if len(body) > maxDocumentBytes {
		return "", errors.New("OAuth token response exceeds 1 MiB")
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OAuth token endpoint returned HTTP %d", response.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.AccessToken == "" {
		return "", errors.New("OAuth token endpoint returned no access_token")
	}
	return result.AccessToken, nil
}

func (s *Service) IssueSession(principal Principal, secure bool) (string, error) {
	if principal.Subject == "" || principal.AccountID == "" || !principal.HasScope(readScope) {
		return "", errors.New("principal cannot receive a browser read session")
	}
	now := s.now()
	if principal.ExpiresAt.IsZero() || !principal.ExpiresAt.After(now) {
		return "", errors.New("principal cannot receive a session after its access token expires")
	}
	expiresAt := now.Add(sessionTTL)
	if principal.ExpiresAt.Before(expiresAt) {
		expiresAt = principal.ExpiresAt
	}
	session := BrowserSession{
		Subject: principal.Subject, AccountID: principal.AccountID,
		Scope: readScope, ExpiresAt: expiresAt.Unix(), AuthTime: now.Unix(),
	}
	data, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	maxAge := int(expiresAt.Sub(now).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	return setCookie(sessionCookieName, s.sign(data), maxAge, secure), nil
}

func (s *Service) Session(cookieHeader string) *BrowserSession {
	data, ok := s.unsignUnambiguous(cookieHeader, sessionCookieName)
	if !ok {
		return nil
	}
	var session BrowserSession
	if json.Unmarshal(data, &session) != nil || session.Subject == "" || session.AccountID == "" || session.ExpiresAt <= s.now().Unix() || !session.HasScope(readScope) {
		return nil
	}
	return &session
}

func (s *Service) ClearTransaction(secure bool) string {
	return setCookie(txnCookieName, "", 0, secure)
}
func (s *Service) ClearSession(secure bool) string {
	return setCookie(sessionCookieName, "", 0, secure)
}

func setCookie(name, value string, maxAge int, secure bool) string {
	result := fmt.Sprintf("%s=%s; Path=/; Max-Age=%d; HttpOnly; SameSite=Lax", name, value, maxAge)
	if secure {
		result += "; Secure"
	}
	return result
}

func (s *Service) sign(payload []byte) string {
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Service) unsign(value string) ([]byte, bool) {
	if len(value) > 16<<10 {
		return nil, false
	}
	separator := strings.LastIndexByte(value, '.')
	if separator <= 0 || separator == len(value)-1 {
		return nil, false
	}
	body, tag := value[:separator], value[separator+1:]
	provided, err := base64.RawURLEncoding.Strict().DecodeString(tag)
	if err != nil || len(provided) != sha256.Size {
		return nil, false
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(body))
	if subtle.ConstantTimeCompare(mac.Sum(nil), provided) != 1 {
		return nil, false
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(body)
	return data, err == nil
}

func (s *Service) unsignUnambiguous(header, name string) ([]byte, bool) {
	var result []byte
	valid := 0
	for _, value := range cookieValues(header, name) {
		if data, ok := s.unsign(value); ok {
			result, valid = data, valid+1
		}
	}
	return result, valid == 1
}

func cookieValues(header, name string) []string {
	var values []string
	for _, part := range strings.Split(header, ";") {
		key, value, found := strings.Cut(part, "=")
		if found && strings.TrimSpace(key) == name {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}
