package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/meta"
)

// registerAuthAPI wires user administration, MCP key lifecycle, and DB
// profile grant management. Everything here is meta-DB backed; in standalone
// mode the endpoints respond with a clear "auth disabled" error.
func (s *Server) registerAuthAPI(mux *http.ServeMux) {
	// ---- users (admin) ----
	mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		users, err := s.Meta.Store.ListUsers(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
	})
	mux.HandleFunc("POST /api/users", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			Username    string `json:"username"`
			Password    string `json:"password"`
			Role        string `json:"role"`
			DisplayName string `json:"display_name"`
			Email       string `json:"email"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		u, err := s.Meta.CreateLocalUser(r.Context(), req.Username, req.Password, req.Role, req.DisplayName, req.Email)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.adminAudit(r, "user_create", u.Username, nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u})
	})
	mux.HandleFunc("PUT /api/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		u, err := s.Meta.Store.GetUserByID(r.Context(), r.PathValue("id"))
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		var req struct {
			Role        *string `json:"role"`
			IsActive    *bool   `json:"is_active"`
			DisplayName *string `json:"display_name"`
			Email       *string `json:"email"`
			Password    *string `json:"password"` // admin reset (local accounts)
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if req.Role != nil {
			if *req.Role != meta.RoleAdmin && *req.Role != meta.RoleUser && *req.Role != meta.RoleDBA {
				writeAPIError(w, http.StatusBadRequest, errEmpty("role must be admin, dba, or user"))
				return
			}
			// 마지막 admin 강등 방지
			if u.Role == meta.RoleAdmin && *req.Role != meta.RoleAdmin {
				if n, _ := s.countAdmins(r.Context()); n <= 1 {
					writeAPIError(w, http.StatusBadRequest, errEmpty("cannot demote the last admin"))
					return
				}
			}
			u.Role = *req.Role
		}
		if req.IsActive != nil {
			if !*req.IsActive && u.Role == meta.RoleAdmin {
				if n, _ := s.countAdmins(r.Context()); n <= 1 {
					writeAPIError(w, http.StatusBadRequest, errEmpty("cannot deactivate the last admin"))
					return
				}
			}
			u.IsActive = *req.IsActive
		}
		if req.DisplayName != nil {
			u.DisplayName = *req.DisplayName
		}
		if req.Email != nil {
			u.Email = *req.Email
		}
		if req.Password != nil && *req.Password != "" {
			if u.Provider != meta.ProviderLocal {
				writeAPIError(w, http.StatusBadRequest, errEmpty("SSO 계정의 비밀번호는 Keycloak에서 관리됩니다"))
				return
			}
			hash, err := meta.HashPassword(*req.Password)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, err)
				return
			}
			u.PasswordHash = hash
		}
		if err := s.Meta.Store.UpdateUser(r.Context(), u); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		if !u.IsActive {
			_ = s.Meta.Store.RevokeUserSessions(r.Context(), u.ID) // 즉시 세션 차단
		}
		s.adminAudit(r, "user_update", u.Username, nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u})
	})

	// ---- MCP key lifecycle ----
	mux.HandleFunc("GET /api/mcp-keys", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, ok := s.requireActor(w, r)
		if !ok {
			return
		}
		target := actor.ID
		if q := r.URL.Query().Get("user_id"); q != "" || r.URL.Query().Get("all") == "true" {
			if !actor.IsAdmin() {
				writeAPIError(w, http.StatusForbidden, errEmpty("admin role required to list other users' keys"))
				return
			}
			target = q // ""=all when all=true
		}
		keys, err := s.Meta.Store.ListKeys(r.Context(), target)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		now := time.Now()
		out := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyView(k, now))
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": out})
	})
	mux.HandleFunc("POST /api/mcp-keys", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, ok := s.requireActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Name     string `json:"name"`
			TTLHours int    `json:"ttl_hours"` // 0 = 무기한
			UserID   string `json:"user_id"`   // admin이 타 사용자 키 발급 시
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		target := actor.ID
		if req.UserID != "" && req.UserID != actor.ID {
			if !actor.IsAdmin() {
				writeAPIError(w, http.StatusForbidden, errEmpty("admin role required to issue keys for other users"))
				return
			}
			if _, err := s.Meta.Store.GetUserByID(r.Context(), req.UserID); err != nil {
				writeAPIError(w, http.StatusNotFound, err)
				return
			}
			target = req.UserID
		}
		// the master token is not a real account and cannot own keys; it may
		// still mint keys for real users by specifying user_id.
		if target == "master" {
			writeAPIError(w, http.StatusBadRequest, errEmpty("master token cannot own keys; specify user_id for a real user"))
			return
		}
		raw, k, err := s.Meta.CreateMCPKey(r.Context(), target, req.Name, time.Duration(req.TTLHours)*time.Hour)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		s.adminAudit(r, "mcp_key_create", k.KeyPrefix, nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "key": raw, "key_info": keyView(k, time.Now()),
			"notice": "이 키는 지금 한 번만 표시됩니다. 안전한 곳에 보관하세요.",
		})
	})
	mux.HandleFunc("POST /api/mcp-keys/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, k, ok := s.requireKeyAccess(w, r)
		if !ok {
			return
		}
		raw, nk, err := s.Meta.RotateMCPKey(r.Context(), k.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		s.adminAudit(r, "mcp_key_rotate", k.KeyPrefix+" -> "+nk.KeyPrefix+" by "+actorName(actor), nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "key": raw, "key_info": keyView(nk, time.Now()),
			"notice": "기존 키는 즉시 폐기되었습니다. 새 키는 지금 한 번만 표시됩니다.",
		})
	})
	mux.HandleFunc("DELETE /api/mcp-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, k, ok := s.requireKeyAccess(w, r)
		if !ok {
			return
		}
		if err := s.Meta.Store.RevokeKey(r.Context(), k.ID); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.adminAudit(r, "mcp_key_revoke", k.KeyPrefix+" by "+actorName(actor), nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": k.ID})
	})

	// ---- settings (admin) ----
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		view, err := s.Meta.SettingsView(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"settings":    view,
			"sso_enabled": s.OIDC != nil,
			"boot_only": map[string]any{ // 읽기전용 안내
				"meta_db":   "환경변수 JAMYPG_META_DB / -meta-db (부팅 전용)",
				"addr":      "-addr (부팅 전용, 재기동 필요)",
				"transport": "-transport (부팅 전용)",
				"data_dir":  s.cat().DataDir,
			},
		})
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		actor, _ := s.authenticate(r)
		var req map[string]*string // null=삭제, ""=비우기, 값=설정
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		for key, val := range req {
			if val == nil {
				_ = s.Meta.Store.DeleteSetting(r.Context(), key)
				continue
			}
			if err := s.Meta.ApplySetting(r.Context(), key, *val, actorName(actor)); err != nil {
				writeAPIError(w, http.StatusBadRequest, errEmpty("unknown or invalid setting: "+key))
				return
			}
		}
		if err := s.ApplySettings(r.Context()); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		s.adminAudit(r, "settings_update", strings.Join(keysOf(req), ","), nil)
		view, _ := s.Meta.SettingsView(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": view, "sso_enabled": s.OIDC != nil,
			"note": "런타임 적용됨(재기동 불필요): 마스터 토큰·허용 Origin·OIDC(SSO)."})
	})

	// ---- profile grants ----
	mux.HandleFunc("GET /api/db-profiles/{id}/grants", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, rec, ok := s.requireProfileManage(w, r)
		if !ok {
			return
		}
		_ = actor
		grants, err := s.Meta.Store.ListGrants(r.Context(), rec.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		out := make([]map[string]any, 0, len(grants))
		for _, g := range grants {
			name := g.UserID
			if u, err := s.Meta.Store.GetUserByID(r.Context(), g.UserID); err == nil {
				name = u.Username
			}
			out = append(out, map[string]any{"user_id": g.UserID, "username": name,
				"permission": g.Permission, "granted_by": g.GrantedBy, "granted_at": g.GrantedAt})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"profile": rec.ID, "owner_id": rec.OwnerID, "visibility": rec.Visibility, "grants": out,
		})
	})
	mux.HandleFunc("PUT /api/db-profiles/{id}/grants", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, rec, ok := s.requireProfileManage(w, r)
		if !ok {
			return
		}
		var req struct {
			Username   string `json:"username"`
			UserID     string `json:"user_id"`
			Permission string `json:"permission"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if req.Permission != meta.PermUse && req.Permission != meta.PermManage {
			writeAPIError(w, http.StatusBadRequest, errEmpty("permission must be use or manage"))
			return
		}
		target := req.UserID
		if target == "" && req.Username != "" {
			u, err := s.Meta.Store.GetUserByUsername(r.Context(), req.Username)
			if err != nil {
				writeAPIError(w, http.StatusNotFound, errEmpty("user not found: "+req.Username))
				return
			}
			target = u.ID
		}
		if target == "" {
			writeAPIError(w, http.StatusBadRequest, errEmpty("username or user_id is required"))
			return
		}
		if err := s.Meta.Store.SetGrant(r.Context(), meta.Grant{
			ProfileID: rec.ID, UserID: target, Permission: req.Permission, GrantedBy: actorName(actor),
		}); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.adminAudit(r, "profile_grant", rec.ID+" -> "+target+" ("+req.Permission+")", nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	// Lightweight visibility toggle (private <-> shared) that leaves the
	// profile definition untouched — used by the "전체 공개" switch. Only the
	// owner or an admin may change visibility (manage grantees cannot).
	mux.HandleFunc("PUT /api/db-profiles/{id}/visibility", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, ok := s.requireActor(w, r)
		if !ok {
			return
		}
		rec, err := s.Meta.Store.GetProfile(r.Context(), r.PathValue("id"))
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		if !actor.IsAdmin() && rec.OwnerID != actor.ID {
			writeAPIError(w, http.StatusForbidden, errEmpty("only the owner or an admin can change visibility"))
			return
		}
		var req struct {
			Visibility string `json:"visibility"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if req.Visibility != meta.VisibilityPrivate && req.Visibility != meta.VisibilityShared {
			writeAPIError(w, http.StatusBadRequest, errEmpty("visibility must be private or shared"))
			return
		}
		rec.Visibility = req.Visibility
		if err := s.Meta.Store.UpsertProfile(r.Context(), rec, false); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.adminAudit(r, "profile_visibility", rec.ID+" -> "+req.Visibility+" by "+actorName(actor), nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": rec.ID, "visibility": req.Visibility})
	})
	mux.HandleFunc("DELETE /api/db-profiles/{id}/grants/{userID}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		_, rec, ok := s.requireProfileManage(w, r)
		if !ok {
			return
		}
		if err := s.Meta.Store.RemoveGrant(r.Context(), rec.ID, r.PathValue("userID")); err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		s.adminAudit(r, "profile_grant_remove", rec.ID+" -x-> "+r.PathValue("userID"), nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// ---- MCP activity history + stats (personalized; admin sees all) ----
	mux.HandleFunc("GET /api/mcp-activity", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, scope, ok := s.activityScope(w, r)
		if !ok {
			return
		}
		limit := atoiDefault(r.URL.Query().Get("limit"), 200)
		acts, err := s.Meta.Store.ListActivity(r.Context(), meta.ActivityFilter{UserID: scope, Limit: limit})
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"activity": acts, "is_admin": actor.IsAdmin(), "scope": scopeLabel(scope),
		})
	})
	// admin: recurring clarification rounds aggregated from activity — each
	// row is a candidate glossary/metric-dictionary entry that would remove
	// the re-question at the source.
	mux.HandleFunc("GET /api/clarification-suggestions", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		acts, err := s.Meta.Store.ListActivity(r.Context(), meta.ActivityFilter{Limit: 5000})
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"suggestions": clarificationSuggestions(acts)})
	})
	mux.HandleFunc("GET /api/mcp-stats", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) {
			return
		}
		actor, scope, ok := s.activityScope(w, r)
		if !ok {
			return
		}
		// aggregate over a generous recent window
		acts, err := s.Meta.Store.ListActivity(r.Context(), meta.ActivityFilter{UserID: scope, Limit: 5000})
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, computeActivityStats(acts, actor.IsAdmin() && scope == ""))
	})
}

// activityScope resolves whose activity the caller may read. Non-admins are
// pinned to their own user id; admins default to all and may pass ?user= or
// ?all=true. Returns (actor, scopeUserID, ok) where scope "" means all users.
func (s *Server) activityScope(w http.ResponseWriter, r *http.Request) (*meta.User, string, bool) {
	actor, ok := s.requireActor(w, r)
	if !ok {
		return nil, "", false
	}
	if !actor.IsAdmin() {
		return actor, actor.ID, true // own only
	}
	if uid := r.URL.Query().Get("user"); uid != "" {
		return actor, uid, true
	}
	if r.URL.Query().Get("all") == "true" {
		return actor, "", true // all users
	}
	return actor, actor.ID, true // admin default: own, opt into all
}

func scopeLabel(scope string) string {
	if scope == "" {
		return "all"
	}
	return scope
}

func keysOf(m map[string]*string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keyView(k *meta.MCPKey, now time.Time) map[string]any {
	return map[string]any{
		"id": k.ID, "user_id": k.UserID, "name": k.Name, "key_prefix": k.KeyPrefix,
		"status": k.Status(now), "created_at": k.CreatedAt, "expires_at": k.ExpiresAt,
		"last_used_at": k.LastUsedAt, "revoked_at": k.RevokedAt, "rotated_from": k.RotatedFrom,
	}
}

// requireMeta guards meta-only endpoints in standalone mode.
func (s *Server) requireMeta(w http.ResponseWriter) bool {
	if s.Meta == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "auth is disabled: no meta db configured",
			"hint":  "-meta-db 'postgres://user:pw@host:5432/jamypg' (또는 JAMYPG_META_DB)로 기동하면 로그인/사용자/MCP 키/권한 관리가 활성화됩니다.",
		})
		return false
	}
	return true
}

// requireKeyAccess loads the key and checks owner-or-admin.
func (s *Server) requireKeyAccess(w http.ResponseWriter, r *http.Request) (*meta.User, *meta.MCPKey, bool) {
	actor, ok := s.requireActor(w, r)
	if !ok {
		return nil, nil, false
	}
	k, err := s.Meta.Store.GetKeyByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err)
		return nil, nil, false
	}
	if !actor.IsAdmin() && k.UserID != actor.ID {
		writeAPIError(w, http.StatusForbidden, errEmpty("this key belongs to another user"))
		return nil, nil, false
	}
	return actor, k, true
}

// requireProfileManage loads the profile record and checks manage permission.
func (s *Server) requireProfileManage(w http.ResponseWriter, r *http.Request) (*meta.User, *meta.ProfileRecord, bool) {
	actor, ok := s.requireActor(w, r)
	if !ok {
		return nil, nil, false
	}
	rec, err := s.Meta.Store.GetProfile(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err)
		return nil, nil, false
	}
	grants, _ := s.Meta.Store.ListGrants(r.Context(), rec.ID)
	if !meta.CanManageProfile(actor, *rec, grants) {
		writeAPIError(w, http.StatusForbidden, errEmpty("manage permission required (owner, admin, or manage grant)"))
		return nil, nil, false
	}
	return actor, rec, true
}

func (s *Server) countAdmins(ctx context.Context) (int, error) {
	users, err := s.Meta.Store.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if u.Role == meta.RoleAdmin && u.IsActive {
			n++
		}
	}
	return n, nil
}

// mcpListProfiles returns the profiles the MCP caller may use, with masked
// connect info, for the list_db_profiles tool. Works in both modes.
func (s *Server) mcpListProfiles(ctx context.Context) map[string]any {
	profiles := []map[string]any{}
	if !s.authEnabled() {
		list, err := dbconn.LoadProfiles(s.opDir())
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		for _, p := range list {
			profiles = append(profiles, profileSummary(p, ""))
		}
		return map[string]any{
			"profiles":         profiles,
			"driver_available": s.DB.Available(),
			"driver_note":      s.DB.DriverNote(),
			"drivers":          s.DB.DriverCapabilities(),
			"count":            len(profiles),
		}
	}
	u := userFrom(ctx)
	recs, err := s.Meta.Store.ListProfiles(ctx)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, rec := range recs {
		grants, _ := s.Meta.Store.ListGrants(ctx, rec.ID)
		if !meta.CanUseProfile(u, *rec, grants) {
			continue
		}
		var p dbconn.Profile
		if json.Unmarshal(rec.Definition, &p) != nil {
			continue
		}
		p.ID = rec.ID
		perm := meta.PermUse
		if u != nil && (u.IsAdmin() || rec.OwnerID == u.ID) {
			perm = "owner"
		} else if meta.CanManageProfile(u, *rec, grants) {
			perm = meta.PermManage
		}
		profiles = append(profiles, profileSummary(dbconn.ApplyDefaults(p), perm))
	}
	return map[string]any{
		"profiles":         profiles,
		"driver_available": s.DB.Available(),
		"driver_note":      s.DB.DriverNote(),
		"drivers":          s.DB.DriverCapabilities(),
		"count":            len(profiles),
		"note":             "run_sql_safely / explain_sql / run_evaluation 의 profile 인자로 아래 id를 사용하세요.",
	}
}

// usableProfiles returns the raw (defaults-applied) profiles the caller is
// permitted to use, for profile routing. Standalone mode returns all
// db_profiles.json entries; auth mode applies owner/shared/grant checks.
func (s *Server) usableProfiles(ctx context.Context) ([]dbconn.Profile, error) {
	if !s.authEnabled() {
		return dbconn.LoadProfiles(s.opDir())
	}
	u := userFrom(ctx)
	recs, err := s.Meta.Store.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	var out []dbconn.Profile
	for _, rec := range recs {
		grants, _ := s.Meta.Store.ListGrants(ctx, rec.ID)
		if !meta.CanUseProfile(u, *rec, grants) {
			continue
		}
		var p dbconn.Profile
		if json.Unmarshal(rec.Definition, &p) != nil {
			continue
		}
		p.ID = rec.ID
		out = append(out, dbconn.ApplyDefaults(p))
	}
	return out, nil
}

// routeProfile runs the profile router for a SQL string, gated by the same
// per-profile authorization used for execution.
func (s *Server) routeProfile(ctx context.Context, sql string) (dbconn.RouteDecision, error) {
	profs, err := s.usableProfiles(ctx)
	if err != nil {
		return dbconn.RouteDecision{}, err
	}
	return s.DB.RouteProfile(ctx, sql, "", profs), nil
}

// routeResult renders a RouteDecision as an LLM-facing map.
func routeResult(dec dbconn.RouteDecision) map[string]any {
	out := map[string]any{
		"decisive":          dec.Decisive,
		"reason":            dec.Reason,
		"referenced_tables": dec.Tables,
		"dialect":           dec.Dialect,
		"candidates":        dec.Candidates,
	}
	if dec.Selected != "" {
		out["selected_profile"] = dec.Selected
	}
	if len(dec.Excluded) > 0 {
		out["excluded"] = dec.Excluded
	}
	return out
}

// profileSummary is an LLM-friendly, secret-free view of a profile.
func profileSummary(p dbconn.Profile, permission string) map[string]any {
	out := map[string]any{
		"id":                    p.ID,
		"name":                  p.Name,
		"connect_string":        p.ConnectString, // host/service only; credentials are password_ref
		"username":              p.Username,
		"query_timeout_seconds": p.Policy.QueryTimeoutSeconds,
		"default_max_rows":      p.Policy.DefaultMaxRows,
		"max_rows":              p.Policy.MaxRows,
	}
	if permission != "" {
		out["my_permission"] = permission
	}
	return out
}

// ---- profile permission helpers shared with dbapi/MCP tools ----

// canUseProfileID checks use permission; standalone mode always allows (the
// legacy token gate applies instead), stdio (nil user) is local-trusted.
func (s *Server) canUseProfileID(ctx context.Context, u *meta.User, profileID string) error {
	if !s.authEnabled() || u == nil || u.IsAdmin() {
		return nil
	}
	rec, err := s.Meta.Store.GetProfile(ctx, profileID)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return errEmpty("db profile not found: " + profileID)
		}
		return err
	}
	grants, _ := s.Meta.Store.ListGrants(ctx, rec.ID)
	if !meta.CanUseProfile(u, *rec, grants) {
		return errEmpty("no permission to use db profile '" + profileID + "' — 소유자에게 grant(use)를 요청하세요")
	}
	return nil
}

// metaProfileStore adapts the meta DB to the db connector's ProfileStore.
type metaProfileStore struct{ svc *meta.Service }

func (m metaProfileStore) GetProfileByID(ctx context.Context, id string) (dbconn.Profile, error) {
	rec, err := m.svc.Store.GetProfile(ctx, id)
	if err != nil {
		return dbconn.Profile{}, errEmpty("db profile not found: " + id)
	}
	var p dbconn.Profile
	if err := json.Unmarshal(rec.Definition, &p); err != nil {
		return dbconn.Profile{}, err
	}
	p.ID = rec.ID
	return dbconn.ApplyDefaults(p), nil
}

func (m metaProfileStore) ListAllProfiles(ctx context.Context) ([]dbconn.Profile, error) {
	recs, err := m.svc.Store.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]dbconn.Profile, 0, len(recs))
	for _, rec := range recs {
		var p dbconn.Profile
		if json.Unmarshal(rec.Definition, &p) != nil {
			continue
		}
		p.ID = rec.ID
		out = append(out, dbconn.ApplyDefaults(p))
	}
	return out, nil
}

// EnableMeta activates the meta-backed auth subsystem (called by main). It
// snapshots the flag/env-provided values as the immutable default layer for
// settings, so deleting a stored setting reverts to the original boot value
// rather than whatever ApplySettings last wrote.
func (s *Server) EnableMeta(svc *meta.Service, oidc *OIDCProvider) {
	s.Meta = svc
	s.OIDC = oidc
	s.DB.SetProfileStore(metaProfileStore{svc: svc})
	s.bootDefaults = map[string]string{
		meta.SetAdminToken: s.Options.AdminToken,
		meta.SetCacheTTL:   strconv.Itoa(defaultCacheTTLSeconds),
	}
	if len(s.Options.AllowedOrigins) > 0 {
		s.bootDefaults[meta.SetAllowOrigins] = strings.Join(s.Options.AllowedOrigins, ",")
	}
	if oidc != nil {
		s.bootDefaults[meta.SetOIDCIssuer] = oidc.Issuer
		s.bootDefaults[meta.SetOIDCClientID] = oidc.ClientID
		s.bootDefaults[meta.SetOIDCSecret] = oidc.ClientSecret
		s.bootDefaults[meta.SetOIDCRedirect] = oidc.RedirectURL
	}
}

// ApplySettings loads effective settings (stored over bootstrap) and applies
// the runtime-tunable ones: master token, allow-origins, and the OIDC
// provider. Called at startup and after any settings change.
func (s *Server) ApplySettings(ctx context.Context) error {
	if s.Meta == nil {
		return nil
	}
	eff, err := s.Meta.EffectiveSettings(ctx, s.bootDefaults)
	if err != nil {
		return err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.Options.AdminToken = eff[meta.SetAdminToken]
	if v := strings.TrimSpace(eff[meta.SetAllowOrigins]); v != "" {
		parts := strings.Split(v, ",")
		out := parts[:0]
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		s.Options.AllowedOrigins = out
	} else {
		s.Options.AllowedOrigins = nil
	}
	// rebuild OIDC provider when all four values are present, else disable
	iss, cid, sec, red := eff[meta.SetOIDCIssuer], eff[meta.SetOIDCClientID], eff[meta.SetOIDCSecret], eff[meta.SetOIDCRedirect]
	if iss != "" && cid != "" && sec != "" && red != "" {
		s.OIDC = &OIDCProvider{Issuer: iss, ClientID: cid, ClientSecret: sec, RedirectURL: red}
	} else {
		s.OIDC = nil
	}
	// result cache TTL (blank → default, invalid → left unchanged)
	if v := strings.TrimSpace(eff[meta.SetCacheTTL]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			s.queryCache.SetTTL(n)
		}
	} else {
		s.queryCache.SetTTL(defaultCacheTTLSeconds)
	}
	return nil
}

var _ = strings.TrimSpace // keep strings import if unused paths change
