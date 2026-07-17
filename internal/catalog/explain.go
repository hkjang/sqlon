package catalog

import (
	"regexp"
	"sort"
	"strings"
)

// SQL → natural language (DBA co-pilot). ExplainSQL describes, in plain Korean,
// what a SELECT/DML statement does — which tables (with their logical names from
// the catalog), what it filters/groups/orders on, and which aggregates it
// computes. Heuristic (regex over masked SQL), so it summarizes structure rather
// than executing or fully parsing the statement.

var (
	expSelectListRE = regexp.MustCompile(`(?is)\bSELECT\s+(?:DISTINCT\s+)?(.*?)\s+\bFROM\b`)
	expFromRE       = regexp.MustCompile(`(?is)\bFROM\b(.*?)(?:\bWHERE\b|\bGROUP\s+BY\b|\bHAVING\b|\bORDER\s+BY\b|\bLIMIT\b|$)`)
	expWhereRE      = regexp.MustCompile(`(?is)\bWHERE\b(.*?)(?:\bGROUP\s+BY\b|\bHAVING\b|\bORDER\s+BY\b|\bLIMIT\b|$)`)
	expGroupRE      = regexp.MustCompile(`(?is)\bGROUP\s+BY\b(.*?)(?:\bHAVING\b|\bORDER\s+BY\b|\bLIMIT\b|$)`)
	expOrderRE      = regexp.MustCompile(`(?is)\bORDER\s+BY\b(.*?)(?:\bLIMIT\b|\bOFFSET\b|$)`)
	expLimitRE      = regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)`)
	expJoinRE       = regexp.MustCompile(`(?i)\b(?:INNER|LEFT|RIGHT|FULL|CROSS)?\s*(?:OUTER\s+)?JOIN\b`)
	expAggRE        = regexp.MustCompile(`(?i)\b(COUNT|SUM|AVG|MIN|MAX)\s*\(`)
	expStmtRE       = regexp.MustCompile(`(?is)^\s*(?:WITH\b.*?\)\s*)?(SELECT|INSERT|UPDATE|DELETE)\b`)
)

// ExplainSQLWords returns a structured + prose description of the SQL.
func (c *Catalog) ExplainSQLWords(sql string) map[string]any {
	masked := maskSQL(sql)

	stmt := "SELECT"
	if m := expStmtRE.FindStringSubmatch(masked); m != nil {
		stmt = strings.ToUpper(m[1])
	}

	// tables (resolved against the catalog, with logical names)
	fqns := c.sqlTables(sql)
	type tbl struct {
		FQN     string `json:"fqn"`
		Logical string `json:"logical_name,omitempty"`
		Desc    string `json:"description,omitempty"`
	}
	var tables []tbl
	labels := map[string]string{}
	for _, fqn := range fqns {
		t, ok := c.ResolveTable(fqn)
		if !ok {
			continue
		}
		lab := t.FQN
		if t.LogicalName != "" {
			lab = t.LogicalName
		}
		labels[fqn] = lab
		tables = append(tables, tbl{FQN: t.FQN, Logical: t.LogicalName, Desc: t.Description})
	}

	aggs := map[string]bool{}
	for _, m := range expAggRE.FindAllStringSubmatch(masked, -1) {
		aggs[strings.ToUpper(m[1])] = true
	}
	aggList := make([]string, 0, len(aggs))
	for a := range aggs {
		aggList = append(aggList, a)
	}
	sort.Strings(aggList)

	selectList := ""
	if m := expSelectListRE.FindStringSubmatch(masked); m != nil {
		selectList = normSpace(m[1])
	}
	star := strings.Contains(selectList, "*")

	groupCols := splitCols(firstGroup(expGroupRE, masked))
	orderCols := splitCols(firstGroup(expOrderRE, masked))
	hasWhere := false
	if m := expWhereRE.FindStringSubmatch(masked); m != nil && strings.TrimSpace(m[1]) != "" {
		hasWhere = true
	}
	joinCount := len(expJoinRE.FindAllString(masked, -1))
	limit := ""
	if m := expLimitRE.FindStringSubmatch(masked); m != nil {
		limit = m[1]
	}

	// prose
	var b strings.Builder
	tableNames := make([]string, 0, len(tables))
	for _, fqn := range fqns {
		if l, ok := labels[fqn]; ok {
			tableNames = append(tableNames, l)
		}
	}
	subject := "데이터"
	if len(tableNames) > 0 {
		subject = strings.Join(tableNames, ", ")
	}
	switch stmt {
	case "SELECT":
		if len(aggList) > 0 && len(groupCols) > 0 {
			b.WriteString(subject + " 에서 " + strings.Join(groupCols, ", ") + " 별로 " + aggPhrase(aggList) + " 을(를) 집계합니다")
		} else if len(aggList) > 0 {
			b.WriteString(subject + " 전체에 대해 " + aggPhrase(aggList) + " 을(를) 계산합니다")
		} else if star {
			b.WriteString(subject + " 의 모든 컬럼을 조회합니다")
		} else {
			b.WriteString(subject + " 에서 데이터를 조회합니다")
		}
	case "INSERT":
		b.WriteString(subject + " 에 행을 추가합니다")
	case "UPDATE":
		b.WriteString(subject + " 의 행을 수정합니다")
	case "DELETE":
		b.WriteString(subject + " 의 행을 삭제합니다")
	}
	if joinCount > 0 {
		b.WriteString("(테이블 " + itoa(len(tables)) + "개를 조인)")
	}
	if hasWhere {
		b.WriteString(". 조건에 맞는 행으로 한정합니다")
	}
	if len(orderCols) > 0 {
		b.WriteString(". " + strings.Join(orderCols, ", ") + " 순으로 정렬합니다")
	}
	if limit != "" {
		b.WriteString(". 최대 " + limit + "행을 반환합니다")
	}
	b.WriteString(".")

	return map[string]any{
		"statement":   stmt,
		"tables":      tables,
		"aggregates":  aggList,
		"group_by":    groupCols,
		"order_by":    orderCols,
		"has_filter":  hasWhere,
		"join_count":  joinCount,
		"limit":       limit,
		"select_star": star,
		"summary":     b.String(),
		"note":        "카탈로그 논리명을 활용한 구조 요약입니다(정적 분석, 실행하지 않음).",
	}
}

func aggPhrase(aggs []string) string {
	m := map[string]string{"COUNT": "건수", "SUM": "합계", "AVG": "평균", "MIN": "최소값", "MAX": "최대값"}
	parts := make([]string, 0, len(aggs))
	for _, a := range aggs {
		if p, ok := m[a]; ok {
			parts = append(parts, p)
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, "·")
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func splitCols(s string) []string {
	s = normSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		f := strings.Fields(strings.TrimSpace(p)) // drop ASC/DESC
		if len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

func normSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
