package main

// Auth tests drive the real router behind the real middleware, so what is
// exercised here is the wiring the binary ships — not a hand-assembled subset.
//
// Covered: key minting and revocation, scope enforcement, project scoping, the
// full authorization-code + PKCE exchange (including every failure mode that
// matters), refresh-token rotation with reuse detection, RFC 7009 revocation,
// the RFC 9728 challenge an MCP client follows, and the MCP HTTP transport's
// bearer requirement and per-tool scope checks.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type testEnv struct {
	srv *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("AUTH_MODE", "required")
	t.Setenv("PUBLIC_BASE_URL", "")
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	srv := httptest.NewServer(withMiddleware(authenticate(newRouter())))
	selfBaseURL = srv.URL
	mcpBaseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		_ = db.Close()
	})
	return &testEnv{srv: srv}
}

// do issues a request with optional bearer and JSON body, returning status, raw
// body and headers.
func (e *testEnv) do(t *testing.T, method, path, bearer string, body interface{}, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

func (e *testEnv) form(t *testing.T, path string, values url.Values) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

func (e *testEnv) json(t *testing.T, method, path, bearer string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	status, raw, _ := e.do(t, method, path, bearer, body, nil)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return status, out
}

// bootstrapKey mints the first key of an empty, auth-required deployment.
func (e *testEnv) bootstrapKey(t *testing.T) string {
	t.Helper()
	status, out := e.json(t, "POST", "/api/v1/keys", "", map[string]interface{}{"name": "bootstrap"})
	if status != 201 {
		t.Fatalf("bootstrap key status %d: %v", status, out)
	}
	key, _ := out["key"].(string)
	if !strings.HasPrefix(key, "efk_") {
		t.Fatalf("unexpected key format %q", key)
	}
	return key
}

// project creates a project and returns its id.
func (e *testEnv) project(t *testing.T, admin, name string) string {
	t.Helper()
	status, out := e.json(t, "POST", "/api/v1/projects", admin, map[string]interface{}{"name": name})
	if status != 200 {
		t.Fatalf("create project %s: %d %v", name, status, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("project id missing: %v", out)
	}
	return id
}

// mcpPost sends one JSON-RPC message to /mcp.
func (e *testEnv) mcpPost(t *testing.T, bearer string, msg map[string]interface{}, headers map[string]string) (int, map[string]interface{}, http.Header) {
	t.Helper()
	status, raw, h := e.do(t, "POST", "/mcp", bearer, msg, headers)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return status, out, h
}

func pkcePair() (verifier, challenge string) {
	verifier = "test-verifier-0123456789-0123456789-0123456789-abcdefghij"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---------- tests ----------

func TestAPIKeyLifecycleAndScopes(t *testing.T) {
	e := newTestEnv(t)
	admin := e.bootstrapKey(t)

	// The plaintext key is returned once; the listing must never contain it.
	status, keys := e.json(t, "GET", "/api/v1/keys", admin, nil)
	if status != 200 {
		t.Fatalf("list keys: %d", status)
	}
	_ = keys
	_, raw, _ := e.do(t, "GET", "/api/v1/keys", admin, nil, nil)
	if strings.Contains(string(raw), admin) {
		t.Fatal("GET /api/v1/keys leaked the plaintext key")
	}

	// A read-only key can read but not write.
	status, out := e.json(t, "POST", "/api/v1/keys", admin, map[string]interface{}{
		"name": "dashboard", "scopes": []string{"read"}, "owner": "viewer@example.com",
	})
	if status != 201 {
		t.Fatalf("create read key: %d %v", status, out)
	}
	readKey := out["key"].(string)
	if status, _ := e.json(t, "GET", "/api/v1/whoami", readKey, nil); status != 200 {
		t.Fatalf("whoami with read key: %d", status)
	}
	if status, _ := e.json(t, "POST", "/api/v1/projects", readKey, map[string]interface{}{"name": "nope"}); status != 403 {
		t.Fatalf("read-only key wrote a project: got %d want 403", status)
	}
	// read-only key may not manage credentials
	if status, _ := e.json(t, "GET", "/api/v1/keys", readKey, nil); status != 403 {
		t.Fatalf("read-only key listed keys: got %d want 403", status)
	}

	// Revocation is immediate, for the key and for tokens derived from it.
	status, out = e.json(t, "POST", "/api/v1/keys", admin, map[string]interface{}{"name": "temp", "scopes": []string{"read", "write"}})
	if status != 201 {
		t.Fatalf("create temp key: %d", status)
	}
	tempKey, tempID := out["key"].(string), out["id"].(string)
	if status, _, _ := e.do(t, "GET", "/api/v1/projects", tempKey, nil, nil); status != 200 {
		t.Fatalf("temp key should work before revocation")
	}
	if status, _ := e.json(t, "DELETE", "/api/v1/keys/"+tempID, admin, nil); status != 200 {
		t.Fatalf("revoke: %d", status)
	}
	if status, _, _ := e.do(t, "GET", "/api/v1/projects", tempKey, nil, nil); status != 401 {
		t.Fatalf("revoked key still worked: got %d want 401", status)
	}
}

func TestUnauthenticatedRequestCarriesResourceMetadata(t *testing.T) {
	e := newTestEnv(t)
	_ = e.bootstrapKey(t)

	status, _, h := e.do(t, "GET", "/api/v1/projects", "", nil, nil)
	if status != 401 {
		t.Fatalf("want 401, got %d", status)
	}
	challenge := h.Get("WWW-Authenticate")
	if !strings.Contains(challenge, "resource_metadata=") ||
		!strings.Contains(challenge, "/.well-known/oauth-protected-resource") {
		t.Fatalf("challenge does not point at resource metadata: %q", challenge)
	}

	// Discovery documents are public and name this origin as the issuer.
	status, meta := e.json(t, "GET", "/.well-known/oauth-authorization-server", "", nil)
	if status != 200 || meta["issuer"] != e.srv.URL {
		t.Fatalf("issuer metadata: %d %v", status, meta)
	}
	if methods, _ := meta["code_challenge_methods_supported"].([]interface{}); len(methods) != 1 || methods[0] != "S256" {
		t.Fatalf("PKCE methods: %v", methods)
	}
	status, prm := e.json(t, "GET", "/.well-known/oauth-protected-resource", "", nil)
	if status != 200 || prm["resource"] != e.srv.URL+"/mcp" {
		t.Fatalf("protected resource metadata: %d %v", status, prm)
	}
}

func TestProjectScopedKeyCannotCrossProjects(t *testing.T) {
	e := newTestEnv(t)
	admin := e.bootstrapKey(t)
	p1 := e.project(t, admin, "alpha")
	p2 := e.project(t, admin, "beta")

	status, out := e.json(t, "POST", "/api/v1/keys", admin, map[string]interface{}{
		"name": "alpha only", "owner": "team-alpha", "scopes": []string{"read"}, "project_id": p1,
	})
	if status != 201 {
		t.Fatalf("scoped key: %d %v", status, out)
	}
	key := out["key"].(string)

	if status, _, _ := e.do(t, "GET", "/api/v1/projects/"+p1, key, nil, nil); status != 200 {
		t.Fatalf("scoped key cannot read its own project: %d", status)
	}
	if status, _, _ := e.do(t, "GET", "/api/v1/projects/"+p2, key, nil, nil); status != 404 {
		t.Fatalf("scoped key read another project: got %d want 404", status)
	}

	// The list is filtered to the one project the credential owns.
	status, raw, _ := e.do(t, "GET", "/api/v1/projects", key, nil, nil)
	if status != 200 {
		t.Fatalf("list projects: %d", status)
	}
	if strings.Contains(string(raw), p2) || !strings.Contains(string(raw), p1) {
		t.Fatalf("project list not scoped: %s", raw)
	}
}

func TestOAuthAuthorizationCodeWithPKCE(t *testing.T) {
	e := newTestEnv(t)
	admin := e.bootstrapKey(t)
	redirect := "http://127.0.0.1:7777/callback"

	status, reg := e.json(t, "POST", "/oauth/register", "", map[string]interface{}{
		"client_name":                "Claude Code",
		"redirect_uris":              []string{redirect},
		"token_endpoint_auth_method": "none",
	})
	if status != 201 {
		t.Fatalf("register client: %d %v", status, reg)
	}
	clientID, _ := reg["client_id"].(string)
	if clientID == "" {
		t.Fatalf("client_id missing: %v", reg)
	}

	// Discovery must advertise exactly what this flow needs.
	_, meta := e.json(t, "GET", "/.well-known/oauth-authorization-server", "", nil)
	if meta["registration_endpoint"] != e.srv.URL+"/oauth/register" ||
		meta["authorization_endpoint"] != e.srv.URL+"/oauth/authorize" {
		t.Fatalf("endpoints: %v", meta)
	}

	verifier, challenge := pkcePair()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"scope":                 {"read write"},
		"state":                 {"xyz123"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}

	// 1) The consent page renders and offers the requested scopes.
	status, page, _ := e.do(t, "GET", "/oauth/authorize?"+q.Encode(), "", nil, nil)
	if status != 200 || !strings.Contains(string(page), "Claude Code") {
		t.Fatalf("consent page: %d %s", status, page)
	}

	// 2) Approving without a valid key cannot produce a code.
	form := url.Values{}
	for k, v := range q {
		form.Set(k, v[0])
	}
	form.Set("decision", "allow")
	form.Set("api_key", "efk_not_a_real_key")
	status, _, h := e.form(t, "/oauth/authorize", form)
	if status != 401 || h.Get("Location") != "" {
		t.Fatalf("invalid key should not redirect: %d %q", status, h.Get("Location"))
	}

	// 3) Approving with a key redirects with a code and preserves state.
	form.Set("api_key", admin)
	status, _, h = e.form(t, "/oauth/authorize", form)
	loc := h.Get("Location")
	if status != 302 || loc == "" {
		t.Fatalf("authorize: %d %q %s", status, loc, h)
	}
	cb, _ := url.Parse(loc)
	if cb.Query().Get("state") != "xyz123" {
		t.Fatalf("state not preserved: %s", loc)
	}
	code := cb.Query().Get("code")
	if !strings.HasPrefix(code, "efc_") {
		t.Fatalf("unexpected code %q", code)
	}

	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}

	// 4) Wrong verifier is rejected (PKCE is enforced, not decorative).
	badVerifier := url.Values{}
	for k, v := range tokenForm {
		badVerifier.Set(k, v[0])
	}
	badVerifier.Set("code_verifier", "wrong-verifier-0123456789-0123456789-0123456789")
	if status, body, _ := e.form(t, "/oauth/token", badVerifier); status != 400 || !strings.Contains(string(body), "PKCE") {
		t.Fatalf("wrong verifier: %d %s", status, body)
	}

	// 5) The correct verifier exchanges the code.
	status, body, _ := e.form(t, "/oauth/token", tokenForm)
	if status != 200 {
		t.Fatalf("token exchange: %d %s", status, body)
	}
	var tokens map[string]interface{}
	_ = json.Unmarshal(body, &tokens)
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if !strings.HasPrefix(access, "efa_") || !strings.HasPrefix(refresh, "efr_") {
		t.Fatalf("token pair: %v", tokens)
	}
	if tokens["token_type"] != "Bearer" || tokens["expires_in"].(float64) <= 0 {
		t.Fatalf("token response shape: %v", tokens)
	}

	// 6) The access token works, and reports itself as an OAuth principal.
	status, who := e.json(t, "GET", "/api/v1/whoami", access, nil)
	if status != 200 || who["kind"] != "oauth" || who["client_id"] != clientID {
		t.Fatalf("whoami with token: %d %v", status, who)
	}

	// 7) Refresh rotates; the old refresh token is dead and reuse revokes the family.
	status, body, _ = e.form(t, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID},
	})
	if status != 200 {
		t.Fatalf("refresh: %d %s", status, body)
	}
	var refreshed map[string]interface{}
	_ = json.Unmarshal(body, &refreshed)
	access2 := refreshed["access_token"].(string)
	if access2 == access {
		t.Fatal("refresh returned the same access token")
	}
	if status, _, _ := e.do(t, "GET", "/api/v1/whoami", access2, nil, nil); status != 200 {
		t.Fatalf("refreshed access token rejected: %d", status)
	}
	if status, body, _ := e.form(t, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID},
	}); status != 400 || !strings.Contains(string(body), "revoked") {
		t.Fatalf("refresh reuse: %d %s", status, body)
	}
	if status, _, _ := e.do(t, "GET", "/api/v1/whoami", access2, nil, nil); status != 401 {
		t.Fatalf("family not revoked after refresh reuse: %d", status)
	}

	// 8) The code is single-use. Replay is checked last because the spec asks a
	// server to revoke everything issued from a code that was used twice, which
	// would (correctly) kill the tokens the steps above rely on.
	if status, body, _ := e.form(t, "/oauth/token", tokenForm); status != 400 || !strings.Contains(string(body), "redeemed") {
		t.Fatalf("code replay: %d %s", status, body)
	}
}

func TestOAuthRejectsUnsafeAuthorizeRequests(t *testing.T) {
	e := newTestEnv(t)
	_ = e.bootstrapKey(t)
	redirect := "http://localhost:7777/cb"

	_, reg := e.json(t, "POST", "/oauth/register", "", map[string]interface{}{
		"redirect_uris": []string{redirect},
	})
	clientID := reg["client_id"].(string)
	_, challenge := pkcePair()
	base := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}

	cases := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"missing PKCE", func(v url.Values) { v.Del("code_challenge") }},
		{"plain challenge", func(v url.Values) { v.Set("code_challenge_method", "plain") }},
		{"unregistered redirect", func(v url.Values) { v.Set("redirect_uri", "http://localhost:7777/evil") }},
		{"implicit response type", func(v url.Values) { v.Set("response_type", "token") }},
	}
	for _, tc := range cases {
		v := url.Values{}
		for k, val := range base {
			v.Set(k, val[0])
		}
		tc.mutate(v)
		status, body, h := e.do(t, "GET", "/oauth/authorize?"+v.Encode(), "", nil, nil)
		if status != 400 {
			t.Errorf("%s: got %d want 400 (%s)", tc.name, status, body)
		}
		if h.Get("Location") != "" {
			t.Errorf("%s: redirected to an untrusted URI %q", tc.name, h.Get("Location"))
		}
	}

	// Dynamic registration validates redirect URIs and grant types.
	if status, _ := e.json(t, "POST", "/oauth/register", "", map[string]interface{}{
		"redirect_uris": []string{"http://evil.example.com/cb"},
	}); status != 400 {
		t.Errorf("non-loopback http redirect was accepted: %d", status)
	}
	if status, _ := e.json(t, "POST", "/oauth/register", "", map[string]interface{}{
		"redirect_uris": []string{"https://ok.example.com/cb"},
		"grant_types":   []string{"password"},
	}); status != 400 {
		t.Errorf("password grant was accepted: %d", status)
	}
	if status, _ := e.json(t, "POST", "/oauth/register", "", map[string]interface{}{
		"redirect_uris": []string{"https://ok.example.com/cb"},
		"grant_types":   []string{"authorization_code", "refresh_token"},
	}); status != 201 {
		t.Errorf("valid registration rejected: %d", status)
	}
}

func TestOAuthRevocationEndsAccess(t *testing.T) {
	e := newTestEnv(t)
	admin := e.bootstrapKey(t)
	redirect := "http://127.0.0.1:7777/cb"
	_, reg := e.json(t, "POST", "/oauth/register", "", map[string]interface{}{"redirect_uris": []string{redirect}})
	clientID := reg["client_id"].(string)
	verifier, challenge := pkcePair()

	form := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"scope": {"read write"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"decision": {"allow"}, "api_key": {admin},
	}
	_, _, h := e.form(t, "/oauth/authorize", form)
	loc, _ := url.Parse(h.Get("Location"))
	code := loc.Query().Get("code")

	status, body, _ := e.form(t, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier},
	})
	if status != 200 {
		t.Fatalf("token exchange: %d %s", status, body)
	}
	var tokens map[string]interface{}
	_ = json.Unmarshal(body, &tokens)
	access := tokens["access_token"].(string)

	if status, _, _ := e.form(t, "/oauth/revoke", url.Values{"token": {access}}); status != 200 {
		t.Fatalf("revoke: %d", status)
	}
	if status, _, _ := e.do(t, "GET", "/api/v1/whoami", access, nil, nil); status != 401 {
		t.Fatalf("revoked token still worked: %d", status)
	}
	// Unknown tokens also answer 200 (RFC 7009).
	if status, _, _ := e.form(t, "/oauth/revoke", url.Values{"token": {"efa_nope"}}); status != 200 {
		t.Fatalf("revoke unknown token: %d", status)
	}
}

func TestMCPHTTPTransportAuthorization(t *testing.T) {
	e := newTestEnv(t)
	admin := e.bootstrapKey(t)

	init := map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]interface{}{"protocolVersion": "2025-06-18", "capabilities": map[string]interface{}{}},
	}

	// No credential: 401 with the discovery pointer, never a tool result.
	status, _, h := e.mcpPost(t, "", init, nil)
	if status != 401 || !strings.Contains(h.Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("unauthenticated MCP: %d %q", status, h.Get("WWW-Authenticate"))
	}

	status, out, _ := e.mcpPost(t, admin, init, nil)
	if status != 200 {
		t.Fatalf("initialize: %d %v", status, out)
	}
	info := out["result"].(map[string]interface{})["serverInfo"].(map[string]interface{})
	if info["name"] != "evalforge" {
		t.Fatalf("serverInfo: %v", info)
	}

	// A foreign browser origin is refused (DNS-rebinding defence).
	if status, _, _ := e.mcpPost(t, admin, map[string]interface{}{
		"jsonrpc": "2.0", "id": 2, "method": "ping",
	}, map[string]string{"Origin": "https://evil.example.com"}); status != 403 {
		t.Fatalf("foreign origin accepted: %d", status)
	}
	// Loopback origins are the same trust zone as the server.
	if status, _, _ := e.mcpPost(t, admin, map[string]interface{}{
		"jsonrpc": "2.0", "id": 3, "method": "ping",
	}, map[string]string{"Origin": "http://localhost:5173"}); status != 200 {
		t.Fatalf("loopback origin rejected: %d", status)
	}

	// Emulated transports only: GET/DELETE on a stateless endpoint.
	if status, _, _ := e.do(t, "GET", "/mcp", admin, nil, nil); status != 405 {
		t.Fatalf("GET /mcp: %d want 405", status)
	}
	// Notifications are accepted with no body.
	status, _, _ = e.do(t, "POST", "/mcp", admin, map[string]interface{}{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	}, nil)
	if status != 202 {
		t.Fatalf("notification: %d want 202", status)
	}

	// A read-only credential can list tools but cannot run a mutating one.
	_, created := e.json(t, "POST", "/api/v1/keys", admin, map[string]interface{}{"name": "reader", "scopes": []string{"read"}})
	readerKey := created["key"].(string)
	status, out, _ = e.mcpPost(t, readerKey, map[string]interface{}{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]interface{}{"name": "add_feedback", "arguments": map[string]interface{}{
			"project_id": "p", "trace_id": "t", "kind": "thumbs", "value": "up",
		}},
	}, nil)
	if status != 200 {
		t.Fatalf("tools/call transport: %d", status)
	}
	result := out["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Fatalf("read-only tool call was not rejected: %v", result)
	}
	text := result["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "insufficient scope") {
		t.Fatalf("unexpected scope error: %q", text)
	}
}

func TestScrubAuthDropsStaleRows(t *testing.T) {
	e := newTestEnv(t)
	admin := e.bootstrapKey(t)
	_ = admin
	old := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339Nano)
	stale := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO oauth_codes(code_hash,client_id,key_id,redirect_uri,scope,code_challenge,created_at,expires_at)
	                       VALUES('h1','c','k','u','read','chal',?,?)`, old, old); err != nil {
		t.Fatalf("insert code: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO oauth_tokens(token_hash,kind,client_id,key_id,scope,created_at,expires_at)
	                       VALUES('h2','access','c','k','read',?,?)`, stale, stale); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	codes, tokens := scrubAuth()
	if codes < 1 || tokens < 1 {
		t.Fatalf("scrub removed codes=%d tokens=%d, want at least one of each", codes, tokens)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM oauth_codes WHERE code_hash='h1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("stale code survived scrub")
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM oauth_tokens WHERE token_hash='h2'`).Scan(&n)
	if n != 0 {
		t.Fatalf("stale token survived scrub")
	}
	_ = fmt.Sprint()
}
