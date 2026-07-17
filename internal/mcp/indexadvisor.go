package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Index advisor (DBA co-pilot). Mines the query audit log (query-*.jsonl) for
// slow, successful queries and, using the catalog's predicate-column extraction
// + index-coverage check, proposes candidate indexes with ready-to-review DDL.
// Read-only and advisory: it never creates anything — a DBA reviews the DDL.

type indexCandidate struct {
	Table       string  `json:"table"`
	Column      string  `json:"column"`
	Occurrences int     `json:"occurrences"` // # slow queries filtering/joining on it
	AvgMs       int64   `json:"avg_ms"`      // avg elapsed of those queries
	MaxMs       int64   `json:"max_ms"`      //
	DDL         string  `json:"ddl"`         // suggested CREATE INDEX
	SampleSQL   string  `json:"sample_sql"`  // one representative slow query
	Score       float64 `json:"score"`       // occurrences * avg_ms (ranking)

	// populated only when verify=true and a profile is given (live EXPLAIN)
	Verified     bool   `json:"verified,omitempty"`      // an EXPLAIN was run for this candidate
	PlanConfirms bool   `json:"plan_confirms,omitempty"` // plan shows a full/seq scan on the table
	PlanCost     int64  `json:"plan_cost,omitempty"`     // estimated total cost from EXPLAIN
	VerifyNote   string `json:"verify_note,omitempty"`
}

// mcpSuggestIndexes analyzes the query audit log and proposes indexes.
func (s *Server) mcpSuggestIndexes(profile string, minElapsedMs, days int) map[string]any {
	if minElapsedMs <= 0 {
		minElapsedMs = 200
	}
	if days <= 0 {
		days = 7
	}
	cat, catSource := s.catalogFor(profile)

	dir := filepath.Join(s.opDir(), "audit")
	cutoff := time.Now().AddDate(0, 0, -days)

	type agg struct {
		occ    int
		sumMs  int64
		maxMs  int64
		sample string
		table  string
		column string
	}
	byCol := map[string]*agg{}
	scanned, slow := 0, 0

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "query-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// query-YYYYMMDD.jsonl → skip files older than the window
		if day := strings.TrimSuffix(strings.TrimPrefix(name, "query-"), ".jsonl"); len(day) == 8 {
			if t, err := time.Parse("20060102", day); err == nil && t.Before(cutoff.Truncate(24*time.Hour)) {
				continue
			}
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var rec struct {
				Entry struct {
					SQLText   string `json:"sql_text"`
					ProfileID string `json:"db_profile_id"`
					ElapsedMs int64  `json:"elapsed_ms"`
					Success   bool   `json:"success"`
				} `json:"entry"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			en := rec.Entry
			if en.SQLText == "" || !en.Success {
				continue
			}
			if profile != "" && !strings.EqualFold(en.ProfileID, profile) {
				continue
			}
			scanned++
			if en.ElapsedMs < int64(minElapsedMs) {
				continue
			}
			slow++
			for _, pc := range cat.PredicateColumns(en.SQLText) {
				if pc.Indexed {
					continue // already covered
				}
				key := pc.Table + "|" + pc.Column
				a := byCol[key]
				if a == nil {
					a = &agg{table: pc.Table, column: pc.Column, sample: truncateAdvisor(en.SQLText, 200)}
					byCol[key] = a
				}
				a.occ++
				a.sumMs += en.ElapsedMs
				if en.ElapsedMs > a.maxMs {
					a.maxMs = en.ElapsedMs
				}
			}
		}
		_ = f.Close()
	}

	cands := make([]indexCandidate, 0, len(byCol))
	for _, a := range byCol {
		avg := a.sumMs / int64(a.occ)
		cands = append(cands, indexCandidate{
			Table: a.table, Column: a.column, Occurrences: a.occ,
			AvgMs: avg, MaxMs: a.maxMs, SampleSQL: a.sample,
			DDL:   createIndexDDL(a.table, a.column),
			Score: float64(a.occ) * float64(avg),
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].Table+cands[i].Column < cands[j].Table+cands[j].Column
	})

	res := map[string]any{
		"profile":         profile,
		"window_days":     days,
		"min_elapsed_ms":  minElapsedMs,
		"scanned_queries": scanned,
		"slow_queries":    slow,
		"candidates":      cands,
		"count":           len(cands),
		"catalog_source":  catSource,
		"note":            "감사 로그의 느린 쿼리에서 인덱스 미적용 필터/조인/정렬 컬럼을 집계한 검토용 제안입니다. DDL은 DBA가 검토 후 수행하세요(자동 실행하지 않으며, 실행계획·카디널리티·쓰기부하를 함께 고려하세요).",
	}
	return res
}

// createIndexDDL renders a review-ready CREATE INDEX for a schema.table.column.
func createIndexDDL(fqn, column string) string {
	schema, table := "", fqn
	if i := strings.LastIndex(fqn, "."); i >= 0 {
		schema, table = fqn[:i], fqn[i+1:]
	}
	idx := "idx_" + strings.ToLower(sanitizeIdent(table)) + "_" + strings.ToLower(sanitizeIdent(column))
	target := table
	if schema != "" {
		target = schema + "." + table
	}
	return "CREATE INDEX " + idx + " ON " + strings.ToLower(target) + " (" + strings.ToLower(column) + ");"
}

func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func truncateAdvisor(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
