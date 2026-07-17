package mcp

import (
	"context"
	"errors"
	"strings"

	"sqlon/internal/catalog"
	"sqlon/internal/dbconn"
)

// execute_with_repair consolidates the validate → execute → diagnose loop into
// a single call that, on any recoverable failure, returns exactly what an LLM
// needs to fix the SQL in one more turn: the failure phase, the classified
// error code + Korean hint, and the catalog schema of the referenced tables.
// It halves round-trips versus calling validate_sql / run_sql_safely /
// diagnose separately. Read-only execution and all guardrails are unchanged —
// this only reshapes the response for self-correction.

type repairArgs struct {
	SQL            string `json:"sql"`
	Question       string `json:"question"`
	Limit          int    `json:"limit"`
	Profile        string `json:"profile"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Fresh          bool   `json:"fresh"`
	ApprovePlan    bool   `json:"approve_plan"`
}

func (s *Server) executeWithRepair(ctx context.Context, a repairArgs) (map[string]any, error) {
	if strings.TrimSpace(a.SQL) == "" {
		return map[string]any{"status": "blocked", "error": "sql is required"}, nil
	}

	// profile="auto" → route, mirroring run_sql_safely (never guess).
	if strings.EqualFold(strings.TrimSpace(a.Profile), "auto") {
		dec, err := s.routeProfile(ctx, a.SQL)
		if err != nil {
			return map[string]any{"status": "routing_failed", "error": err.Error()}, nil
		}
		if !dec.Decisive {
			out := routeResult(dec)
			out["status"] = "profile_choice_required"
			out["notice"] = "여러 프로파일이 이 쿼리를 처리할 수 있어 자동 선택하지 않았습니다. candidates 중 하나의 profile_id로 다시 호출하세요."
			return out, nil
		}
		a.Profile = dec.Selected
	}

	if ids := s.pendingClarifications(ctx); len(ids) > 0 && a.Profile != "" {
		return map[string]any{
			"status":  "clarification_required",
			"error":   "이 세션에 답변되지 않은 blocking 재질문이 있습니다.",
			"pending": ids,
			"notice":  "clarifications의 질문을 사용자에게 전달해 답을 받은 뒤 prepare_sql_context를 다시 호출하세요.",
		}, nil
	}
	if a.Profile != "" {
		if err := s.canUseProfileID(ctx, userFrom(ctx), a.Profile); err != nil {
			return map[string]any{"status": "forbidden", "error": err.Error(),
				"notice": "list_db_profiles로 사용 가능한 프로파일을 확인하세요."}, nil
		}
	}

	// validate against the profile's workspace catalog when one exists — the
	// correct catalog for the target DB in multi-DB use.
	vcat, _ := s.catalogFor(a.Profile)
	v := vcat.ValidateSQL(catalog.ValidateRequest{SQL: a.SQL, Limit: a.Limit})

	if a.Profile == "" {
		out := map[string]any{
			"status":      "dry_run_only",
			"validation":  v,
			"bounded_sql": v.BoundedSQL,
			"notice":      "db 프로파일이 없어 검증만 수행했습니다. 실제 실행하려면 profile을 전달하세요.",
		}
		if !v.Valid {
			out["repair"] = s.repairKit(vcat, "validation", "", v)
		}
		return out, nil
	}

	if !v.Valid {
		return map[string]any{
			"status":     "needs_fix",
			"phase":      "validation",
			"validation": v,
			"repair":     s.repairKit(vcat, "validation", "", v),
			"notice":     "검증 실패 SQL은 실행되지 않습니다. repair.fix_hints와 repair.schema_context로 컬럼/테이블을 고쳐 재시도하세요(최대 2회).",
		}, nil
	}

	execAs := "mcp"
	if u := userFrom(ctx); u != nil {
		execAs = u.Username
	}
	result, masked, cached, err := s.executeGuarded(ctx, a.Profile, a.SQL, dbconn.ExecOptions{
		MaxRows:        a.Limit,
		TimeoutSeconds: a.TimeoutSeconds,
		User:           execAs,
		ApprovePlan:    a.ApprovePlan,
	}, a.Fresh)
	if err != nil {
		var cost *dbconn.PlanCostError
		if errors.As(err, &cost) {
			return map[string]any{
				"status":    "needs_fix",
				"phase":     "cost_ceiling",
				"live_plan": cost.Plan,
				"measure":   cost.Measure,
				"limit":     cost.Limit,
				"estimated": cost.Actual,
				"error":     err.Error(),
				"repair": map[string]any{
					"phase":       "cost_ceiling",
					"suggestions": planSuggestionsFromPlan(cost.Plan),
					"guidance":    "예상 실행 비용이 절대 상한을 초과했습니다. approve_plan으로 우회 불가 — 기간·LIMIT·필터로 쿼리를 반드시 좁히세요.",
				},
			}, nil
		}
		var gate *dbconn.PlanGateError
		if errors.As(err, &gate) {
			return map[string]any{
				"status":    "needs_fix",
				"phase":     "plan",
				"live_plan": gate.Plan,
				"threshold": gate.Threshold,
				"error":     err.Error(),
				"repair": map[string]any{
					"phase":       "plan",
					"suggestions": planSuggestions(gate),
					"guidance":    "실행계획 위험도가 임계값 이상입니다. 기간·LIMIT 조건을 좁혀 재생성하는 것을 우선하세요. 감수하고 실행하려면 approve_plan=true.",
				},
			}, nil
		}
		return map[string]any{
			"status":     "needs_fix",
			"phase":      "execution",
			"error":      err.Error(),
			"error_code": dbErrCodeOf(err.Error()),
			"repair":     s.repairKit(vcat, "execution", err.Error(), v),
			"notice":     "DB 실행 오류입니다. repair.hint와 repair.schema_context를 반영해 SQL을 고쳐 재시도하세요(최대 2회).",
		}, nil
	}

	out := map[string]any{"validation": map[string]any{"valid": true, "warnings": len(v.Warnings)}}
	if cached {
		out["cached"] = true
	}
	if len(masked) > 0 {
		out["masked_columns"] = masked
	}
	diag := s.diagnoseResult(a.SQL, result)
	if result != nil && result.RowCount == 0 {
		out["status"] = "executed_empty"
		out["result"] = result
		out["repair"] = map[string]any{
			"phase":          "empty_result",
			"zero_row_hints": vcat.DiagnoseZeroRows(a.SQL),
			"schema_context": s.schemaForSQL(vcat, a.Question, a.SQL),
			"guidance":       "결과가 0행입니다. 조건을 완화해 재실행하거나, 데이터가 없음을 근거와 함께 답하세요.",
		}
		return out, nil
	}
	out["status"] = "executed"
	out["result"] = result
	if diag != nil {
		out["result_diagnosis"] = diag
	}
	return out, nil
}

// repairKit assembles the fix-context block: classified error code + hint and
// the schema of the tables the SQL touches, so the model can correct names in
// place.
func (s *Server) repairKit(c *catalog.Catalog, phase, errMsg string, v catalog.ValidationResult) map[string]any {
	kit := map[string]any{"phase": phase}
	if phase == "validation" {
		kit["errors"] = v.Errors
		kit["fix_hints"] = v.FixHints
		if v.RetryGuidance != "" {
			kit["retry_guidance"] = v.RetryGuidance
		}
	}
	if errMsg != "" {
		kit["error_code"] = dbErrCodeOf(errMsg)
		if h := dbHint(errMsg); h != "" {
			kit["hint"] = h
		}
	}
	tables := referencedTableNames(v)
	if len(tables) > 0 {
		kit["schema_context"] = c.SchemaContext("", tables, 60)
	}
	return kit
}

// schemaForSQL returns the catalog schema for the tables named in the SQL's
// validation, used for empty-result guidance.
func (s *Server) schemaForSQL(c *catalog.Catalog, question, sql string) any {
	v := c.ValidateSQL(catalog.ValidateRequest{SQL: sql})
	tables := referencedTableNames(v)
	if len(tables) == 0 {
		return nil
	}
	return c.SchemaContext(question, tables, 60)
}

func referencedTableNames(v catalog.ValidationResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range v.ReferencedTables {
		name := strings.TrimSpace(t.Table)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	return out
}

// planSuggestions surfaces the plan gate's own risk factors/suggestions when
// present, with a sane fallback.
func planSuggestions(gate *dbconn.PlanGateError) any {
	if gate != nil && gate.Plan != nil {
		return planSuggestionsFromPlan(gate.Plan)
	}
	return defaultNarrowHints()
}

func planSuggestionsFromPlan(p *dbconn.PlanResult) any {
	if p != nil && len(p.Suggestions) > 0 {
		return p.Suggestions
	}
	return defaultNarrowHints()
}

func defaultNarrowHints() []string {
	return []string{"기간 조건을 좁히세요", "LIMIT을 낮추세요", "인덱스가 있는 컬럼으로 필터하세요"}
}

// dbErrCodeOf extracts the PG-/MY- classified code from a driver error string
// (mirrors dbHint's normalization) so callers get a stable code field.
func dbErrCodeOf(errMsg string) string {
	if m := pgStateRE.FindStringSubmatch(errMsg); m != nil {
		return "PG-" + strings.ToUpper(m[1])
	}
	if m := myErrnoRE.FindStringSubmatch(errMsg); m != nil {
		return "MY-" + m[1]
	}
	up := strings.ToUpper(errMsg)
	switch {
	case strings.Contains(up, "TIMEOUT"):
		return "TIMEOUT"
	case strings.Contains(up, "CANCEL"):
		return "CANCELED"
	}
	return ""
}
