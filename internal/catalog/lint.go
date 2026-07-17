package catalog

import (
	"regexp"
	"sort"
	"strings"
)

// SQL anti-pattern linter (DBA co-pilot). LintSQL runs a set of static,
// catalog-aware heuristics over a single statement and reports classic
// performance/correctness smells with a concrete suggestion. It is advisory:
// it flags patterns, it does not rewrite the SQL. Catalog-aware rules
// (non-sargable function, inequality on an indexed column) consult the
// resolved tables' index coverage to avoid noise on un-indexed columns.

// LintFinding is one anti-pattern hit.
type LintFinding struct {
	Rule       string `json:"rule"`
	Severity   string `json:"severity"` // high | medium | low | info
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
	Snippet    string `json:"snippet,omitempty"`
}

var (
	lintSelectStarRE   = regexp.MustCompile(`(?i)\bSELECT\s+(?:DISTINCT\s+)?\*`)
	lintDistinctRE     = regexp.MustCompile(`(?i)\bSELECT\s+DISTINCT\b`)
	lintNotInRE        = regexp.MustCompile(`(?i)\bNOT\s+IN\s*\(\s*SELECT\b`)
	lintOrderByRE      = regexp.MustCompile(`(?i)\bORDER\s+BY\b`)
	lintLimitRE        = regexp.MustCompile(`(?i)\b(?:LIMIT|FETCH\s+FIRST|OFFSET)\b`)
	lintOrRE           = regexp.MustCompile(`(?i)\bWHERE\b[\s\S]*?\bOR\b`)
	lintLeadWildcardRE = regexp.MustCompile(`(?i)\bLIKE\s+N?'%`)
	// FUNC(col) <op> — a function wrapping a bare column on the predicate side
	lintFuncPredRE = regexp.MustCompile(`(?i)\b([a-z_][\w]*)\s*\(\s*([a-z_][\w$#]*(?:\s*\.\s*[a-z_][\w$#]*)?)\s*\)\s*(?:=|<|>|<=|>=|<>|!=|\bLIKE\b|\bIN\b)`)
	// col <> / col != — inequality against a bare column
	lintInequalityRE = regexp.MustCompile(`(?i)\b([a-z_][\w$#]*(?:\s*\.\s*[a-z_][\w$#]*)?)\s*(?:<>|!=)`)
	// old-style comma join in FROM: FROM a, b (before WHERE/GROUP/etc.)
	lintCommaFromRE = regexp.MustCompile(`(?i)\bFROM\s+([a-z_][\w$#.]*(?:\s+\w+)?\s*,\s*[a-z_][\w$#.]*)`)
	lintDmlRE       = regexp.MustCompile(`(?i)^\s*(?:WITH\b[\s\S]*?\)\s*)?(UPDATE|DELETE)\b`)
	lintWhereRE     = regexp.MustCompile(`(?i)\bWHERE\b`)
	// known SQL keywords that lintFuncPredRE would otherwise treat as functions
	notAFunction = map[string]bool{
		"IN": true, "AND": true, "OR": true, "NOT": true, "ON": true, "WHERE": true,
		"WHEN": true, "THEN": true, "CASE": true, "VALUES": true, "EXISTS": true, "ALL": true, "ANY": true,
	}
)

// LintSQL returns anti-pattern findings for the SQL, most severe first.
func (c *Catalog) LintSQL(sql string) []LintFinding {
	masked := maskSQL(sql) // strings/comments blanked → no false hits inside literals
	var out []LintFinding
	add := func(rule, sev, msg, sug, snip string) {
		out = append(out, LintFinding{Rule: rule, Severity: sev, Message: msg, Suggestion: sug, Snippet: strings.TrimSpace(snip)})
	}

	// index coverage of the query's columns (uppercase col name → indexed)
	indexed := c.indexedColumns(sql)

	if m := lintSelectStarRE.FindString(masked); m != "" {
		add("select_star", "medium",
			"SELECT * 는 불필요한 컬럼까지 읽어 네트워크·정렬·커버링 인덱스 활용을 방해합니다.",
			"필요한 컬럼만 명시하세요.", m)
	}
	if lintDistinctRE.MatchString(masked) {
		add("select_distinct", "low",
			"SELECT DISTINCT 는 조인 팬아웃(중복 폭증)을 가리는 신호일 수 있습니다.",
			"중복 원인(부정확한 조인 조건)을 먼저 점검하고, 정말 필요할 때만 사용하세요.", "")
	}
	// maskSQL blanks the literal (the '%' with it), so match the leading wildcard
	// on the raw SQL. `LIKE '%…` requires the quote immediately before %, which
	// keeps it from tripping on unrelated text.
	if lintLeadWildcardRE.MatchString(sql) {
		add("leading_wildcard_like", "medium",
			"선두 와일드카드 LIKE('%…')는 B-tree 인덱스를 사용하지 못해 풀스캔을 유발합니다.",
			"접두 검색으로 바꾸거나(‘abc%’), 전문검색(tsvector/FULLTEXT)·트라이그램 인덱스를 검토하세요.", "LIKE '%…'")
	}
	if m := lintNotInRE.FindString(masked); m != "" {
		add("not_in_subquery", "medium",
			"NOT IN (SELECT …) 는 서브쿼리에 NULL이 하나라도 있으면 결과가 비고, 최적화도 불리합니다.",
			"NOT EXISTS 또는 LEFT JOIN … IS NULL 로 바꾸세요.", m)
	}
	if lintOrderByRE.MatchString(masked) && !lintLimitRE.MatchString(masked) {
		add("order_by_no_limit", "info",
			"LIMIT 없는 ORDER BY 는 전체 결과를 정렬합니다(대형 결과셋에서 비용 큼).",
			"페이지네이션이 목적이라면 LIMIT/OFFSET(또는 키셋 페이지네이션)을 함께 사용하세요.", "")
	}
	if lintOrRE.MatchString(masked) {
		add("or_in_where", "low",
			"WHERE 절의 OR 는 인덱스 사용을 막을 수 있습니다.",
			"동일 컬럼이면 IN (…), 서로 다른 컬럼이면 UNION ALL 분해를 검토하세요.", "")
	}
	if m := lintCommaFromRE.FindString(masked); m != "" {
		add("implicit_cross_join", "medium",
			"콤마 조인(FROM a, b)은 조인 조건 누락 시 카테시안 곱을 만들기 쉽습니다.",
			"명시적 JOIN … ON 구문을 사용하세요.", strings.TrimSpace(m))
	}
	if m := lintDmlRE.FindStringSubmatch(masked); m != nil && !lintWhereRE.MatchString(masked) {
		add("missing_where_dml", "high",
			strings.ToUpper(m[1])+" 문에 WHERE 절이 없어 전체 행에 영향을 줍니다.",
			"의도한 범위로 한정하는 WHERE 절을 반드시 추가하세요.", strings.ToUpper(m[1]))
	}

	// catalog-aware: function-wrapped indexed column (non-sargable)
	fseen := map[string]bool{}
	for _, m := range lintFuncPredRE.FindAllStringSubmatch(masked, -1) {
		fn := strings.ToUpper(m[1])
		if notAFunction[fn] {
			continue
		}
		col := lastSegment(m[2])
		if col == "" || fseen[col] {
			continue
		}
		if indexed[col] {
			fseen[col] = true
			add("non_sargable_function", "high",
				"인덱스 컬럼 "+col+" 을(를) 함수("+fn+")로 감싸면 인덱스를 사용하지 못합니다.",
				"컬럼을 함수 없이 그대로 두고 상수 쪽을 변환하거나, 함수 기반 인덱스를 만드세요.", m[0])
		}
	}
	// catalog-aware: inequality on an indexed column
	iseen := map[string]bool{}
	for _, m := range lintInequalityRE.FindAllStringSubmatch(masked, -1) {
		col := lastSegment(m[1])
		if col == "" || iseen[col] || !indexed[col] {
			continue
		}
		iseen[col] = true
		add("inequality_on_indexed", "low",
			"인덱스 컬럼 "+col+" 에 대한 부등호(<>, !=)는 인덱스 범위 스캔을 활용하기 어렵습니다.",
			"가능하면 동등/범위 조건으로 재구성하거나, 해당 조건의 선택도를 확인하세요.", strings.TrimSpace(m[0]))
	}

	sev := map[string]int{"high": 0, "medium": 1, "low": 2, "info": 3}
	sort.SliceStable(out, func(i, j int) bool { return sev[out[i].Severity] < sev[out[j].Severity] })
	return out
}

// indexedColumns returns the set of column names (uppercase) that are covered by
// an index (PK, marked indexed, or leading index column) across the tables the
// SQL references.
func (c *Catalog) indexedColumns(sql string) map[string]bool {
	out := map[string]bool{}
	for _, fqn := range c.sqlTables(sql) {
		t, ok := c.ResolveTable(fqn)
		if !ok {
			continue
		}
		for _, col := range t.Columns {
			if t.columnHasLeadingIndex(col.Name) {
				out[strings.ToUpper(col.Name)] = true
			}
		}
	}
	return out
}

// SQLTables returns the catalog-resolved table FQNs referenced by the SQL.
// Exported for callers (workload/index advisor) outside the package.
func (c *Catalog) SQLTables(sql string) []string { return c.sqlTables(sql) }

// lastSegment returns the trailing identifier of a possibly-dotted, possibly
// spaced reference (e.g. "t . col" → "COL"), uppercased and cleaned.
func lastSegment(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndex(raw, "."); i >= 0 {
		raw = raw[i+1:]
	}
	return cleanIdent(raw)
}
