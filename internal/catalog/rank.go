package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// RankCandidates scores multiple candidate SQLs with objective, server-side
// signals — validation errors, policy warnings, risk estimate, result-schema
// coverage, metric conformity — and returns them best-first. This implements
// self-consistency for complex questions without trusting LLM self-grading.
func (c *Catalog) RankCandidates(question string, sqls []string, expectedOutputs, metrics []string, limit int) map[string]any {
	type ranked struct {
		Index      int      `json:"index"`
		SQL        string   `json:"sql"`
		Score      float64  `json:"score"`
		Valid      bool     `json:"valid"`
		Risk       string   `json:"risk"`
		RiskScore  int      `json:"risk_score"`
		Errors     int      `json:"errors"`
		Warnings   int      `json:"warnings"`
		Deductions []string `json:"deductions,omitempty"`
		BoundedSQL string   `json:"bounded_sql,omitempty"`
	}
	out := make([]ranked, 0, len(sqls))
	for i, sql := range sqls {
		req := ValidateRequest{SQL: sql, Limit: limit, Metrics: metrics, ExpectedOutputs: expectedOutputs}
		v := c.ValidateSQL(req)
		ex := c.ExplainSQL(req)
		riskScore, _ := ex["risk_score"].(int)
		risk, _ := ex["risk"].(string)
		score := 100.0
		var deductions []string
		if !v.Valid {
			score -= 1000
			deductions = append(deductions, "validation failed")
		}
		score -= float64(len(v.Errors)) * 50
		for _, e := range v.Errors {
			deductions = append(deductions, "error "+e.Code+": "+e.Message)
		}
		for _, w := range v.Warnings {
			penalty := 5.0
			switch {
			case w.Code == "EXPECTED_OUTPUT_MISSING" || w.Code == "METRIC_MISMATCH":
				penalty = 25
			case strings.HasPrefix(w.Code, "LEARNED_"):
				penalty = 15
			case w.Code == "UNVERIFIED_JOIN" || w.Code == "LOW_CONFIDENCE_JOIN":
				penalty = 20
			}
			score -= penalty
			deductions = append(deductions, fmt.Sprintf("warning %s (-%.0f): %s", w.Code, penalty, w.Message))
		}
		score -= float64(riskScore) * 0.5
		if riskScore > 0 {
			deductions = append(deductions, fmt.Sprintf("risk_score %d (-%.1f)", riskScore, float64(riskScore)*0.5))
		}
		if hasRowBound(strings.ToUpper(maskSQL(sql))) {
			score += 10
		} else {
			deductions = append(deductions, "no explicit row bound")
		}
		out = append(out, ranked{
			Index: i, SQL: sql, Score: round(score), Valid: v.Valid,
			Risk: risk, RiskScore: riskScore,
			Errors: len(v.Errors), Warnings: len(v.Warnings),
			Deductions: deductions, BoundedSQL: v.BoundedSQL,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	res := map[string]any{
		"question":   question,
		"ranking":    out,
		"candidates": len(sqls),
	}
	if len(out) > 0 {
		res["best_index"] = out[0].Index
		res["best_sql"] = out[0].SQL
		if !out[0].Valid {
			res["guidance"] = "모든 후보가 검증에 실패했습니다. 1위 후보의 deductions와 validate_sql fix_hints를 반영해 재생성하세요."
		} else if len(out) > 1 && out[1].Valid && out[0].Score-out[1].Score < 10 {
			res["guidance"] = "상위 후보 점수가 근접합니다. deductions를 비교해 업무 의도에 맞는 쪽을 선택하세요."
		}
	}
	return res
}
