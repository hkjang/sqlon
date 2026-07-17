package mcp

import (
	"context"
	"encoding/json"
	"net/http"
)

// REST surface for privileged read-only inspection. Write-capable dba*
// methods remain internal executors and are reachable only through an
// approved ChangePlan (/api/changes).

func (s *Server) registerDBAConsole(mux *http.ServeMux) {
	// the console page itself
	mux.HandleFunc("GET /admin/dba-console", s.guardPage(s.serveWebUI("webui/dba-console.html", "text/html; charset=utf-8")))

	// read body helper
	decode := func(w http.ResponseWriter, r *http.Request, dst any) bool {
		if r.Body == nil {
			return true
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil && err.Error() != "EOF" {
			writeAPIError(w, http.StatusBadRequest, err)
			return false
		}
		return true
	}

	// ---- inspection (POST with {profile} for uniformity) ----
	post := func(path string, fn func(ctx context.Context, req dbaReq) map[string]any) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			if !s.requireDBA(w, r) {
				return
			}
			var req dbaReq
			if !decode(w, r, &req) {
				return
			}
			if req.Profile == "" {
				writeAPIError(w, http.StatusBadRequest, errEmpty("profile is required"))
				return
			}
			writeJSON(w, http.StatusOK, fn(r.Context(), req))
		})
	}

	post("/api/dba/overview", func(ctx context.Context, q dbaReq) map[string]any { return s.dbaOverview(ctx, q.Profile) })
	post("/api/dba/users", func(ctx context.Context, q dbaReq) map[string]any { return s.dbaListUsers(ctx, q.Profile) })
	post("/api/dba/databases", func(ctx context.Context, q dbaReq) map[string]any { return s.dbaListDatabases(ctx, q.Profile) })
	post("/api/dba/settings", func(ctx context.Context, q dbaReq) map[string]any { return s.dbaListSettings(ctx, q.Profile, q.Filter) })
	post("/api/dba/sessions", func(ctx context.Context, q dbaReq) map[string]any { return s.dbaListSessions(ctx, q.Profile) })

}

// dbaReq is the union request body for the DBA console endpoints.
type dbaReq struct {
	Profile    string `json:"profile"`
	Filter     string `json:"filter"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	CanLogin   *bool  `json:"can_login"`
	Superuser  *bool  `json:"superuser"`
	CreateDB   *bool  `json:"createdb"`
	CreateRole *bool  `json:"createrole"`
	Confirm    bool   `json:"confirm"`
	Privileges string `json:"privileges"`
	Object     string `json:"object"`
	Grantee    string `json:"grantee"`
	Revoke     bool   `json:"revoke"`
	WithGrant  bool   `json:"with_grant"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Encoding   string `json:"encoding"`
	Parameter  string `json:"parameter"`
	Value      string `json:"value"`
	Scope      string `json:"scope"`
	PID        int64  `json:"pid"`
	CancelOnly bool   `json:"cancel_only"`
	Operation  string `json:"operation"`
	Target     string `json:"target"`
	SQL        string `json:"sql"`
}
