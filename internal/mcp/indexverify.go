package mcp

import (
	"context"
	"strconv"
	"strings"

	"sqlon/internal/dbconn"
)

// Index candidate verification (DBA co-pilot). suggestIndexesVerified runs the
// audit-log index advisor and then, when verify=true and a profile is given,
// runs a live EXPLAIN on each top candidate's sample query to check whether the
// plan really does a full/sequential scan on the candidate table — evidence that
// the proposed index would help. This trims false positives (columns that look
// worth indexing from the log but that the planner already covers). Read-only:
// EXPLAIN never executes the query.

const maxVerifyCandidates = 5 // bound live EXPLAINs per call

// suggestIndexesVerified wraps mcpSuggestIndexes with optional live-plan checks.
func (s *Server) suggestIndexesVerified(ctx context.Context, profile string, minElapsedMs, days int, verify bool) map[string]any {
	res := s.mcpSuggestIndexes(profile, minElapsedMs, days)
	if !verify || profile == "" {
		return res
	}
	cands, ok := res["candidates"].([]indexCandidate)
	if !ok || len(cands) == 0 {
		res["verified"] = true
		return res
	}
	confirmed := 0
	for i := range cands {
		if i >= maxVerifyCandidates {
			cands[i].VerifyNote = "not verified (beyond top " + strconv.Itoa(maxVerifyCandidates) + ")"
			continue
		}
		sql := cands[i].SampleSQL
		if strings.TrimSpace(sql) == "" {
			continue
		}
		plan, err := s.DB.ExplainPlan(ctx, profile, sql)
		if err != nil {
			cands[i].VerifyNote = "explain failed: " + truncateAdvisor(err.Error(), 120)
			continue
		}
		cands[i].Verified = true
		cands[i].PlanCost = plan.TotalCost
		if planHasFullScanOn(plan, cands[i].Table) {
			cands[i].PlanConfirms = true
			cands[i].VerifyNote = "실행계획에서 대상 테이블 풀스캔 확인 — 인덱스 효과 기대"
			confirmed++
		} else {
			cands[i].VerifyNote = "실행계획에 대상 테이블 풀스캔이 없음 — 우선순위 낮음(플래너가 이미 커버 가능)"
		}
	}
	res["candidates"] = cands
	res["verified"] = true
	res["plan_confirmed_count"] = confirmed
	res["verify_note"] = "상위 후보에 대해 실제 EXPLAIN(읽기 전용)을 수행해 풀스캔 여부로 인덱스 효과를 검증했습니다. plan_confirms=true 후보를 우선 검토하세요."
	return res
}

// planHasFullScanOn reports whether any plan step is a full/sequential scan on
// the given table (matched by the table's last name segment, dialect-agnostic).
func planHasFullScanOn(plan *dbconn.PlanResult, tableFQN string) bool {
	name := tableFQN
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(strings.Trim(name, "\"`[]"))
	for _, st := range plan.Steps {
		op := strings.ToUpper(st.Operation + " " + st.Options)
		isScan := strings.Contains(op, "SEQ SCAN") || // postgres
			strings.Contains(op, "TABLE ACCESS FULL") || // oracle-style
			strings.Contains(op, "FULL") || // generic full scan
			op == "ALL" || strings.HasPrefix(op, "ALL ") // mysql type=ALL
		if !isScan {
			continue
		}
		obj := strings.ToLower(strings.Trim(st.ObjectName, "\"`[]"))
		if obj == "" || obj == name || strings.HasSuffix(obj, "."+name) {
			return true
		}
	}
	return false
}
