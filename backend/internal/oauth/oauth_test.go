package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKeyID = "test-p256-key"

func newTestService(t *testing.T) (*Service, *ecdsa.PrivateKey, *httptest.Server) {
	t.Helper()
	return newTestServiceAt(t, time.Now)
}

func newTestServiceAt(t *testing.T, now func() time.Time) (*Service, *ecdsa.PrivateKey, *httptest.Server) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testJWKS(privateKey, testKeyID))
	}))
	t.Cleanup(server.Close)
	service, err := New(context.Background(), Config{
		Issuer: server.URL, PublicBaseURL: "http://127.0.0.1:8080",
		AllowedClientIDs: []string{"ruview"}, SessionSecret: strings.Repeat("s", 32),
		DataDir: t.TempDir(), Now: now,
	})
	if err != nil {
		t.Fatalf("create OAuth service: %v", err)
	}
	return service, privateKey, server
}

func testJWKS(key *ecdsa.PrivateKey, kid string) []byte {
	x, y := key.PublicKey.X.FillBytes(make([]byte, 32)), key.PublicKey.Y.FillBytes(make([]byte, 32))
	doc := map[string]any{"keys": []any{map[string]string{
		"kty": "EC", "crv": "P-256", "kid": kid,
		"x": base64.RawURLEncoding.EncodeToString(x),
		"y": base64.RawURLEncoding.EncodeToString(y),
	}}}
	body, _ := json.Marshal(doc)
	return body
}

func signedToken(t *testing.T, key *ecdsa.PrivateKey, kid, algorithm string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": algorithm, "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	r, sigS, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), sigS.FillBytes(make([]byte, 32))...)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func validClaims(now time.Time) map[string]any {
	typ := accessTokenType
	account := "account-42"
	return map[string]any{
		"typ": typ, "sub": "user-7", "account_id": account,
		"client_id": "ruview", "jti": "token-9", "scope": "sensing:read",
		"exp": now.Add(5 * time.Minute).Unix(),
	}
}

func TestVerifyAccessTokenChecksSignatureAndClaims(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	service, key, _ := newTestServiceAt(t, func() time.Time { return now })
	token := signedToken(t, key, testKeyID, "ES256", validClaims(now))
	principal, err := service.VerifyAccessToken(context.Background(), token, readScope)
	if err != nil {
		t.Fatalf("valid read token rejected: %v", err)
	}
	if principal.Subject != "user-7" || principal.AccountID != "account-42" || !principal.HasScope(readScope) {
		t.Fatalf("unexpected verified principal: %+v", principal)
	}

	wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyAccessToken(context.Background(), signedToken(t, wrongKey, testKeyID, "ES256", validClaims(now)), readScope); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong signing key error=%v, want invalid signature", err)
	}
	if _, err := service.VerifyAccessToken(context.Background(), token, "sensing:admin"); !errors.Is(err, ErrMissingScope) {
		t.Fatalf("read token admin-scope error=%v, want missing scope", err)
	}
}

func TestVerifyAccessTokenRejectsUntrustedClaimVariants(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	service, key, _ := newTestServiceAt(t, func() time.Time { return now })
	tests := []struct {
		name      string
		algorithm string
		mutate    func(map[string]any)
		want      error
	}{
		{name: "wrong algorithm", algorithm: "HS256", want: ErrWrongAlgorithm},
		{name: "wrong token type", algorithm: "ES256", mutate: func(c map[string]any) { c["typ"] = "setup" }, want: ErrWrongTokenType},
		{name: "wrong audience", algorithm: "ES256", mutate: func(c map[string]any) { c["client_id"] = "another-product" }, want: ErrWrongAudience},
		{name: "missing account", algorithm: "ES256", mutate: func(c map[string]any) { delete(c, "account_id") }, want: ErrMissingAccount},
		{name: "empty subject", algorithm: "ES256", mutate: func(c map[string]any) { c["sub"] = "" }, want: ErrMissingAccount},
		{name: "setup credential", algorithm: "ES256", mutate: func(c map[string]any) { c["setup"] = true }, want: ErrLongLivedToken},
		{name: "workload credential", algorithm: "ES256", mutate: func(c map[string]any) { c["workload"] = true }, want: ErrLongLivedToken},
		{name: "expired token", algorithm: "ES256", mutate: func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }, want: ErrExpiredToken},
		{name: "future not-before", algorithm: "ES256", mutate: func(c map[string]any) { c["nbf"] = now.Add(time.Minute).Unix() }, want: ErrExpiredToken},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := validClaims(now)
			if test.mutate != nil {
				test.mutate(claims)
			}
			token := signedToken(t, key, testKeyID, test.algorithm, claims)
			if _, err := service.VerifyAccessToken(context.Background(), token, readScope); !errors.Is(err, test.want) {
				t.Fatalf("verification error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestUnknownKeyIsNotAccepted(t *testing.T) {
	service, key, _ := newTestService(t)
	token := signedToken(t, key, "unpublished-key", "ES256", validClaims(time.Now()))
	if _, err := service.VerifyAccessToken(context.Background(), token, readScope); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key error=%v", err)
	}
}

func TestJWKSCacheUsesFreshKeysAndFallsBackWhileOffline(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var current = time.Unix(1_800_000_000, 0)
	var requests atomic.Int32
	var offline atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if offline.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(testJWKS(key, testKeyID))
	}))
	defer server.Close()
	cache := newKeyCache(server.URL, server.Client(), func() time.Time { return current })
	if err := cache.warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.key(context.Background(), testKeyID); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("fresh-key request count=%d, want 1", got)
	}
	offline.Store(true)
	current = current.Add(jwksTTL + time.Second)
	if _, err := cache.key(context.Background(), testKeyID); err != nil {
		t.Fatalf("stale known key should work offline: %v", err)
	}
	if _, err := cache.key(context.Background(), testKeyID); err != nil {
		t.Fatalf("rate-limited stale key should work: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("offline refresh count=%d, want one bounded attempt", got)
	}
}

func TestPKCETransactionAndSignedBrowserSession(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	service, _, _ := newTestServiceAt(t, func() time.Time { return now })
	location, transactionCookie, err := service.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	state := query.Get("state")
	if query.Get("code_challenge_method") != "S256" || query.Get("scope") != readScope || query.Get("redirect_uri") != service.RedirectURI() {
		t.Fatalf("authorization parameters are incomplete: %v", query)
	}
	verifier, ok := cookieValue(transactionCookie, txnCookieName)
	if !ok {
		t.Fatalf("transaction cookie missing from %q", transactionCookie)
	}
	returnedVerifier, err := service.VerifierForCallback(txnCookieName+"="+verifier, state)
	if err != nil || returnedVerifier == "" {
		t.Fatalf("valid PKCE transaction rejected: verifier=%q err=%v", returnedVerifier, err)
	}
	digest := sha256.Sum256([]byte(returnedVerifier))
	if query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatal("PKCE challenge did not match the server-side verifier")
	}
	if _, err := service.VerifierForCallback(txnCookieName+"="+verifier, state+"x"); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("mismatched state error=%v", err)
	}
	if _, err := service.VerifierForCallback(txnCookieName+"="+verifier+"; "+txnCookieName+"="+verifier, state); !errors.Is(err, ErrInvalidTxn) {
		t.Fatalf("duplicate signed transaction cookie error=%v", err)
	}
	if !strings.Contains(transactionCookie, "; Secure") || !strings.Contains(transactionCookie, "HttpOnly") || !strings.Contains(transactionCookie, "SameSite=Lax") {
		t.Fatalf("transaction cookie security attributes missing: %q", transactionCookie)
	}

	principal := Principal{Subject: "user-7", AccountID: "account-42", Scopes: []string{readScope, "sensing:admin"}, ExpiresAt: now.Add(5 * time.Minute)}
	sessionCookie, err := service.IssueSession(principal, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sessionCookie, "access_token") || !strings.Contains(sessionCookie, "HttpOnly") || !strings.Contains(sessionCookie, "; Secure") {
		t.Fatalf("session cookie contains token or lacks attributes: %q", sessionCookie)
	}
	value, ok := cookieValue(sessionCookie, sessionCookieName)
	if !ok {
		t.Fatal("session cookie value missing")
	}
	session := service.Session(sessionCookieName + "=" + value)
	if session == nil || session.Subject != "user-7" || session.AccountID != "account-42" || session.Scope != readScope || session.HasScope("sensing:admin") {
		t.Fatalf("unexpected browser session: %+v", session)
	}
	if service.Session(sessionCookieName+"="+value+"; "+sessionCookieName+"="+value) != nil {
		t.Fatal("ambiguous duplicate session cookies were accepted")
	}
	now = now.Add(sessionTTL + time.Second)
	if service.Session(sessionCookieName+"="+value) != nil {
		t.Fatal("expired browser session was accepted")
	}
	if !strings.Contains(service.ClearSession(true), "Max-Age=0") {
		t.Fatal("session logout cookie does not expire immediately")
	}
}

func TestBrowserSessionCannotOutliveAccessToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	service, _, _ := newTestServiceAt(t, func() time.Time { return now })
	principal := Principal{
		Subject: "user-7", AccountID: "account-42", Scopes: []string{readScope},
		ExpiresAt: now.Add(10 * time.Minute),
	}
	cookie, err := service.IssueSession(principal, false)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := cookieValue(cookie, sessionCookieName)
	if !ok {
		t.Fatalf("session cookie missing from %q", cookie)
	}
	session := service.Session(sessionCookieName + "=" + value)
	if session == nil || session.ExpiresAt != principal.ExpiresAt.Unix() {
		t.Fatalf("session expiry=%v, want access-token expiry %d", session, principal.ExpiresAt.Unix())
	}
	if strings.Contains(cookie, "Max-Age=3600") {
		t.Fatalf("cookie lifetime was not capped to access-token expiry: %q", cookie)
	}

	now = principal.ExpiresAt.Add(time.Second)
	if service.Session(sessionCookieName+"="+value) != nil {
		t.Fatal("browser session remained valid after its access token expired")
	}
	if _, err := service.IssueSession(Principal{
		Subject: "user-7", AccountID: "account-42", Scopes: []string{readScope},
		ExpiresAt: now.Add(-time.Second),
	}, false); err == nil {
		t.Fatal("session was issued from an expired access token")
	}
}

func TestSessionSecretIsPersistedWithRestrictedPermissions(t *testing.T) {
	dir := t.TempDir()
	secret, err := loadSessionSecret(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) < 32 {
		t.Fatalf("generated secret has only %d bytes", len(secret))
	}
	path := dir + string(os.PathSeparator) + "session-secret"
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("session secret permissions=%o, want 600", info.Mode().Perm())
	}
	second, err := loadSessionSecret(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != string(second) {
		t.Fatal("persisted session secret changed between starts")
	}
	if _, err := loadSessionSecret(dir, "too-short"); err == nil {
		t.Fatal("short configured secret accepted")
	}
}

func cookieValue(header, name string) (string, bool) {
	first, _, _ := strings.Cut(header, ";")
	key, value, ok := strings.Cut(first, "=")
	return value, ok && key == name
}

func TestParseJWKSRejectsInvalidCurvePoints(t *testing.T) {
	bad := fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-256","kid":"bad","x":"%s","y":"%s"}]}`,
		base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if _, err := parseJWKS([]byte(bad)); err == nil {
		t.Fatal("off-curve JWK accepted")
	}
	if _, err := parseJWKS([]byte(`{"keys":[{"kty":"RSA","kid":"rsa"}]}`)); err == nil {
		t.Fatal("JWKS without an ES256 key accepted")
	}
}
