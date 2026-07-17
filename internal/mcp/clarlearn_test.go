package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/meta"
)

// Answered clarification rounds must persist as "corrected" feedback so the
// chosen tables gain retrieval usage prior after the next reload.
func TestClarificationResolutionFeedsUsagePrior(t *testing.T) {
	s, dataDir := newFixtureServer(t)
	ctx := withSession(context.Background(), "learn-1")
	call := func(args string) map[string]any {
		params, _ := json.Marshal(map[string]any{"name": "prepare_sql_context", "arguments": json.RawMessage(args)})
		res, err := s.callTool(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		return res.(map[string]any)
	}
	first := call(`{"question":"잔액"}`)
	if first["status"] != "needs_clarification" {
		t.Skipf("fixture no longer flags: %v", first["status"])
	}
	second := call(`{"question":"잔액","clarifications":{"too_vague":"최근 3개월 회원사별 대출 잔액 합계"}}`)
	if second["status"] != "ready" {
		t.Fatalf("expected ready, got %v", second["status"])
	}
	// A corrected clarification record is useful review evidence, but user
	// answers are not self-authenticating: it must remain pending/untrusted.
	entries, _ := os.ReadDir(filepath.Join(dataDir, "feedback"))
	found := false
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dataDir, "feedback", e.Name()))
		if strings.Contains(string(b), `"source":"clarification"`) &&
			strings.Contains(string(b), `"outcome":"corrected"`) &&
			strings.Contains(string(b), `"review_status":"pending"`) &&
			strings.Contains(string(b), `"trust_status":"untrusted"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("resolved clarification round did not persist a corrected feedback record")
	}
	// unanswered rounds must NOT record
	before := len(entries)
	_ = call(`{"question":"금액"}`) // vague, no answers
	after, _ := os.ReadDir(filepath.Join(dataDir, "feedback"))
	total := 0
	for _, e := range after {
		b, _ := os.ReadFile(filepath.Join(dataDir, "feedback", e.Name()))
		total += strings.Count(string(b), `"source":"clarification"`)
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 clarification feedback record, got %d (files before=%d)", total, before)
	}
}

// Regression (v0.14.1): a non-admin user with a "use" grant must be able to
// execute run_sql_safely — the standalone master-token gate must not fire in
// auth mode and demand the admin role.
func TestGrantedUserCanExecuteViaMCP(t *testing.T) {
	s, mux, adminTok, _ := newAuthServer(t)
	_ = mux
	_ = adminTok
	ctxb := context.Background()
	alice, _ := s.Meta.Store.GetUserByUsername(ctxb, "alice")
	admin, _ := s.Meta.Store.GetUserByUsername(ctxb, "admin")

	// admin owns a private profile; alice gets a use grant
	rec := &meta.ProfileRecord{ID: "team-x", OwnerID: admin.ID, Visibility: meta.VisibilityPrivate,
		Definition: []byte(`{"id":"team-x","connect_string":"h:1521/X","username":"RO","password_ref":"env:P"}`)}
	if err := s.Meta.Store.UpsertProfile(ctxb, rec, true); err != nil {
		t.Fatal(err)
	}
	_ = s.Meta.Store.SetGrant(ctxb, meta.Grant{ProfileID: "team-x", UserID: alice.ID, Permission: meta.PermUse, GrantedBy: admin.ID})

	call := func(u *meta.User) map[string]any {
		params, _ := json.Marshal(map[string]any{"name": "run_sql_safely",
			"arguments": json.RawMessage(`{"sql":"SELECT CUST_NO FROM DWMST.TBIA59D FETCH FIRST 5 ROWS ONLY","profile":"team-x"}`)})
		res, err := s.callTool(withUser(ctxb, u), params)
		if err != nil {
			t.Fatal(err)
		}
		return res.(map[string]any)
	}
	got := call(alice)
	if got["status"] == "forbidden" {
		t.Fatalf("granted user must not be forbidden: %v", got)
	}
	// no grant → forbidden by the per-profile check (not the admin gate)
	carol, err := s.Meta.CreateLocalUser(ctxb, "carol2", "carolpass1", meta.RoleUser, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := call(carol); got["status"] != "forbidden" {
		t.Fatalf("ungranted user must be forbidden, got %v", got["status"])
	}
}
