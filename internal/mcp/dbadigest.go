package mcp

import (
	"fmt"
	"strings"
)

// DBA digest (DBA co-pilot, proactive). A compact operational snapshot distilled
// from the workload report and the index advisor: query volume, error rate, tail
// latency, slow-query count, the hottest tables, and the top index candidates —
// with a one-line headline. Read-only. Designed to be posted to the digest
// webhook on each scheduler tick so a DBA gets a periodic "what needs attention"
// nudge without opening the console.

// mcpDBADigest builds the compact DBA digest for a profile (empty = all).
func (s *Server) mcpDBADigest(profile string, days, slowMs int) map[string]any {
	if days <= 0 {
		days = 7
	}
	if slowMs <= 0 {
		slowMs = 200
	}
	wl := s.mcpWorkloadReport(profile, days, slowMs)
	idx := s.mcpSuggestIndexes(profile, slowMs, days)

	total, _ := wl["total_queries"].(int)
	errRate, _ := wl["error_rate"].(float64)
	slow, _ := wl["slow_queries"].(int)
	lat, _ := wl["latency_ms"].(map[string]any)
	var p95, maxMs int64
	if lat != nil {
		p95, _ = lat["p95"].(int64)
		maxMs, _ = lat["max"].(int64)
	}

	// top 3 index candidates, trimmed to the essentials
	top := []map[string]any{}
	if cands, ok := idx["candidates"].([]indexCandidate); ok {
		for i, c := range cands {
			if i >= 3 {
				break
			}
			top = append(top, map[string]any{
				"table": c.Table, "column": c.Column,
				"occurrences": c.Occurrences, "avg_ms": c.AvgMs, "ddl": c.DDL,
			})
		}
	}
	idxCount, _ := idx["count"].(int)

	headline := dbaDigestHeadline(total, errRate, slow, p95, idxCount)

	return map[string]any{
		"profile":               profile,
		"window_days":           days,
		"slow_ms":               slowMs,
		"total_queries":         total,
		"error_rate":            errRate,
		"slow_queries":          slow,
		"latency_p95_ms":        p95,
		"latency_max_ms":        maxMs,
		"top_tables":            wl["top_tables"],
		"index_candidate_count": idxCount,
		"top_index_candidates":  top,
		"peak_hour":             wl["peak_hour"],
		"headline":              headline,
		"note":                  "감사 로그 기반 DBA 운영 요약(읽기 전용). 인덱스 후보 DDL은 검토 후 수행하세요. 상세: /admin/dba, suggest_indexes, workload_report.",
	}
}

// dbaDigestHeadline renders a one-line human summary for alerting.
func dbaDigestHeadline(total int, errRate float64, slow int, p95 int64, idxCount int) string {
	if total == 0 {
		return "최근 기간에 기록된 쿼리가 없습니다."
	}
	parts := []string{fmt.Sprintf("쿼리 %d건", total)}
	if errRate > 0 {
		parts = append(parts, fmt.Sprintf("오류율 %.0f%%", errRate*100))
	}
	parts = append(parts, fmt.Sprintf("p95 %dms", p95))
	if slow > 0 {
		parts = append(parts, fmt.Sprintf("느린쿼리 %d건", slow))
	}
	if idxCount > 0 {
		parts = append(parts, fmt.Sprintf("인덱스 후보 %d건", idxCount))
	}
	flag := "✅"
	if errRate >= 0.1 || idxCount > 0 || slow > 0 {
		flag = "⚠️"
	}
	return flag + " " + strings.Join(parts, " · ")
}
