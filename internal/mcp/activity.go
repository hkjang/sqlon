package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"

	"sqlon/internal/meta"
)

// activityID mints a short random id for an activity row.
func activityID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return "act_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// recordMCPActivity captures a curated MCP tool call into the per-user history
// that powers /admin/history and the stats dashboard. It runs only in auth mode
// for an identified user, records just the meaningful events (a driving prompt,
// a generated query, an execution), correlates each query with the most recent
// prompt from the same session, and is entirely best-effort — a failure here
// must never affect the tool result.
func (s *Server) recordMCPActivity(ctx context.Context, params json.RawMessage, result any, elapsed time.Duration) {
	if !s.authEnabled() || s.Meta == nil {
		return
	}
	u := userFrom(ctx)
	if u == nil {
		return
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return
	}
	var args map[string]any
	_ = json.Unmarshal(p.Arguments, &args)
	getStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}

	act := &meta.MCPActivity{
		ID:        activityID(),
		UserID:    u.ID,
		Username:  u.Username,
		SessionID: sessionFrom(ctx),
		Tool:      p.Name,
		ElapsedMs: elapsed.Milliseconds(),
	}

	switch p.Name {
	case "prepare_sql_context", "analyze_question":
		act.Kind = meta.ActivityPrompt
		act.Prompt = getStr("question")
		if act.Prompt == "" {
			return
		}
	case "validate_sql", "explain_sql":
		act.Kind = meta.ActivityGenerate
		act.SQL = getStr("sql")
		if act.SQL == "" {
			return
		}
		act.Status = generateStatus(resultMap(result))
	case "run_sql_safely":
		act.Kind = meta.ActivityExecute
		act.SQL = getStr("sql")
		act.Profile = getStr("profile")
		if act.SQL == "" {
			return
		}
		act.Status, act.RowCount = executeOutcome(resultMap(result))
	default:
		return // ignore pure lookups (search/schema/joins/etc.)
	}

	// correlate a generated/executed query with the last prompt in the session
	if act.Kind != meta.ActivityPrompt && act.Prompt == "" {
		if prompt, err := s.Meta.Store.LastPromptForSession(ctx, act.SessionID, u.ID); err == nil {
			act.Prompt = prompt
		}
	}

	// stash the agent-sent metadata + arguments so temperature/params are visible
	if snap := marshalParams(p.Meta, args); snap != nil {
		act.Params = snap
	}

	if err := s.Meta.Store.RecordActivity(ctx, act); err != nil {
		log.Printf("mcp activity record failed: %v", err)
	}
}

// computeActivityStats aggregates a slice of activity rows into the dashboard
// payload: totals by kind, execution outcomes, per-tool counts, a daily
// timeline, and (for the admin all-users view) a per-user breakdown.
func computeActivityStats(acts []*meta.MCPActivity, perUser bool) map[string]any {
	byKind := map[string]int{}
	byTool := map[string]int{}
	byStatus := map[string]int{}
	byDay := map[string]int{}
	byUser := map[string]int{}
	userNames := map[string]string{}
	var executions, totalRows, generates, validGen int

	for _, a := range acts {
		byKind[a.Kind]++
		byTool[a.Tool]++
		day := a.CreatedAt.Format("2006-01-02")
		byDay[day]++
		if perUser {
			byUser[a.UserID]++
			userNames[a.UserID] = a.Username
		}
		switch a.Kind {
		case meta.ActivityExecute:
			executions++
			totalRows += a.RowCount
			st := a.Status
			if st == "" {
				st = "unknown"
			}
			byStatus[st]++
		case meta.ActivityGenerate:
			generates++
			if a.Status == "valid" {
				validGen++
			}
		}
	}

	// last 14 days timeline (oldest→newest), zero-filled
	timeline := []map[string]any{}
	now := time.Now()
	for i := 13; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		timeline = append(timeline, map[string]any{"day": d, "count": byDay[d]})
	}

	users := []map[string]any{}
	if perUser {
		for id, n := range byUser {
			users = append(users, map[string]any{"user_id": id, "username": userNames[id], "count": n})
		}
	}

	return map[string]any{
		"total":           len(acts),
		"prompts":         byKind[meta.ActivityPrompt],
		"generated":       generates,
		"generated_valid": validGen,
		"executions":      executions,
		"total_rows":      totalRows,
		"by_tool":         byTool,
		"by_exec_status":  byStatus,
		"timeline":        timeline,
		"per_user":        perUser,
		"users":           users,
	}
}

// clarificationSuggestions mines recorded prompt activity for recurring
// clarification answers. Each cluster ("잔액" was re-asked N times, users
// answered X) is a candidate glossary/metric-dictionary entry — promoting it
// removes the re-question at the source, so the loop tightens over time.
func clarificationSuggestions(acts []*meta.MCPActivity) []map[string]any {
	type agg struct {
		count   int
		answers map[string]int
		last    time.Time
	}
	byID := map[string]*agg{}
	for _, act := range acts {
		if act.Kind != meta.ActivityPrompt || len(act.Params) == 0 {
			continue
		}
		var p struct {
			Arguments struct {
				Clarifications map[string]string `json:"clarifications"`
			} `json:"arguments"`
		}
		if json.Unmarshal(act.Params, &p) != nil || len(p.Arguments.Clarifications) == 0 {
			continue
		}
		for id, ans := range p.Arguments.Clarifications {
			a := byID[id]
			if a == nil {
				a = &agg{answers: map[string]int{}}
				byID[id] = a
			}
			a.count++
			a.answers[ans]++
			if act.CreatedAt.After(a.last) {
				a.last = act.CreatedAt
			}
		}
	}
	out := []map[string]any{}
	for id, a := range byID {
		if a.count < 2 {
			continue // one-offs aren't patterns
		}
		top, topN := "", 0
		for ans, n := range a.answers {
			if n > topN {
				top, topN = ans, n
			}
		}
		suggestion := "용어사전(glossary)에 항목 추가를 검토하세요."
		if strings.HasPrefix(id, "metric:") {
			suggestion = "지표사전(metrics)에 '" + strings.TrimPrefix(id, "metric:") + "' 정의 추가를 검토하세요: " + top
		}
		out = append(out, map[string]any{
			"clarification_id": id,
			"occurrences":      a.count,
			"top_answer":       top,
			"top_answer_count": topN,
			"last_seen":        a.last,
			"suggestion":       suggestion,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["occurrences"].(int) > out[j]["occurrences"].(int) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// resultMap normalizes a tool result (usually a struct) into a generic map via
// a JSON round-trip, so the outcome readers can inspect it uniformly.
func resultMap(result any) map[string]any {
	if m, ok := result.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// generateStatus reads validate_sql/explain_sql results for a coarse verdict.
func generateStatus(m map[string]any) string {
	if m == nil {
		return ""
	}
	if v, ok := m["valid"].(bool); ok {
		if v {
			return "valid"
		}
		return "invalid"
	}
	if risk, ok := m["risk"].(string); ok {
		return "risk:" + risk
	}
	return "ok"
}

// executeOutcome reads run_sql_safely results for status + row count.
func executeOutcome(m map[string]any) (string, int) {
	if m == nil {
		return "", 0
	}
	status, _ := m["status"].(string)
	rows := 0
	if res, ok := m["result"].(map[string]any); ok {
		if rc, ok := res["row_count"].(float64); ok {
			rows = int(rc)
		} else if rc, ok := res["row_count"].(int); ok {
			rows = rc
		}
	}
	return status, rows
}

// marshalParams builds the stored params blob from the agent _meta and a
// trimmed copy of the tool arguments (bulky text like full SQL is already
// captured in dedicated columns, so drop it here to keep the blob small).
func marshalParams(metaRaw json.RawMessage, args map[string]any) json.RawMessage {
	out := map[string]any{}
	if len(metaRaw) > 0 && string(metaRaw) != "null" {
		var mm any
		if json.Unmarshal(metaRaw, &mm) == nil {
			out["_meta"] = mm
		}
	}
	if len(args) > 0 {
		trimmed := map[string]any{}
		for k, v := range args {
			if k == "sql" || k == "question" {
				continue // already columns
			}
			trimmed[k] = v
		}
		if len(trimmed) > 0 {
			out["arguments"] = trimmed
		}
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}
