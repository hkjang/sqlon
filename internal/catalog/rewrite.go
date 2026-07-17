package catalog

import (
	"regexp"
	"strings"
)

// lintJoinRE detects any explicit JOIN so SELECT * expansion refuses
// multi-table queries (column ownership would be ambiguous).
var lintJoinRE = regexp.MustCompile(`(?i)\bJOIN\b`)

// SQL rewrite co-pilot. SuggestRewrites turns anti-pattern lint findings into
// concrete before→after rewrite guidance. It is deliberately advisory and
// never claims semantic equivalence automatically: arbitrary SQL cannot be
// safely rewritten by static heuristics, so each suggestion is a template the
// DBA adapts and MUST verify (with EXPLAIN) before adopting. The one exception
// that is safe to materialize is expanding SELECT * into the resolved table's
// real column list from the catalog.

// RewriteSuggestion is one anti-pattern with a rewrite template. AutoApplicable
// is true only when the rewrite is a safe, exact transformation (currently just
// SELECT * column expansion); it is false for every semantic transformation,
// which requires human review.
type RewriteSuggestion struct {
	Rule           string `json:"rule"`
	Severity       string `json:"severity"`
	Message        string `json:"message"`
	Before         string `json:"before"`
	After          string `json:"after"`
	Note           string `json:"note"`
	AutoApplicable bool   `json:"auto_applicable"`
}

// rewriteTemplates maps a lint rule to an illustrative before→after pattern.
// These are generic templates, not drop-in SQL — the parameters (columns,
// tables, predicates) must be adapted to the actual query.
var rewriteTemplates = map[string]struct{ before, after, note string }{
	"not_in_subquery": {
		before: "... WHERE x NOT IN (SELECT y FROM t2)",
		after:  "... WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.y = x)",
		note:   "NOT EXISTS는 서브쿼리의 NULL에 영향받지 않고 대개 더 효율적입니다. 상관 조건(t2.y = x)을 실제 컬럼으로 맞추세요.",
	},
	"or_in_where": {
		before: "WHERE a = 1 OR a = 2            -- 같은 컬럼\nWHERE a = 1 OR b = 2            -- 다른 컬럼",
		after:  "WHERE a IN (1, 2)              -- 같은 컬럼: IN\n(SELECT ... WHERE a = 1) UNION ALL (SELECT ... WHERE b = 2)  -- 다른 컬럼: UNION ALL",
		note:   "같은 컬럼의 OR는 IN으로, 서로 다른 컬럼의 OR는 각 조건이 인덱스를 타도록 UNION ALL로 분해하세요(결과 중복 여부 확인).",
	},
	"leading_wildcard_like": {
		before: "WHERE col LIKE '%abc'",
		after:  "-- 접두 검색이면: WHERE col LIKE 'abc%'\n-- 부분 검색이 필요하면 트라이그램(pg_trgm) 또는 전문검색(FULLTEXT/tsvector) 인덱스 사용",
		note:   "선두 와일드카드는 B-tree 인덱스를 쓰지 못합니다. 검색 요구사항에 맞는 인덱스 전략으로 바꾸세요.",
	},
	"non_sargable_function": {
		before: "WHERE DATE(ts_col) = '2026-07-17'",
		after:  "WHERE ts_col >= '2026-07-17' AND ts_col < '2026-07-18'",
		note:   "인덱스 컬럼을 함수로 감싸지 말고 상수 쪽을 범위로 변환하세요. 불가하면 함수 기반 인덱스를 검토하세요.",
	},
	"implicit_cross_join": {
		before: "FROM a, b WHERE a.id = b.a_id",
		after:  "FROM a JOIN b ON a.id = b.a_id",
		note:   "명시적 JOIN … ON은 조인 조건 누락으로 인한 카테시안 곱을 예방합니다.",
	},
	"select_star": {
		before: "SELECT * FROM t",
		after:  "SELECT c1, c2, c3 FROM t   -- 실제로 필요한 컬럼만",
		note:   "필요한 컬럼만 명시하면 I/O·정렬 비용이 줄고 커버링 인덱스를 활용할 수 있습니다.",
	},
}

// SuggestRewrites runs the linter and attaches rewrite templates. For
// select_star it materializes the real column list of the resolved single
// table (a safe, exact expansion) and marks it auto-applicable.
func (c *Catalog) SuggestRewrites(sql string) []RewriteSuggestion {
	findings := c.LintSQL(sql)
	var out []RewriteSuggestion
	for _, f := range findings {
		tmpl, ok := rewriteTemplates[f.Rule]
		if !ok {
			continue // finding has prose guidance but no structured rewrite
		}
		s := RewriteSuggestion{Rule: f.Rule, Severity: f.Severity, Message: f.Message, Before: tmpl.before, After: tmpl.after, Note: tmpl.note}
		if f.Rule == "select_star" {
			if expanded, ok := c.expandSelectStar(sql); ok {
				s.After = expanded
				s.Note = "카탈로그에서 확인한 실제 컬럼으로 확장했습니다. 필요 없는 컬럼은 제거하세요."
				s.AutoApplicable = true
			}
		}
		out = append(out, s)
	}
	return out
}

// expandSelectStar returns an explicit column list for SELECT * when the query
// resolves to exactly one catalog table AND has a single table reference (no
// JOIN and no comma-join), so column ownership is unambiguous. A self-join of
// one table still resolves to one FQN but is ambiguous, so those are refused.
func (c *Catalog) expandSelectStar(sql string) (string, bool) {
	masked := maskSQL(sql)
	if lintJoinRE.MatchString(masked) || lintCommaFromRE.MatchString(masked) {
		return "", false // multiple table references → ambiguous
	}
	tables := c.sqlTables(sql)
	if len(tables) != 1 {
		return "", false // multiple/zero tables → ambiguous, do not guess
	}
	t, ok := c.ResolveTable(tables[0])
	if !ok || len(t.Columns) == 0 {
		return "", false
	}
	names := make([]string, 0, len(t.Columns))
	for _, col := range t.Columns {
		names = append(names, col.Name)
	}
	return "SELECT " + strings.Join(names, ", ") + " FROM " + t.FQN, true
}
