package mcp

import (
	"context"
	"encoding/json"
	"net/http"
)

// REST surface for the DBA console (/admin/dba-console). Every route is gated
// by requireDBA (dba/admin role, or standalone master token) and delegates to
// the same dba* methods the MCP tools use, so the UI and the agent share one
// audited, privileged code path.

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

	// ---- mutation ----
	post("/api/dba/create-user", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaCreateUser(ctx, q.Profile, dbaUserOpts{
			Username: q.Username, Password: q.Password, CanLogin: q.CanLogin,
			Superuser: q.Superuser, CreateDB: q.CreateDB, CreateRole: q.CreateRole,
		})
	})
	post("/api/dba/alter-user", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaAlterUser(ctx, q.Profile, dbaUserOpts{
			Username: q.Username, Password: q.Password, CanLogin: q.CanLogin,
			Superuser: q.Superuser, CreateDB: q.CreateDB, CreateRole: q.CreateRole,
		})
	})
	post("/api/dba/drop-user", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaDropUser(ctx, q.Profile, q.Username, q.Confirm)
	})
	post("/api/dba/grant", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaGrant(ctx, q.Profile, q.Privileges, q.Object, q.Grantee, q.Revoke, q.WithGrant)
	})
	post("/api/dba/create-database", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaCreateDatabase(ctx, q.Profile, q.Name, q.Owner, q.Encoding)
	})
	post("/api/dba/drop-database", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaDropDatabase(ctx, q.Profile, q.Name, q.Confirm)
	})
	post("/api/dba/set-parameter", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaSetParameter(ctx, q.Profile, q.Parameter, q.Value, q.Scope)
	})
	post("/api/dba/terminate-session", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaTerminateSession(ctx, q.Profile, q.PID, q.CancelOnly)
	})
	post("/api/dba/maintenance", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaRunMaintenance(ctx, q.Profile, q.Operation, q.Target)
	})
	post("/api/dba/execute", func(ctx context.Context, q dbaReq) map[string]any {
		return s.dbaExecute(ctx, q.Profile, q.SQL, q.Confirm)
	})
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
