package mcp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sqlon/internal/meta"
)

// fakeIdP is a stand-in Keycloak: a real RSA key pair, OIDC discovery and a
// JWKS endpoint, and a signer that mints access tokens with whatever claims a
// test asks for. Tokens are verified through the production path (discovery
// → jwks_uri → signature), not through any test seam.
type fakeIdP struct {
	srv  *httptest.Server
	keys map[string]*rsa.PrivateKey // kid → key
	kids []string

	// Fault knobs for the JWKS cache tests: discoveries counts every discovery
	// round trip, down makes discovery answer 503, and hold (when non-nil)
	// parks discovery until the channel is closed — a slow IdP.
	faultMu     sync.Mutex
	discoveries int
	down        bool
	hold        chan struct{}
}

func (idp *fakeIdP) setFault(down bool, hold chan struct{}) {
	idp.faultMu.Lock()
	defer idp.faultMu.Unlock()
	idp.down, idp.hold = down, hold
}

func (idp *fakeIdP) discoveryCount() int {
	idp.faultMu.Lock()
	defer idp.faultMu.Unlock()
	return idp.discoveries
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{keys: map[string]*rsa.PrivateKey{}}
	idp.addKey(t, "k1")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		idp.faultMu.Lock()
		idp.discoveries++
		down, hold := idp.down, idp.hold
		idp.faultMu.Unlock()
		if hold != nil {
			<-hold
		}
		if down {
			http.Error(w, "idp down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.srv.URL, "jwks_uri": idp.srv.URL + "/jwks",
			"authorization_endpoint": idp.srv.URL + "/auth", "token_endpoint": idp.srv.URL + "/token",
			"userinfo_endpoint": idp.srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		keys := []map[string]any{}
		for _, kid := range idp.kids {
			pub := idp.keys[kid].PublicKey
			keys = append(keys, map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *fakeIdP) addKey(t *testing.T, kid string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp.keys[kid] = k
	idp.kids = append(idp.kids, kid)
}

func b64json(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// sign mints an RS256 token with kid over the given claims, defaulting the
// claims a real Keycloak access token always carries.
func (idp *fakeIdP) sign(t *testing.T, kid string, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	full := map[string]any{
		"iss": idp.srv.URL, "typ": "Bearer", "sub": "sub-bob", "aud": "account", "azp": "claude-mcp",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
		"preferred_username": "bob", "scope": "openid profile email",
	}
	for k, v := range claims {
		if v == nil {
			delete(full, k)
			continue
		}
		full[k] = v
	}
	hdr := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	if h, ok := claims["_hdr_typ"]; ok {
		hdr["typ"] = h
		delete(full, "_hdr_typ")
	}
	signing := b64json(hdr) + "." + b64json(full)
	d := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, 0x5, d[:]) // crypto.SHA256
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (idp *fakeIdP) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	kid := idp.kids[len(idp.kids)-1]
	return idp.sign(t, kid, idp.keys[kid], claims)
}

// hs256 forges a token whose "signature" is an HMAC — the alg-confusion
// attack the verifier must refuse by algorithm before any key is consulted.
func (idp *fakeIdP) hs256(claims map[string]any) string {
	full := map[string]any{"iss": idp.srv.URL, "typ": "Bearer", "sub": "sub-bob", "aud": "account", "azp": "claude-mcp",
		"exp": time.Now().Add(5 * time.Minute).Unix()}
	for k, v := range claims {
		full[k] = v
	}
	signing := b64json(map[string]any{"alg": "HS256", "typ": "JWT", "kid": "k1"}) + "." + b64json(full)
	mac := hmac.New(sha256.New, []byte("public-key-bytes"))
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

const testResource = "https://sqlon.example.com/mcp"

// newOAuthServer returns an auth-enabled server with MCP SSO switched on
// against the fake IdP and one SSO-registered user (bob, role user).
func newOAuthServer(t *testing.T) (*Server, *http.ServeMux, *fakeIdP, string) {
	t.Helper()
	s, mux, adminTok, _ := newAuthServer(t)
	idp := newFakeIdP(t)
	ctx := context.Background()
	if _, err := s.Meta.UpsertSSOUser(ctx, "sub-bob", "bob", "Bob", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		meta.SetOIDCIssuer:       idp.srv.URL,
		meta.SetOIDCRedirect:     "https://sqlon.example.com/auth/sso/callback",
		meta.SetMCPOAuthEnabled:  "true",
		meta.SetMCPOAuthAudience: "claude-mcp",
	} {
		if err := s.Meta.ApplySetting(ctx, k, v, "test"); err != nil {
			t.Fatal(k, err)
		}
	}
	if err := s.ApplySettings(ctx); err != nil {
		t.Fatal(err)
	}
	return s, mux, idp, adminTok
}

func mcpWithBearer(t *testing.T, mux *http.ServeMux, tok, body string) *httptest.ResponseRecorder {
	t.Helper()
	if body == "" {
		body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	}
	return doReq(t, mux, "POST", "/mcp", body, map[string]string{"Authorization": "Bearer " + tok, "Accept": "application/json"})
}

func TestMCPOAuthOffByDefault(t *testing.T) {
	s, mux, _, _ := newAuthServer(t)
	if err := s.ApplySettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := s.mcpOAuthConfigSnapshot(); c.Enabled || c.Active() {
		t.Fatalf("fresh install must have MCP SSO off: %+v", c)
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		if rec := doReq(t, mux, "GET", p, "", nil); rec.Code != 404 {
			t.Fatalf("%s while off: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	// A JWT-shaped bearer is refused exactly like any other bad key: same
	// status, no OAuth vocabulary in the challenge header.
	idp := newFakeIdP(t)
	rec := mcpWithBearer(t, mux, idp.token(t, nil), "")
	if rec.Code != 401 || strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("token while off: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if !strings.Contains(rec.Body.String(), "관리자가 서버 설정에서 MCP SSO(OAuth)를 켜야") {
		t.Fatalf("refusal should say the feature is off: %s", rec.Body.String())
	}
	rec = doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Accept": "application/json"})
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer realm="sqlon"` {
		t.Fatalf("plain 401 must be unchanged: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestMCPOAuthMetadataAndChallenge(t *testing.T) {
	s, mux, idp, _ := newOAuthServer(t)
	c := s.mcpOAuthConfigSnapshot()
	if !c.Active() || c.Resource != testResource {
		t.Fatalf("config: %+v", c)
	}
	wantMeta := "https://sqlon.example.com/.well-known/oauth-protected-resource/mcp"
	if c.MetadataURL() != wantMeta {
		t.Fatalf("metadata url %q", c.MetadataURL())
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		rec := doReq(t, mux, "GET", p, "", nil)
		if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: %d %q", p, rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
		}
		var doc struct {
			Resource string   `json:"resource"`
			AS       []string `json:"authorization_servers"`
			Bearer   []string `json:"bearer_methods_supported"`
			Scopes   []string `json:"scopes_supported"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Resource != testResource || len(doc.AS) != 1 || doc.AS[0] != idp.srv.URL ||
			len(doc.Bearer) != 1 || doc.Bearer[0] != "header" || len(doc.Scopes) != 1 || doc.Scopes[0] != "mcp:read" {
			t.Fatalf("%s: %s", p, rec.Body.String())
		}
	}
	// No credential on /mcp: the 401 points at the metadata.
	rec := doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Accept": "application/json"})
	if rec.Code != 401 {
		t.Fatalf("no cred: %d", rec.Code)
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(wa, `Bearer realm="sqlon", resource_metadata="`+wantMeta+`"`) || strings.Contains(wa, "invalid_token") {
		t.Fatalf("challenge: %q", wa)
	}
	// A rejected credential adds error="invalid_token".
	rec = mcpWithBearer(t, mux, idp.token(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), "")
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("rejected token challenge: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// REST 401s never carry the OAuth challenge — MCP path only.
	rec = doReq(t, mux, "GET", "/api/mcp-keys", "", nil)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("rest 401: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestMCPOAuthAcceptsTokenForThisResource(t *testing.T) {
	s, mux, idp, _ := newOAuthServer(t)
	// Formal path: Audience mapper put the resource identifier in aud.
	rec := mcpWithBearer(t, mux, idp.token(t, map[string]any{"aud": []string{"account", testResource}, "azp": "something-else"}), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "search_schema") {
		t.Fatalf("aud=resource: %d %s", rec.Code, rec.Body.String())
	}
	// Plain path: Keycloak 26 default shape (aud=account, client in azp) with
	// the client id on the administrator's list.
	rec = mcpWithBearer(t, mux, idp.token(t, nil), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "search_schema") {
		t.Fatalf("azp listed: %d %s", rec.Code, rec.Body.String())
	}
	// aud may also be a bare string naming a listed audience.
	rec = mcpWithBearer(t, mux, idp.token(t, map[string]any{"aud": "claude-mcp", "azp": nil}), "")
	if rec.Code != 200 {
		t.Fatalf("aud string listed: %d %s", rec.Code, rec.Body.String())
	}
	// The SSO subject is the registered account: a tool call is attributed to bob.
	body := callMCPToolHTTP(t, mux, "list_db_profiles", `{}`, map[string]string{"Authorization": "Bearer " + idp.token(t, nil), "Accept": "application/json"})
	if strings.Contains(body, "forbidden") {
		t.Fatalf("read tool under mcp:read must pass: %s", body)
	}
	// No account was created or touched by the token path.
	if n, _ := s.Meta.Store.CountUsers(context.Background()); n != 3 {
		t.Fatalf("users = %d, want 3 (admin, alice, bob)", n)
	}
}

func TestMCPOAuthRejectsForeignAudience(t *testing.T) {
	_, mux, idp, _ := newOAuthServer(t)
	logs := captureLog(t)
	tok := idp.token(t, map[string]any{"aud": "account", "azp": "other-app"})
	rec := mcpWithBearer(t, mux, tok, "")
	if rec.Code != 401 {
		t.Fatalf("foreign audience must be 401: %d", rec.Code)
	}
	b := rec.Body.String()
	for _, want := range []string{"aud=[account]", `azp="other-app"`, "mcp.oauth.audience", `"other-app"`, testResource} {
		if !strings.Contains(b, want) {
			t.Fatalf("refusal must name what it saw and what to set (%q): %s", want, b)
		}
	}
	// The log line names the subject, the verbatim cause and the request id.
	refusalLogLine(t, logs, "foreign audience", `audience [account] / azp "other-app" not accepted`, rec.Header().Get("X-Request-Id"), tok)
	if !strings.Contains(logs.String(), `sub="sub-bob"`) {
		t.Fatalf("refusal log must name the subject: %s", logs.String())
	}
}

// captureLog routes the standard logger into a buffer for the test's
// lifetime, the same logger the runtime points at stderr (log.New(rt.Stderr)
// in internal/app/runtime.go), so refusal lines can be asserted on.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// refusalLogLine finds the single "mcp oauth: refused" line in buf and
// checks it carries the verbatim cause and a non-empty request id (wantID
// when the client supplied one) — without the token itself.
func refusalLogLine(t *testing.T, buf *bytes.Buffer, name, cause, wantID, token string) {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "mcp oauth: refused:") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Errorf("%s: want exactly one refusal log line, got %d: %q", name, len(lines), buf.String())
		return
	}
	line := lines[0]
	if !strings.Contains(line, "cause="+cause) && !strings.Contains(line, cause) {
		t.Errorf("%s: refusal log lacks cause %q: %s", name, cause, line)
	}
	_, after, ok := strings.Cut(line, "request_id=")
	id, _, _ := strings.Cut(after, " ")
	if !ok || id == "" {
		t.Errorf("%s: refusal log lacks a request_id: %s", name, line)
	}
	if wantID != "" && id != wantID {
		t.Errorf("%s: refusal log request_id %q, want the client's %q: %s", name, id, wantID, line)
	}
	if token != "" && strings.Contains(line, token) {
		t.Errorf("%s: refusal log must never contain the token: %s", name, line)
	}
}

func TestMCPOAuthRejectsBadTokens(t *testing.T) {
	_, mux, idp, _ := newOAuthServer(t)
	other := newFakeIdP(t) // different issuer, different keys
	foreignKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := []struct {
		name, token, want, cause string
	}{
		{"expired", idp.token(t, map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()}), "만료", "token expired at"},
		{"no exp", idp.token(t, map[string]any{"exp": nil}), "만료", "(exp present=false)"},
		{"not yet valid", idp.token(t, map[string]any{"nbf": time.Now().Add(10 * time.Minute).Unix()}), "아직 유효하지", "token not valid before"},
		{"other issuer", other.token(t, nil), "유효하지 않습니다", "signature mismatch"},
		{"issuer claim mismatch", idp.token(t, map[string]any{"iss": "https://elsewhere.example/realms/x"}), "발급자", `issuer "https://elsewhere.example/realms/x" is not "` + idp.srv.URL + `"`},
		{"id token", idp.token(t, map[string]any{"typ": "ID"}), "typ=ID", `token typ "ID" is not Bearer`},
		{"refresh token", idp.token(t, map[string]any{"typ": "Refresh"}), "액세스 토큰이 아닙니다", `token typ "Refresh" is not Bearer`},
		{"header typ ID", idp.token(t, map[string]any{"typ": nil, "_hdr_typ": "ID"}), "ID 토큰", "header typ is ID"},
		{"hs256", idp.hs256(nil), "유효하지 않습니다", `alg "HS256" not accepted`},
		{"cnf", idp.token(t, map[string]any{"cnf": map[string]any{"jkt": "abc"}}), "cnf", "token carries cnf"},
		{"no sub", idp.token(t, map[string]any{"sub": ""}), "sub", "sub missing"},
		{"unknown key", idp.sign(t, "k1", foreignKey, nil), "유효하지 않습니다", "signature mismatch"},
		{"garbage", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.bm90LWEtc2ln", "유효하지 않습니다", `unknown kid ""`},
	}
	logs := captureLog(t)
	for i, tc := range cases {
		logs.Reset()
		// Odd cases send their own correlation id, even ones let the server
		// mint one: both must show up in the log and the response.
		hdr := map[string]string{"Authorization": "Bearer " + tc.token, "Accept": "application/json"}
		wantID := ""
		if i%2 == 1 {
			wantID = "client-req-" + strings.ReplaceAll(tc.name, " ", "-")
			hdr["X-Request-Id"] = wantID
		}
		rec := doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, hdr)
		if rec.Code != 401 {
			t.Errorf("%s: code %d, want 401 (%s)", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: body %q lacks %q", tc.name, rec.Body.String(), tc.want)
		}
		gotID := rec.Header().Get("X-Request-Id")
		if gotID == "" || (wantID != "" && gotID != wantID) {
			t.Errorf("%s: response X-Request-Id %q, want %q (or any non-empty)", tc.name, gotID, wantID)
		}
		refusalLogLine(t, logs, tc.name, tc.cause, gotID, tc.token)
	}
}

func TestMCPOAuthRequiresRegisteredActiveAccount(t *testing.T) {
	s, mux, idp, _ := newOAuthServer(t)
	ctx := context.Background()
	before, _ := s.Meta.Store.CountUsers(ctx)
	rec := mcpWithBearer(t, mux, idp.token(t, map[string]any{"sub": "sub-stranger", "preferred_username": "stranger"}), "")
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "먼저 웹 콘솔에 Keycloak(SSO)으로 한 번 로그인") {
		t.Fatalf("unregistered: %d %s", rec.Code, rec.Body.String())
	}
	// A token whose username matches a *local* account must not log in as it.
	rec = mcpWithBearer(t, mux, idp.token(t, map[string]any{"sub": "sub-not-alice", "preferred_username": "alice"}), "")
	if rec.Code != 401 {
		t.Fatalf("username-only match must not authenticate: %d", rec.Code)
	}
	if after, _ := s.Meta.Store.CountUsers(ctx); after != before {
		t.Fatalf("token path created an account: %d → %d", before, after)
	}
	// Deactivated account: refused even with a perfectly valid token.
	bob, _ := s.Meta.Store.GetUserByProviderSubject(ctx, meta.ProviderKeycloak, "sub-bob")
	bob.IsActive = false
	if err := s.Meta.Store.UpdateUser(ctx, bob); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	tok := idp.token(t, nil)
	rec = doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Authorization": "Bearer " + tok, "Accept": "application/json", "X-Request-Id": "req-inactive-bob"})
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "비활성") {
		t.Fatalf("inactive: %d %s", rec.Code, rec.Body.String())
	}
	refusalLogLine(t, logs, "inactive", `account "bob" (sub "sub-bob") is deactivated`, "req-inactive-bob", tok)
}

func TestMCPOAuthTokenIsRefusedOutsideMCP(t *testing.T) {
	_, mux, idp, _ := newOAuthServer(t)
	tok := idp.token(t, nil)
	// Same token, REST paths: never accepted.
	for _, p := range []string{"/api/mcp-keys", "/api/db-profiles", "/api/settings"} {
		rec := doReq(t, mux, "GET", p, "", map[string]string{"Authorization": "Bearer " + tok})
		if rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("REST %s with SSO token must be refused: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	// /auth/me does not treat it as a login either.
	rec := doReq(t, mux, "GET", "/auth/me", "", map[string]string{"Authorization": "Bearer " + tok})
	if !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("/auth/me with SSO token: %s", rec.Body.String())
	}
	// Sanity: the same token still opens /mcp.
	if rec := mcpWithBearer(t, mux, tok, ""); rec.Code != 200 {
		t.Fatalf("/mcp: %d", rec.Code)
	}
}

func TestMCPOAuthScopeCeiling(t *testing.T) {
	s, mux, idp, _ := newOAuthServer(t)
	ctx := context.Background()
	// An SSO-registered admin: role alone is not enough under the default
	// ceiling (mcp:read) — the administrator must add mcp:admin.
	root, err := s.Meta.UpsertSSOUser(ctx, "sub-root", "root", "Root", "")
	if err != nil {
		t.Fatal(err)
	}
	root.Role = meta.RoleAdmin
	if err := s.Meta.Store.UpdateUser(ctx, root); err != nil {
		t.Fatal(err)
	}
	rootTok := func(claims map[string]any) map[string]string {
		c := map[string]any{"sub": "sub-root"}
		for k, v := range claims {
			c[k] = v
		}
		return map[string]string{"Authorization": "Bearer " + idp.token(t, c), "Accept": "application/json"}
	}
	body := callMCPToolHTTP(t, mux, "reload_catalog", `{}`, rootTok(nil))
	if !strings.Contains(body, "forbidden") || !strings.Contains(body, "mcp:admin") || !strings.Contains(body, "mcp.oauth.scopes") {
		t.Fatalf("admin tool under mcp:read ceiling must be refused with the scope to add: %s", body)
	}
	body = callMCPToolHTTP(t, mux, "dba_overview", `{}`, rootTok(nil))
	if !strings.Contains(body, "forbidden") || !strings.Contains(body, "mcp:dba") {
		t.Fatalf("dba tool under mcp:read ceiling: %s", body)
	}
	// Lift the ceiling: now the admin role carries the day.
	if err := s.Meta.ApplySetting(ctx, meta.SetMCPOAuthScopes, "mcp:read mcp:admin", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySettings(ctx); err != nil {
		t.Fatal(err)
	}
	body = callMCPToolHTTP(t, mux, "reload_catalog", `{}`, rootTok(nil))
	if strings.Contains(body, "forbidden") {
		t.Fatalf("reload_catalog with mcp:admin: %s", body)
	}
	// The ceiling never lifts what the role lacks: bob (user) with the same
	// ceiling still hits the role gate first.
	body = callMCPToolHTTP(t, mux, "reload_catalog", `{}`, map[string]string{"Authorization": "Bearer " + idp.token(t, nil), "Accept": "application/json"})
	if !strings.Contains(body, "requires admin privileges") {
		t.Fatalf("user role must still be refused: %s", body)
	}
	// Token scope in our vocabulary narrows the ceiling (intersection) …
	body = callMCPToolHTTP(t, mux, "reload_catalog", `{}`, rootTok(map[string]any{"scope": "openid mcp:read"}))
	if !strings.Contains(body, "mcp:admin") {
		t.Fatalf("token scope mcp:read must narrow the ceiling: %s", body)
	}
	// … and an empty intersection is a refusal at the door, not an open one.
	rec := mcpWithBearer(t, mux, idp.token(t, map[string]any{"sub": "sub-root", "scope": "openid mcp:dba"}), "")
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "공통 항목이 없습니다") {
		t.Fatalf("empty intersection: %d %s", rec.Code, rec.Body.String())
	}
	// Resources and prompts sit under mcp:read: a ceiling without it closes them.
	if err := s.Meta.ApplySetting(ctx, meta.SetMCPOAuthScopes, "mcp:admin", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySettings(ctx); err != nil {
		t.Fatal(err)
	}
	rec = mcpWithBearer(t, mux, idp.token(t, map[string]any{"sub": "sub-root"}),
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"sqlon://catalog/summary"}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "mcp:read") {
		t.Fatalf("resources/read without mcp:read: %d %s", rec.Code, rec.Body.String())
	}
	// Keys are never under the ceiling: an admin's key runs reload_catalog as before.
	adminRec := doReq(t, mux, "POST", "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	var adminCookie string
	for _, c := range adminRec.Result().Cookies() {
		if c.Name == meta.SessionCookie {
			adminCookie = c.Value
		}
	}
	keyRec := doReq(t, mux, "POST", "/api/mcp-keys", `{"name":"t"}`, withCookie(adminCookie))
	var key struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(keyRec.Body.Bytes(), &key)
	body = callMCPToolHTTP(t, mux, "reload_catalog", `{}`, map[string]string{"Authorization": "Bearer " + key.Key, "Accept": "application/json"})
	if strings.Contains(body, "forbidden") {
		t.Fatalf("key must be unaffected by the SSO ceiling: %s", body)
	}
}

func TestMCPOAuthSettingsValidationAndView(t *testing.T) {
	_, mux, adminTok, _ := newAuthServer(t)
	bad := []string{
		`{"mcp.oauth.scopes":"mcp:read mcp:write"}`,
		`{"mcp.oauth.enabled":"maybe"}`,
		`{"mcp.oauth.resource":"https://sqlon.example.com/mcp?x=1"}`,
		`{"mcp.oauth.resource":"sqlon.example.com/mcp"}`,
		`{"mcp.oauth.audience":"a\"b"}`,
	}
	for _, b := range bad {
		if rec := doReq(t, mux, "PUT", "/api/settings", b, withCookie(adminTok)); rec.Code != 400 {
			t.Fatalf("%s must be rejected: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	// Switched on without an issuer: stored, but inactive with a reason.
	rec := doReq(t, mux, "PUT", "/api/settings", `{"mcp.oauth.enabled":"true","mcp.oauth.resource":"https://sqlon.example.com/mcp"}`, withCookie(adminTok))
	if rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		MCPOAuth struct {
			Enabled     bool     `json:"enabled"`
			Active      bool     `json:"active"`
			Reason      string   `json:"reason"`
			Resource    string   `json:"resource"`
			MetadataURL string   `json:"metadata_url"`
			Scopes      []string `json:"scopes"`
		} `json:"mcp_oauth"`
		Settings []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"settings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.MCPOAuth.Enabled || out.MCPOAuth.Active || !strings.Contains(out.MCPOAuth.Reason, "oidc_issuer") ||
		out.MCPOAuth.Resource != "https://sqlon.example.com/mcp" ||
		out.MCPOAuth.MetadataURL != "https://sqlon.example.com/.well-known/oauth-protected-resource/mcp" ||
		len(out.MCPOAuth.Scopes) != 1 || out.MCPOAuth.Scopes[0] != "mcp:read" {
		t.Fatalf("view: %s", rec.Body.String())
	}
	var boolType string
	for _, d := range out.Settings {
		if d.Key == meta.SetMCPOAuthEnabled {
			boolType = d.Type
		}
	}
	if boolType != "bool" {
		t.Fatalf("enabled switch must be typed bool: %s", rec.Body.String())
	}
	// Inactive ⇒ metadata absent and no OAuth challenge.
	if rec := doReq(t, mux, "GET", "/.well-known/oauth-protected-resource/mcp", "", nil); rec.Code != 404 {
		t.Fatalf("metadata while inactive: %d", rec.Code)
	}
	rec = doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Accept": "application/json"})
	if strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("inactive must not advertise metadata: %q", rec.Header().Get("WWW-Authenticate"))
	}
	// /auth/me tells the key page (public facts only).
	rec = doReq(t, mux, "GET", "/auth/me", "", withCookie(adminTok))
	if !strings.Contains(rec.Body.String(), `"mcp_oauth":{"active":false`) {
		t.Fatalf("/auth/me: %s", rec.Body.String())
	}
	// Boot defaults from flags/env behave like any other default layer.
	s2, _ := newFixtureServer(t)
	s2.Options.MCPOAuthEnabled = true
	s2.Options.MCPOAuthScopes = "mcp:read mcp:dba"
	s2.EnableMeta(meta.NewService(meta.NewMemStore()), &OIDCProvider{Issuer: "https://kc.example.com/realms/r/", ClientID: "web", ClientSecret: "s", RedirectURL: "https://sqlon.example.com/auth/sso/callback"})
	if err := s2.ApplySettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := s2.mcpOAuthConfigSnapshot()
	if !c.Active() || c.Issuer != "https://kc.example.com/realms/r" || c.Resource != "https://sqlon.example.com/mcp" || strings.Join(c.Scopes, " ") != "mcp:read mcp:dba" {
		t.Fatalf("boot defaults: %+v", c)
	}
}

// A rejected PUT must leave the store untouched (the OpenAPI contract), and
// the pre-existing cache TTL setting must still be manageable alongside the
// MCP SSO entries.
func TestSettingsPutValidatesBeforeStoringAndKeepsCacheTTL(t *testing.T) {
	s, mux, adminTok, _ := newAuthServer(t)
	body := `{"mcp.oauth.resource":"https://sqlon.example.com/mcp","mcp.oauth.audience":"claude-mcp","cache_ttl_seconds":"30","mcp.oauth.scopes":"mcp:write"}`
	rec := doReq(t, mux, "PUT", "/api/settings", body, withCookie(adminTok))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "mcp.oauth.scopes") {
		t.Fatalf("invalid scope must reject the whole request: %d %s", rec.Code, rec.Body.String())
	}
	stored, _ := s.Meta.Store.GetSettings(context.Background())
	for _, k := range []string{meta.SetMCPOAuthResource, meta.SetMCPOAuthAudience, meta.SetCacheTTL, meta.SetMCPOAuthScopes} {
		if v, ok := stored[k]; ok {
			t.Fatalf("rejected PUT must store nothing, but %s=%q was saved", k, v)
		}
	}
	rec = doReq(t, mux, "PUT", "/api/settings", `{"cache_ttl_seconds":"30"}`, withCookie(adminTok))
	if rec.Code != 200 {
		t.Fatalf("cache_ttl_seconds must remain a known setting: %d %s", rec.Code, rec.Body.String())
	}
	if stored, _ = s.Meta.Store.GetSettings(context.Background()); stored[meta.SetCacheTTL] != "30" {
		t.Fatalf("cache_ttl_seconds not stored: %v", stored)
	}
	var out struct {
		Settings []struct {
			Key string `json:"key"`
		} `json:"settings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	found := false
	for _, d := range out.Settings {
		found = found || d.Key == meta.SetCacheTTL
	}
	if !found {
		t.Fatalf("cache_ttl_seconds missing from settings view: %s", rec.Body.String())
	}
}

func TestMCPOAuthJWKSRotationAndThrottle(t *testing.T) {
	s, mux, idp, _ := newOAuthServer(t)
	if rec := mcpWithBearer(t, mux, idp.token(t, nil), ""); rec.Code != 200 {
		t.Fatalf("k1: %d", rec.Code)
	}
	// Keycloak rotates: a token under a new kid triggers one refetch and passes.
	idp.addKey(t, "k2")
	s.jwksKeys.mu.Lock()
	s.jwksKeys.nextRefetch = time.Time{}
	s.jwksKeys.mu.Unlock()
	if rec := mcpWithBearer(t, mux, idp.token(t, nil), ""); rec.Code != 200 {
		t.Fatalf("k2 after rotation: %d %s", rec.Code, rec.Body.String())
	}
	// Immediately after, an unknown kid does not trigger another round trip.
	forged, _ := rsa.GenerateKey(rand.Reader, 2048)
	rec := mcpWithBearer(t, mux, idp.sign(t, "k3", forged, nil), "")
	if rec.Code != 401 {
		t.Fatalf("unknown kid: %d", rec.Code)
	}
	s.jwksKeys.mu.Lock()
	throttled := time.Now().Before(s.jwksKeys.nextRefetch) && s.jwksKeys.keys["k3"] == nil && len(s.jwksKeys.keys) == 2
	s.jwksKeys.mu.Unlock()
	if !throttled {
		t.Fatal("unknown-kid refetch must be throttled")
	}
}

// A slow IdP must not stand between a cached key and the request that needs
// it: the network round trip for an unknown kid happens outside the cache
// lock, and a waiter that gives up (ctx) is not held hostage by it.
func TestJWKSCacheFetchDoesNotBlockCachedKeys(t *testing.T) {
	idp := newFakeIdP(t)
	c := newJWKSCache()
	ctx := context.Background()
	if _, err := c.key(ctx, idp.srv.URL, "k1"); err != nil {
		t.Fatal(err)
	}
	// From here on discovery hangs until released.
	hold := make(chan struct{})
	idp.setFault(false, hold)
	c.mu.Lock()
	c.nextRefetch = time.Time{}
	c.mu.Unlock()
	slowDone := make(chan error, 1)
	go func() {
		_, err := c.key(ctx, idp.srv.URL, "k-unknown")
		slowDone <- err
	}()
	// Wait until the slow fetch has actually reached the IdP.
	for deadline := time.Now().Add(3 * time.Second); idp.discoveryCount() < 2; {
		if time.Now().After(deadline) {
			t.Fatal("slow fetch never reached the IdP")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if _, err := c.key(ctx, idp.srv.URL, "k1"); err != nil {
		t.Fatalf("cached kid during a slow fetch: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("cached kid waited %v on the slow fetch", d)
	}
	// A second unknown-kid caller joins the in-flight fetch rather than
	// starting another, and honours its own ctx while waiting.
	wctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := c.key(wctx, idp.srv.URL, "k-other"); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("waiter must fail with its own ctx, got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waiter ignored its ctx: %v", d)
	}
	if n := idp.discoveryCount(); n != 2 {
		t.Fatalf("discovery calls = %d, want 2 (warm-up + one shared slow fetch)", n)
	}
	close(hold)
	if err := <-slowDone; err == nil || !strings.Contains(err.Error(), "unknown kid") {
		t.Fatalf("slow fetch: %v", err)
	}
}

// Failed discovery/JWKS is remembered like a success: within the refetch
// window no further round trips are made, and keys already held keep
// serving after the TTL has run out while the IdP is down.
func TestJWKSCacheThrottlesFailedFetch(t *testing.T) {
	idp := newFakeIdP(t)
	ctx := context.Background()

	// (a) Cold cache, IdP down: one round trip, then throttled refusals.
	c := newJWKSCache()
	idp.setFault(true, nil)
	for i := 0; i < 5; i++ {
		if _, err := c.key(ctx, idp.srv.URL, "k1"); err == nil {
			t.Fatalf("call %d: down IdP must not yield a key", i)
		}
	}
	if n := idp.discoveryCount(); n != 1 {
		t.Fatalf("cold cache, down IdP: %d discovery calls, want 1", n)
	}

	// (b) Warm cache, TTL expired, IdP down: one refetch attempt, the old key
	// keeps serving, further attempts wait for the throttle window.
	idp.setFault(false, nil)
	c = newJWKSCache()
	if _, err := c.key(ctx, idp.srv.URL, "k1"); err != nil {
		t.Fatal(err)
	}
	base := idp.discoveryCount()
	idp.setFault(true, nil)
	c.mu.Lock()
	c.fetchedAt = time.Now().Add(-2 * jwksTTL)
	c.nextRefetch = time.Time{}
	c.mu.Unlock()
	for i := 0; i < 5; i++ {
		if _, err := c.key(ctx, idp.srv.URL, "k1"); err != nil {
			t.Fatalf("call %d: known kid must survive a failed refetch: %v", i, err)
		}
		if _, err := c.key(ctx, idp.srv.URL, "k-unknown"); err == nil {
			t.Fatalf("call %d: unknown kid must still be refused", i)
		}
	}
	if n := idp.discoveryCount() - base; n != 1 {
		t.Fatalf("expired cache, down IdP: %d discovery calls, want 1", n)
	}
	// Once the IdP is back and the window has passed, the refetch succeeds.
	idp.setFault(false, nil)
	idp.addKey(t, "k2")
	c.mu.Lock()
	c.nextRefetch = time.Time{}
	c.mu.Unlock()
	if _, err := c.key(ctx, idp.srv.URL, "k2"); err != nil {
		t.Fatalf("recovered IdP: %v", err)
	}
}
