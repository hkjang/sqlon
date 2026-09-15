package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sqlon/internal/meta"
)

// handoffFixture: auth server with one private profile owned by admin and one
// owned by alice, plus a workload audit line so the digest has content.
func handoffFixture(t *testing.T) (*Server, *http.ServeMux, string, string) {
	t.Helper()
	s, mux, adminTok, aliceTok := newAuthServer(t)
	admin, _ := s.Meta.Store.GetUserByUsername(t.Context(), "admin")
	alice, _ := s.Meta.Store.GetUserByUsername(t.Context(), "alice")
	def := `{"name":"n","type":"postgres","connect_string":"127.0.0.1:1/db","username":"u","password_ref":"plain:x"}`
	for _, p := range []struct{ id, owner string }{{"admin-prod", admin.ID}, {"alice-dev", alice.ID}} {
		if err := s.Meta.Store.UpsertProfile(t.Context(), &meta.ProfileRecord{ID: p.id, OwnerID: p.owner, Definition: []byte(def), Visibility: meta.VisibilityPrivate}, true); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(s.opDir(), "audit")
	_ = os.MkdirAll(dir, 0o755)
	line := `{"tool":"run_sql_safely","entry":{"sql_text":"SELECT CUST_NO FROM TS.TBL1 WHERE USE_AMT > 1","sql_hash":"h1","db_profile_id":"alice-dev","elapsed_ms":420,"success":true,"started_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "query-"+time.Now().Format("20060102")+".jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return s, mux, adminTok, aliceTok
}

type claimResp struct {
	Claim       string `json:"claim"`
	Source      string `json:"source"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
	ExpiresAt   string `json:"expires_at"`
	Error       string `json:"error"`
}

func TestHandoffClaimSingleUse(t *testing.T) {
	s, mux, _, aliceTok := handoffFixture(t)

	rec := doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"dba-digest:alice-dev","format":"markdown"}`, withCookie(aliceTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body.String())
	}
	var c claimResp
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	if len(c.Claim) < 22 || c.ContentType != "text/markdown; charset=utf-8" || c.Bytes <= 0 || !strings.HasSuffix(c.Filename, ".md") {
		t.Fatalf("claim shape: %+v", c)
	}
	if c.Source != "http://example.com" { // httptest.NewRequest host, plain http
		t.Fatalf("source = %q", c.Source)
	}
	exp, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil || time.Until(exp) > handoffClaimTTL || time.Until(exp) < handoffClaimTTL-time.Minute {
		t.Fatalf("expires_at = %q (%v)", c.ExpiresAt, err)
	}

	// the claim is the credential: no cookie needed, body is the digest once
	rec = doReq(t, mux, "GET", "/api/v1/handoff/claims/"+c.Claim, "", nil)
	if rec.Code != 200 {
		t.Fatalf("take: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	const cdPrefix = "attachment; filename*=UTF-8''"
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, cdPrefix) || strings.ContainsAny(strings.TrimPrefix(cd, cdPrefix), " \"'") || !strings.HasSuffix(cd, ".md") {
		t.Fatalf("content-disposition = %q", cd)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}
	body := rec.Body.String()
	if rec.Body.Len() != c.Bytes {
		t.Fatalf("bytes %d != announced %d", rec.Body.Len(), c.Bytes)
	}
	for _, want := range []string{"# DBA 다이제스트 — `alice-dev`", "총 쿼리: 1건", "## 많이 조회된 테이블", "TBL1", "출처: sqlon http://example.com"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// second take → 404, indistinguishable from unknown
	if rec = doReq(t, mux, "GET", "/api/v1/handoff/claims/"+c.Claim, "", nil); rec.Code != 404 {
		t.Fatalf("second take = %d", rec.Code)
	}
	if rec = doReq(t, mux, "GET", "/api/v1/handoff/claims/nope", "", nil); rec.Code != 404 {
		t.Fatalf("unknown claim = %d", rec.Code)
	}

	// the claim itself never reaches the audit log; the document does
	files, _ := filepath.Glob(filepath.Join(s.opDir(), "audit", "audit-*.jsonl"))
	var audit strings.Builder
	for _, f := range files {
		b, _ := os.ReadFile(f)
		audit.Write(b)
	}
	if strings.Contains(audit.String(), c.Claim) {
		t.Fatal("claim token leaked into the audit log")
	}
	if !strings.Contains(audit.String(), "handoff_claim_create") || !strings.Contains(audit.String(), "handoff_claim_serve") {
		t.Fatalf("audit missing handoff entries:\n%s", audit.String())
	}
}

func TestHandoffClaimExpires(t *testing.T) {
	s, mux, _, aliceTok := handoffFixture(t)
	now := time.Now()
	s.handoff.now = func() time.Time { return now }

	rec := doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"dba-digest","format":"markdown"}`, withCookie(aliceTok))
	if rec.Code != 201 {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body.String())
	}
	var c claimResp
	_ = json.Unmarshal(rec.Body.Bytes(), &c)

	now = now.Add(handoffClaimTTL + time.Second)
	if rec = doReq(t, mux, "GET", "/api/v1/handoff/claims/"+c.Claim, "", nil); rec.Code != 404 {
		t.Fatalf("expired claim = %d", rec.Code)
	}
	// and it is gone from the store, not merely refused
	s.handoff.mu.Lock()
	n := len(s.handoff.claims)
	s.handoff.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired claims retained: %d", n)
	}
}

func TestHandoffClaimBoundToReadableDocument(t *testing.T) {
	_, mux, adminTok, aliceTok := handoffFixture(t)

	// alice cannot mint a claim for admin's private profile — 404, no claim
	rec := doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"dba-digest:admin-prod","format":"markdown"}`, withCookie(aliceTok))
	if rec.Code != 404 || strings.Contains(rec.Body.String(), `"claim"`) {
		t.Fatalf("other user's document: %d %s", rec.Code, rec.Body.String())
	}
	// unknown profile, unknown resource kind
	for _, res := range []string{"dba-digest:missing", "incident:1", "", "dba-digest:"} {
		rec = doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"`+res+`","format":"markdown"}`, withCookie(adminTok))
		if rec.Code != 404 {
			t.Fatalf("resource %q = %d %s", res, rec.Code, rec.Body.String())
		}
	}
	// only markdown is sent
	rec = doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"dba-digest:alice-dev","format":"docx"}`, withCookie(aliceTok))
	if rec.Code != 400 {
		t.Fatalf("docx = %d", rec.Code)
	}
	// login required
	rec = doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"dba-digest:alice-dev","format":"markdown"}`, nil)
	if rec.Code != 401 {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	// admin reads everything
	rec = doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"dba-digest:alice-dev","format":"markdown"}`, withCookie(adminTok))
	if rec.Code != 201 {
		t.Fatalf("admin issue = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandoffTargetsFromSettings(t *testing.T) {
	_, mux, adminTok, aliceTok := handoffFixture(t)

	get := func(tok string) (int, map[string]any) {
		rec := doReq(t, mux, "GET", "/api/v1/handoff/targets", "", withCookie(tok))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// fresh install: nothing configured → no targets, so no button
	code, out := get(aliceTok)
	if code != 200 {
		t.Fatalf("targets = %d", code)
	}
	if ts, _ := out["targets"].([]any); len(ts) != 0 {
		t.Fatalf("default targets should be empty: %v", out)
	}
	if code, _ = get(""); code != 401 {
		t.Fatalf("anonymous targets = %d", code)
	}

	// admin sets the allow list; kanpic (no markdown), an unknown service and
	// a malformed entry are dropped; the public URL overrides the request host
	body := `{"handoff_targets":"weekly=https://weekly.intra/, ptium=https://ptium.intra, kanpic=https://kanpic.intra, umm=https://umm.intra, muni=ftp://muni, junk, Muni=https://muni.intra:8443",` +
		`"handoff_public_url":"https://sqlon.intra"}`
	if rec := doReq(t, mux, "PUT", "/api/settings", body, withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	_, out = get(aliceTok)
	raw, _ := json.Marshal(out["targets"])
	var got []handoffTarget
	_ = json.Unmarshal(raw, &got)
	want := []handoffTarget{{"muni", "https://muni.intra:8443"}, {"ptium", "https://ptium.intra"}, {"weekly", "https://weekly.intra"}}
	if len(got) != len(want) {
		t.Fatalf("targets = %s", raw)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets = %s", raw)
		}
	}
	if out["source"] != "https://sqlon.intra" || out["format"] != "markdown" {
		t.Fatalf("source/format = %v", out)
	}
	rec := doReq(t, mux, "POST", "/api/v1/handoff/claims", `{"resource":"dba-digest:alice-dev","format":"markdown"}`, withCookie(aliceTok))
	var c claimResp
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	if c.Source != "https://sqlon.intra" {
		t.Fatalf("claim source = %q", c.Source)
	}

	// clearing the setting hides the button again
	if rec := doReq(t, mux, "PUT", "/api/settings", `{"handoff_targets":null,"handoff_public_url":null}`, withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("clear: %d", rec.Code)
	}
	_, out = get(aliceTok)
	if ts, _ := out["targets"].([]any); len(ts) != 0 || out["source"] != "http://example.com" {
		t.Fatalf("after clear: %v", out)
	}
}

func TestHandoffSourceHonorsForwardedHeaders(t *testing.T) {
	_, mux, _, aliceTok := handoffFixture(t)
	hdr := withCookie(aliceTok)
	hdr["X-Forwarded-Proto"] = "https"
	hdr["X-Forwarded-Host"] = "sqlon.intra"
	rec := doReq(t, mux, "GET", "/api/v1/handoff/targets", "", hdr)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["source"] != "https://sqlon.intra" {
		t.Fatalf("source = %v", out["source"])
	}
}

func TestRenderDBADigestMarkdownEscapesCells(t *testing.T) {
	d := map[string]any{
		"profile": "p", "window_days": 7, "slow_ms": 200, "total_queries": 3, "error_rate": 0.5,
		"slow_queries": 1, "latency_p95_ms": int64(10), "latency_max_ms": int64(20), "index_candidate_count": 1,
		"headline": "h", "peak_hour": 14,
		"top_tables":           []countItem{{Key: "a|b", Count: 2}},
		"top_index_candidates": []map[string]any{{"table": "t", "column": "c", "occurrences": 1, "avg_ms": int64(5), "ddl": "CREATE INDEX\n ON t(c)"}},
	}
	md := string(renderDBADigestMarkdown(d, "https://src", time.Date(2026, 9, 16, 3, 0, 0, 0, time.UTC)))
	for _, want := range []string{"| a\\|b | 2 |", "| t | c | 1 | 5 | `CREATE INDEX ON t(c)` |", "피크 시간대: 14시", "오류율: 50.0%", "생성 2026-09-16T03:00:00Z"} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q:\n%s", want, md)
		}
	}
}
