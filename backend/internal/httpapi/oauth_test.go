package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ruvnet/ruview/backend/internal/config"
	"github.com/ruvnet/ruview/backend/internal/model"
	"github.com/ruvnet/ruview/backend/internal/oauth"
	"github.com/ruvnet/ruview/backend/internal/recording"
	"github.com/ruvnet/ruview/backend/internal/security"
	"github.com/ruvnet/ruview/backend/internal/state"
	stream "github.com/ruvnet/ruview/backend/internal/ws"
)

func TestBrowserOAuthFlowAuthorizesReadAndScopesWebSocketTickets(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokenCh := make(chan string, 1)
	formCh := make(chan url.Values, 1)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/jwks.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(httpapiTestJWKS(privateKey))
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			formCh <- r.Form
			token := httpapiTestJWT(t, privateKey)
			tokenCh <- token
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token, "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()

	service, err := oauth.New(context.Background(), oauth.Config{
		Issuer: issuer.URL, JWKSURL: issuer.URL + "/.well-known/jwks.json",
		AllowedClientIDs: []string{"ruview"}, PublicBaseURL: "http://127.0.0.1:8080",
		SessionSecret: strings.Repeat("s", 32), DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("initialize OAuth: %v", err)
	}
	apiServer, api := newOAuthTestAPIServer(t, service)
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

	start, err := client.Get(apiServer.URL + "/oauth/start")
	if err != nil {
		t.Fatal(err)
	}
	if start.StatusCode != http.StatusFound {
		t.Fatalf("OAuth start status=%d", start.StatusCode)
	}
	authorizeURL, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	stateValue := authorizeURL.Query().Get("state")
	if stateValue == "" || authorizeURL.Query().Get("code_challenge_method") != "S256" || authorizeURL.Query().Get("scope") != "sensing:read" {
		t.Fatalf("invalid authorization request: %s", start.Header.Get("Location"))
	}
	var txnCookie *http.Cookie
	for _, cookie := range start.Cookies() {
		if cookie.Name == "ruview_oauth_txn" {
			txnCookie = cookie
		}
	}
	start.Body.Close()
	if txnCookie == nil || !txnCookie.HttpOnly || txnCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("transaction cookie missing or lacks security attributes: %+v", txnCookie)
	}

	callbackURL := apiServer.URL + "/oauth/callback?code=one-time-code&state=" + url.QueryEscape(stateValue)
	callback, err := http.NewRequest(http.MethodGet, callbackURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	callback.AddCookie(txnCookie)
	completed, err := client.Do(callback)
	if err != nil {
		t.Fatal(err)
	}
	if completed.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(completed.Body)
		completed.Body.Close()
		t.Fatalf("OAuth callback status=%d body=%s", completed.StatusCode, body)
	}
	if len(completed.Header.Values("Set-Cookie")) != 2 {
		t.Fatalf("callback should issue session and clear transaction cookies: %v", completed.Header.Values("Set-Cookie"))
	}
	var sessionCookie *http.Cookie
	for _, cookie := range completed.Cookies() {
		if cookie.Name == "ruview_session" && cookie.Value != "" {
			sessionCookie = cookie
		}
	}
	completed.Body.Close()
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie missing or insecure: %+v", sessionCookie)
	}
	form := <-formCh
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "one-time-code" || form.Get("client_id") != "ruview" || form.Get("redirect_uri") != service.RedirectURI() || len(form.Get("code_verifier")) < 43 {
		t.Fatalf("token exchange did not use the PKCE authorization-code grant: %v", form)
	}
	token := <-tokenCh

	statusRequest, _ := http.NewRequest(http.MethodGet, apiServer.URL+"/oauth/status", nil)
	statusRequest.AddCookie(sessionCookie)
	status, err := client.Do(statusRequest)
	if err != nil {
		t.Fatal(err)
	}
	var statusBody map[string]any
	if err := json.NewDecoder(status.Body).Decode(&statusBody); err != nil {
		t.Fatal(err)
	}
	status.Body.Close()
	if status.StatusCode != http.StatusOK || statusBody["signed_in"] != true || statusBody["account"] != "account-42" || statusBody["oauth_enabled"] != true {
		t.Fatalf("unexpected OAuth status: status=%d payload=%v", status.StatusCode, statusBody)
	}

	unauthorized, err := client.Get(apiServer.URL + "/api/v1/sensing/latest")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API without credentials status=%d", unauthorized.StatusCode)
	}

	browserRead, _ := http.NewRequest(http.MethodGet, apiServer.URL+"/api/v1/sensing/latest", nil)
	browserRead.AddCookie(sessionCookie)
	readResponse, err := client.Do(browserRead)
	if err != nil {
		t.Fatal(err)
	}
	readResponse.Body.Close()
	if readResponse.StatusCode != http.StatusOK {
		t.Fatalf("browser read status=%d", readResponse.StatusCode)
	}
	staticRead, _ := http.NewRequest(http.MethodGet, apiServer.URL+"/api/v1/sensing/latest", nil)
	staticRead.Header.Set("Authorization", "Bearer legacy-static-token")
	staticResponse, err := client.Do(staticRead)
	if err != nil {
		t.Fatal(err)
	}
	staticResponse.Body.Close()
	if staticResponse.StatusCode != http.StatusOK {
		t.Fatalf("legacy static token stopped working alongside OAuth: status=%d", staticResponse.StatusCode)
	}

	adminRequest, _ := http.NewRequest(http.MethodPost, apiServer.URL+"/api/v1/adaptive/train", strings.NewReader(`{}`))
	adminRequest.AddCookie(sessionCookie)
	adminResponse, err := client.Do(adminRequest)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse.Body.Close()
	if adminResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("browser admin mutation status=%d, want 403", adminResponse.StatusCode)
	}

	bearerRead, _ := http.NewRequest(http.MethodGet, apiServer.URL+"/api/v1/sensing/latest", nil)
	bearerRead.Header.Set("Authorization", "Bearer "+token)
	bearerResponse, err := client.Do(bearerRead)
	if err != nil {
		t.Fatal(err)
	}
	bearerResponse.Body.Close()
	if bearerResponse.StatusCode != http.StatusOK {
		t.Fatalf("OAuth bearer read status=%d", bearerResponse.StatusCode)
	}
	connectWebSocketWithCookie(t, apiServer.URL, sessionCookie)

	ticketRequest, _ := http.NewRequest(http.MethodPost, apiServer.URL+"/api/v1/ws-ticket", nil)
	ticketRequest.AddCookie(sessionCookie)
	ticketResponse, err := client.Do(ticketRequest)
	if err != nil {
		t.Fatal(err)
	}
	var ticketBody struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(ticketResponse.Body).Decode(&ticketBody); err != nil {
		t.Fatal(err)
	}
	ticketResponse.Body.Close()
	if ticketResponse.StatusCode != http.StatusOK || ticketBody.Ticket == "" {
		t.Fatalf("ticket issue status=%d body=%+v", ticketResponse.StatusCode, ticketBody)
	}
	connectWebSocketWithTicket(t, apiServer.URL, ticketBody.Ticket, http.StatusSwitchingProtocols)
	connectWebSocketWithTicket(t, apiServer.URL, ticketBody.Ticket, http.StatusUnauthorized)

	adminScope := "sensing:admin"
	adminTicket, err := api.Tickets.Issue(security.TicketGrant{Scopes: &adminScope, Subject: "admin-only"})
	if err != nil {
		t.Fatal(err)
	}
	connectWebSocketWithTicket(t, apiServer.URL, adminTicket, http.StatusUnauthorized)

	logout, err := client.Get(apiServer.URL + "/oauth/logout")
	if err != nil {
		t.Fatal(err)
	}
	logoutCookies := logout.Header.Values("Set-Cookie")
	logout.Body.Close()
	if logout.StatusCode != http.StatusFound || len(logoutCookies) != 2 || !strings.Contains(strings.Join(logoutCookies, " "), "Max-Age=0") {
		t.Fatalf("logout did not clear both OAuth cookies: status=%d cookies=%v", logout.StatusCode, logoutCookies)
	}
}

func newOAuthTestAPIServer(t *testing.T, service *oauth.Service) (*httptest.Server, *Server) {
	t.Helper()
	hub := stream.NewHub()
	store := state.New("auto", hub)
	hosts, err := security.NewHostAllowlist(nil)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	cfg := config.Config{
		BindAddr: "127.0.0.1", HTTPPort: 8080, WSPort: 8765,
		UDPBindAddr: "127.0.0.1", UDPPort: 5005, Source: "auto",
		UIPath: t.TempDir(), DataDir: dataDir, APIToken: "legacy-static-token",
	}
	models, err := model.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := recording.New(dataDir, hub, false)
	if err != nil {
		t.Fatal(err)
	}
	api := New(cfg, store, security.NewTicketStore(), hosts, models, recorder, nil)
	api.OAuth = service
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server, api
}

func connectWebSocketWithTicket(t *testing.T, httpURL, ticket string, wantStatus int) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(httpURL, "http") + "/ws/sensing?ticket=" + url.QueryEscape(ticket)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL, nil)
	if wantStatus == http.StatusSwitchingProtocols {
		if err != nil {
			t.Fatalf("WebSocket ticket status: %v (response=%v)", err, response)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "test complete")
		return
	}
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "unexpectedly accepted")
		t.Fatalf("WebSocket ticket unexpectedly accepted; response=%v", response)
	}
	if response == nil || response.StatusCode != wantStatus {
		t.Fatalf("WebSocket rejected with response=%v, error=%v; want status %d", response, err, wantStatus)
	}
}

func connectWebSocketWithCookie(t *testing.T, httpURL string, cookie *http.Cookie) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(httpURL, "http") + "/ws/sensing"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}},
	})
	if err != nil {
		t.Fatalf("OAuth browser session WebSocket handshake failed: %v (response=%v)", err, response)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "test complete")
}

func httpapiTestJWKS(key *ecdsa.PrivateKey) []byte {
	x := base64.RawURLEncoding.EncodeToString(key.PublicKey.X.FillBytes(make([]byte, 32)))
	y := base64.RawURLEncoding.EncodeToString(key.PublicKey.Y.FillBytes(make([]byte, 32)))
	body, _ := json.Marshal(map[string]any{"keys": []any{map[string]string{
		"kty": "EC", "crv": "P-256", "kid": "integration-key", "x": x, "y": y,
	}}})
	return body
}

func httpapiTestJWT(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "integration-key", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"typ": "access", "sub": "user-7", "account_id": "account-42", "client_id": "ruview",
		"scope": "sensing:read", "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": "integration-token",
	})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	r, sigS, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Errorf("sign test token: %v", err)
		return ""
	}
	signature := append(r.FillBytes(make([]byte, 32)), sigS.FillBytes(make([]byte, 32))...)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
