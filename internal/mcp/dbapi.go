package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"sqlon/internal/catalog"
	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/meta"
)

// registerDBAPI wires the DB query connector REST surface: DB profile
// CRUD/test, query validate/execute/preview/metadata/history/cancel, and
// metrics.
//
// Two storage/authorization modes:
//   - standalone (no meta DB): profiles live in db_profiles.json; the legacy
//     master-token gate guards mutations and query execution.
//   - meta DB active: profiles are per-user records in Postgres with
//     owner/visibility/grants; every actor is authenticated and permission-
//     checked (admin has full access).
func (s *Server) registerDBAPI(mux *http.ServeMux) {
	// ---- profiles ----
	mux.HandleFunc("GET /api/db-profiles", func(w http.ResponseWriter, r *http.Request) {
		if s.authEnabled() {
			s.listProfilesMeta(w, r)
			return
		}
		profiles, err := dbconn.LoadProfiles(s.opDir())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		masked := make([]map[string]any, 0, len(profiles))
		for _, p := range profiles {
			masked = append(masked, p.Masked())
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"profiles":         masked,
			"driver_available": s.DB.Available(),
			"driver_note":      s.DB.DriverNote(),
			"drivers":          s.DB.DriverCapabilities(),
		})
	})
	mux.HandleFunc("POST /api/db-profiles", func(w http.ResponseWriter, r *http.Request) {
		s.upsertProfile(w, r, "", true)
	})
	mux.HandleFunc("PUT /api/db-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.upsertProfile(w, r, r.PathValue("id"), false)
	})
	mux.HandleFunc("DELETE /api/db-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if s.authEnabled() {
			actor, ok := s.requireActor(w, r)
			if !ok {
				return
			}
			rec, err := s.Meta.Store.GetProfile(r.Context(), id)
			if err != nil {
				writeAPIError(w, http.StatusNotFound, err)
				return
			}
			// 삭제는 소유자 또는 admin만 (manage grant보다 엄격)
			if !actor.IsAdmin() && rec.OwnerID != actor.ID {
				writeAPIError(w, http.StatusForbidden, errEmpty("only the owner or an admin can delete a profile"))
				return
			}
			if err := s.Meta.Store.DeleteProfile(r.Context(), id); err != nil {
				writeAPIError(w, http.StatusBadRequest, err)
				return
			}
			s.DB.Invalidate(id)
			s.adminAudit(r, "db_profile_delete", id+" by "+actorName(actor), nil)
			writeJSON(w, http.StatusOK, map[string]any{"removed": true, "id": id})
			return
		}
		if !s.requireAdmin(w, r) {
			return
		}
		profiles, err := dbconn.RemoveProfile(s.opDir(), id)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.saveProfiles(profiles); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		s.DB.Invalidate(id)
		s.adminAudit(r, "db_profile_delete", id, nil)
		writeJSON(w, http.StatusOK, map[string]any{"removed": true, "id": id})
	})
	mux.HandleFunc("POST /api/db-profiles/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		// opens a real DB pool — gate like the other query endpoints so the
		// standalone -admin-token applies too
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if err := s.canUseProfileID(r.Context(), actor, id); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		res := s.DB.Ping(r.Context(), id)
		s.adminAudit(r, "db_profile_test", id+" by "+actorName(actor), nil)
		writeJSON(w, http.StatusOK, res)
	})

	// ---- query ----
	mux.HandleFunc("POST /api/query/validate", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireActor(w, r); !ok {
			return
		}
		var req struct {
			SQL             string   `json:"sql"`
			ProfileID       string   `json:"profile_id"`
			Metrics         []string `json:"metrics"`
			ExpectedOutputs []string `json:"expected_outputs"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		res := map[string]any{
			"catalog": s.cat().ValidateSQL(catalog.ValidateRequest{SQL: req.SQL, Metrics: req.Metrics, ExpectedOutputs: req.ExpectedOutputs}),
		}
		var denied []string
		dialectType := s.cat().Dialect
		if req.ProfileID != "" {
			if p, err := s.profileByID(r, req.ProfileID); err == nil {
				denied = p.Policy.DeniedKeywords
				dialectType = p.Type
			}
		}
		var guardErr error
		if d, err := dbconn.DialectFor(dialectType); err == nil {
			guardErr = dbconn.ValidateReadOnly(d, req.SQL, denied)
		} else {
			guardErr = dbconn.ValidateReadOnlySQL(req.SQL, denied)
		}
		if guardErr != nil {
			res["connector"] = map[string]any{"valid": false, "error": guardErr.Error()}
		} else {
			res["connector"] = map[string]any{"valid": true}
		}
		writeJSON(w, http.StatusOK, res)
	})

	// POST /api/ask — one-call NL2SQL pipeline bundle (read-only, no DB touch).
	// Backs the /admin/ask screen and mirrors the prepare_sql_context MCP tool.
	mux.HandleFunc("POST /api/ask", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireActor(w, r); !ok {
			return
		}
		var req struct {
			Question         string            `json:"question"`
			Profile          string            `json:"profile"`
			Tables           []string          `json:"tables"`
			Limit            int               `json:"limit"`
			Clarifications   map[string]string `json:"clarifications"`
			PreviousQuestion string            `json:"previous_question"`
			PreviousSQL      string            `json:"previous_sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.Question) == "" {
			writeAPIError(w, http.StatusBadRequest, errEmpty("question is required"))
			return
		}
		if req.Profile != "" {
			if err := s.canUseProfileID(r.Context(), userFrom(r.Context()), req.Profile); err != nil {
				writeAPIError(w, http.StatusForbidden, err)
				return
			}
		}
		vcat, source := s.catalogFor(req.Profile)
		out := vcat.PrepareFollowup(req.Question, req.PreviousQuestion, req.PreviousSQL, req.Tables, req.Limit, time.Now(), req.Clarifications)
		out["catalog_source"] = source
		if req.Profile != "" {
			out["profile"] = req.Profile
		}
		writeJSON(w, http.StatusOK, out)
	})

	execute := func(preview bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			actor, ok := s.requireQueryActor(w, r)
			if !ok {
				return
			}
			var req struct {
				ProfileID      string `json:"profile_id"`
				SQL            string `json:"sql"`
				MaxRows        int    `json:"max_rows"`
				TimeoutSeconds int    `json:"timeout_seconds"`
				Binds          []any  `json:"binds"`
				User           string `json:"user"`
				TraceID        string `json:"trace_id"`
				Fresh          bool   `json:"fresh"`        // true → bypass the 60s result cache
				ApprovePlan    bool   `json:"approve_plan"` // true → bypass the plan-approval gate
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
				writeAPIError(w, http.StatusBadRequest, err)
				return
			}
			if req.ProfileID == "" || strings.TrimSpace(req.SQL) == "" {
				writeAPIError(w, http.StatusBadRequest, errEmpty("profile_id and sql are required"))
				return
			}
			if err := s.canUseProfileID(r.Context(), actor, req.ProfileID); err != nil {
				writeAPIError(w, http.StatusForbidden, err)
				return
			}
			// 카탈로그 검증을 먼저 통과해야 실행 (검증 실패 SQL 실행 금지)
			v := s.cat().ValidateSQL(catalog.ValidateRequest{SQL: req.SQL, Limit: req.MaxRows})
			if !v.Valid {
				writeJSON(w, http.StatusOK, map[string]any{"executed": false, "reason": "catalog validation failed", "validation": v})
				return
			}
			execUser := req.User
			if actor != nil {
				execUser = actor.Username // 인증 사용자로 강제 (감사 신뢰성)
			}
			result, masked, cached, err := s.executeGuarded(r.Context(), req.ProfileID, req.SQL, dbconn.ExecOptions{
				MaxRows:        req.MaxRows,
				TimeoutSeconds: req.TimeoutSeconds,
				Preview:        preview,
				Binds:          req.Binds,
				User:           execUser,
				TraceID:        req.TraceID,
				ApprovePlan:    req.ApprovePlan,
			}, req.Fresh)
			if err != nil {
				var gate *dbconn.PlanGateError
				if errors.As(err, &gate) {
					writeJSON(w, http.StatusOK, map[string]any{
						"executed":   false,
						"status":     "plan_approval_required",
						"live_plan":  gate.Plan,
						"threshold":  gate.Threshold,
						"error":      err.Error(),
						"notice":     "실행계획 위험도가 임계값 이상입니다. 조건을 좁혀 재생성하거나, 사용자 승인 후 approve_plan=true로 다시 호출하세요.",
						"validation": v,
					})
					return
				}
				out := map[string]any{"executed": false, "error": err.Error(), "validation": v}
				if h := dbHint(err.Error()); h != "" {
					out["hint"] = h
				}
				writeJSON(w, http.StatusOK, out)
				return
			}
			out := map[string]any{"executed": true, "result": result, "validation_warnings": len(v.Warnings)}
			if len(v.Lint) > 0 {
				out["lint_warnings"] = v.Lint
			}
			if cached {
				out["cached"] = true
			}
			if len(masked) > 0 {
				out["masked_columns"] = masked
			}
			if d := s.diagnoseResult(req.SQL, result); d != nil {
				out["result_diagnosis"] = d
			}
			writeJSON(w, http.StatusOK, out)
		}
	}
	mux.HandleFunc("POST /api/query/execute", execute(false))
	mux.HandleFunc("POST /api/query/preview", execute(true))

	mux.HandleFunc("POST /api/query/route", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		_ = actor
		var req struct {
			SQL string `json:"sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		dec, err := s.routeProfile(r.Context(), req.SQL)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, routeResult(dec))
	})

	// ---- metadata sync (FR-META-001..005) ----
	mux.HandleFunc("GET /api/metadata/sources", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, s.mcpMetadataSources(r.Context()))
	})
	mux.HandleFunc("POST /api/metadata/discover", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Source string `json:"source"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.Source); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpDiscoverMetadata(r.Context(), req.Source))
	})
	mux.HandleFunc("POST /api/metadata/sync", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Source       string   `json:"source"`
			Schemas      []string `json:"schemas"`
			Incremental  *bool    `json:"incremental"`
			IncludeViews bool     `json:"include_views"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.Source); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		incremental := req.Incremental == nil || *req.Incremental
		writeJSON(w, http.StatusOK, s.mcpRunMetadataSync(r.Context(), req.Source, req.Schemas, incremental, req.IncludeViews))
	})
	mux.HandleFunc("POST /api/db/health", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Profile string `json:"profile"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		if err := s.canUseProfileID(r.Context(), actor, req.Profile); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpDBHealthReport(r.Context(), req.Profile))
	})
	mux.HandleFunc("POST /api/db/index-advisor", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Profile      string `json:"profile"`
			MinElapsedMs int    `json:"min_elapsed_ms"`
			Days         int    `json:"days"`
			Verify       bool   `json:"verify"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		if req.Profile != "" {
			if err := s.canUseProfileID(r.Context(), actor, req.Profile); err != nil {
				writeAPIError(w, http.StatusForbidden, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, s.suggestIndexesVerified(r.Context(), req.Profile, req.MinElapsedMs, req.Days, req.Verify))
	})
	mux.HandleFunc("POST /api/db/workload", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Profile string `json:"profile"`
			Days    int    `json:"days"`
			SlowMs  int    `json:"slow_ms"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		if req.Profile != "" {
			if err := s.canUseProfileID(r.Context(), actor, req.Profile); err != nil {
				writeAPIError(w, http.StatusForbidden, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, s.mcpWorkloadReport(req.Profile, req.Days, req.SlowMs))
	})
	mux.HandleFunc("POST /api/db/digest", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Profile string `json:"profile"`
			Days    int    `json:"days"`
			SlowMs  int    `json:"slow_ms"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		if req.Profile != "" {
			if err := s.canUseProfileID(r.Context(), actor, req.Profile); err != nil {
				writeAPIError(w, http.StatusForbidden, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, s.mcpDBADigest(req.Profile, req.Days, req.SlowMs))
	})
	mux.HandleFunc("POST /api/query/lint", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		var req struct {
			SQL     string `json:"sql"`
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.SQL) == "" {
			writeAPIError(w, http.StatusBadRequest, errEmpty("sql is required"))
			return
		}
		vcat, src := s.catalogFor(req.Profile)
		findings := vcat.LintSQL(req.SQL)
		writeJSON(w, http.StatusOK, map[string]any{"findings": findings, "count": len(findings), "catalog_source": src})
	})
	mux.HandleFunc("POST /api/query/explain-words", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		var req struct {
			SQL     string `json:"sql"`
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.SQL) == "" {
			writeAPIError(w, http.StatusBadRequest, errEmpty("sql is required"))
			return
		}
		vcat, _ := s.catalogFor(req.Profile)
		writeJSON(w, http.StatusOK, vcat.ExplainSQLWords(req.SQL))
	})
	mux.HandleFunc("POST /api/metadata/describe", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Profile      string   `json:"profile"`
			Schemas      []string `json:"schemas"`
			Table        string   `json:"table"`
			IncludeViews bool     `json:"include_views"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.Profile); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpDescribeDBSchema(r.Context(), req.Profile, req.Schemas, req.Table, req.IncludeViews))
	})
	mux.HandleFunc("POST /api/metadata/apply", func(w http.ResponseWriter, r *http.Request) {
		// applying a snapshot mutates the operational catalog → admin only
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			Source string `json:"source"`
			Prune  bool   `json:"prune"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
		}
		res := s.mcpApplyMetadataSync(req.Source, req.Prune)
		s.adminAudit(r, "metadata.apply", req.Source, nil)
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/metadata/snapshots/{source}", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		src := r.PathValue("source")
		if err := s.canUseProfileID(r.Context(), actor, src); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpSyncStatus(src))
	})
	mux.HandleFunc("POST /api/metadata/diff", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Source string `json:"source"`
			From   string `json:"from"`
			To     string `json:"to"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.Source); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpDiffSnapshots(req.Source, req.From, req.To))
	})
	mux.HandleFunc("POST /api/metadata/profile", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Source      string   `json:"source"`
			Tables      []string `json:"tables"`
			Mode        string   `json:"mode"`
			SampleLimit int      `json:"sample_limit"`
			PIIColumns  []string `json:"pii_columns"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.Source); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpProfileMetadata(r.Context(), req.Source, req.Tables, req.Mode, req.SampleLimit, req.PIIColumns))
	})
	mux.HandleFunc("GET /api/metadata/profiles/{source}", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		src := r.PathValue("source")
		if err := s.canUseProfileID(r.Context(), actor, src); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		writeJSON(w, http.StatusOK, s.mcpProfileStatus(src))
	})

	mux.HandleFunc("POST /api/query/explain", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			ProfileID string `json:"profile_id"`
			SQL       string `json:"sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		res := map[string]any{
			"static": s.cat().ExplainSQL(catalog.ValidateRequest{SQL: req.SQL}),
		}
		if req.ProfileID != "" {
			if err := s.canUseProfileID(r.Context(), actor, req.ProfileID); err != nil {
				writeAPIError(w, http.StatusForbidden, err)
				return
			}
			plan, err := s.DB.ExplainPlan(r.Context(), req.ProfileID, req.SQL)
			if err != nil {
				res["live_plan_error"] = err.Error()
			} else {
				res["live_plan"] = plan
			}
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/query/metadata", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			ProfileID string `json:"profile_id"`
			SQL       string `json:"sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.ProfileID); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		cols, err := s.DB.Metadata(r.Context(), req.ProfileID, req.SQL)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "columns": cols})
	})
	mux.HandleFunc("POST /api/query/simulate-index", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			ProfileID string   `json:"profile_id"`
			Table     string   `json:"table_name"`
			Columns   []string `json:"columns"`
			SQL       string   `json:"sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.ProfileID); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		res, err := s.DB.VerifyHypotheticalIndex(r.Context(), req.ProfileID, req.Table, req.Columns, req.SQL)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/db/unused-indexes", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		profileID := r.URL.Query().Get("profile_id")
		if err := s.canUseProfileID(r.Context(), actor, profileID); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		list, err := s.DB.ListUnusedIndexes(r.Context(), profileID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"unused_indexes": list})
	})
	mux.HandleFunc("POST /api/db/config-audit", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			ProfileID    string `json:"profile_id"`
			SystemRAM_GB int    `json:"system_ram_gb"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.ProfileID); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		res, err := s.DB.AuditConfiguration(r.Context(), req.ProfileID, req.SystemRAM_GB)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /api/db/alerts", func(w http.ResponseWriter, r *http.Request) {
		_, ok := s.requireActor(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"alerts": collector.GetAlerts()})
	})
	mux.HandleFunc("GET /api/query/history", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireActor(w, r)
		if !ok {
			return
		}
		limit := atoiDefault(r.URL.Query().Get("limit"), 50)
		history := s.DB.History(limit)
		running := s.DB.Running()
		// 인증 모드에서 비관리자는 본인 실행만
		if s.authEnabled() && !actor.IsAdmin() {
			mine := history[:0]
			for _, h := range history {
				if h.User == actor.Username {
					mine = append(mine, h)
				}
			}
			history = mine
			myRunning := []map[string]any{}
			for _, e := range running {
				if e["user"] == actor.Username {
					myRunning = append(myRunning, e)
				}
			}
			running = myRunning
		}
		writeJSON(w, http.StatusOK, map[string]any{"history": history, "running": running})
	})
	mux.HandleFunc("POST /api/query/cancel/{executionId}", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		id := r.PathValue("executionId")
		err := s.DB.CancelAs(id, actorName(actor), s.isAdminActor(actor))
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		s.adminAudit(r, "query_cancel", id+" by "+actorName(actor), nil)
		writeJSON(w, http.StatusOK, map[string]any{"canceled": true, "execution_id": id})
	})

	// ---- async jobs: long-running queries detached from the HTTP request ----
	mux.HandleFunc("POST /api/query/submit", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			ProfileID      string `json:"profile_id"`
			SQL            string `json:"sql"`
			MaxRows        int    `json:"max_rows"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if req.ProfileID == "" || strings.TrimSpace(req.SQL) == "" {
			writeAPIError(w, http.StatusBadRequest, errEmpty("profile_id and sql are required"))
			return
		}
		if err := s.canUseProfileID(r.Context(), actor, req.ProfileID); err != nil {
			writeAPIError(w, http.StatusForbidden, err)
			return
		}
		// same rule as sync execute: invalid SQL never runs
		if v := s.cat().ValidateSQL(catalog.ValidateRequest{SQL: req.SQL, Limit: req.MaxRows}); !v.Valid {
			writeJSON(w, http.StatusOK, map[string]any{"submitted": false, "reason": "catalog validation failed", "validation": v})
			return
		}
		job, refuse := s.submitAsyncQuery(req.ProfileID, req.SQL, actorName(actor), dbconn.ExecOptions{
			MaxRows: req.MaxRows, TimeoutSeconds: req.TimeoutSeconds, User: actorName(actor),
		})
		if refuse != "" {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"submitted": false, "reason": refuse})
			return
		}
		s.adminAudit(r, "query_submit_async", job.ID+" ("+req.ProfileID+") by "+actorName(actor), nil)
		writeJSON(w, http.StatusAccepted, map[string]any{"submitted": true, "job_id": job.ID,
			"poll": "/api/query/job/" + job.ID, "result_ttl_minutes": 10})
	})
	mux.HandleFunc("GET /api/query/job/{jobId}", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		job, found := s.asyncJobs.jobView(r.PathValue("jobId"))
		if !found {
			writeAPIError(w, http.StatusNotFound, errEmpty("job not found (완료 후 10분이 지나면 삭제됩니다)"))
			return
		}
		if !s.isAdminActor(actor) && job.User != actorName(actor) {
			writeAPIError(w, http.StatusForbidden, errEmpty("this job belongs to another user"))
			return
		}
		writeJSON(w, http.StatusOK, job)
	})
	mux.HandleFunc("POST /api/query/job/{jobId}/cancel", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		if !s.asyncJobs.cancelJob(r.PathValue("jobId"), actorName(actor), s.isAdminActor(actor)) {
			writeAPIError(w, http.StatusNotFound, errEmpty("job not found or not yours"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"canceled": true})
	})

	// ---- metrics ----
	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.DB.Snapshot())
	})
	mux.HandleFunc("GET /metrics", s.serveMetrics)
}

// requireQueryActor gates DB-touching query endpoints: meta mode → any
// authenticated user (per-profile checks follow); standalone → legacy
// master-token gate.
func (s *Server) requireQueryActor(w http.ResponseWriter, r *http.Request) (*meta.User, bool) {
	if s.authEnabled() {
		return s.requireActor(w, r)
	}
	if !s.requireAdmin(w, r) {
		return nil, false
	}
	return nil, true
}

// profileByID reads a profile from whichever store is active.
func (s *Server) profileByID(r *http.Request, id string) (dbconn.Profile, error) {
	if s.authEnabled() {
		return metaProfileStore{svc: s.Meta}.GetProfileByID(r.Context(), id)
	}
	return dbconn.GetProfile(s.opDir(), id)
}

// listProfilesMeta returns the profiles visible to the actor with ownership
// and permission annotations.
func (s *Server) listProfilesMeta(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireActor(w, r)
	if !ok {
		return
	}
	recs, err := s.Meta.Store.ListProfiles(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	out := []map[string]any{}
	for _, rec := range recs {
		grants, _ := s.Meta.Store.ListGrants(r.Context(), rec.ID)
		if !meta.CanUseProfile(actor, *rec, grants) {
			continue
		}
		var p dbconn.Profile
		if json.Unmarshal(rec.Definition, &p) != nil {
			continue
		}
		p.ID = rec.ID
		view := dbconn.ApplyDefaults(p).Masked()
		view["visibility"] = rec.Visibility
		view["owner_id"] = rec.OwnerID
		if owner, err := s.Meta.Store.GetUserByID(r.Context(), rec.OwnerID); err == nil {
			view["owner"] = owner.Username
		}
		switch {
		case actor.IsAdmin() || rec.OwnerID == actor.ID:
			view["my_permission"] = "owner"
		case meta.CanManageProfile(actor, *rec, grants):
			view["my_permission"] = meta.PermManage
		default:
			view["my_permission"] = meta.PermUse
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profiles":         out,
		"driver_available": s.DB.Available(),
		"driver_note":      s.DB.DriverNote(),
		"drivers":          s.DB.DriverCapabilities(),
		"auth_enabled":     true,
	})
}

// upsertProfile handles POST(create)/PUT(update) in both storage modes.
func (s *Server) upsertProfile(w http.ResponseWriter, r *http.Request, pathID string, create bool) {
	if s.authEnabled() {
		s.upsertProfileMeta(w, r, pathID, create)
		return
	}
	if !s.requireAdmin(w, r) {
		return
	}
	var p dbconn.Profile
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if pathID != "" {
		if p.ID != "" && p.ID != pathID {
			writeAPIError(w, http.StatusBadRequest, errEmpty("body id does not match path id"))
			return
		}
		p.ID = pathID
	}
	profiles, err := dbconn.UpsertProfile(s.opDir(), p, create)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.saveProfiles(profiles); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	s.DB.Invalidate(p.ID)
	action := "db_profile_update"
	if create {
		action = "db_profile_create"
	}
	s.adminAudit(r, action, p.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "id": p.ID, "profiles": len(profiles)})
}

func (s *Server) upsertProfileMeta(w http.ResponseWriter, r *http.Request, pathID string, create bool) {
	actor, ok := s.requireActor(w, r)
	if !ok {
		return
	}
	if actor.ID == "master" && create {
		writeAPIError(w, http.StatusBadRequest, errEmpty("master token cannot own profiles; log in as a real user"))
		return
	}
	var req struct {
		dbconn.Profile
		Visibility string `json:"visibility"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	p := req.Profile
	if pathID != "" {
		if p.ID != "" && p.ID != pathID {
			writeAPIError(w, http.StatusBadRequest, errEmpty("body id does not match path id"))
			return
		}
		p.ID = pathID
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = meta.VisibilityPrivate
	}
	if visibility != meta.VisibilityPrivate && visibility != meta.VisibilityShared {
		writeAPIError(w, http.StatusBadRequest, errEmpty("visibility must be private or shared"))
		return
	}
	if err := p.Validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	definition, err := json.Marshal(p)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	if create {
		rec := &meta.ProfileRecord{ID: p.ID, OwnerID: actor.ID, Definition: definition, Visibility: visibility}
		if err := s.Meta.Store.UpsertProfile(r.Context(), rec, true); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
	} else {
		rec, err := s.Meta.Store.GetProfile(r.Context(), p.ID)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		grants, _ := s.Meta.Store.ListGrants(r.Context(), rec.ID)
		if !meta.CanManageProfile(actor, *rec, grants) {
			writeAPIError(w, http.StatusForbidden, errEmpty("manage permission required (owner, admin, or manage grant)"))
			return
		}
		// visibility 변경은 소유자/admin만
		if req.Visibility != "" && req.Visibility != rec.Visibility && !actor.IsAdmin() && rec.OwnerID != actor.ID {
			writeAPIError(w, http.StatusForbidden, errEmpty("only the owner or an admin can change visibility"))
			return
		}
		rec.Definition = definition
		rec.Visibility = visibility
		if err := s.Meta.Store.UpsertProfile(r.Context(), rec, false); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
	}
	s.DB.Invalidate(p.ID)
	action := "db_profile_update"
	if create {
		action = "db_profile_create"
	}
	s.adminAudit(r, action, p.ID+" by "+actorName(actor), nil)
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "id": p.ID, "visibility": visibility})
}

// saveProfiles persists via the dataset pipeline so the previous file is
// backed up and dataset status stays coherent (standalone mode only).
func (s *Server) saveProfiles(profiles []dbconn.Profile) error {
	// the DB profile registry is operational (not part of the active catalog's
	// NL2SQL datasets), so it persists to the fixed operational dir — surviving
	// an active-catalog hot-swap to a profile workspace.
	return dbconn.SaveProfiles(s.opDir(), profiles)
}

type errEmpty string

func (e errEmpty) Error() string { return string(e) }
