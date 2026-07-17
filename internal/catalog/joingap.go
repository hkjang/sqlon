package catalog

import (
	"path/filepath"
	"sort"
	"strings"
)

// Join-gap analysis: golden queries whose expected tables have no path in the
// join graph are exactly where Join Path Recall is lost. For each unconnected
// pair this suggests candidate join keys (shared column names, PK/key-like
// names first) as ready-to-review relations.json entries — operator approves
// via /admin/editor?ds=relations (human-in-the-loop, design §7).

type JoinGap struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Questions  []string `json:"questions"` // golden questions blocked by this gap
	Candidates []string `json:"candidate_join_keys,omitempty"`
	Suggested  any      `json:"suggested_relation,omitempty"` // paste-ready relations.json entry
}

// SuggestJoinRelations scans the golden set for expected-table pairs with no
// join path and proposes join keys from shared columns.
func (c *Catalog) SuggestJoinRelations(goldenPath string) (map[string]any, error) {
	if goldenPath == "" {
		goldenPath = filepath.Join(c.DataDir, "golden_queries.json")
	}
	var golden []GoldenQuery
	if err := readJSON(goldenPath, &golden); err != nil {
		return nil, err
	}
	gaps := map[string]*JoinGap{} // "FROM->TO"
	for _, g := range golden {
		if len(g.ExpectedTables) < 2 {
			continue
		}
		out, err := c.GetJoinPaths(JoinPathRequest{Tables: g.ExpectedTables})
		if err != nil {
			continue
		}
		paths, _ := out["join_paths"].([]JoinPathResult)
		for _, p := range paths {
			if p.Found {
				continue
			}
			key := p.From + "->" + p.To
			gap := gaps[key]
			if gap == nil {
				gap = &JoinGap{From: p.From, To: p.To}
				gaps[key] = gap
			}
			gap.Questions = appendUnique(gap.Questions, g.Question)
		}
	}
	// suggest join keys per gap: shared column names, key-like first
	keyLike := func(name string) int {
		u := strings.ToUpper(name)
		switch {
		case strings.HasSuffix(u, "_NO") || strings.HasSuffix(u, "_ID") || u == "ID":
			return 0
		case strings.HasSuffix(u, "_CD") || strings.HasSuffix(u, "_SNO") || strings.HasSuffix(u, "_KEY"):
			return 1
		default:
			return 2
		}
	}
	out := []JoinGap{}
	for _, gap := range gaps {
		ft, ok1 := c.ResolveTable(gap.From)
		tt, ok2 := c.ResolveTable(gap.To)
		if ok1 && ok2 {
			toCols := map[string]bool{}
			for _, col := range tt.Columns {
				toCols[strings.ToUpper(col.Name)] = true
			}
			shared := []string{}
			for _, col := range ft.Columns {
				if toCols[strings.ToUpper(col.Name)] {
					shared = append(shared, col.Name)
				}
			}
			sort.SliceStable(shared, func(i, j int) bool { return keyLike(shared[i]) < keyLike(shared[j]) })
			if len(shared) > 5 {
				shared = shared[:5]
			}
			gap.Candidates = shared
			if len(shared) > 0 {
				gap.Suggested = map[string]any{
					"base_schema": ft.Schema, "base_table": ft.Name, "base_column": shared[0],
					"reference_schema": tt.Schema, "reference_table": tt.Name, "reference_column": shared[0],
					"join_type": "INNER", "confidence": 0.5,
					"description": "골든셋 조인 갭에서 자동 제안 — 운영자 검토 필요",
				}
			}
		}
		out = append(out, *gap)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Questions) > len(out[j].Questions) })
	return map[string]any{
		"golden_path": goldenPath,
		"gap_count":   len(out),
		"gaps":        out,
		"next_step":   "suggested_relation을 검토해 /admin/editor?ds=relations 에 추가하면 Join Path Recall이 회복됩니다. confidence·조인키는 반드시 사람이 확인하세요.",
	}, nil
}
