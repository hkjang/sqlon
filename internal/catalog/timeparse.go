package catalog

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TimeRange is a resolved temporal expression from a question. Start/End are
// inclusive YYYYMMDD strings; rendering adapts to each column's semantic type.
type TimeRange struct {
	Expression  string `json:"expression"`
	Start       string `json:"start,omitempty"`
	End         string `json:"end,omitempty"`
	Granularity string `json:"granularity,omitempty"` // day | month | quarter | year
	Comparison  string `json:"comparison,omitempty"`  // prev_month | prev_year_month
	Note        string `json:"note,omitempty"`
}

var (
	reRecentMonths = regexp.MustCompile(`최근\s*(\d+)\s*개월`)
	reRecentYears  = regexp.MustCompile(`최근\s*(\d+)\s*년`)
	reYearMonth    = regexp.MustCompile(`(20\d{2})\s*년\s*(\d{1,2})\s*월`)
	reYearOnly     = regexp.MustCompile(`(20\d{2})\s*년`)
	reHalf         = regexp.MustCompile(`(?:(20\d{2})\s*년\s*)?(상반기|하반기)`)
	reMonthOnly    = regexp.MustCompile(`(?:^|[^\d])(\d{1,2})\s*월(?:[^별]|$)`)
)

const ymd = "20060102"

// ParseTimeExpressions extracts temporal ranges, comparisons, and reporting
// granularity from a Korean/English question relative to `now`.
func ParseTimeExpressions(question string, now time.Time) []TimeRange {
	q := question
	var out []TimeRange
	add := func(tr TimeRange) { out = append(out, tr) }
	today := now
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	if strings.Contains(q, "오늘") || strings.Contains(q, "금일") {
		add(TimeRange{Expression: "오늘", Start: today.Format(ymd), End: today.Format(ymd), Granularity: "day"})
	}
	if strings.Contains(q, "어제") || strings.Contains(q, "전일") {
		y := today.AddDate(0, 0, -1)
		add(TimeRange{Expression: "어제", Start: y.Format(ymd), End: y.Format(ymd), Granularity: "day"})
	}
	if strings.Contains(q, "이번 달") || strings.Contains(q, "이번달") || strings.Contains(q, "당월") {
		add(TimeRange{Expression: "이번 달", Start: startOfMonth.Format(ymd), End: today.Format(ymd), Granularity: "month"})
	}
	// "전월 대비" is a comparison, not a standalone period — only treat 전월 as
	// a range when it is not part of a comparison phrase.
	standalonePrevMonth := strings.Contains(q, "지난달") || strings.Contains(q, "지난 달") ||
		(strings.Contains(q, "전월") && !strings.Contains(q, "전월 대비") && !strings.Contains(q, "전월대비"))
	if standalonePrevMonth {
		prev := startOfMonth.AddDate(0, -1, 0)
		add(TimeRange{Expression: "지난달", Start: prev.Format(ymd), End: startOfMonth.AddDate(0, 0, -1).Format(ymd), Granularity: "month"})
	}
	if strings.Contains(q, "올해") || strings.Contains(q, "금년") {
		add(TimeRange{Expression: "올해", Start: fmt.Sprintf("%04d0101", now.Year()), End: today.Format(ymd), Granularity: "year"})
	}
	if strings.Contains(q, "작년") || strings.Contains(q, "전년도") {
		add(TimeRange{Expression: "작년", Start: fmt.Sprintf("%04d0101", now.Year()-1), End: fmt.Sprintf("%04d1231", now.Year()-1), Granularity: "year"})
	}
	if m := reRecentMonths.FindStringSubmatch(q); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		start := startOfMonth.AddDate(0, -n, 0)
		add(TimeRange{Expression: "최근 " + m[1] + "개월", Start: start.Format(ymd), End: today.Format(ymd), Granularity: "month",
			Note: "당월 1일 기준 " + m[1] + "개월 전부터 오늘까지"})
	}
	if m := reRecentYears.FindStringSubmatch(q); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		add(TimeRange{Expression: "최근 " + m[1] + "년", Start: today.AddDate(-n, 0, 0).Format(ymd), End: today.Format(ymd), Granularity: "year"})
	}
	yearMonthSeen := map[string]bool{}
	for _, m := range reYearMonth.FindAllStringSubmatch(q, -1) {
		year, _ := strconv.Atoi(m[1])
		mon, _ := strconv.Atoi(m[2])
		if mon < 1 || mon > 12 {
			continue
		}
		start := time.Date(year, time.Month(mon), 1, 0, 0, 0, 0, now.Location())
		add(TimeRange{Expression: m[1] + "년 " + m[2] + "월", Start: start.Format(ymd), End: start.AddDate(0, 1, -1).Format(ymd), Granularity: "month"})
		yearMonthSeen[m[1]] = true
	}
	if m := reHalf.FindStringSubmatch(q); len(m) == 3 {
		year := now.Year()
		if m[1] != "" {
			year, _ = strconv.Atoi(m[1])
		}
		if m[2] == "상반기" {
			add(TimeRange{Expression: strconv.Itoa(year) + "년 상반기", Start: fmt.Sprintf("%04d0101", year), End: fmt.Sprintf("%04d0630", year), Granularity: "month"})
		} else {
			add(TimeRange{Expression: strconv.Itoa(year) + "년 하반기", Start: fmt.Sprintf("%04d0701", year), End: fmt.Sprintf("%04d1231", year), Granularity: "month"})
		}
	}
	// bare year, only when not already consumed by "YYYY년 M월" or 반기
	if !strings.Contains(q, "상반기") && !strings.Contains(q, "하반기") {
		for _, m := range reYearOnly.FindAllStringSubmatch(q, -1) {
			if yearMonthSeen[m[1]] {
				continue
			}
			year, _ := strconv.Atoi(m[1])
			add(TimeRange{Expression: m[1] + "년", Start: fmt.Sprintf("%04d0101", year), End: fmt.Sprintf("%04d1231", year), Granularity: "year"})
		}
	}
	if strings.Contains(q, "전월 대비") || strings.Contains(q, "전월대비") {
		add(TimeRange{Expression: "전월 대비", Comparison: "prev_month", Note: "동일 지표를 전월 구간으로도 집계하여 비교해야 함"})
	}
	if strings.Contains(q, "전년 동월") || strings.Contains(q, "전년동월") {
		add(TimeRange{Expression: "전년 동월 대비", Comparison: "prev_year_month", Note: "동일 지표를 전년 같은 달 구간으로도 집계하여 비교해야 함"})
	}
	for expr, gran := range map[string]string{"일별": "day", "월별": "month", "분기별": "quarter", "연도별": "year", "년도별": "year"} {
		if strings.Contains(q, expr) {
			add(TimeRange{Expression: expr, Granularity: gran, Note: "GROUP BY 보고 단위"})
		}
	}
	// raw dates like 2025-06-01 / 20250601
	for _, d := range yyyyMMddRE.FindAllString(q, -1) {
		clean := strings.NewReplacer("-", "", ".", "", "/", "").Replace(d)
		add(TimeRange{Expression: d, Start: clean, End: clean, Granularity: "day"})
	}
	return out
}

// RenderTimeCondition converts a TimeRange into a SQL predicate appropriate
// for the column's semantic type (DATE, TIMESTAMP, YYYYMMDD string, YYYYMM
// string). alias may be empty.
func RenderTimeCondition(col *Column, tr TimeRange, alias string) string {
	if col == nil || tr.Start == "" {
		return ""
	}
	name := col.Name
	if alias != "" {
		name = alias + "." + col.Name
	}
	switch col.SemanticType {
	case "MONTH_YYYYMM":
		s, e := tr.Start[:6], tr.End[:6]
		if s == e {
			return name + " = '" + s + "'"
		}
		return name + " BETWEEN '" + s + "' AND '" + e + "'"
	case "DATE_YYYYMMDD":
		if tr.Start == tr.End {
			return name + " = '" + tr.Start + "'"
		}
		return name + " >= '" + tr.Start + "' AND " + name + " <= '" + tr.End + "'"
	case "DATE", "TIMESTAMP", "DATETIME_YYYYMMDDHH24MISS":
		s := tr.Start[:4] + "-" + tr.Start[4:6] + "-" + tr.Start[6:]
		endT, err := time.Parse(ymd, tr.End)
		if err != nil {
			return ""
		}
		eExcl := endT.AddDate(0, 0, 1).Format("2006-01-02")
		if col.SemanticType == "DATETIME_YYYYMMDDHH24MISS" {
			return name + " >= '" + tr.Start + "000000' AND " + name + " < '" + endT.AddDate(0, 0, 1).Format(ymd) + "000000'"
		}
		return name + " >= DATE '" + s + "' AND " + name + " < DATE '" + eExcl + "'"
	default:
		// unknown semantic type: fall back to string range with a caveat handled by caller
		if tr.Start == tr.End {
			return name + " = '" + tr.Start + "'"
		}
		return name + " >= '" + tr.Start + "' AND " + name + " <= '" + tr.End + "'"
	}
}

// inferSemanticType classifies date-like columns from names and data types so
// time conditions match the physical format (char(6) vs varchar(8) vs DATE).
// The generic *_DT/*_MON/*_DT_TM naming suffixes work for any dataset;
// wellKnown additionally recognizes operator-configured exact column names
// that don't follow those suffixes (e.g. legacy abbreviations).
func inferSemanticType(col *Column, wellKnown []string) string {
	name := col.Name
	dt := strings.ToUpper(col.DataType)
	length := strings.TrimSpace(col.LengthPrecision)
	isWellKnown := false
	for _, n := range wellKnown {
		if n == name {
			isWellKnown = true
			break
		}
	}
	switch {
	case dt == "DATE":
		return "DATE"
	case strings.HasPrefix(dt, "TIMESTAMP"):
		return "TIMESTAMP"
	case strings.HasSuffix(name, "_DT_TM") || (strings.HasPrefix(dt, "VARCHAR") && length == "14" && strings.Contains(name, "DT")):
		return "DATETIME_YYYYMMDDHH24MISS"
	case strings.Contains(name, "YR_MON") || (strings.HasSuffix(name, "_MON") && (length == "6" || length == "")):
		if strings.HasPrefix(dt, "VARCHAR") || strings.HasPrefix(dt, "CHAR") {
			return "MONTH_YYYYMM"
		}
	case strings.HasSuffix(name, "_DT") || isWellKnown:
		if strings.HasPrefix(dt, "VARCHAR") || strings.HasPrefix(dt, "CHAR") {
			return "DATE_YYYYMMDD"
		}
	}
	return ""
}

// ResolveTime is the tool-facing wrapper: parse expressions and, when a table
// is given, render ready-to-use predicates for its date columns.
func (c *Catalog) ResolveTime(question, tableName string, now time.Time) map[string]any {
	ranges := ParseTimeExpressions(question, now)
	res := map[string]any{
		"question":    question,
		"now":         now.Format(ymd),
		"time_ranges": ranges,
	}
	if tableName == "" {
		return res
	}
	t, ok := c.ResolveTable(tableName)
	if !ok {
		res["error"] = "table not found: " + tableName
		return res
	}
	type rendered struct {
		Column       string `json:"column"`
		LogicalName  string `json:"logical_name,omitempty"`
		SemanticType string `json:"semantic_type,omitempty"`
		Expression   string `json:"expression"`
		Condition    string `json:"condition"`
	}
	var conds []rendered
	for _, col := range t.Columns {
		if col.SemanticType == "" {
			continue
		}
		for _, tr := range ranges {
			if cond := RenderTimeCondition(col, tr, ""); cond != "" {
				conds = append(conds, rendered{Column: col.Name, LogicalName: col.LogicalName, SemanticType: col.SemanticType, Expression: tr.Expression, Condition: cond})
			}
		}
	}
	res["table"] = t.FQN
	res["rendered_conditions"] = conds
	return res
}
