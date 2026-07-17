package catalog

import (
	"regexp"
	"strings"
)

var (
	reEqLiteral  = regexp.MustCompile(`(?i)([A-Z0-9_]+)\.?([A-Z0-9_]*)\s*=\s*'([^']+)'`)
	reBetween    = regexp.MustCompile(`(?i)([A-Z0-9_.]+)\s+BETWEEN\s+'([^']+)'\s+AND\s+'([^']+)'`)
	reFromTables = regexp.MustCompile(`(?i)(?:FROM|JOIN)\s+([A-Z0-9_]+\.[A-Z0-9_]+)`)
	rePeriodLit  = regexp.MustCompile(`^\d{6}(\d{2})?$`)
	reWherePreds = regexp.MustCompile(`(?i)(?:WHERE|AND)\s+([A-Z0-9_.]+\s*(?:=|>=|<=|>|<|LIKE|IN|BETWEEN)[^)\n]{1,60})`)
)

// DiagnoseZeroRows explains why a validated, executed SELECT returned no rows
// and suggests concrete relaxations, grounded in catalog metadata (code
// dictionaries, time semantics). Returned hints are ordered most-likely first.
func (c *Catalog) DiagnoseZeroRows(sql string) []string {
	hints := []string{}
	upper := strings.ToUpper(sql)

	// resolve FROM/JOIN tables so column lookups are scoped
	tables := []*Table{}
	for _, m := range reFromTables.FindAllStringSubmatch(sql, -1) {
		if t, ok := c.ResolveTable(m[1]); ok {
			tables = append(tables, t)
		}
	}
	findColumn := func(name string) *Column {
		name = strings.ToUpper(name)
		for _, t := range tables {
			for _, col := range t.Columns {
				if strings.EqualFold(col.Name, name) {
					return col
				}
			}
		}
		return nil
	}

	// 1) equality literals vs code dictionaries: a value outside the code set
	// guarantees zero rows — the most common cause.
	for _, m := range reEqLiteral.FindAllStringSubmatch(sql, -1) {
		colName, val := m[1], m[3]
		if m[2] != "" {
			colName = m[2] // alias.column form
		}
		col := findColumn(colName)
		if col == nil || col.CodeDict == "" {
			continue
		}
		if !strings.Contains(strings.ToUpper(col.CodeDict), strings.ToUpper(val)) {
			dict := col.CodeDict
			if len([]rune(dict)) > 120 {
				dict = string([]rune(dict)[:120]) + "…"
			}
			hints = append(hints, "'"+val+"'는 "+col.Name+" 코드사전에 없는 값일 수 있습니다. 코드사전: "+dict)
		}
	}

	// 2) time-range predicates: future/most-recent periods are often not yet
	// loaded — suggest stepping back one period.
	for _, m := range reBetween.FindAllStringSubmatch(sql, -1) {
		if rePeriodLit.MatchString(m[2]) && rePeriodLit.MatchString(m[3]) {
			hints = append(hints, "기간 조건("+m[1]+" BETWEEN '"+m[2]+"' AND '"+m[3]+"')이 데이터 적재 범위 밖일 수 있습니다. 종료 기간을 한 기간 앞으로 당기거나 범위를 넓혀 확인하세요.")
			break
		}
	}
	if len(hints) == 0 && c.Overrides != nil {
		for _, col := range c.Overrides.WellKnownDateColumns {
			if strings.Contains(upper, strings.ToUpper(col)) {
				hints = append(hints, "기준월/기준일("+col+") 조건이 최신 적재월보다 뒤일 수 있습니다. 한 기간 전 기준으로 재시도해보세요.")
				break
			}
		}
	}

	// 3) generic: list the WHERE predicates so the model can relax them one at
	// a time instead of regenerating blindly.
	preds := []string{}
	for _, m := range reWherePreds.FindAllStringSubmatch(sql, -1) {
		p := strings.TrimSpace(m[1])
		if len(preds) < 3 && p != "" {
			preds = append(preds, p)
		}
	}
	if len(preds) > 0 {
		hints = append(hints, "조건을 하나씩 완화해 원인을 좁히세요: "+strings.Join(preds, " / "))
	}
	if len(hints) == 0 {
		hints = append(hints, "조인 조건이 과도하게 좁히는지(INNER JOIN → LEFT JOIN 검토), 유효/삭제 필터가 반대로 걸렸는지 확인하세요.")
	}
	return hints
}
