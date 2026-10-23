package main

// Authentication and authorization.
//
// Two credentials, one principal:
//
//	API key      long-lived, minted by an admin, sent as `Authorization: Bearer`
//	             or `X-API-Key`. This is the human/CI credential.
//	OAuth token  short-lived, issued by the authorization-code + PKCE flow, so a
//	             remote MCP client (Claude Code, Cursor, VS Code) never holds a
//	             long-lived secret and the user can revoke it centrally.
//
// The OAuth server is a deliberately small RFC 6749/7636/7591/7009/8414/9728
// implementation, because the clients we care about are MCP hosts and they all
// require exactly this profile: public client, authorization code, PKCE S256,
// dynamic registration, no implicit grant, no password grant.
//
// Multi-user shape: every credential carries an owner label and an optional
// project scope. Enforcement lives in one middleware, so a credential can only
// ever see the project it was minted for.
//
// Enable enforcement with AUTH_MODE=required. The default (`off`) keeps the
// local single-user developer loop unchanged; auth endpoints still work, which
// lets `scripts/auth_test.py` exercise the real flows on a private server.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	authSchema = `
CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  owner TEXT NOT NULL DEFAULT '',
  scopes TEXT NOT NULL DEFAULT 'read,write',
  project_id TEXT,
  created_at TEXT NOT NULL,
  last_used_at TEXT,
  expires_at TEXT,
  revoked_at TEXT
);
CREATE TABLE IF NOT EXISTS oauth_clients (
  client_id TEXT PRIMARY KEY,
  client_secret_hash TEXT,
  client_name TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT NOT NULL,
  grant_types TEXT NOT NULL DEFAULT 'authorization_code',
  scope TEXT,
  token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_codes (
  code_hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  project_id TEXT,
  redirect_uri TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  code_challenge TEXT NOT NULL,
  code_challenge_method TEXT NOT NULL DEFAULT 'S256',
  resource TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at TEXT
);
CREATE TABLE IF NOT EXISTS oauth_tokens (
  token_hash TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  client_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  project_id TEXT,
  scope TEXT NOT NULL DEFAULT '',
  resource TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revoked_at TEXT,
  rotated_from TEXT
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_key ON oauth_tokens(key_id);
`

	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	authCodeTTL     = 5 * time.Minute
)

// authEnabled is true when the deployment requires credentials.
func authEnabled() bool {
	return strings.EqualFold(os.Getenv("AUTH_MODE"), "required")
}

// issuer is the public origin used in discovery documents and token audiences.
// PUBLIC_BASE_URL exists because a proxied deployment is not reachable at the
// address the process listens on.
func issuer() string {
	if v := strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"); v != "" {
		return v
	}
	return selfBaseURL
}

// ---------- principal ----------

type principal struct {
	Kind      string // "api_key" or "oauth"
	KeyID     string // the api_keys row this credential derives from
	Owner     string
	ProjectID string
	Scopes    map[string]bool
	ClientID  string
	TokenHash string
}

func (p *principal) can(scope string) bool {
	if p == nil {
		return true // auth disabled: no credential, no restriction
	}
	if p.Scopes["admin"] {
		return true
	}
	return p.Scopes[scope]
}

func (p *principal) hasAdmin() bool { return p != nil && p.Scopes["admin"] }

type principalKeyType string

const principalKey principalKeyType = "principal"

func withPrincipal(ctx context.Context, p *principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey).(*principal)
	return p
}

// ---------- random / hashing helpers ----------

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func constEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func splitScopes(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func joinScopes(m map[string]bool) string {
	order := []string{"read", "write", "admin"}
	out := []string{}
	for _, k := range order {
		if m[k] {
			out = append(out, k)
		}
	}
	for k := range m {
		if k != "read" && k != "write" && k != "admin" {
			out = append(out, k)
		}
	}
	return strings.Join(out, " ")
}

func validScopes(raw []string) (map[string]bool, error) {
	m := map[string]bool{}
	for _, s := range raw {
		for _, part := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
			if part == "" {
				continue
			}
			if part != "read" && part != "write" && part != "admin" {
				return nil, fmt.Errorf("unknown scope %q (supported: read, write, admin)", part)
			}
			m[part] = true
		}
	}
	if len(m) == 0 {
		m["read"] = true
		m["write"] = true
	}
	return m, nil
}

// ---------- API keys ----------

type apiKey struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Owner      string   `json:"owner"`
	Scopes     []string `json:"scopes"`
	ProjectID  string   `json:"project_id,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
}

func insertAPIKey(name, owner string, scopes map[string]bool, projectID string, ttl time.Duration) (string, apiKey, error) {
	raw := "efk_" + randToken(32)
	prefix := raw[:12]
	id := "key_" + randToken(9)
	expires := ""
	if ttl > 0 {
		expires = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	sc := joinScopes(scopes)
	_, err := db.Exec(
		`INSERT INTO api_keys(id,name,prefix,key_hash,owner,scopes,project_id,created_at,expires_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		id, name, prefix, hashToken(raw), owner, sc, nullIfEmpty(projectID), nowUTC(), nullIfEmpty(expires))
	if err != nil {
		return "", apiKey{}, err
	}
	return raw, apiKey{
		ID: id, Name: name, Prefix: prefix, Owner: owner,
		Scopes: strings.Fields(sc), ProjectID: projectID,
		CreatedAt: nowUTC(), ExpiresAt: expires,
	}, nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// lookupAPIKey resolves a bearer/API-key string to a principal. The hash is the
// primary key lookup, so a leaked database still does not yield usable keys.
func lookupAPIKey(raw string) (*principal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty credential")
	}
	var (
		id, owner, scopes, createdAt  string
		projectID, expiresAt, revoked sql.NullString
	)
	err := db.QueryRow(
		`SELECT id,owner,scopes,project_id,created_at,expires_at,revoked_at
		   FROM api_keys WHERE key_hash=?`, hashToken(raw)).
		Scan(&id, &owner, &scopes, &projectID, &createdAt, &expiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("unknown API key")
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid && revoked.String != "" {
		return nil, errors.New("API key revoked")
	}
	if expiresAt.Valid && expiresAt.String != "" && time.Now().UTC().After(parseTime(expiresAt.String)) {
		return nil, errors.New("API key expired")
	}
	// Throttled last-used stamp: one write per minute per key, not per request.
	_, _ = db.Exec(
		`UPDATE api_keys SET last_used_at=? WHERE id=? AND (last_used_at IS NULL OR last_used_at < ?)`,
		nowUTC(), id, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
	return &principal{
		Kind: "api_key", KeyID: id, Owner: owner,
		ProjectID: projectID.String, Scopes: splitScopes(scopes),
	}, nil
}

func revokeAPIKey(id string) error {
	_, err := db.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, nowUTC(), id)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE oauth_tokens SET revoked_at=? WHERE key_id=? AND revoked_at IS NULL`, nowUTC(), id)
	return err
}

// ---------- OAuth access tokens ----------

func lookupAccessToken(raw string) (*principal, error) {
	var (
		keyID, owner, scopes, clientID, expiresAt string
		projectID, revoked                        sql.NullString
	)
	h := hashToken(raw)
	err := db.QueryRow(
		`SELECT key_id,owner,scope,client_id,expires_at,project_id,revoked_at
		   FROM oauth_tokens WHERE token_hash=? AND kind='access'`, h).
		Scan(&keyID, &owner, &scopes, &clientID, &expiresAt, &projectID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("unknown access token")
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid && revoked.String != "" {
		return nil, errors.New("access token revoked")
	}
	if time.Now().UTC().After(parseTime(expiresAt)) {
		return nil, errors.New("access token expired")
	}
	// A key revoked after the token was issued must also kill the token.
	if _, err := lookupAPIKeyByID(keyID); err != nil {
		return nil, err
	}
	return &principal{
		Kind: "oauth", KeyID: keyID, Owner: owner, ProjectID: projectID.String,
		Scopes: splitScopes(scopes), ClientID: clientID, TokenHash: h,
	}, nil
}

func lookupAPIKeyByID(id string) (string, error) {
	var owner string
	var revoked sql.NullString
	err := db.QueryRow(`SELECT owner,revoked_at FROM api_keys WHERE id=?`, id).Scan(&owner, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("credential no longer exists")
	}
	if err != nil {
		return "", err
	}
	if revoked.Valid && revoked.String != "" {
		return "", errors.New("credential revoked")
	}
	return owner, nil
}

func issueTokenPair(clientID, keyID, owner, projectID, scope, resource, rotatedFrom string) (map[string]interface{}, error) {
	access := "efa_" + randToken(32)
	refresh := "efr_" + randToken(40)
	now := time.Now().UTC()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ins := `INSERT INTO oauth_tokens(token_hash,kind,client_id,key_id,owner,project_id,scope,resource,created_at,expires_at,rotated_from)
	        VALUES(?,?,?,?,?,?,?,?,?,?,?)`
	if _, err := tx.Exec(ins, hashToken(access), "access", clientID, keyID, owner,
		nullIfEmpty(projectID), scope, nullIfEmpty(resource), now.Format(time.RFC3339Nano),
		now.Add(accessTokenTTL).Format(time.RFC3339Nano), nullIfEmpty(rotatedFrom)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ins, hashToken(refresh), "refresh", clientID, keyID, owner,
		nullIfEmpty(projectID), scope, nullIfEmpty(resource), now.Format(time.RFC3339Nano),
		now.Add(refreshTokenTTL).Format(time.RFC3339Nano), nullIfEmpty(rotatedFrom)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"access_token": access, "token_type": "Bearer",
		"expires_in": int(accessTokenTTL.Seconds()), "refresh_token": refresh,
		"scope": scope,
	}, nil
}

// ---------- middleware ----------

// publicPath is exempt from credential checks: liveness, discovery and the
// endpoints that *produce* credentials cannot require credentials.
func publicPath(path string) bool {
	switch {
	case path == "/health":
		return true
	case strings.HasPrefix(path, "/.well-known/"):
		return true
	case strings.HasPrefix(path, "/oauth/"):
		return true
	}
	return false
}

// requiredScope maps a request to the scope it needs.
func requiredScope(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/api/v1/keys") {
		return "admin"
	}
	// The MCP channel itself only needs read: POST /mcp is the envelope for
	// every JSON-RPC message, and mcpDispatch decides per tool whether a write
	// scope is required. Demanding write here would make read-only agents
	// unable to call any read tool.
	if strings.HasPrefix(r.URL.Path, "/mcp") {
		return "read"
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return "read"
	}
	return "write"
}

// pathProjectID extracts {pid} from /api/v1/projects/{pid}/...
func pathProjectID(path string) string {
	rest, ok := strings.CutPrefix(path, "/api/v1/projects/")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

func bearerToken(r *http.Request) string {
	if v := r.Header.Get("X-API-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	if after, ok := strings.CutPrefix(h, "bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// authenticate is the single gate every route passes through. It never
// distinguishes "unknown key" from "revoked key" in the response, so it cannot
// be used to enumerate credentials.
func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		var (
			p   *principal
			err error
		)
		if tok := bearerToken(r); tok != "" {
			if strings.HasPrefix(tok, "efa_") {
				p, err = lookupAccessToken(tok)
			} else {
				p, err = lookupAPIKey(tok)
			}
		}

		if err != nil {
			challengeRequest(w, r, err.Error())
			return
		}
		if p == nil {
			if !authEnabled() || bootstrapWindow(r) {
				next.ServeHTTP(w, r)
				return
			}
			challengeRequest(w, r, "authentication required")
			return
		}

		if scope := requiredScope(r); !p.can(scope) {
			drainBody(r)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "insufficient scope: " + scope + " required",
			})
			return
		}
		// Project-scoped credentials cannot address another project. Answer 404
		// rather than 403 so the response does not confirm the project exists.
		if p.ProjectID != "" {
			if pid := pathProjectID(r.URL.Path); pid != "" && pid != p.ProjectID {
				errJSON(w, http.StatusNotFound, "project not found")
				return
			}
		}
		setAuditOwner(r.Context(), p.Owner)
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// bootstrapWindow lets the very first key be minted on a fresh, auth-required
// deployment. Without it every request would need a credential that does not
// exist yet. It applies only to key creation while zero keys are registered.
func bootstrapWindow(r *http.Request) bool {
	if r.Method != http.MethodPost || strings.TrimRight(r.URL.Path, "/") != "/api/v1/keys" {
		return false
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		return false
	}
	if n == 0 {
		logf("bootstrap allowed: creating the first API key without credentials")
		return true
	}
	return false
}

// challengeRequest emits 401 with the RFC 9728 pointer that MCP clients follow
// to discover the authorization server.
// drainBody consumes a request body that a rejection path is not going to read.
// Leaving bytes unread makes net/http close the socket with RST, and some
// clients (Python on Windows, for one) then report a connection reset instead
// of the 401/403 we just wrote.
func drainBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
}

func challengeRequest(w http.ResponseWriter, r *http.Request, reason string) {
	drainBody(r)
	w.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Bearer realm="evalforge", resource_metadata="%s/.well-known/oauth-protected-resource"`, issuer()))
	if strings.HasPrefix(r.URL.Path, "/mcp") || strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	errJSON(w, http.StatusUnauthorized, reason)
}

// ---------- whoami ----------

func handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		writeJSON(w, 200, map[string]interface{}{
			"authenticated": false, "auth_mode": "off",
			"note": "AUTH_MODE is not 'required'; every endpoint is open on this deployment.",
		})
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"authenticated": true, "kind": p.Kind, "owner": p.Owner,
		"project_id": p.ProjectID, "scopes": strings.Fields(joinScopes(p.Scopes)),
		"client_id": p.ClientID,
	})
}

// ---------- API key endpoints ----------

func handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query(`SELECT id,name,prefix,owner,scopes,project_id,created_at,last_used_at,expires_at,revoked_at
		                         FROM api_keys ORDER BY created_at DESC`)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []apiKey{}
		for rows.Next() {
			var k apiKey
			var scopes string
			var project, used, expires, revoked sql.NullString
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Owner, &scopes, &project,
				&k.CreatedAt, &used, &expires, &revoked); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			k.Scopes = strings.Fields(scopes)
			k.ProjectID, k.LastUsedAt, k.ExpiresAt, k.RevokedAt = project.String, used.String, expires.String, revoked.String
			out = append(out, k)
		}
		writeJSON(w, 200, out)

	case http.MethodPost:
		var body struct {
			Name      string   `json:"name"`
			Owner     string   `json:"owner"`
			Scopes    []string `json:"scopes"`
			ProjectID string   `json:"project_id"`
			ExpiresIn int      `json:"expires_in_days"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			errJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			errJSON(w, 400, "name is required")
			return
		}
		scopes, err := validScopes(body.Scopes)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		// A bootstrap key must be able to administer the deployment, otherwise
		// the first key could lock everybody out of key management.
		if principalFrom(r.Context()) == nil && authEnabled() {
			scopes["admin"] = true
		}
		if body.ProjectID != "" {
			var exists int
			if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE id=?`, body.ProjectID).Scan(&exists); err != nil || exists == 0 {
				errJSON(w, 400, "project not found: "+body.ProjectID)
				return
			}
		}
		ttl := time.Duration(body.ExpiresIn) * 24 * time.Hour
		raw, key, err := insertAPIKey(body.Name, body.Owner, scopes, body.ProjectID, ttl)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		logf("api key created", "key_id", key.ID, "owner", key.Owner, "scopes", strings.Join(key.Scopes, ","))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 201, map[string]interface{}{
			"id": key.ID, "name": key.Name, "owner": key.Owner, "scopes": key.Scopes,
			"project_id": key.ProjectID, "created_at": key.CreatedAt, "expires_at": key.ExpiresAt,
			// The plaintext key is returned exactly once and never stored.
			"key": raw, "prefix": key.Prefix,
			"note": "store this now; only a hash is kept server-side",
		})

	case http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/keys/")
		if id == "" || id == r.URL.Path {
			errJSON(w, 400, "key id required: DELETE /api/v1/keys/{id}")
			return
		}
		if err := revokeAPIKey(id); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		logf("api key revoked", "key_id", id)
		writeJSON(w, 200, map[string]string{"ok": "revoked", "id": id})
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// ---------- discovery ----------

func handleOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	base := issuer()
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, map[string]interface{}{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"revocation_endpoint":                   base + "/oauth/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"read", "write", "admin"},
		"service_documentation":                 base + "/docs",
	})
}

func handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := issuer()
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, map[string]interface{}{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{"read", "write"},
		"bearer_methods_supported": []string{"header"},
		"resource_documentation":   base + "/docs",
	})
}

// ---------- dynamic client registration (RFC 7591) ----------

type oauthClient struct {
	ClientID                string
	ClientName              string
	RedirectURIs            []string
	TokenEndpointAuthMethod string
	Scope                   string
}

func handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "method not allowed")
		return
	}
	var body struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		Scope                   string   `json:"scope"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON: "+err.Error())
		return
	}
	if len(body.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, u := range body.RedirectURIs {
		if !validRedirectURI(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect_uri must be https, or http on loopback: "+u)
			return
		}
	}
	for _, g := range body.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata",
				"unsupported grant_type "+g+"; only authorization_code and refresh_token are allowed")
			return
		}
	}
	method := body.TokenEndpointAuthMethod
	if method == "" {
		method = "none"
	}
	switch method {
	case "none", "client_secret_post", "client_secret_basic":
	default:
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata",
			"unsupported token_endpoint_auth_method "+method)
		return
	}
	scopes := body.Scope
	if strings.TrimSpace(scopes) == "" {
		scopes = "read write"
	}

	clientID := "efc_" + randToken(16)
	secret := ""
	var secretHash interface{}
	if method != "none" {
		secret = "efs_" + randToken(32)
		secretHash = hashToken(secret)
	}
	urisJSON, _ := json.Marshal(body.RedirectURIs)
	name := body.ClientName
	if name == "" {
		name = "MCP client"
	}
	_, err := db.Exec(`INSERT INTO oauth_clients(client_id,client_secret_hash,client_name,redirect_uris,grant_types,scope,token_endpoint_auth_method,created_at)
	                   VALUES(?,?,?,?,?,?,?,?)`,
		clientID, secretHash, name, string(urisJSON), "authorization_code,refresh_token", scopes, method, nowUTC())
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	logf("oauth client registered", "client_id", clientID, "client_name", name, "auth_method", method)

	resp := map[string]interface{}{
		"client_id":                  clientID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              body.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      scopes,
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, resp)
}

func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

func loadOAuthClient(id string) (*oauthClient, error) {
	var c oauthClient
	var uris string
	err := db.QueryRow(`SELECT client_id,client_name,redirect_uris,scope,token_endpoint_auth_method
	                      FROM oauth_clients WHERE client_id=?`, id).
		Scan(&c.ClientID, &c.ClientName, &uris, &c.Scope, &c.TokenEndpointAuthMethod)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("unknown client_id")
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(uris), &c.RedirectURIs)
	return &c, nil
}

func (c *oauthClient) allowsRedirect(uri string) bool {
	for _, u := range c.RedirectURIs {
		if u == uri {
			return true
		}
	}
	return false
}

// ---------- authorization endpoint ----------

type authorizeParams struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
}

func parseAuthorizeParams(r *http.Request) authorizeParams {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		q = r.PostForm
	}
	return authorizeParams{
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		ResponseType:        q.Get("response_type"),
		Scope:               q.Get("scope"),
		State:               q.Get("state"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		Resource:            q.Get("resource"),
	}
}

// validateAuthorize enforces the OAuth 2.1 profile MCP relies on: code flow,
// PKCE with S256, exact redirect match. Errors that happen *before* the
// redirect target is trusted are rendered, never redirected (RFC 6749 §4.1.2.1).
func (p authorizeParams) validate() (*oauthClient, error) {
	if p.ClientID == "" {
		return nil, errors.New("client_id is required")
	}
	client, err := loadOAuthClient(p.ClientID)
	if err != nil {
		return nil, err
	}
	if p.RedirectURI == "" || !client.allowsRedirect(p.RedirectURI) {
		return nil, errors.New("redirect_uri is not registered for this client")
	}
	if p.ResponseType != "code" {
		return nil, fmt.Errorf("response_type must be code (got %q)", p.ResponseType)
	}
	if p.CodeChallenge == "" {
		return nil, errors.New("code_challenge is required: public clients must use PKCE")
	}
	if p.CodeChallengeMethod != "" && p.CodeChallengeMethod != "S256" {
		return nil, fmt.Errorf("code_challenge_method must be S256 (got %q)", p.CodeChallengeMethod)
	}
	if !validPKCEValue(p.CodeChallenge) {
		return nil, errors.New("code_challenge must be 43-128 characters of base64url")
	}
	return client, nil
}

// pkceMatches verifies code_verifier against code_challenge per RFC 7636 §4.6:
// BASE64URL(SHA256(ASCII(verifier))) compared in constant time. Hashing the
// verifier with the token hash function would be a different, always-failing
// comparison — this helper exists so that mistake cannot be repeated.
func pkceMatches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return constEqual(expected, challenge)
}

func validPKCEValue(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Authorize · EvalForge</title>
<style>
 :root{color-scheme:dark}
 body{background:#0d1117;color:#e6edf3;font:15px/1.55 -apple-system,Segoe UI,Roboto,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0}
 .card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:28px 30px;width:min(480px,92vw)}
 h1{font-size:18px;margin:0 0 4px} .sub{color:#8b949e;font-size:13px;margin-bottom:18px}
 code{background:#0d1117;border:1px solid #30363d;border-radius:6px;padding:1px 6px;font-size:12.5px}
 label{display:block;font-size:12.5px;color:#8b949e;margin:14px 0 6px}
 input[type=password],input[type=text]{width:100%;box-sizing:border-box;background:#0d1117;border:1px solid #30363d;color:#e6edf3;border-radius:8px;padding:10px 12px;font-size:14px}
 .row{display:flex;gap:10px;margin-top:18px}
 button{flex:1;border-radius:8px;padding:10px 14px;font-size:14px;font-weight:600;cursor:pointer;border:1px solid #30363d}
 .allow{background:#238636;border-color:#2ea043;color:#fff}
 .deny{background:#21262d;color:#e6edf3}
 .err{color:#ff7b72;font-size:13px;margin-top:12px}
 ul{margin:8px 0 0;padding-left:18px;color:#c9d1d9;font-size:13px}
</style></head><body>
<div class="card">
  <h1>Authorize {{.ClientName}}</h1>
  <div class="sub">wants to act as you on <strong>EvalForge</strong> · {{.Issuer}}</div>
  {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
  <div style="font-size:13px;color:#8b949e">Requested scopes</div>
  <ul>{{range .Scopes}}<li><code>{{.}}</code></li>{{end}}</ul>
  <div style="font-size:12px;color:#8b949e;margin-top:12px">Redirects to <code>{{.RedirectURI}}</code></div>
  <form method="post" action="/oauth/authorize">
    {{range .Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}
    <label for="api_key">Paste an API key to approve this client</label>
    <input id="api_key" name="api_key" type="password" autocomplete="off" spellcheck="false" placeholder="efk_…" autofocus>
    <div class="row">
      <button class="deny" name="decision" value="deny" formnovalidate>Cancel</button>
      <button class="allow" name="decision" value="allow">Authorize</button>
    </div>
  </form>
  <div class="sub" style="margin:16px 0 0">The client gets short-lived tokens bound to that key's owner and scopes. Revoke the key and every token dies with it.</div>
</div></body></html>`))

func renderConsent(w http.ResponseWriter, status int, p authorizeParams, client *oauthClient, msg string) {
	hidden := []map[string]string{
		{"Name": "client_id", "Value": p.ClientID},
		{"Name": "redirect_uri", "Value": p.RedirectURI},
		{"Name": "response_type", "Value": p.ResponseType},
		{"Name": "state", "Value": p.State},
		{"Name": "code_challenge", "Value": p.CodeChallenge},
		{"Name": "code_challenge_method", "Value": p.CodeChallengeMethod},
		{"Name": "scope", "Value": p.Scope},
		{"Name": "resource", "Value": p.Resource},
	}
	scopes := strings.Fields(p.Scope)
	if len(scopes) == 0 {
		scopes = strings.Fields(client.Scope)
	}
	if len(scopes) == 0 {
		scopes = []string{"read", "write"}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.WriteHeader(status)
	_ = consentTemplate.Execute(w, map[string]interface{}{
		"ClientName": client.ClientName, "Issuer": issuer(), "RedirectURI": p.RedirectURI,
		"Scopes": scopes, "Hidden": hidden, "Error": msg,
	})
}

func handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	p := parseAuthorizeParams(r)
	client, err := p.validate()
	if err != nil {
		// Client/redirect are untrusted here: show the error, never redirect.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "invalid authorization request: %s\n", err)
		return
	}

	if r.Method == http.MethodGet {
		renderConsent(w, http.StatusOK, p, client, "")
		return
	}

	// ---- POST: the consent decision ----
	if r.PostForm.Get("decision") == "deny" {
		redirectWithError(w, r, p, "access_denied", "the user denied the request")
		return
	}
	rawKey := strings.TrimSpace(r.PostForm.Get("api_key"))
	keyPrincipal, keyErr := lookupAPIKey(rawKey)
	if keyErr != nil {
		renderConsent(w, http.StatusUnauthorized, p, client, "That API key was not accepted: "+keyErr.Error())
		return
	}
	if keyPrincipal.ProjectID != "" && p.Resource != "" && !strings.HasPrefix(p.Resource, issuer()) {
		renderConsent(w, http.StatusBadRequest, p, client, "This key is project-scoped and cannot be used for that resource.")
		return
	}

	// Grant at most what the client asked for and the key actually has.
	granted := map[string]bool{}
	requested := strings.Fields(p.Scope)
	if len(requested) == 0 {
		requested = strings.Fields(client.Scope)
	}
	allowed, err := validScopes(requested)
	if err != nil {
		redirectWithError(w, r, p, "invalid_scope", err.Error())
		return
	}
	for s := range allowed {
		if keyPrincipal.can(s) {
			granted[s] = true
		}
	}
	if len(granted) == 0 {
		redirectWithError(w, r, p, "invalid_scope", "the API key does not grant any of the requested scopes")
		return
	}
	scope := joinScopes(granted)

	code := "efc_" + randToken(32)
	resource := p.Resource
	if resource == "" {
		resource = issuer() + "/mcp"
	}
	_, err = db.Exec(`INSERT INTO oauth_codes(code_hash,client_id,key_id,owner,project_id,redirect_uri,scope,
	                   code_challenge,code_challenge_method,resource,created_at,expires_at)
	                  VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		hashToken(code), client.ClientID, keyPrincipal.KeyID, keyPrincipal.Owner,
		nullIfEmpty(keyPrincipal.ProjectID), p.RedirectURI, scope,
		p.CodeChallenge, "S256", resource, nowUTC(),
		time.Now().UTC().Add(authCodeTTL).Format(time.RFC3339Nano))
	if err != nil {
		redirectWithError(w, r, p, "server_error", err.Error())
		return
	}
	logf("oauth code issued", "client_id", client.ClientID, "owner", keyPrincipal.Owner, "scope", scope)

	target, _ := url.Parse(p.RedirectURI)
	q := target.Query()
	q.Set("code", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	target.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func redirectWithError(w http.ResponseWriter, r *http.Request, p authorizeParams, code, desc string) {
	target, err := url.Parse(p.RedirectURI)
	if err != nil {
		errJSON(w, http.StatusBadRequest, desc)
		return
	}
	q := target.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if p.State != "" {
		q.Set("state", p.State)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// ---------- token endpoint ----------

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// clientFromRequest authenticates a confidential client. Public clients send
// nothing, which is why PKCE is mandatory rather than optional here.
func clientFromRequest(r *http.Request, clientID string) (*oauthClient, error) {
	c, err := loadOAuthClient(clientID)
	if err != nil {
		return nil, err
	}
	if c.TokenEndpointAuthMethod == "none" {
		return c, nil
	}
	secret := r.PostForm.Get("client_secret")
	if secret == "" {
		if user, pass, ok := r.BasicAuth(); ok && user == clientID {
			secret = pass
		}
	}
	var stored sql.NullString
	if err := db.QueryRow(`SELECT client_secret_hash FROM oauth_clients WHERE client_id=?`, clientID).Scan(&stored); err != nil {
		return nil, err
	}
	if !stored.Valid || !constEqual(hashToken(secret), stored.String) {
		return nil, errors.New("invalid client credentials")
	}
	return c, nil
}

func handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	grant := r.PostForm.Get("grant_type")
	switch grant {
	case "authorization_code":
		handleAuthCodeGrant(w, r)
	case "refresh_token":
		handleRefreshGrant(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			fmt.Sprintf("grant_type %q is not supported", grant))
	}
}

func handleAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	clientID := r.PostForm.Get("client_id")
	redirectURI := r.PostForm.Get("redirect_uri")
	verifier := r.PostForm.Get("code_verifier")
	if code == "" || clientID == "" || verifier == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request",
			"code, client_id and code_verifier are required")
		return
	}
	if !validPKCEValue(verifier) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code_verifier must be 43-128 base64url characters")
		return
	}
	if _, err := clientFromRequest(r, clientID); err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}

	var (
		keyID, owner, storedRedirect, scope, challenge, method, expiresAt string
		resource                                                          sql.NullString
		projectID, used                                                   sql.NullString
	)
	err := db.QueryRow(`SELECT key_id,owner,redirect_uri,scope,code_challenge,code_challenge_method,
	                           resource,project_id,expires_at,used_at
	                      FROM oauth_codes WHERE code_hash=? AND client_id=?`, hashToken(code), clientID).
		Scan(&keyID, &owner, &storedRedirect, &scope, &challenge, &method, &resource, &projectID, &expiresAt, &used)
	if errors.Is(err, sql.ErrNoRows) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown or already used authorization code")
		return
	}
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if used.Valid && used.String != "" {
		// A replayed code means the code leaked: kill everything it produced.
		revokeTokensForClientKey(clientID, keyID)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code has already been redeemed")
		return
	}
	if time.Now().UTC().After(parseTime(expiresAt)) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code expired")
		return
	}
	if redirectURI != "" && redirectURI != storedRedirect {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if method != "S256" || !pkceMatches(verifier, challenge) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	// Single use: the conditional update is the lock, so two racing exchanges
	// cannot both succeed.
	res, err := db.Exec(`UPDATE oauth_codes SET used_at=? WHERE code_hash=? AND used_at IS NULL`, nowUTC(), hashToken(code))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code has already been redeemed")
		return
	}

	pair, err := issueTokenPair(clientID, keyID, owner, projectID.String, scope, resource.String, "")
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	logf("oauth token issued", "client_id", clientID, "owner", owner)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, pair)
}

func handleRefreshGrant(w http.ResponseWriter, r *http.Request) {
	refresh := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")
	if refresh == "" || clientID == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token and client_id are required")
		return
	}
	if _, err := clientFromRequest(r, clientID); err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}
	var (
		keyID, owner, scope, expiresAt string
		projectID, resource, revoked   sql.NullString
	)
	err := db.QueryRow(`SELECT key_id,owner,scope,resource,project_id,expires_at,revoked_at
	                      FROM oauth_tokens WHERE token_hash=? AND kind='refresh' AND client_id=?`,
		hashToken(refresh), clientID).
		Scan(&keyID, &owner, &scope, &resource, &projectID, &expiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if revoked.Valid && revoked.String != "" {
		// Reuse of a rotated refresh token: the family is compromised.
		revokeTokensForClientKey(clientID, keyID)
		logf("refresh token reuse detected; revoked family", "client_id", clientID, "key_id", keyID)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token already used; all tokens for this client were revoked")
		return
	}
	if time.Now().UTC().After(parseTime(expiresAt)) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token expired")
		return
	}
	if _, err := lookupAPIKeyByID(keyID); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	res, err := db.Exec(`UPDATE oauth_tokens SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL`,
		nowUTC(), hashToken(refresh))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token already redeemed")
		return
	}
	pair, err := issueTokenPair(clientID, keyID, owner, projectID.String, scope, resource.String, hashToken(refresh))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, pair)
}

func revokeTokensForClientKey(clientID, keyID string) {
	_, _ = db.Exec(`UPDATE oauth_tokens SET revoked_at=? WHERE client_id=? AND key_id=? AND revoked_at IS NULL`,
		nowUTC(), clientID, keyID)
}

// ---------- revocation (RFC 7009) ----------

func handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	h := hashToken(token)
	var clientID, keyID, kind string
	err := db.QueryRow(`SELECT client_id,key_id,kind FROM oauth_tokens WHERE token_hash=?`, h).
		Scan(&clientID, &keyID, &kind)
	if err == nil {
		// Revoking a refresh token revokes the whole family, otherwise the
		// holder of the access token keeps working after a "revoke".
		if kind == "refresh" {
			revokeTokensForClientKey(clientID, keyID)
		} else {
			_, _ = db.Exec(`UPDATE oauth_tokens SET revoked_at=? WHERE token_hash=?`, nowUTC(), h)
		}
		logf("token revoked", "client_id", clientID, "kind", kind)
	}
	// RFC 7009 §2.2: always 200, even for unknown tokens.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// ---------- housekeeping ----------

// scrubAuth deletes credentials that expired long enough ago to be useless.
// Opik ships the same job as a token scrub worker; here it is a ticker.
func scrubAuth() (codes, tokens int64) {
	cutoff := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if r, err := db.Exec(`DELETE FROM oauth_codes WHERE expires_at < ?`, cutoff); err == nil {
		codes, _ = r.RowsAffected()
	}
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
	if r, err := db.Exec(`DELETE FROM oauth_tokens WHERE expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)`,
		old, old); err == nil {
		tokens, _ = r.RowsAffected()
	}
	if codes+tokens > 0 {
		logf("auth scrub", "codes", codes, "tokens", tokens)
	}
	return
}

func startAuthScrubber(ctx context.Context) {
	scrubAuth()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				scrubAuth()
			}
		}
	}()
}

// mintAPIKey prints a fresh key for bootstrapping without HTTP. It is a CLI
// affordance (`evalforge --create-key "name" --owner a@b`), not an API.
func mintAPIKey(args []string) {
	if err := openDB(); err != nil {
		fmt.Fprintln(os.Stderr, "db open:", err)
		os.Exit(1)
	}
	defer db.Close()
	name, owner, projectID := "", "", ""
	scopes := map[string]bool{"read": true, "write": true, "admin": true}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--owner":
			if i+1 < len(args) {
				i++
				owner = args[i]
			}
		case "--project":
			if i+1 < len(args) {
				i++
				projectID = args[i]
			}
		default:
			if name == "" && !strings.HasPrefix(args[i], "--") {
				name = args[i]
			}
		}
	}
	if name == "" {
		name = "bootstrap"
	}
	raw, key, err := insertAPIKey(name, owner, scopes, projectID, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create key:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "created %s (%s…) for owner %q; stdout carries the key\n", key.ID, key.Prefix, owner)
	fmt.Println(raw)
}
