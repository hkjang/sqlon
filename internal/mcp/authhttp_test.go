package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sqlon/internal/fleet"
	"sqlon/internal/meta"
)

// newAuthServer builds a server with an in-memory meta store, a bootstrap
// admin, and a second regular user, returning their session tokens.
func newAuthServer(t *testing.T) (*Server, *http.ServeMux, string, string) {
	t.Helper()
	s, _ := newFixtureServer(t)
	svc := meta.NewService(meta.NewMemStore())
	if _, _, _, err := svc.Bootstrap(context.Background(), "admin:adminpass1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateLocalUser(context.Background(), "alice", "alicepass1", meta.RoleUser, "Alice", ""); err != nil {
		t.Fatal(err)
	}
	s.EnableMeta(svc, nil)
	mux := http.NewServeMux()
	s.Register(mux)

	login := func(u, p string) string {
		rec := doReq(t, mux, "POST", "/auth/login", `{"username":"`+u+`","password":"`+p+`"}`, nil)
		if rec.Code != 200 {
			t.Fatalf("login %s: %d %s", u, rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == meta.SessionCookie {
				return c.Value
			}
		}
		t.Fatalf("no session cookie for %s", u)
		return ""
	}
	return s, mux, login("admin", "adminpass1"), login("alice", "alicepass1")
}

func withCookie(token string) map[string]string {
	return map[string]string{"Cookie": meta.SessionCookie + "=" + token}
}

func withLegacyCookie(token string) map[string]string {
	return map[string]string{"Cookie": meta.LegacySessionCookie + "=" + token}
}

func TestAuthLoginLogoutAndMe(t *testing.T) {
	_, mux, adminTok, _ := newAuthServer(t)

	// me with session
	rec := doReq(t, mux, "GET", "/auth/me", "", withCookie(adminTok))
	var me struct {
		Authenticated bool                  `json:"authenticated"`
		User          struct{ Role string } `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if !me.Authenticated || me.User.Role != "admin" {
		t.Fatalf("me: %s", rec.Body.String())
	}
	// 기존 브라우저 세션은 한 릴리스 동안 레거시 쿠키명으로도 인증한다.
	rec = doReq(t, mux, "GET", "/auth/me", "", withLegacyCookie(adminTok))
	me = struct {
		Authenticated bool                  `json:"authenticated"`
		User          struct{ Role string } `json:"user"`
	}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if !me.Authenticated {
		t.Fatalf("legacy session cookie rejected: %s", rec.Body.String())
	}
	// bad login
	rec = doReq(t, mux, "POST", "/auth/login", `{"username":"admin","password":"nope"}`, nil)
	if rec.Code != 401 {
		t.Fatalf("bad login should be 401: %d", rec.Code)
	}
	// logout revokes
	rec = doReq(t, mux, "POST", "/auth/logout", "", withCookie(adminTok))
	if rec.Code != 200 {
		t.Fatalf("logout: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/auth/me", "", withCookie(adminTok))
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Authenticated {
		t.Fatal("session must be revoked after logout")
	}
}

func TestFleetAPIAppliesProfilePermissionsAndReturnsEvidenceEnvelope(t *testing.T) {
	s, mux, adminTok, aliceTok := newAuthServer(t)
	admin, _ := s.Meta.Store.GetUserByUsername(t.Context(), "admin")
	alice, _ := s.Meta.Store.GetUserByUsername(t.Context(), "alice")
	definitions := []struct {
		id, owner string
		body      string
	}{
		{"admin-prod", admin.ID, `{"name":"관리자 운영 DB","type":"postgres","service_name":"payments","environment":"production","criticality":"critical","connect_string":"127.0.0.1:1/db","username":"monitor","password_ref":"plain:hidden"}`},
		{"alice-dev", alice.ID, `{"name":"Alice 개발 DB","type":"postgres","service_name":"catalog","environment":"development","criticality":"low","connect_string":"127.0.0.1:1/db","username":"monitor","password_ref":"plain:hidden"}`},
	}
	for _, d := range definitions {
		if err := s.Meta.Store.UpsertProfile(t.Context(), &meta.ProfileRecord{ID: d.id, OwnerID: d.owner, Definition: []byte(d.body), Visibility: meta.VisibilityPrivate}, true); err != nil {
			t.Fatal(err)
		}
	}

	if rec := doReq(t, mux, "GET", "/api/fleet/instances", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated fleet inventory = %d", rec.Code)
	}
	rec := doReq(t, mux, "GET", "/api/fleet/instances", "", withCookie(aliceTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice inventory: %d %s", rec.Code, rec.Body.String())
	}
	var inventory fleet.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Summary.Total != 1 || inventory.Data[0].ID != "alice-dev" || inventory.Data[0].ServiceName != "catalog" {
		t.Fatalf("profile ACL not applied: %+v", inventory)
	}
	if strings.Contains(rec.Body.String(), "hidden") || strings.Contains(rec.Body.String(), "connect_string") {
		t.Fatalf("fleet response leaked connection secret/detail: %s", rec.Body.String())
	}

	rec = doReq(t, mux, "GET", "/api/fleet/instances", "", withCookie(adminTok))
	if err := json.Unmarshal(rec.Body.Bytes(), &inventory); err != nil || inventory.Summary.Total != 2 {
		t.Fatalf("admin fleet visibility: status=%d inventory=%+v err=%v", rec.Code, inventory, err)
	}

	// MCP and REST consume the same fleet service and ACL-filtered profiles.
	params := json.RawMessage(`{"name":"list_database_instances","arguments":{}}`)
	result, err := s.callTool(withUser(t.Context(), alice), params)
	if err != nil {
		t.Fatal(err)
	}
	mcpInventory, ok := result.(fleet.Health)
	if !ok || mcpInventory.Summary.Total != 1 || mcpInventory.Data[0].ID != "alice-dev" {
		t.Fatalf("MCP fleet inventory diverged from REST: %#v", result)
	}

	// Session/lock observation must apply the same profile ACL before any
	// system query is attempted, and must not leak the secret definition.
	if rec := doReq(t, mux, "GET", "/api/observability/sessions?profile=alice-dev", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous session observation = %d", rec.Code)
	}
	if rec := doReq(t, mux, "GET", "/api/observability/sessions?profile=admin-prod", "", withCookie(aliceTok)); rec.Code != http.StatusNotFound {
		t.Fatalf("Alice observed an unauthorized profile: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "GET", "/api/observability/sessions?profile=alice-dev", "", withCookie(aliceTok))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"profile_id":"alice-dev"`) || strings.Contains(rec.Body.String(), "hidden") {
		t.Fatalf("allowed observation envelope or masking failed: %d %s", rec.Code, rec.Body.String())
	}

	params = json.RawMessage(`{"name":"get_lock_tree","arguments":{"profile":"admin-prod"}}`)
	result, err = s.callTool(withUser(t.Context(), alice), params)
	if err != nil {
		t.Fatal(err)
	}
	if resultMap, ok := result.(map[string]any); !ok || resultMap["status"] != "not_found" {
		t.Fatalf("MCP lock tool bypassed profile ACL: %#v", result)
	}

	if rec := doReq(t, mux, "GET", "/api/observability/workload?profile=admin-prod", "", withCookie(aliceTok)); rec.Code != http.StatusNotFound {
		t.Fatalf("Alice read unauthorized workload history: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "GET", "/api/observability/workload?profile=alice-dev", "", withCookie(aliceTok))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"not_collected"`) || strings.Contains(rec.Body.String(), "hidden") {
		t.Fatalf("stored workload ACL/no-implicit-query contract failed: %d %s", rec.Code, rec.Body.String())
	}
	params = json.RawMessage(`{"name":"get_storage_status","arguments":{"profile":"admin-prod"}}`)
	result, err = s.callTool(withUser(t.Context(), alice), params)
	if err != nil {
		t.Fatal(err)
	}
	if resultMap, ok := result.(map[string]any); !ok || resultMap["status"] != "not_found" {
		t.Fatalf("MCP storage tool bypassed profile ACL: %#v", result)
	}
}

func TestAdminEndpointsRequireAdminRole(t *testing.T) {
	_, mux, adminTok, aliceTok := newAuthServer(t)

	// user list: admin ok, alice forbidden, anon unauthorized
	if rec := doReq(t, mux, "GET", "/api/users", "", withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("admin user list: %d", rec.Code)
	}
	if rec := doReq(t, mux, "GET", "/api/users", "", withCookie(aliceTok)); rec.Code != 403 {
		t.Fatalf("alice user list must be 403: %d", rec.Code)
	}
	if rec := doReq(t, mux, "GET", "/api/users", "", nil); rec.Code != 401 {
		t.Fatalf("anon user list must be 401: %d", rec.Code)
	}
	// dataset mutation is admin-only
	if rec := doReq(t, mux, "PUT", "/api/datasets/glossary", `{"entries":[]}`, withCookie(aliceTok)); rec.Code != 403 {
		t.Fatalf("alice dataset put must be 403: %d", rec.Code)
	}
	if rec := doReq(t, mux, "PUT", "/api/datasets/glossary", `{"entries":[]}`, withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("admin dataset put: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMCPKeyEndpointsAndAuth(t *testing.T) {
	s, mux, _, aliceTok := newAuthServer(t)

	// alice issues a key
	rec := doReq(t, mux, "POST", "/api/mcp-keys", `{"name":"laptop","ttl_hours":0}`, withCookie(aliceTok))
	if rec.Code != 200 {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Key     string `json:"key"`
		KeyInfo struct {
			ID string `json:"id"`
		} `json:"key_info"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Key, "ssk_") {
		t.Fatalf("key must start with ssk_: %q", out.Key)
	}
	// the key authenticates an MCP request
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Authorization": "Bearer " + out.Key, "Accept": "application/json"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "search_schema") {
		t.Fatalf("mcp with key: %d %s", rec.Code, rec.Body.String()[:120])
	}
	// no key → 401 on /mcp
	rec = doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Accept": "application/json"})
	if rec.Code != 401 {
		t.Fatalf("mcp without key must be 401: %d", rec.Code)
	}
	// revoke → key stops working
	rec = doReq(t, mux, "DELETE", "/api/mcp-keys/"+out.KeyInfo.ID, "", withCookie(aliceTok))
	if rec.Code != 200 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rec = doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Authorization": "Bearer " + out.Key, "Accept": "application/json"})
	if rec.Code != 401 {
		t.Fatalf("revoked key must be 401: %d", rec.Code)
	}
	_ = s
}

func TestPerUserProfilePermissions(t *testing.T) {
	s, mux, adminTok, aliceTok := newAuthServer(t)
	// bob: a third user with no grants
	if _, err := s.Meta.CreateLocalUser(context.Background(), "bob", "bobpass12", meta.RoleUser, "", ""); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, mux, "POST", "/auth/login", `{"username":"bob","password":"bobpass12"}`, nil)
	var bobTok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == meta.SessionCookie {
			bobTok = c.Value
		}
	}

	// alice creates a private profile
	body := `{"id":"alice-db","connect_string":"h:1521/S","username":"APP_RO","password_ref":"env:X","visibility":"private"}`
	if rec := doReq(t, mux, "POST", "/api/db-profiles", body, withCookie(aliceTok)); rec.Code != 200 {
		t.Fatalf("alice create profile: %d %s", rec.Code, rec.Body.String())
	}
	// bob cannot see it in the list
	rec = doReq(t, mux, "GET", "/api/db-profiles", "", withCookie(bobTok))
	if strings.Contains(rec.Body.String(), "alice-db") {
		t.Fatal("bob must not see alice's private profile")
	}
	// bob cannot test/execute it (403)
	if rec := doReq(t, mux, "POST", "/api/db-profiles/alice-db/test", "", withCookie(bobTok)); rec.Code != 403 {
		t.Fatalf("bob test must be 403: %d %s", rec.Code, rec.Body.String())
	}
	// admin sees everything
	rec = doReq(t, mux, "GET", "/api/db-profiles", "", withCookie(adminTok))
	if !strings.Contains(rec.Body.String(), "alice-db") {
		t.Fatal("admin must see alice's profile")
	}
	// alice grants bob use
	if rec := doReq(t, mux, "PUT", "/api/db-profiles/alice-db/grants",
		`{"username":"bob","permission":"use"}`, withCookie(aliceTok)); rec.Code != 200 {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	// now bob sees it and can test (test reaches driver → stub error, but not 403)
	rec = doReq(t, mux, "GET", "/api/db-profiles", "", withCookie(bobTok))
	if !strings.Contains(rec.Body.String(), "alice-db") {
		t.Fatal("bob must see the profile after grant")
	}
	if rec := doReq(t, mux, "POST", "/api/db-profiles/alice-db/test", "", withCookie(bobTok)); rec.Code == 403 {
		t.Fatal("bob test must not be 403 after use grant")
	}
	// bob (use only) cannot manage grants
	if rec := doReq(t, mux, "PUT", "/api/db-profiles/alice-db/grants",
		`{"username":"admin","permission":"use"}`, withCookie(bobTok)); rec.Code != 403 {
		t.Fatalf("bob (use) must not manage grants: %d", rec.Code)
	}
	// bob cannot delete alice's profile
	if rec := doReq(t, mux, "DELETE", "/api/db-profiles/alice-db", "", withCookie(bobTok)); rec.Code != 403 {
		t.Fatalf("bob delete must be 403: %d", rec.Code)
	}
}

func TestStandaloneUnaffected(t *testing.T) {
	// no meta → /mcp open, /auth/me reports disabled
	_, mux := newAdminMux(t, "")
	rec := doReq(t, mux, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Accept": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("standalone mcp must be open: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/auth/me", "", nil)
	if !strings.Contains(rec.Body.String(), `"auth_enabled":false`) {
		t.Fatalf("standalone me: %s", rec.Body.String())
	}
	// meta-only endpoints report unavailable
	if rec := doReq(t, mux, "GET", "/api/users", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("standalone users must be 503: %d", rec.Code)
	}
}

var _ = httptest.NewRecorder

func TestMCPToolAdminGuardMetaMode(t *testing.T) {
	_, mux, _, aliceTok := newAuthServer(t)
	// alice (user role) issues her own key
	rec := doReq(t, mux, "POST", "/api/mcp-keys", `{"name":"k","ttl_hours":0}`, withCookie(aliceTok))
	var out struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	hdr := map[string]string{"Authorization": "Bearer " + out.Key, "Accept": "application/json"}

	// read-only tool works for a plain user
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_schema","arguments":{"question":"카드"}}}`, hdr)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "forbidden") {
		t.Fatalf("read-only tool should work for user: %d %s", rec.Code, rec.Body.String()[:160])
	}
	// admin-only tool (reload_catalog) is forbidden for a plain user over MCP
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reload_catalog","arguments":{}}}`, hdr)
	if !strings.Contains(rec.Body.String(), "admin privileges") {
		t.Fatalf("reload_catalog must be admin-forbidden for user: %s", rec.Body.String())
	}
	// put_dataset likewise
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"put_dataset","arguments":{"name":"glossary","content":{"entries":[]}}}}`, hdr)
	if !strings.Contains(rec.Body.String(), "admin privileges") {
		t.Fatalf("put_dataset must be admin-forbidden for user: %s", rec.Body.String())
	}
}

func TestStandaloneMCPTokenGate(t *testing.T) {
	// standalone WITH an admin token: mutating MCP tools require the token,
	// read-only tools stay open, and stdio-style (no token configured) is open.
	srv, _ := newFixtureServer(t)
	srv.Options.AdminToken = "sekrit"
	mux := http.NewServeMux()
	srv.Register(mux)

	acc := map[string]string{"Accept": "application/json"}
	tok := map[string]string{"Accept": "application/json", "X-Admin-Token": "sekrit"}

	// read-only tool open without token
	rec := doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_schema","arguments":{"question":"카드"}}}`, acc)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "forbidden") {
		t.Fatalf("standalone read-only tool must be open: %d", rec.Code)
	}
	// mutating tool WITHOUT token → forbidden
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reload_catalog","arguments":{}}}`, acc)
	if !strings.Contains(rec.Body.String(), "admin privileges") {
		t.Fatalf("standalone mutating tool must require token: %s", rec.Body.String())
	}
	// mutating tool WITH token → allowed
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"reload_catalog","arguments":{}}}`, tok)
	if strings.Contains(rec.Body.String(), "admin privileges") {
		t.Fatalf("standalone mutating tool must pass with token: %s", rec.Body.String())
	}
}

func TestAdminPageGuardsForRole(t *testing.T) {
	_, mux, adminTok, aliceTok := newAuthServer(t)
	// non-admin loading /admin/users is redirected (302), admin gets 200
	rec := doReq(t, mux, "GET", "/admin/users", "", withCookie(aliceTok))
	if rec.Code != http.StatusFound {
		t.Fatalf("alice /admin/users must redirect: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/admin/users", "", withCookie(adminTok))
	if rec.Code != 200 {
		t.Fatalf("admin /admin/users must be 200: %d", rec.Code)
	}
	// unauthenticated → redirect to login
	rec = doReq(t, mux, "GET", "/admin", "", nil)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/auth/login") {
		t.Fatalf("anon /admin must redirect to login: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestListDBProfilesTool(t *testing.T) {
	_, mux, _, aliceTok := newAuthServer(t)
	rec := doReq(t, mux, "POST", "/api/mcp-keys", `{"name":"k","ttl_hours":0}`, withCookie(aliceTok))
	var out struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	hdr := map[string]string{"Authorization": "Bearer " + out.Key, "Accept": "application/json"}
	// alice has no profiles yet → empty list, but tool works
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_db_profiles","arguments":{}}}`, hdr)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count"`) {
		t.Fatalf("list_db_profiles: %d %s", rec.Code, rec.Body.String()[:200])
	}
	// create a profile via REST then it should appear
	doReq(t, mux, "POST", "/api/db-profiles",
		`{"id":"p1","connect_string":"h:1521/S","username":"U","password_ref":"env:X"}`, withCookie(aliceTok))
	rec = doReq(t, mux, "POST", "/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_db_profiles","arguments":{}}}`, hdr)
	if !strings.Contains(rec.Body.String(), "p1") {
		t.Fatalf("profile should be listed for owner: %s", rec.Body.String())
	}
	// secret must never leak
	if strings.Contains(rec.Body.String(), "password_ref") || strings.Contains(rec.Body.String(), "env:X") {
		t.Fatalf("list_db_profiles leaked password_ref: %s", rec.Body.String())
	}
}

func TestSettingsRuntimeApply(t *testing.T) {
	s, mux, adminTok, aliceTok := newAuthServer(t)
	// non-admin forbidden
	if rec := doReq(t, mux, "GET", "/api/settings", "", withCookie(aliceTok)); rec.Code != 403 {
		t.Fatalf("alice settings must be 403: %d", rec.Code)
	}
	// set a master admin token via settings → applied at runtime
	rec := doReq(t, mux, "PUT", "/api/settings", `{"admin_token":"live-token-123"}`, withCookie(adminTok))
	if rec.Code != 200 {
		t.Fatalf("put settings: %d %s", rec.Code, rec.Body.String())
	}
	if s.Options.AdminToken != "live-token-123" {
		t.Fatalf("admin token not applied at runtime: %q", s.Options.AdminToken)
	}
	// GET masks the secret
	rec = doReq(t, mux, "GET", "/api/settings", "", withCookie(adminTok))
	if strings.Contains(rec.Body.String(), "live-token-123") {
		t.Fatalf("settings GET leaked secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "set)") {
		t.Fatalf("secret should show as set: %s", rec.Body.String())
	}
	// enabling OIDC via settings turns on SSO at runtime
	doReq(t, mux, "PUT", "/api/settings",
		`{"oidc_issuer":"https://kc/realms/x","oidc_client_id":"c","oidc_client_secret":"s","oidc_redirect_url":"https://h/auth/sso/callback"}`,
		withCookie(adminTok))
	if s.OIDC == nil {
		t.Fatal("OIDC provider should be built from settings")
	}
	// delete a setting (null) reverts
	doReq(t, mux, "PUT", "/api/settings", `{"admin_token":null}`, withCookie(adminTok))
	if s.Options.AdminToken != "" {
		t.Fatalf("admin token should revert after delete: %q", s.Options.AdminToken)
	}
}

func TestDatasetPGRoundTrip(t *testing.T) {
	s, mux, adminTok, _ := newAuthServer(t)
	if err := s.InitDatasetStore(context.Background()); err != nil {
		t.Fatalf("init dataset store: %v", err)
	}
	// after init, fixture's physical/logical models were imported to the DB
	if _, err := s.Meta.Store.GetDataset(context.Background(), "physical_models"); err != nil {
		t.Fatalf("physical_models should be imported to meta DB: %v", err)
	}
	// storage reported as postgres
	rec := doReq(t, mux, "GET", "/api/datasets", "", withCookie(adminTok))
	if !strings.Contains(rec.Body.String(), `"storage":"postgres"`) {
		t.Fatalf("storage should be postgres: %s", rec.Body.String()[:120])
	}
	// put a glossary via REST → lands in the DB
	rec = doReq(t, mux, "PUT", "/api/datasets/glossary",
		`{"entries":[{"term":"고객","synonyms":["cust_no"],"category":"entity"}]}`, withCookie(adminTok))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "postgres meta DB") {
		t.Fatalf("put glossary (meta): %d %s", rec.Code, rec.Body.String())
	}
	row, err := s.Meta.Store.GetDataset(context.Background(), "glossary")
	if err != nil || !strings.Contains(string(row.Content), "고객") {
		t.Fatalf("glossary not persisted to meta DB: %v", err)
	}
	if len(s.cat().Glossary.Entries) != 1 {
		t.Fatalf("catalog not hot-swapped from meta put: %+v", s.cat().Glossary)
	}
	// reload materializes from DB and recompiles
	rec = doReq(t, mux, "POST", "/api/reload", "", withCookie(adminTok))
	if rec.Code != 200 {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.cat().Glossary.Entries) != 1 {
		t.Fatal("glossary should survive reload from meta DB")
	}
}

func TestMeIncludesVersionAndSelfProfileUpdate(t *testing.T) {
	s, mux, _, aliceTok := newAuthServer(t)

	// /auth/me carries the server version for the sidebar footer
	rec := doReq(t, mux, "GET", "/auth/me", "", withCookie(aliceTok))
	var me struct {
		Version string `json:"version"`
		User    struct {
			DisplayName string `json:"display_name"`
			Email       string `json:"email"`
			Role        string `json:"role"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Version != Version {
		t.Fatalf("me.version=%q want %q", me.Version, Version)
	}

	// alice edits her own display name + email
	rec = doReq(t, mux, "PUT", "/auth/profile",
		`{"display_name":"Alice Kim","email":"alice@example.com"}`, withCookie(aliceTok))
	if rec.Code != 200 {
		t.Fatalf("profile update: %d %s", rec.Code, rec.Body.String())
	}
	u, err := s.Meta.Store.GetUserByUsername(context.Background(), "alice")
	if err != nil || u.DisplayName != "Alice Kim" || u.Email != "alice@example.com" {
		t.Fatalf("profile not persisted: %+v err=%v", u, err)
	}
	// role must be untouched by a self-profile edit
	if u.Role != meta.RoleUser {
		t.Fatalf("self profile edit must not change role: %s", u.Role)
	}
	// password hash preserved: alice can still log in
	rec = doReq(t, mux, "POST", "/auth/login", `{"username":"alice","password":"alicepass1"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("alice login after profile edit: %d %s", rec.Code, rec.Body.String())
	}

	// anonymous cannot update a profile
	if rec := doReq(t, mux, "PUT", "/auth/profile", `{"display_name":"x"}`, nil); rec.Code != 401 {
		t.Fatalf("anon profile update must be 401: %d", rec.Code)
	}
}

func TestAdminShareProfileGrantAndVisibility(t *testing.T) {
	s, mux, adminTok, aliceTok := newAuthServer(t)

	// admin registers a profile (owned by admin)
	body := `{"id":"team-dw","name":"팀DW","connect_string":"h:1521/DW","username":"RO","password_ref":"env:P","visibility":"private"}`
	if rec := doReq(t, mux, "POST", "/api/db-profiles", body, withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	listIDs := func(tok string) map[string]string {
		rec := doReq(t, mux, "GET", "/api/db-profiles", "", withCookie(tok))
		var out struct {
			Profiles []struct {
				ID            string `json:"id"`
				MyPermission  string `json:"my_permission"`
				ConnectString string `json:"connect_string"`
			} `json:"profiles"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		m := map[string]string{}
		for _, p := range out.Profiles {
			m[p.ID] = p.MyPermission
		}
		return m
	}

	// alice can't see the private profile yet
	if _, ok := listIDs(aliceTok)["team-dw"]; ok {
		t.Fatal("alice should not see admin's private profile")
	}
	// admin grants use to alice by username
	if rec := doReq(t, mux, "PUT", "/api/db-profiles/team-dw/grants",
		`{"username":"alice","permission":"use"}`, withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	if listIDs(aliceTok)["team-dw"] != "use" {
		t.Fatal("alice should have use after grant")
	}

	// admin flips to shared; definition must survive
	if rec := doReq(t, mux, "PUT", "/api/db-profiles/team-dw/visibility",
		`{"visibility":"shared"}`, withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("visibility toggle: %d %s", rec.Code, rec.Body.String())
	}
	rec := doReq(t, mux, "GET", "/api/db-profiles", "", withCookie(adminTok))
	if !strings.Contains(rec.Body.String(), "h:1521/DW") {
		t.Fatalf("definition wiped by visibility toggle: %s", rec.Body.String())
	}

	// a brand-new user (no grant) can use it purely via shared visibility
	if _, err := s.Meta.CreateLocalUser(context.Background(), "carol", "carolpass1", meta.RoleUser, "", ""); err != nil {
		t.Fatal(err)
	}
	carol := ""
	if r := doReq(t, mux, "POST", "/auth/login", `{"username":"carol","password":"carolpass1"}`, nil); r.Code == 200 {
		for _, c := range r.Result().Cookies() {
			if c.Name == meta.SessionCookie {
				carol = c.Value
			}
		}
	}
	if listIDs(carol)["team-dw"] != "use" {
		t.Fatal("carol should use the shared profile without an explicit grant")
	}
	// carol (non-owner, non-admin) cannot change visibility
	if r := doReq(t, mux, "PUT", "/api/db-profiles/team-dw/visibility", `{"visibility":"private"}`, withCookie(carol)); r.Code != 403 {
		t.Fatalf("carol visibility change must be 403: %d", r.Code)
	}
}

func TestMCPActivityHistoryAndStatsScope(t *testing.T) {
	s, mux, adminTok, aliceTok := newAuthServer(t)
	ctx := context.Background()
	alice, _ := s.Meta.Store.GetUserByUsername(ctx, "alice")
	admin, _ := s.Meta.Store.GetUserByUsername(ctx, "admin")

	// seed activity: alice a prompt + an execution; admin one generate
	_ = s.Meta.Store.RecordActivity(ctx, &meta.MCPActivity{ID: "a1", UserID: alice.ID, Username: "alice",
		SessionID: "s1", Tool: "prepare_sql_context", Kind: meta.ActivityPrompt, Prompt: "지점별 연체율"})
	_ = s.Meta.Store.RecordActivity(ctx, &meta.MCPActivity{ID: "a2", UserID: alice.ID, Username: "alice",
		SessionID: "s1", Tool: "run_sql_safely", Kind: meta.ActivityExecute, SQL: "SELECT 1", Status: "executed", RowCount: 5})
	_ = s.Meta.Store.RecordActivity(ctx, &meta.MCPActivity{ID: "a3", UserID: admin.ID, Username: "admin",
		Tool: "validate_sql", Kind: meta.ActivityGenerate, SQL: "SELECT 2", Status: "valid"})

	countActivity := func(tok, query string) (int, string) {
		rec := doReq(t, mux, "GET", "/api/mcp-activity"+query, "", withCookie(tok))
		if rec.Code != 200 {
			t.Fatalf("activity %s: %d %s", query, rec.Code, rec.Body.String())
		}
		var out struct {
			Activity []map[string]any `json:"activity"`
			Scope    string           `json:"scope"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return len(out.Activity), out.Scope
	}

	// alice sees only her 2 rows, even with all=true (pinned to self)
	if n, _ := countActivity(aliceTok, ""); n != 2 {
		t.Fatalf("alice own activity = %d, want 2", n)
	}
	if n, scope := countActivity(aliceTok, "?all=true"); n != 2 || scope == "all" {
		t.Fatalf("alice all=true must stay own: n=%d scope=%s", n, scope)
	}
	// admin all=true sees everyone (3)
	if n, scope := countActivity(adminTok, "?all=true"); n != 3 || scope != "all" {
		t.Fatalf("admin all activity = %d scope=%s, want 3/all", n, scope)
	}
	// admin can filter by a specific user
	if n, _ := countActivity(adminTok, "?user="+alice.ID); n != 2 {
		t.Fatalf("admin filtered to alice = %d, want 2", n)
	}

	// stats: alice own totals
	rec := doReq(t, mux, "GET", "/api/mcp-stats", "", withCookie(aliceTok))
	var st struct {
		Total      int  `json:"total"`
		Prompts    int  `json:"prompts"`
		Executions int  `json:"executions"`
		TotalRows  int  `json:"total_rows"`
		PerUser    bool `json:"per_user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Total != 2 || st.Prompts != 1 || st.Executions != 1 || st.TotalRows != 5 || st.PerUser {
		t.Fatalf("alice stats wrong: %+v", st)
	}
	// admin all stats → per_user breakdown present
	rec = doReq(t, mux, "GET", "/api/mcp-stats?all=true", "", withCookie(adminTok))
	if !strings.Contains(rec.Body.String(), `"per_user":true`) {
		t.Fatalf("admin all stats should be per_user: %s", rec.Body.String())
	}
}
