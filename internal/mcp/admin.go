package mcp

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/catalog"
	"sqlon/internal/change"
	"sqlon/internal/dbconn"
	"sqlon/internal/fleet"
)

//go:embed webui
var webuiFS embed.FS

// registerAdmin wires the REST management API (consumed by /admin and any
// HTTP client), the Swagger UI at /docs, and the admin console at /admin.
// The REST layer reuses the exact same dataset operations as the MCP tools
// (put_dataset / remove_dataset / reload_catalog), so behavior — validation,
// backup, hot-swap, rollback — is identical on both surfaces.
func (s *Server) registerAdmin(mux *http.ServeMux) {
	// static UI
	mux.HandleFunc("GET /{$}", s.guardPage(s.serveWebUI("webui/fleet.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /welcome", s.serveWebUI("webui/landing.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /admin/nav.js", s.serveWebUI("webui/nav.js", "application/javascript"))
	mux.HandleFunc("GET /admin/onboarding.md", s.serveWebUI("webui/onboarding.md", "text/markdown; charset=utf-8"))
	mux.HandleFunc("GET /admin/logo-transparent.png", s.serveWebUI("webui/logo-transparent.png", "image/png"))
	mux.HandleFunc("GET /favicon.ico", s.serveWebUI("webui/favicon.ico", "image/x-icon"))
	mux.HandleFunc("GET /admin/ask", s.guardPage(s.serveWebUI("webui/ask.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/history", s.guardPage(s.serveWebUI("webui/history.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/stats", s.guardPage(s.serveWebUI("webui/stats.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin", s.guardPage(s.serveWebUI("webui/admin.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/editor", s.guardPage(s.serveWebUI("webui/editor.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/db", s.guardPage(s.serveWebUI("webui/db.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/sessions", s.guardPage(s.serveWebUI("webui/sessions.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/workload", s.guardPage(s.serveWebUI("webui/workload.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/reviews", s.guardPage(s.serveWebUI("webui/reviews.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/quality", s.guardPage(s.serveWebUI("webui/quality.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/openmetadata", s.guardPage(s.serveWebUI("webui/openmetadata.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/profile-catalogs", s.guardPage(s.serveWebUI("webui/profile-catalogs.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/dba", s.guardPage(s.serveWebUI("webui/dba.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/users", s.guardAdminPage(s.serveWebUI("webui/users.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/keys", s.guardPage(s.serveWebUI("webui/keys.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /admin/settings", s.guardAdminPage(s.serveWebUI("webui/settings.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /docs", s.serveWebUI("webui/docs.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /docs/swagger-ui.css", s.serveWebUI("webui/swagger-ui.css", "text/css"))
	mux.HandleFunc("GET /docs/swagger-ui-bundle.js", s.serveWebUI("webui/swagger-ui-bundle.js", "application/javascript"))
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAPISpec))
	})

	// read-only API
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.cat().Health())
	})
	// Fleet APIs and MCP tools share the same permission-filtered service path.
	mux.HandleFunc("GET /api/fleet/instances", func(w http.ResponseWriter, r *http.Request) {
		profiles, _, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, fleet.New(s.DB).InventoryProfiles(profiles))
	})
	// A failed target never prevents other profile results from being returned.
	mux.HandleFunc("GET /api/fleet/health", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, fleet.NewWithOperations(s.DB, s.Collector).HealthProfiles(ctx, profiles))
	})
	// Session and lock observation use fixed engine-native system queries. The
	// profile is resolved only from the caller's permission-filtered fleet.
	mux.HandleFunc("GET /api/observability/sessions", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		profile, ok := allowedProfile(profiles, r.URL.Query().Get("profile"))
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
			return
		}
		writeJSON(w, http.StatusOK, s.Observability.Sessions(ctx, profile))
	})
	mux.HandleFunc("GET /api/observability/locks", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		profile, ok := allowedProfile(profiles, r.URL.Query().Get("profile"))
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
			return
		}
		writeJSON(w, http.StatusOK, s.Observability.Locks(ctx, profile))
	})
	mux.HandleFunc("GET /api/observability/replication", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		profile, ok := allowedProfile(profiles, r.URL.Query().Get("profile"))
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
			return
		}
		writeJSON(w, http.StatusOK, s.Observability.Replication(ctx, profile))
	})
	for path, kind := range map[string]string{
		"/api/observability/workload": "workload",
		"/api/observability/top-sql":  "top_sql",
		"/api/observability/capacity": "capacity",
	} {
		viewKind := kind
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if _, ok := s.requireQueryActor(w, r); !ok {
				return
			}
			profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
			if !ok {
				return
			}
			profile, ok := allowedProfile(profiles, r.URL.Query().Get("profile"))
			if !ok {
				writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
				return
			}
			fresh := strings.EqualFold(r.URL.Query().Get("fresh"), "true")
			writeJSON(w, http.StatusOK, operationalView(viewKind, s.operationalSnapshot(ctx, profile, fresh)))
		})
	}
	mux.HandleFunc("GET /api/observability/history", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		profile, ok := allowedProfile(profiles, r.URL.Query().Get("profile"))
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
			return
		}
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		if hours <= 0 || hours > 24*90 {
			hours = 24
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		history, warnings, err := s.Collector.History(ctx, profile.ID, time.Now().UTC().Add(-time.Duration(hours)*time.Hour), limit)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		traceID := "history-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if len(history) > 0 {
			traceID = history[0].TraceID
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": history, "warnings": warnings, "limitations": []string{}, "collected_at": time.Now().UTC(), "trace_id": traceID})
	})
	mux.HandleFunc("POST /api/collector/run", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, s.Collector.CollectAll(ctx, profiles, true))
	})
	// Change management is intentionally separate from the legacy DBA executor.
	// Plan creation and approval are shared service calls; no endpoint here can
	// execute privileged SQL.
	mux.HandleFunc("GET /api/changes", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": s.Changes.List(), "collected_at": time.Now().UTC()})
	})
	mux.HandleFunc("POST /api/changes", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		var p change.Plan
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.Changes.Create(p, r.Header.Get("Idempotency-Key"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"status": "ok", "data": created, "collected_at": time.Now().UTC()})
	})
	mux.HandleFunc("GET /api/changes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		p, ok := s.Changes.Get(r.PathValue("id"))
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("change plan not found"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": p})
	})
	mux.HandleFunc("POST /api/changes/{id}/submit", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		p, err := s.Changes.Submit(r.PathValue("id"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": p})
	})
	mux.HandleFunc("POST /api/changes/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		actor := "dba"
		if u := userFrom(r.Context()); u != nil {
			actor = u.Username
		}
		p, err := s.Changes.Approve(r.PathValue("id"), actor)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": p})
	})
	mux.HandleFunc("POST /api/changes/{id}/execute", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		id := r.PathValue("id")
		approved, ok := s.Changes.Get(id)
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("change plan not found"))
			return
		}
		if approved.RequiredApprovals > 0 {
			approvalID := strings.TrimSpace(r.Header.Get("X-Approval-ID"))
			valid := false
			for _, approval := range approved.Approvals {
				if subtle.ConstantTimeCompare([]byte(approval.ID), []byte(approvalID)) == 1 {
					valid = true
					break
				}
			}
			if !valid {
				writeAPIError(w, http.StatusForbidden, errEmpty("a valid X-Approval-ID is required"))
				return
			}
		}
		p, err := s.Changes.Execute(r.Context(), id, approvedChangeRunner{server: s})
		actor := "dba"
		if u := userFrom(r.Context()); u != nil {
			actor = u.Username
		}
		audit := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "tool": "sqlon:change_execute", "change_id": id, "actor": actor, "db_profile_id": p.ProfileID}
		if err != nil {
			audit["is_error"] = true
			audit["error"] = err.Error()
			s.appendAudit(audit)
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.appendAudit(audit)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": p, "collected_at": time.Now().UTC()})
	})
	// Rollback runs the approved plan's compensation steps in reverse. It is
	// only reachable from rollback_required, i.e. after an approved execution
	// failed — so no separate approval id is required, matching MCP semantics.
	mux.HandleFunc("POST /api/changes/{id}/rollback", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		id := r.PathValue("id")
		p, err := s.Changes.Rollback(r.Context(), id, approvedChangeRunner{server: s})
		actor := "dba"
		if u := userFrom(r.Context()); u != nil {
			actor = u.Username
		}
		audit := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "tool": "sqlon:change_rollback", "change_id": id, "actor": actor, "db_profile_id": p.ProfileID}
		if err != nil {
			audit["is_error"] = true
			audit["error"] = err.Error()
			s.appendAudit(audit)
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.appendAudit(audit)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": p, "collected_at": time.Now().UTC()})
	})
	mux.HandleFunc("POST /api/changes/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		p, err := s.Changes.Cancel(r.PathValue("id"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "data": p})
	})
	mux.HandleFunc("GET /api/metadata/quality", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("gate") == "true" {
			writeJSON(w, http.StatusOK, s.cat().QualityGate())
			return
		}
		writeJSON(w, http.StatusOK, s.cat().QualityReport())
	})
	mux.HandleFunc("POST /api/metadata/suggest", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tables []string `json:"tables"`
			Kinds  []string `json:"kinds"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		writeJSON(w, http.StatusOK, s.cat().SuggestSemanticMetadata(req.Tables, req.Kinds))
	})
	mux.HandleFunc("POST /api/metadata/candidates", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tables []string `json:"tables"`
			Kinds  []string `json:"kinds"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		writeJSON(w, http.StatusOK, s.cat().SuggestModelCandidates(req.Tables, req.Kinds))
	})
	mux.HandleFunc("GET /api/metadata/digest", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.MetadataDigest())
	})
	mux.HandleFunc("GET /api/openmetadata/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.omStatus(r.Context()))
	})
	mux.HandleFunc("GET /api/openmetadata/config", func(w http.ResponseWriter, r *http.Request) {
		url, token, source := s.omConfig()
		writeJSON(w, http.StatusOK, map[string]any{"url": url, "has_token": token != "", "source": source})
	})
	mux.HandleFunc("POST /api/openmetadata/test", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, s.omTestConfig(r.Context(), req.URL, req.Token))
	})
	mux.HandleFunc("PUT /api/openmetadata/config", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		if err := s.saveOMConfig(req.URL, req.Token); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.adminAudit(r, "openmetadata.config", s.reviewerFromRequest(r), nil)
		writeJSON(w, http.StatusOK, s.omStatus(r.Context()))
	})
	mux.HandleFunc("POST /api/openmetadata/import", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope           string `json:"scope"`
			MaxTables       int    `json:"max_tables"`
			IncludeGlossary *bool  `json:"include_glossary"`
			Apply           bool   `json:"apply"`
			ToReview        bool   `json:"to_review"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		if (req.Apply || req.ToReview) && !s.requireAdmin(w, r) {
			return
		}
		includeGlossary := req.IncludeGlossary == nil || *req.IncludeGlossary
		res := s.omImport(r.Context(), req.Scope, req.MaxTables, includeGlossary, req.Apply, req.ToReview)
		if req.Apply || req.ToReview {
			s.adminAudit(r, "openmetadata.import", s.reviewerFromRequest(r), nil)
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/openmetadata/drift", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope     string `json:"scope"`
			MaxTables int    `json:"max_tables"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		writeJSON(w, http.StatusOK, s.omDrift(r.Context(), req.Scope, req.MaxTables))
	})
	mux.HandleFunc("POST /api/openmetadata/lineage", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope     string `json:"scope"`
			MaxTables int    `json:"max_tables"`
			DryRun    *bool  `json:"dry_run"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		dryRun := req.DryRun == nil || *req.DryRun
		if !dryRun && !s.requireAdmin(w, r) {
			return
		}
		res := s.omExportLineage(r.Context(), req.Scope, req.MaxTables, dryRun)
		if !dryRun {
			s.adminAudit(r, "openmetadata.lineage", s.reviewerFromRequest(r), nil)
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/openmetadata/export", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope     string `json:"scope"`
			MaxTables int    `json:"max_tables"`
			DryRun    *bool  `json:"dry_run"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		dryRun := req.DryRun == nil || *req.DryRun
		if !dryRun && !s.requireAdmin(w, r) {
			return
		}
		res := s.omExport(r.Context(), req.Scope, req.MaxTables, dryRun)
		if !dryRun {
			s.adminAudit(r, "openmetadata.export", s.reviewerFromRequest(r), nil)
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/profile-catalogs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.listProfileCatalogs(r.Context()))
	})
	mux.HandleFunc("POST /api/profile-catalogs/{profile}/openmetadata-import", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		profile := r.PathValue("profile")
		var req struct {
			Scope string `json:"scope"`
			Apply bool   `json:"apply"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		res := s.omImportToProfile(r.Context(), profile, req.Scope, req.Apply)
		if req.Apply {
			s.adminAudit(r, "profile_catalog.openmetadata_import", profile, nil)
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/profile-catalogs/build-all", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			Profiles []string `json:"profiles"`
			Prune    bool     `json:"prune"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		res := s.buildAllProfileCatalogs(r.Context(), req.Profiles, req.Prune)
		s.adminAudit(r, "profile_catalog.build_all", "", nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/profile-catalogs/active", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.activeCatalogInfo())
	})
	mux.HandleFunc("POST /api/profile-catalogs/active", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			Profile string `json:"profile"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		res := s.setActiveCatalog(req.Profile)
		s.adminAudit(r, "catalog.activate", req.Profile, nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/profile-catalogs/{profile}", func(w http.ResponseWriter, r *http.Request) {
		profile := r.PathValue("profile")
		if err := s.canUseProfileID(r.Context(), userFrom(r.Context()), profile); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.getProfileCatalog(profile))
	})
	mux.HandleFunc("GET /api/profile-catalogs/{profile}/schemas", func(w http.ResponseWriter, r *http.Request) {
		profile := r.PathValue("profile")
		if err := s.canUseProfileID(r.Context(), userFrom(r.Context()), profile); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		schemas, err := s.metasyncService().DiscoverSchemas(r.Context(), profile)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"profile": profile, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"profile": profile, "schemas": schemas, "count": len(schemas)})
	})
	mux.HandleFunc("POST /api/profile-catalogs/{profile}/build", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		profile := r.PathValue("profile")
		var req struct {
			Schemas []string `json:"schemas"`
			Prune   bool     `json:"prune"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		res := s.buildProfileCatalog(r.Context(), profile, req.Schemas, req.Prune)
		s.adminAudit(r, "profile_catalog.build", profile, nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/profile-catalogs/{profile}/dataset/{name}", func(w http.ResponseWriter, r *http.Request) {
		profile := r.PathValue("profile")
		if err := s.canUseProfileID(r.Context(), userFrom(r.Context()), profile); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.getProfileDataset(profile, r.PathValue("name")))
	})
	mux.HandleFunc("PUT /api/profile-catalogs/{profile}/dataset/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		profile, name := r.PathValue("profile"), r.PathValue("name")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		res := s.putProfileDataset(profile, name, body)
		s.adminAudit(r, "profile_catalog.put_dataset", profile+"/"+name, nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/audit/verify", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, s.VerifyAuditChain(r.URL.Query().Get("day")))
	})
	mux.HandleFunc("GET /api/metadata/impact", func(w http.ResponseWriter, r *http.Request) {
		table := r.URL.Query().Get("table")
		column := r.URL.Query().Get("column")
		if table == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "table query parameter is required"})
			return
		}
		writeJSON(w, http.StatusOK, s.cat().AnalyzeImpact(table, column))
	})
	mux.HandleFunc("GET /api/reviews", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var tables, kinds []string
		if v := q.Get("tables"); v != "" {
			tables = splitCSV(v)
		}
		if v := q.Get("kinds"); v != "" {
			kinds = splitCSV(v)
		}
		writeJSON(w, http.StatusOK, s.cat().ReviewCandidates(tables, kinds, q.Get("status")))
	})
	mux.HandleFunc("GET /api/reviews/apply", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.cat().ApprovedOverrides())
	})
	mux.HandleFunc("POST /api/reviews/apply", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		res, _ := s.applyApprovedCandidates()
		s.adminAudit(r, "reviews.apply", s.reviewerFromRequest(r), nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/golden/candidates", func(w http.ResponseWriter, r *http.Request) {
		limit := 0
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		writeJSON(w, http.StatusOK, s.cat().SuggestGoldenFromFeedback(limit))
	})
	mux.HandleFunc("POST /api/golden/promote", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			FeedbackIDs []string `json:"feedback_ids"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		res := s.cat().PromoteGolden(req.FeedbackIDs, time.Now())
		if applied, _ := res["applied"].(int); applied > 0 {
			// meta-DB mode: persist before reload or the promotion is reverted
			if err := s.persistDatasetsToDB("golden_queries.json"); err != nil {
				res["persist_error"] = "file applied but meta DB write failed: " + err.Error()
				writeJSON(w, http.StatusOK, res)
				return
			}
			if reload, err := s.reloadCatalog(); err == nil {
				res["reloaded"] = reload
			} else {
				res["reload_error"] = err.Error()
			}
		}
		s.adminAudit(r, "golden.promote", s.reviewerFromRequest(r), nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/reviews/decide", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			Decisions []catalog.DecideCandidate `json:"decisions"`
			Reviewer  string                    `json:"reviewer"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		reviewer := req.Reviewer
		if reviewer == "" {
			reviewer = s.reviewerFromRequest(r)
		}
		res := s.cat().DecideCandidates(req.Decisions, reviewer, time.Now())
		s.adminAudit(r, "reviews.decide", reviewer, nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/datasets", func(w http.ResponseWriter, _ *http.Request) {
		storage := "file"
		if s.datasetsInDB() {
			storage = "postgres"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data_dir": s.cat().DataDir,
			"datasets": s.cat().DatasetStatus(),
			"storage":  storage, // file | postgres (meta DB is source of truth)
		})
	})
	mux.HandleFunc("GET /api/datasets/{name}", func(w http.ResponseWriter, r *http.Request) {
		rows := 5
		if v := r.URL.Query().Get("sample_rows"); v != "" {
			rows = atoiDefault(v, 5)
		}
		res, err := s.cat().DatasetSample(r.PathValue("name"), rows)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/datasets/{name}/content", func(w http.ResponseWriter, r *http.Request) {
		d, b, err := catalog.DatasetContent(s.cat().DataDir, r.PathValue("name"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Dataset-File", d.File)
		if b == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/datasets/{name}/backups", func(w http.ResponseWriter, r *http.Request) {
		d, backups, err := catalog.ListDatasetBackups(s.cat().DataDir, r.PathValue("name"))
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dataset": d.Name, "file": d.File, "backups": backups})
	})

	// mutating API (admin token enforced when configured)
	mux.HandleFunc("PUT /api/datasets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		force := r.URL.Query().Get("force") == "true"
		name := r.PathValue("name")
		res, err := s.putDataset(name, body, force)
		s.adminAudit(r, "put_dataset", name, err)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("DELETE /api/datasets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		name := r.PathValue("name")
		res, err := s.removeDataset(name)
		s.adminAudit(r, "remove_dataset", name, err)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/datasets/{name}/restore", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			Backup string `json:"backup"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		name := r.PathValue("name")
		res, err := s.restoreDataset(name, req.Backup)
		s.adminAudit(r, "restore_dataset", name+" <- "+req.Backup, err)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/reload", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		res, err := s.reloadCatalog()
		s.adminAudit(r, "reload_catalog", "", err)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
}

func (s *Server) fleetProfilesForRequest(w http.ResponseWriter, r *http.Request) ([]dbconn.Profile, context.Context, bool) {
	ctx := r.Context()
	if s.authEnabled() {
		actor, ok := s.requireActor(w, r)
		if !ok {
			return nil, ctx, false
		}
		ctx = withUser(ctx, actor)
	}
	profiles, err := s.usableProfiles(ctx)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return nil, ctx, false
	}
	return profiles, ctx, true
}

func allowedProfile(profiles []dbconn.Profile, id string) (dbconn.Profile, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return dbconn.Profile{}, false
	}
	for _, profile := range profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return dbconn.Profile{}, false
}

// restoreDataset copies a backup over the live file (backing up the current
// file first, so a restore is itself reversible), then hot-swaps the catalog.
func (s *Server) restoreDataset(name, backupName string) (map[string]any, error) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	dataDir := s.cat().DataDir
	d, backupPath, err := catalog.ResolveBackupPath(dataDir, name, backupName)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(backupPath)
	if err != nil {
		return nil, err
	}
	return s.applyDatasetBytes(d, dataDir, b, "restored from "+backupName)
}

func (s *Server) applyDatasetBytes(d catalog.DatasetInfo, dataDir string, content []byte, action string) (map[string]any, error) {
	res, err := s.putDatasetLocked(d.Name, content, true)
	if err != nil {
		return nil, err
	}
	res["action"] = action
	return res, nil
}

// reviewerFromRequest resolves who is recording a candidate decision: the
// authenticated user when auth is on, else the X-Reviewer header, else "admin".
func (s *Server) reviewerFromRequest(r *http.Request) string {
	if s.authEnabled() {
		if u, err := s.authenticate(r); err == nil && u != nil && u.Username != "" {
			return u.Username
		}
	}
	if h := strings.TrimSpace(r.Header.Get("X-Reviewer")); h != "" {
		return h
	}
	return "admin"
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	// meta DB active: admin ROLE required (master token counts as admin)
	if s.authEnabled() {
		u, err := s.authenticate(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "authentication required",
				"hint":  "로그인 세션, MCP 키, 또는 X-Admin-Token이 필요합니다.",
			})
			return false
		}
		if !u.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "admin role required",
				"hint":  "이 작업은 관리자 전용입니다. 관리자에게 역할 승격을 요청하세요.",
			})
			return false
		}
		return true
	}
	// standalone: legacy master-token gate
	token := strings.TrimSpace(s.Options.AdminToken)
	if token == "" {
		return true // auth disabled (internal network); enable with -admin-token
	}
	got := r.Header.Get("X-Admin-Token")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "admin token required",
			"hint":  "X-Admin-Token 헤더 또는 Authorization: Bearer <token>을 보내세요. 토큰은 서버 -admin-token 플래그로 설정됩니다.",
		})
		return false
	}
	return true
}

// requireDBA gates the REST DBA console: meta mode needs the dba (or admin)
// role; standalone falls back to the master-token gate exactly like admin.
func (s *Server) requireDBA(w http.ResponseWriter, r *http.Request) bool {
	if s.authEnabled() {
		u, err := s.authenticate(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "authentication required",
				"hint":  "로그인 세션, MCP 키, 또는 X-Admin-Token이 필요합니다.",
			})
			return false
		}
		if !u.IsDBA() {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "dba or admin role required",
				"hint":  "DBA 콘솔은 dba/admin 역할 전용입니다. 관리자에게 역할 승격을 요청하세요.",
			})
			return false
		}
		return true
	}
	// standalone: same master-token gate as admin
	return s.requireAdmin(w, r)
}

// adminAudit records REST mutations into the same audit JSONL as MCP calls.
func (s *Server) adminAudit(r *http.Request, action, detail string, callErr error) {
	entry := map[string]any{
		"ts":     time.Now().Format(time.RFC3339Nano),
		"tool":   "admin:" + action,
		"detail": detail,
		"remote": r.RemoteAddr,
	}
	if callErr != nil {
		entry["is_error"] = true
		entry["error"] = callErr.Error()
	}
	s.appendAudit(entry)
}

func (s *Server) serveWebUI(path, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		b, err := webuiFS.ReadFile(path)
		if err != nil {
			http.Error(w, "asset not found: "+path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	}
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func atoiDefault(s string, def int) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
