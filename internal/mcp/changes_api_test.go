package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/change"
	"sqlon/internal/meta"
)

func newChangeMux(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	s, dir := newFixtureServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	return mux, dir
}

const changePlanBody = `{
  "id": "chg-http-1",
  "profile_id": "prod-pg",
  "target": "public.orders",
  "reason": "인덱스 추가로 조회 지연 개선",
  "risk": "medium",
  "steps": [
    {"order": 1, "command": "CREATE INDEX idx ON orders(created_at)", "verification": "SELECT 1", "compensation": "DROP INDEX idx"}
  ]
}`

func TestChangeAPILifecycleAndPersistence(t *testing.T) {
	mux, dir := newChangeMux(t)

	rec := doReq(t, mux, "POST", "/api/changes", changePlanBody, map[string]string{"Idempotency-Key": "req-1"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// Idempotent replay returns the same plan instead of a duplicate error.
	rec = doReq(t, mux, "POST", "/api/changes", changePlanBody, map[string]string{"Idempotency-Key": "req-1"})
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"chg-http-1"`) {
		t.Fatalf("idempotent create: %d %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, mux, "GET", "/api/changes", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"chg-http-1"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, mux, "POST", "/api/changes/chg-http-1/submit", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"review_required"`) {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "POST", "/api/changes/chg-http-1/approve", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"approved"`) {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "POST", "/api/changes/chg-http-1/cancel", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cancelled"`) {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	// Rollback of a plan that never executed must be refused.
	rec = doReq(t, mux, "POST", "/api/changes/chg-http-1/rollback", "", nil)
	if rec.Code != 400 {
		t.Fatalf("rollback of unexecuted plan should be 400, got %d %s", rec.Code, rec.Body.String())
	}

	// The full lifecycle must be durable: a plan file exists on disk and a new
	// server over the same data dir restores the cancelled state.
	entries, err := os.ReadDir(filepath.Join(dir, "changes"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no persisted change plans: %v", err)
	}
	restarted, err := change.NewServiceWithStore(change.NewFileStore(filepath.Join(dir, "changes")))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	p, ok := restarted.Get("chg-http-1")
	if !ok || string(p.State) != "cancelled" || len(p.Approvals) != 1 {
		t.Fatalf("restored plan diverged: ok=%v state=%s approvals=%d", ok, p.State, len(p.Approvals))
	}
}

// A critical plan needs two approvals. Over REST each approval must be
// attributed to the authenticated user — not a shared "dba" placeholder —
// otherwise the second approver is rejected as "already approved" and the
// plan can never leave review_required.
func TestChangeAPIApprovalActorIsAuthenticatedUser(t *testing.T) {
	s, mux, _, aliceTok := newAuthServer(t)
	login := func(u, p string) string {
		t.Helper()
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
	for _, u := range []string{"dan", "erin"} {
		if _, err := s.Meta.CreateLocalUser(t.Context(), u, u+"pass1", meta.RoleDBA, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	danTok, erinTok := login("dan", "danpass1"), login("erin", "erinpass1")

	body := strings.Replace(changePlanBody, `"risk": "medium"`, `"risk": "critical"`, 1)
	if rec := doReq(t, mux, "POST", "/api/changes", body, nil); rec.Code != 401 {
		t.Fatalf("unauthenticated create should be 401, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "POST", "/api/changes", body, withCookie(aliceTok)); rec.Code != 403 {
		t.Fatalf("plain user create should be 403, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "POST", "/api/changes", body, withCookie(danTok)); rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "POST", "/api/changes/chg-http-1/submit", "", withCookie(danTok)); rec.Code != 200 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data change.Plan `json:"data"`
	}
	rec := doReq(t, mux, "POST", "/api/changes/chg-http-1/approve", "", withCookie(danTok))
	if rec.Code != 200 {
		t.Fatalf("first approve: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Data.State != change.ReviewRequired || len(resp.Data.Approvals) != 1 || resp.Data.Approvals[0].Actor != "dan" {
		t.Fatalf("first approval must be attributed to dan and keep the plan in review: %s", rec.Body.String())
	}
	// The same approver cannot count twice.
	rec = doReq(t, mux, "POST", "/api/changes/chg-http-1/approve", "", withCookie(danTok))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "already approved") {
		t.Fatalf("repeat approval by dan should be refused: %d %s", rec.Code, rec.Body.String())
	}
	// A second, distinct DBA completes the quorum.
	rec = doReq(t, mux, "POST", "/api/changes/chg-http-1/approve", "", withCookie(erinTok))
	if rec.Code != 200 {
		t.Fatalf("second approve: %d %s", rec.Code, rec.Body.String())
	}
	resp.Data = change.Plan{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Data.State != change.Approved || len(resp.Data.Approvals) != 2 || resp.Data.Approvals[1].Actor != "erin" {
		t.Fatalf("second approval by erin should approve the plan: %s", rec.Body.String())
	}
}

// readAuditEntries returns every entry written to the audit JSONL under the
// server's data dir, in append order.
func readAuditEntries(t *testing.T, dir string) []map[string]any {
	t.Helper()
	path, err := auditFilePath(dir)
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestRESTAuditNamesAuthenticatedActor(t *testing.T) {
	s, mux, _, _ := newAuthServer(t)
	if _, err := s.Meta.CreateLocalUser(t.Context(), "dan", "danpass1", meta.RoleDBA, "", ""); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, mux, "POST", "/auth/login", `{"username":"dan","password":"danpass1"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login dan: %d %s", rec.Code, rec.Body.String())
	}
	var danTok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == meta.SessionCookie {
			danTok = c.Value
		}
	}
	admin, _ := s.Meta.Store.GetUserByUsername(t.Context(), "admin")
	// Dialect lookup only needs the profile record, not a live connection.
	rec0 := &meta.ProfileRecord{ID: "prod-pg", OwnerID: admin.ID, Visibility: meta.VisibilityShared,
		Definition: []byte(`{"name":"prod","type":"postgres","connect_string":"127.0.0.1:1/db","username":"u","password_ref":"plain:x"}`)}
	if err := s.Meta.Store.UpsertProfile(t.Context(), rec0, true); err != nil {
		t.Fatal(err)
	}

	rec = doReq(t, mux, "POST", "/api/changes/generate",
		`{"profile":"prod-pg","action":"create_index","args":{"table":"public.orders","columns":["created_at"]}}`, withCookie(danTok))
	if rec.Code != 201 {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body.String())
	}

	var found map[string]any
	for _, e := range readAuditEntries(t, s.opDir()) {
		if e["tool"] == "admin:change_generate" {
			found = e
		}
	}
	if found == nil {
		t.Fatal("no admin:change_generate audit entry written")
	}
	if found["actor"] != "dan" {
		t.Fatalf("REST audit must name the authenticated user as actor, got %v", found)
	}
	if strings.Contains(found["detail"].(string), "CREATE INDEX") {
		t.Fatalf("audit detail must not carry generated SQL: %v", found)
	}
}

func TestRESTAuditOmitsActorInStandaloneMode(t *testing.T) {
	s, mux := newAdminMux(t, "")
	rec := doReq(t, mux, "PUT", "/api/datasets/glossary",
		`{"entries":[{"term":"고객","synonyms":["cust_no"],"category":"entity"}]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	var found map[string]any
	for _, e := range readAuditEntries(t, s.opDir()) {
		if e["tool"] == "admin:put_dataset" {
			found = e
		}
	}
	if found == nil {
		t.Fatal("no admin:put_dataset audit entry written")
	}
	if _, has := found["actor"]; has {
		t.Fatalf("standalone mode has no authenticated user; actor must be absent: %v", found)
	}
}

func TestChangeTemplateRejectsIrreversibleAndPasswordOverHTTP(t *testing.T) {
	// No DB profile is configured in the fixture, so the dialect lookup fails
	// before any generation — assert the endpoint is wired and rejects cleanly
	// rather than 404/500.
	mux, _ := newChangeMux(t)
	rec := doReq(t, mux, "POST", "/api/changes/template", `{"profile":"missing","action":"create_user","args":{}}`, nil)
	if rec.Code != 400 {
		t.Fatalf("template with unknown profile should be 400, got %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "404") {
		t.Fatalf("template endpoint not registered: %s", rec.Body.String())
	}
}

func TestChangeAPIRejectsInvalidPlan(t *testing.T) {
	mux, _ := newChangeMux(t)

	var incomplete map[string]any
	if err := json.Unmarshal([]byte(changePlanBody), &incomplete); err != nil {
		t.Fatal(err)
	}
	incomplete["steps"] = []map[string]any{{"order": 1, "command": "DROP TABLE x"}}
	body, _ := json.Marshal(incomplete)
	rec := doReq(t, mux, "POST", "/api/changes", string(body), nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "compensation") {
		t.Fatalf("step without verification/compensation accepted: %d %s", rec.Code, rec.Body.String())
	}
}
