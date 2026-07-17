package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"sqlon/internal/meta"
)

func TestStandaloneHTTPDBProfileToolsHonorAdminToken(t *testing.T) {
	srv, _ := newFixtureServer(t)
	srv.Options.AdminToken = "sekrit"
	mux := http.NewServeMux()
	srv.Register(mux)

	tests := []struct {
		name string
		tool string
		args string
	}{
		{name: "safe execution", tool: "run_sql_safely", args: `{"sql":"SELECT T1.CUST_NO FROM TS.TBL1 T1","profile":"dev-01"}`},
		{name: "live explain", tool: "explain_sql", args: `{"sql":"SELECT T1.CUST_NO FROM TS.TBL1 T1","profile":"dev-01"}`},
		{name: "execution evaluation", tool: "run_evaluation", args: `{"golden_path":"missing-golden.json","profile":"dev-01"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			without := callMCPToolHTTP(t, mux, tt.tool, tt.args, map[string]string{"Accept": "application/json"})
			if !strings.Contains(without, `"status":"forbidden"`) ||
				!strings.Contains(without, "requires the admin token") {
				t.Fatalf("profile DB access must be token-gated: %s", without)
			}

			wrong := callMCPToolHTTP(t, mux, tt.tool, tt.args, map[string]string{"Accept": "application/json", "X-Admin-Token": "wrong"})
			if !strings.Contains(wrong, `"status":"forbidden"`) {
				t.Fatalf("wrong token must be rejected: %s", wrong)
			}

			withToken := callMCPToolHTTP(t, mux, tt.tool, tt.args, map[string]string{"Accept": "application/json", "X-Admin-Token": "sekrit"})
			if strings.Contains(withToken, "requires the admin token") {
				t.Fatalf("valid token must pass the central gate: %s", withToken)
			}
		})
	}
}

func TestStandaloneHTTPDBProfileToolsWithoutConfiguredTokenRemainCompatible(t *testing.T) {
	srv, _ := newFixtureServer(t)
	mux := http.NewServeMux()
	srv.Register(mux)

	tests := []struct {
		tool string
		args string
	}{
		{tool: "run_sql_safely", args: `{"sql":"SELECT T1.CUST_NO FROM TS.TBL1 T1","profile":"dev-01"}`},
		{tool: "explain_sql", args: `{"sql":"SELECT T1.CUST_NO FROM TS.TBL1 T1","profile":"dev-01"}`},
		{tool: "run_evaluation", args: `{"golden_path":"missing-golden.json","profile":"dev-01"}`},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got := callMCPToolHTTP(t, mux, tt.tool, tt.args, map[string]string{"Accept": "application/json"})
			if strings.Contains(got, "requires the admin token") {
				t.Fatalf("token-unconfigured standalone HTTP compatibility regressed: %s", got)
			}
		})
	}
}

func TestDBProfileToolGateCompatibility(t *testing.T) {
	s, _ := newFixtureServer(t)
	s.Options.AdminToken = "sekrit"
	args := json.RawMessage(`{"profile":"dev-01"}`)

	// A direct/local context is the path used by stdio and remains trusted.
	if denied, err := s.authorizeDBProfileTool(context.Background(), "explain_sql", args); err != nil || denied != nil {
		t.Fatalf("stdio/local context must remain trusted: denied=%v err=%v", denied, err)
	}
	if denied, err := s.authorizeDBProfileTool(withHTTPAdmin(context.Background(), true), "explain_sql", args); err != nil || denied != nil {
		t.Fatalf("valid standalone HTTP token must pass: denied=%v err=%v", denied, err)
	}
	if denied, err := s.authorizeDBProfileTool(withHTTPAdmin(context.Background(), false), "explain_sql", args); err != nil || denied == nil {
		t.Fatalf("missing standalone HTTP token must fail: denied=%v err=%v", denied, err)
	}

	// No profile means no external DB access. Retrieval-only evaluation is
	// likewise catalog-local and remains available without the token.
	if denied, err := s.authorizeDBProfileTool(withHTTPAdmin(context.Background(), false), "run_sql_safely", nil); err != nil || denied != nil {
		t.Fatalf("profile-less dry run must remain open: denied=%v err=%v", denied, err)
	}
	retrieval := json.RawMessage(`{"profile":"dev-01","retrieval":true}`)
	if denied, err := s.authorizeDBProfileTool(withHTTPAdmin(context.Background(), false), "run_evaluation", retrieval); err != nil || denied != nil {
		t.Fatalf("retrieval-only evaluation must remain open: denied=%v err=%v", denied, err)
	}

	// Meta auth must continue into owner/grant/shared profile ACL checks instead
	// of being converted into a standalone admin-only policy.
	s.EnableMeta(meta.NewService(meta.NewMemStore()), nil)
	if denied, err := s.authorizeDBProfileTool(withHTTPAdmin(context.Background(), false), "run_evaluation", args); err != nil || denied != nil {
		t.Fatalf("meta mode must defer to profile ACL: denied=%v err=%v", denied, err)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func callMCPToolHTTP(t *testing.T, mux *http.ServeMux, tool, args string, headers map[string]string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":` +
		strconvQuote(tool) + `,"arguments":` + args + `}}`
	rec := doReq(t, mux, http.MethodPost, "/mcp", body, headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
