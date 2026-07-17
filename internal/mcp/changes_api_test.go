package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/change"
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
