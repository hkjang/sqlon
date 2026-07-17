package catalog

import (
	"regexp"
	"sort"
	"strings"
)

type ValidateRequest struct {
	SQL             string   `json:"sql"`
	Limit           int      `json:"limit,omitempty"`
	Metrics         []string `json:"metrics,omitempty"`          // dictionary metric names this SQL claims to implement
	ExpectedOutputs []string `json:"expected_outputs,omitempty"` // business terms the SELECT list must cover (from analyze_question)
}

type ValidationIssue struct {
	Level   string `json:"level"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Table   string `json:"table,omitempty"`
	Column  string `json:"column,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

type FixHint struct {
	Code       string `json:"code"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion"`
}

type TableRef struct {
	Table string `json:"table"`
	Alias string `json:"alias,omitempty"`
}

type ColumnRef struct {
	Table  string `json:"table,omitempty"`
	Alias  string `json:"alias,omitempty"`
	Column string `json:"column"`
}

type ValidationResult struct {
	Valid             bool              `json:"valid"`
	Errors            []ValidationIssue `json:"errors,omitempty"`
	Warnings          []ValidationIssue `json:"warnings,omitempty"`
	FixHints          []FixHint         `json:"fix_hints,omitempty"`
	Hints             []string          `json:"hints,omitempty"`
	PIIColumns        []string          `json:"pii_columns,omitempty"`
	ReferencedTables  []TableRef        `json:"referenced_tables,omitempty"`
	ReferencedColumns []ColumnRef       `json:"referenced_columns,omitempty"`
	BoundedSQL        string            `json:"bounded_sql,omitempty"`
	RetryGuidance     string            `json:"retry_guidance,omitempty"`
	Lint              []LintFinding     `json:"lint,omitempty"` // advisory anti-pattern findings (never block)
}

type tableRefInternal struct {
	Table *Table
	Alias string
	Raw   string
}

var (
	aggFuncRE    = regexp.MustCompile(`(?i)\b(COUNT|SUM|AVG|MIN|MAX|LISTAGG|GROUP_CONCAT|STRING_AGG|MEDIAN|STDDEV|VARIANCE)\s*\([^()]*\)`)
	qualifiedRE  = regexp.MustCompile(`(?i)\b([A-Za-z_][\w$#]*)\s*\.\s*([A-Za-z_][\w$#]*)\b`)
	identRE      = regexp.MustCompile(`[A-Za-z_][\w$#]*`)
	limitKwRE    = regexp.MustCompile(`(?i)\bLIMIT\s+\d+`)
	selectStarRE = regexp.MustCompile(`(?is)\bSELECT\s+(?:\w+\s*\.\s*)?\*`)
	groupByRE    = regexp.MustCompile(`(?is)\bGROUP\s+BY\b(.*?)(?:\bHAVING\b|\bORDER\b|\bFETCH\b|\bOFFSET\b|$)`)
	selectListRE = regexp.MustCompile(`(?is)\bSELECT\b(.*?)\bFROM\b`)
	// Oracle-only functions/syntax — invalid on all supported dialects
	oracleFnRE = regexp.MustCompile(`(?i)\b(NVL\s*\(|NVL2\s*\(|DECODE\s*\(|ADD_MONTHS\s*\(|MONTHS_BETWEEN\s*\(|LISTAGG\s*\(|ROWNUM\b)`)
	// SQL Server-only
	tsqlFnRE = regexp.MustCompile(`(?i)\b(GETDATE\s*\(|ISNULL\s*\(|TOP\s+\d+)`)
	// MySQL-family-only functions — invalid on PostgreSQL
	mysqlOnlyFnRE = regexp.MustCompile(`(?i)\b(IFNULL\s*\(|DATE_FORMAT\s*\(|CURDATE\s*\(|STR_TO_DATE\s*\(|GROUP_CONCAT\s*\()`)
	// PostgreSQL/Oracle functions — invalid on MySQL/MariaDB (default sql_mode)
	pgOnlyFnRE   = regexp.MustCompile(`(?i)\b(TO_CHAR\s*\(|TO_DATE\s*\(|TO_NUMBER\s*\(|DATE_TRUNC\s*\(|STRING_AGG\s*\(|SYSDATE\b)`)
	fetchFirstRE = regexp.MustCompile(`(?i)\bFETCH\s+(FIRST|NEXT)\b`)
	concatPipeRE = regexp.MustCompile(`\|\|`)
)

func (c *Catalog) ValidateSQL(req ValidateRequest) ValidationResult {
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	sql := strings.TrimSpace(req.SQL)
	masked := maskSQL(sql)
	upper := strings.ToUpper(masked)
	res := ValidationResult{Valid: true}
	if sql == "" {
		return ValidationResult{Valid: false, Errors: []ValidationIssue{{Level: "error", Code: "EMPTY_SQL", Message: "sql is empty"}}}
	}
	addErr := func(code, msg, table, column, hint string) {
		res.Errors = append(res.Errors, ValidationIssue{Level: "error", Code: code, Message: msg, Table: table, Column: column, Hint: hint})
	}
	addWarn := func(code, msg, table, column, hint string) {
		res.Warnings = append(res.Warnings, ValidationIssue{Level: "warning", Code: code, Message: msg, Table: table, Column: column, Hint: hint})
	}

	if !regexp.MustCompile(`(?is)^\s*(SELECT|WITH)\b`).MatchString(masked) {
		addErr("NOT_SELECT", "only SELECT or WITH queries are allowed", "", "", "생성 SQL은 조회 전용이어야 합니다. DML/DDL을 제거하세요.")
	}
	for _, kw := range []string{"INSERT", "UPDATE", "DELETE", "MERGE", "DROP", "ALTER", "TRUNCATE", "CREATE", "GRANT", "REVOKE", "EXECUTE"} {
		if regexp.MustCompile(`(?i)\b` + kw + `\b`).MatchString(masked) {
			addErr("BLOCKED_KEYWORD", "blocked SQL keyword: "+kw, "", "", "읽기 전용 정책입니다. "+kw+" 구문을 제거하세요.")
		}
	}

	// dialect checks (postgres | mysql | mariadb; postgres by default)
	dialect := c.Dialect
	if dialect == "" {
		dialect = "postgres"
	}
	if m := oracleFnRE.FindString(masked); m != "" {
		addErr("DIALECT_FUNCTION", "Oracle-only function or syntax detected: "+m, "", "",
			"COALESCE, CASE, 표준 날짜 연산으로 바꾸세요 (NVL→COALESCE, DECODE→CASE, ROWNUM→LIMIT).")
	}
	if m := tsqlFnRE.FindString(masked); m != "" {
		addErr("DIALECT_FUNCTION", "SQL Server-only function or syntax detected: "+m, "", "",
			"표준 함수로 바꾸세요 (ISNULL→COALESCE, GETDATE→CURRENT_TIMESTAMP, TOP n→LIMIT n).")
	}
	switch dialect {
	case "mysql", "mariadb":
		if m := pgOnlyFnRE.FindString(masked); m != "" {
			addWarn("DIALECT_FUNCTION", "function not available on "+dialect+" (default mode): "+m, "", "",
				"MySQL 대응 함수로 바꾸세요 (TO_CHAR→DATE_FORMAT, TO_DATE→STR_TO_DATE, STRING_AGG→GROUP_CONCAT, DATE_TRUNC→DATE_FORMAT).")
		}
		if fetchFirstRE.MatchString(masked) && dialect == "mysql" {
			addErr("DIALECT_LIMIT", "FETCH FIRST is not valid MySQL syntax", "", "", "LIMIT n 으로 바꾸세요.")
		}
		if concatPipeRE.MatchString(masked) {
			addWarn("DIALECT_CONCAT", "|| is logical OR on "+dialect+" default mode, not string concat", "", "",
				"문자열 연결은 CONCAT(a, b)를 사용하세요.")
		}
	default: // postgres
		if strings.Contains(masked, "`") {
			addErr("DIALECT_BACKTICK", "backtick identifiers are not valid PostgreSQL syntax", "", "", "백틱을 제거하거나 큰따옴표를 사용하세요.")
		}
		if m := mysqlOnlyFnRE.FindString(masked); m != "" {
			addWarn("DIALECT_FUNCTION", "MySQL-only function detected: "+m, "", "",
				"PostgreSQL 대응 함수로 바꾸세요 (IFNULL→COALESCE, DATE_FORMAT→TO_CHAR, CURDATE→CURRENT_DATE, STR_TO_DATE→TO_DATE, GROUP_CONCAT→STRING_AGG).")
		}
	}

	refs, aliasMap, refIssues := c.extractTableRefs(masked)
	res.Errors = append(res.Errors, refIssues...)
	for _, ref := range refs {
		res.ReferencedTables = append(res.ReferencedTables, TableRef{Table: ref.Table.FQN, Alias: ref.Alias})
	}

	colRefs, colIssues := c.extractColumnRefs(masked, aliasMap)
	res.ReferencedColumns = colRefs
	for _, issue := range colIssues {
		if issue.Level == "warning" {
			res.Warnings = append(res.Warnings, issue)
		} else {
			res.Errors = append(res.Errors, issue)
		}
	}

	// PII exposure: referenced PII columns or SELECT * over tables with PII.
	for _, cr := range colRefs {
		t, ok := c.ResolveTable(cr.Table)
		if !ok {
			continue
		}
		if col := t.ColumnMap[cr.Column]; col != nil && col.PII {
			res.PIIColumns = append(res.PIIColumns, t.FQN+"."+cr.Column)
			addErr("PII_COLUMN", "PII column must not be exposed", t.FQN, cr.Column, "해당 컬럼을 SELECT에서 제거하거나 마스킹(예: SUBSTR/해시)하고, 집계 결과만 반환하세요.")
		}
	}
	if selectStarRE.MatchString(masked) {
		for _, ref := range refs {
			for _, col := range ref.Table.Columns {
				if col.PII {
					addErr("PII_SELECT_STAR", "SELECT * would expose PII column "+col.Name, ref.Table.FQN, col.Name, "SELECT * 대신 필요한 컬럼만 명시하세요.")
					break
				}
			}
		}
	}

	// forbidden joins
	for i := 0; i < len(refs); i++ {
		for j := i + 1; j < len(refs); j++ {
			if fj, bad := c.IsForbiddenJoin(refs[i].Table.FQN, refs[j].Table.FQN); bad {
				addErr("FORBIDDEN_JOIN", "join between these tables is forbidden by policy", refs[i].Table.FQN+" -> "+refs[j].Table.FQN, "", fj.Reason)
			}
		}
	}

	// cartesian risks
	joinCount := len(regexp.MustCompile(`(?i)\bJOIN\b`).FindAllString(masked, -1))
	onCount := len(regexp.MustCompile(`(?i)\bON\b`).FindAllString(masked, -1))
	usingCount := len(regexp.MustCompile(`(?i)\bUSING\s*\(`).FindAllString(masked, -1))
	if joinCount > onCount+usingCount {
		addErr("JOIN_WITHOUT_ON", "JOIN without ON/USING condition (cartesian product risk)", "", "", "get_join_paths가 반환한 condition을 ON 절에 사용하세요.")
	}
	if commaJoinRE.MatchString(masked) && !regexp.MustCompile(`(?i)\bWHERE\b`).MatchString(masked) {
		addErr("COMMA_JOIN_NO_WHERE", "comma-separated FROM without WHERE (cartesian product)", "", "", "명시적 JOIN ... ON 구문으로 바꾸고 조인 조건을 추가하세요.")
	}

	// ambiguous unqualified columns across multiple referenced tables
	if len(refs) > 1 {
		for _, name := range unqualifiedIdents(masked) {
			cnt := 0
			for _, ref := range refs {
				if ref.Table.ColumnMap[name] != nil {
					cnt++
				}
			}
			if cnt >= 2 {
				addWarn("AMBIGUOUS_COLUMN", "unqualified column exists in multiple referenced tables", "", name, "테이블 alias로 컬럼을 한정하세요 (예: T1."+name+").")
			}
		}
	}

	// GROUP BY consistency (heuristic)
	if m := selectListRE.FindStringSubmatch(masked); len(m) == 2 {
		selectList := m[1]
		stripped := aggFuncRE.ReplaceAllString(selectList, " ")
		hasAgg := aggFuncRE.MatchString(selectList)
		bare := qualifiedRE.FindAllStringSubmatch(stripped, -1)
		if gb := groupByRE.FindStringSubmatch(masked); len(gb) == 2 {
			gbText := strings.ToUpper(gb[1])
			for _, b := range bare {
				colName := cleanIdent(b[2])
				if aliasMap[cleanIdent(b[1])] == nil {
					continue
				}
				if !strings.Contains(gbText, colName) {
					addWarn("GROUP_BY_MISMATCH", "selected column not present in GROUP BY", "", b[1]+"."+b[2], "GROUP BY에 추가하거나 집계 함수로 감싸세요.")
				}
			}
		} else if hasAgg && len(bare) > 0 {
			known := false
			for _, b := range bare {
				if aliasMap[cleanIdent(b[1])] != nil {
					known = true
					break
				}
			}
			if known {
				addWarn("MISSING_GROUP_BY", "aggregate mixed with non-aggregated columns but no GROUP BY", "", "", "비집계 컬럼을 GROUP BY에 추가하세요.")
			}
		}
	}

	// metadata-driven filter policies
	for _, ref := range refs {
		for _, issue := range c.requiredFilterWarnings(masked, ref) {
			if issue.Level == "error" {
				res.Errors = append(res.Errors, issue)
			} else {
				res.Warnings = append(res.Warnings, issue)
			}
		}
		for _, df := range c.DefaultFiltersFor(ref.Table.FQN) {
			if !defaultFilterPresent(sql, df.Condition, ref) {
				hint := "필수 predicate를 WHERE/ON/HAVING 절에 정확히 추가하세요: " + df.Condition
				if df.Reason != "" {
					hint += " (" + df.Reason + ")"
				}
				message := "operator default filter is missing or does not match exactly: " + df.Condition
				if strings.EqualFold(strings.TrimSpace(df.Enforcement), "error") {
					addErr("DEFAULT_FILTER_MISSING", message, ref.Table.FQN, "", hint)
				} else {
					addWarn("DEFAULT_FILTER_MISSING", message, ref.Table.FQN, "", hint)
				}
			}
		}
	}

	// date-condition sanity: date-typed columns compared against wrong-shaped literals
	for _, w := range dateConditionWarnings(masked, refs) {
		res.Warnings = append(res.Warnings, w)
	}

	// code-value domain: literals compared to code columns must exist in the
	// code dictionary (catches invented codes like TX_STAT_CD = '77')
	for _, issue := range codeValueIssues(sql, refs) {
		if issue.Level == "error" {
			res.Errors = append(res.Errors, issue)
		} else {
			res.Warnings = append(res.Warnings, issue)
		}
	}

	// result-schema conformity: every expected output term must be covered by
	// the SELECT list (catches dropped dimensions/metrics in complex queries)
	res.Warnings = append(res.Warnings, c.expectedOutputIssues(masked, refs, req.ExpectedOutputs)...)

	// metric expression conformity
	for _, name := range req.Metrics {
		defs := c.LookupMetrics(name)
		if len(defs) == 0 {
			continue
		}
		normSQL := normalizeSQL(masked)
		found := false
		for _, d := range defs {
			if d.Expression != "" && strings.Contains(normSQL, normalizeSQL(d.Expression)) {
				found = true
				break
			}
		}
		if !found {
			addWarn("METRIC_MISMATCH", "SQL does not contain the dictionary expression for metric '"+name+"'", "", "", "지표 사전 expression을 그대로 사용하세요: "+defs[0].Expression)
		}
	}

	if len(refs) > 0 && !hasRowBound(upper) && !regexp.MustCompile(`(?i)\bCOUNT\s*\(`).MatchString(masked) {
		addWarn("NO_ROW_BOUND", "query has no explicit row bound", "", "", "탐색성 쿼리에는 LIMIT n 을 추가하세요.")
		res.BoundedSQL = addRowBound(sql, limit)
	}
	res.Warnings = append(res.Warnings, c.joinWarnings(masked, refs, aliasMap)...)
	res.Warnings = append(res.Warnings, c.learnedRuleWarnings(masked, refs)...)
	res.Valid = len(res.Errors) == 0
	if res.BoundedSQL == "" {
		res.BoundedSQL = sql
	}
	res.FixHints = buildFixHints(res)
	res.Hints = validationHints(res)
	if !res.Valid {
		res.RetryGuidance = "fix_hints를 반영해 SQL을 수정한 뒤 validate_sql을 다시 호출하세요. 자동 수정은 최대 2회까지만 시도하고, 그래도 실패하면 실패 원인과 수정 제안을 사용자에게 반환하세요."
	}
	// advisory anti-pattern lint rides along with the validation result; it never
	// changes res.Valid (smells are warnings, not blockers).
	res.Lint = c.LintSQL(sql)
	return res
}

var commaJoinRE = regexp.MustCompile(`(?is)\bFROM\s+[A-Za-z_][\w$#]*(?:\s*\.\s*[A-Za-z_][\w$#]*)?(?:\s+(?:AS\s+)?[A-Za-z_][\w$#]*)?\s*,`)

func buildFixHints(res ValidationResult) []FixHint {
	var out []FixHint
	for _, e := range res.Errors {
		h := FixHint{Code: e.Code, Issue: e.Message}
		switch {
		case e.Table != "" && e.Column != "":
			h.Issue += " (" + e.Table + "." + e.Column + ")"
		case e.Table != "":
			h.Issue += " (" + e.Table + ")"
		case e.Column != "":
			h.Issue += " (" + e.Column + ")"
		}
		h.Suggestion = e.Hint
		if h.Suggestion == "" {
			h.Suggestion = "get_schema_context와 get_join_paths 결과에 있는 식별자와 조인 조건만 사용해 다시 작성하세요."
		}
		out = append(out, h)
	}
	return out
}

// unqualifiedIdents returns identifiers that are not part of an alias.column
// reference (no adjacent dot on either side) and are not SQL keywords.
func unqualifiedIdents(masked string) []string {
	kw := map[string]bool{}
	for _, k := range []string{"SELECT", "FROM", "WHERE", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "OUTER", "CROSS", "ON", "AND", "OR", "NOT",
		"GROUP", "BY", "ORDER", "HAVING", "AS", "IN", "IS", "NULL", "LIKE", "BETWEEN", "CASE", "WHEN", "THEN", "ELSE", "END", "DISTINCT",
		"COUNT", "SUM", "AVG", "MIN", "MAX", "NVL", "TO_CHAR", "TO_DATE", "TO_NUMBER", "SUBSTR", "TRUNC", "FETCH", "FIRST", "ROWS", "ONLY",
		"ASC", "DESC", "UNION", "ALL", "WITH", "EXISTS", "ROWNUM", "SYSDATE", "DATE", "OFFSET", "USING", "COALESCE", "ROUND", "DECODE", "OVER", "PARTITION",
		"LIMIT", "NOW", "CURRENT_DATE", "CURRENT_TIMESTAMP", "INTERVAL", "EXTRACT", "CONCAT", "IFNULL", "DATE_FORMAT", "STR_TO_DATE", "CURDATE",
		"DATE_TRUNC", "STRING_AGG", "GROUP_CONCAT", "DATE_SUB", "DATE_ADD", "YEAR", "MONTH", "DAY", "CAST", "GREATEST", "LEAST"} {
		kw[k] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, loc := range identRE.FindAllStringIndex(masked, -1) {
		name := masked[loc[0]:loc[1]]
		up := strings.ToUpper(name)
		if kw[up] || seen[up] {
			continue
		}
		before := loc[0] - 1
		for before >= 0 && masked[before] == ' ' {
			before--
		}
		after := loc[1]
		for after < len(masked) && masked[after] == ' ' {
			after++
		}
		if (before >= 0 && masked[before] == '.') || (after < len(masked) && masked[after] == '.') {
			continue
		}
		seen[up] = true
		out = append(out, up)
	}
	return out
}

// dateConditionWarnings flags date-typed columns compared to literals whose
// shape doesn't match the column's semantic type.
func dateConditionWarnings(masked string, refs []tableRefInternal) []ValidationIssue {
	var out []ValidationIssue
	re := regexp.MustCompile(`(?i)\b([A-Za-z_][\w$#]*)\s*\.\s*([A-Za-z_][\w$#]*)\s*(?:=|>=|<=|>|<|BETWEEN)`)
	orig := masked
	for _, m := range re.FindAllStringSubmatchIndex(orig, -1) {
		alias := cleanIdent(orig[m[2]:m[3]])
		colName := cleanIdent(orig[m[4]:m[5]])
		var col *Column
		for _, ref := range refs {
			if strings.EqualFold(ref.Alias, alias) || strings.EqualFold(ref.Table.Name, alias) {
				col = ref.Table.ColumnMap[colName]
				break
			}
		}
		if col == nil || col.SemanticType == "" {
			continue
		}
		// maskSQL blanks string literals; inspect the raw tail length between quotes is unavailable.
		// We only sanity-check obvious mismatches available in masked text: DATE literal vs string column.
		tail := orig[m[1]:min(len(orig), m[1]+24)]
		usesDateLiteral := regexp.MustCompile(`(?i)^\s*DATE\s`).MatchString(tail) ||
			regexp.MustCompile(`(?i)TO_DATE|STR_TO_DATE|CURRENT_DATE|CURRENT_TIMESTAMP|NOW\s*\(|CURDATE\s*\(|DATE_TRUNC\s*\(|DATE_SUB|DATE_ADD`).MatchString(tail)
		switch col.SemanticType {
		case "DATE_YYYYMMDD", "MONTH_YYYYMM", "DATETIME_YYYYMMDDHH24MISS":
			if usesDateLiteral {
				out = append(out, ValidationIssue{Level: "warning", Code: "DATE_TYPE_MISMATCH",
					Message: "string-typed date column compared to DATE expression", Column: alias + "." + colName,
					Hint: "이 컬럼은 " + col.SemanticType + " 문자열입니다. 'YYYYMMDD'/'YYYYMM' 문자열 리터럴과 비교하세요."})
			}
		case "DATE", "TIMESTAMP":
			if !usesDateLiteral {
				out = append(out, ValidationIssue{Level: "warning", Code: "DATE_TYPE_MISMATCH",
					Message: "DATE/TIMESTAMP column may be compared to a string literal", Column: alias + "." + colName,
					Hint: "DATE 'YYYY-MM-DD' 리터럴 또는 날짜 함수(postgres: TO_DATE, mysql: STR_TO_DATE)를 사용하세요."})
			}
		}
	}
	return out
}

var (
	codeEqRE = regexp.MustCompile(`(?i)\b([A-Za-z_][\w$#]*)\s*\.\s*([A-Za-z_][\w$#]*)\s*(?:=|<>|!=)\s*'([^']*)'`)
	codeInRE = regexp.MustCompile(`(?i)\b([A-Za-z_][\w$#]*)\s*\.\s*([A-Za-z_][\w$#]*)\s+(?:NOT\s+)?IN\s*\(([^()]*)\)`)
	inItemRE = regexp.MustCompile(`'([^']*)'`)
)

// parseCodeDict extracts code->label pairs from texts like
// "00:정상, 09:수정, 89:D0 Prime, 99:삭제". The bool result reports whether
// the dictionary parsed cleanly enough to treat as exhaustive.
func parseCodeDict(dict string) (map[string]string, bool) {
	parts := strings.Split(dict, ",")
	codes := map[string]string{}
	parsed := 0
	for _, p := range parts {
		kv := strings.SplitN(p, ":", 2)
		if len(kv) != 2 {
			continue
		}
		if m := codeTailRE.FindStringSubmatch(kv[0]); m != nil {
			codes[strings.ToUpper(m[1])] = strings.TrimSpace(kv[1])
			parsed++
		}
	}
	exhaustive := parsed >= 2 && float64(parsed)/float64(len(parts)) >= 0.8
	return codes, exhaustive
}

// codeValueIssues flags string literals compared against code-dictionary
// columns when the literal is not a valid code. Works on the raw SQL because
// maskSQL blanks string literals.
func codeValueIssues(rawSQL string, refs []tableRefInternal) []ValidationIssue {
	findCol := func(alias, colName string) *Column {
		for _, ref := range refs {
			if strings.EqualFold(ref.Alias, alias) || strings.EqualFold(ref.Table.Name, alias) {
				return ref.Table.ColumnMap[cleanIdent(colName)]
			}
		}
		return nil
	}
	var out []ValidationIssue
	seen := map[string]bool{}
	check := func(alias, colName, literal string) {
		col := findCol(alias, colName)
		if col == nil || col.CodeDict == "" {
			return
		}
		codes, exhaustive := parseCodeDict(col.CodeDict)
		if !exhaustive {
			return
		}
		if _, ok := codes[strings.ToUpper(strings.TrimSpace(literal))]; ok {
			return
		}
		key := alias + "." + colName + "=" + literal
		if seen[key] {
			return
		}
		seen[key] = true
		valid := make([]string, 0, len(codes))
		for k, v := range codes {
			valid = append(valid, k+":"+v)
		}
		sort.Strings(valid)
		out = append(out, ValidationIssue{
			Level: "error", Code: "CODE_VALUE_UNKNOWN",
			Message: "literal '" + literal + "' is not a valid code for this column",
			Column:  alias + "." + colName,
			Hint:    "유효 코드: " + strings.Join(capList(valid, 12), ", "),
		})
	}
	for _, m := range codeEqRE.FindAllStringSubmatch(rawSQL, -1) {
		check(m[1], m[2], m[3])
	}
	for _, m := range codeInRE.FindAllStringSubmatch(rawSQL, -1) {
		for _, item := range inItemRE.FindAllStringSubmatch(m[3], -1) {
			check(m[1], m[2], item[1])
		}
	}
	return out
}

// expectedOutputIssues verifies the SELECT list covers each expected business
// term by mapping terms to candidate physical columns of the referenced
// tables via the shared glossary.
func (c *Catalog) expectedOutputIssues(masked string, refs []tableRefInternal, expected []string) []ValidationIssue {
	if len(expected) == 0 || len(refs) == 0 {
		return nil
	}
	m := selectListRE.FindStringSubmatch(masked)
	if len(m) != 2 {
		return nil
	}
	selectList := strings.ToUpper(m[1])
	var out []ValidationIssue
	for _, term := range expected {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		// metric-dictionary terms: expression presence is checked via the
		// metrics parameter, so only verify dimension/column coverage here.
		if len(c.LookupMetrics(term)) > 0 {
			continue
		}
		tokens := c.expandTokens(tokenize(term))
		candidates := []string{}
		covered := false
		for _, ref := range refs {
			for _, cm := range scoreColumns(tokens, ref.Table, 4) {
				if cm.Score < 5 { // ignore incidental search-text matches
					continue
				}
				candidates = appendUnique(candidates, cm.Name)
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(cm.Name) + `\b`).MatchString(selectList) {
					covered = true
				}
			}
		}
		if covered || len(candidates) == 0 {
			continue
		}
		out = append(out, ValidationIssue{
			Level: "warning", Code: "EXPECTED_OUTPUT_MISSING",
			Message: "expected output '" + term + "' is not covered by the SELECT list",
			Hint:    "후보 컬럼: " + strings.Join(capList(candidates, 4), ", ") + " 중 하나를 SELECT(및 GROUP BY)에 포함하세요.",
		})
	}
	return out
}

func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(strings.ToUpper(s)), " ")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (c *Catalog) ExplainSQL(req ValidateRequest) map[string]any {
	v := c.ValidateSQL(req)
	masked := maskSQL(req.SQL)
	upper := strings.ToUpper(masked)
	risk := "low"
	score := 0
	factors := []string{}
	suggestions := []string{}
	if !v.Valid {
		risk = "blocked"
		score += 100
		factors = append(factors, "validation errors present; do not execute")
	}
	score += len(v.Warnings) * 12
	joins := strings.Count(upper, " JOIN ")
	score += joins * 8
	if joins >= 3 {
		factors = append(factors, "3+ joins; verify each ON condition against the join graph")
	}
	hasWhere := strings.Contains(upper, " WHERE ")
	if !hasWhere {
		score += 25
		factors = append(factors, "no WHERE clause: likely full table scan")
		suggestions = append(suggestions, "기간 조건(BS_YR_MON, *_DT) 또는 키 조건을 추가하세요.")
	}
	if !hasRowBound(upper) && !regexp.MustCompile(`(?i)\bCOUNT\s*\(`).MatchString(masked) {
		score += 15
		factors = append(factors, "no row bound")
		suggestions = append(suggestions, "LIMIT n 을 추가하세요.")
	}
	if strings.Contains(upper, " ORDER BY ") && !hasRowBound(upper) {
		score += 10
		factors = append(factors, "ORDER BY without row bound: full sort")
	}
	// large-table heuristics: detail-grain tables need a temporal predicate
	indexHints := []string{}
	indexedFilter := false
	for _, ref := range v.ReferencedTables {
		t, ok := c.ResolveTable(ref.Table)
		if !ok {
			continue
		}
		if t.RowCount > 10_000_000 || strings.Contains(t.Grain, "상세") || strings.HasSuffix(t.Name, "D") {
			hasTemporal := false
			for _, col := range t.Columns {
				if col.SemanticType != "" && regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(col.Name)+`\b`).MatchString(masked) {
					hasTemporal = true
					break
				}
			}
			if !hasTemporal {
				score += 20
				factors = append(factors, t.FQN+": detail/large table without a temporal predicate")
				suggestions = append(suggestions, t.FQN+"에 기준일/기준월 조건을 추가하세요.")
			}
		}
		for _, idx := range t.Indexes {
			if strings.Contains(upper, idx.ColumnName) {
				indexHints = append(indexHints, t.FQN+"."+idx.ColumnName+" matches index "+idx.IndexName)
				indexedFilter = true
			}
		}
	}
	if len(v.ReferencedTables) > 0 && hasWhere && !indexedFilter {
		score += 10
		factors = append(factors, "no indexed column detected in predicates")
		suggestions = append(suggestions, "인덱스 컬럼(CUST_NO, 기준일자 등) 조건을 우선 사용하세요.")
	}
	switch {
	case risk == "blocked":
	case score >= 70:
		risk = "high"
	case score >= 35:
		risk = "medium"
	}
	action := "proceed"
	if risk == "high" {
		action = "regenerate_with_constraints"
	}
	if risk == "blocked" {
		action = "fix_validation_errors"
	}
	return map[string]any{
		"mode":               "static_metadata_explain",
		"risk":               risk,
		"risk_score":         score,
		"risk_factors":       factors,
		"suggestions":        unique(suggestions),
		"recommended_action": action,
		"join_count":         joins,
		"validation":         v,
		"index_hints":        unique(indexHints),
		"execution_notice":   "No database connection is used. This is a static guardrail estimate, not a live DB EXPLAIN. risk=high면 SQL을 실행하지 말고 제안을 반영해 재생성하세요.",
	}
}

// extractCTENames returns the names defined in a leading WITH clause so they
// are not mistaken for catalog tables.
func extractCTENames(masked string) map[string]bool {
	names := map[string]bool{}
	withRE := regexp.MustCompile(`(?is)^\s*WITH\s+`)
	loc := withRE.FindStringIndex(masked)
	if loc == nil {
		return names
	}
	pos := loc[1]
	nameRE := regexp.MustCompile(`(?is)^([A-Za-z_][\w$#]*)\s*(?:\([^()]*\)\s*)?AS\s*\(`)
	for {
		m := nameRE.FindStringSubmatch(masked[pos:])
		if m == nil {
			break
		}
		names[cleanIdent(m[1])] = true
		// skip past the balanced parenthesis block of this CTE body
		open := strings.Index(masked[pos:], "(")
		depth := 0
		i := pos + open
		for ; i < len(masked); i++ {
			switch masked[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		pos = i + 1
		commaRE := regexp.MustCompile(`(?s)^\s*,\s*`)
		cm := commaRE.FindString(masked[pos:])
		if cm == "" {
			break
		}
		pos += len(cm)
	}
	return names
}

// extractDerivedAliases returns aliases that follow a closing parenthesis
// (inline views / subquery factors). Their columns cannot be validated
// against the catalog, so qualifiers using them are skipped.
func extractDerivedAliases(masked string) map[string]bool {
	aliases := map[string]bool{}
	re := regexp.MustCompile(`\)\s+(?:AS\s+)?([A-Za-z_][\w$#]*)`)
	for _, m := range re.FindAllStringSubmatch(masked, -1) {
		a := cleanIdent(m[1])
		if !isSQLClause(a) && strings.ToUpper(a) != "AND" && strings.ToUpper(a) != "OR" {
			aliases[a] = true
		}
	}
	return aliases
}

func (c *Catalog) extractTableRefs(masked string) ([]tableRefInternal, map[string]*Table, []ValidationIssue) {
	re := regexp.MustCompile(`(?is)\b(FROM|JOIN)\s+([A-Za-z_][\w$#]*(?:\s*\.\s*[A-Za-z_][\w$#]*)?)(?:\s+(?:AS\s+)?([A-Za-z_][\w$#]*))?`)
	commaRe := regexp.MustCompile(`^\s*,\s*([A-Za-z_][\w$#]*(?:\s*\.\s*[A-Za-z_][\w$#]*)?)(?:\s+(?:AS\s+)?([A-Za-z_][\w$#]*))?`)
	cteNames := extractCTENames(masked)
	var refs []tableRefInternal
	aliasMap := map[string]*Table{}
	var issues []ValidationIssue
	addRef := func(rawName, rawAlias string) {
		rawName = strings.ReplaceAll(rawName, " ", "")
		alias := cleanIdent(rawAlias)
		if isSQLClause(alias) {
			alias = ""
		}
		if cteNames[cleanIdent(rawName)] {
			// CTE reference: register the alias as known-but-unverifiable so
			// its column qualifiers are not flagged.
			if alias != "" {
				aliasMap[alias] = nil
			}
			aliasMap[cleanIdent(rawName)] = nil
			return
		}
		t, ok := c.ResolveTable(rawName)
		if !ok {
			issues = append(issues, ValidationIssue{Level: "error", Code: "UNKNOWN_TABLE", Message: "referenced table not found in catalog", Table: rawName,
				Hint: "search_schema로 올바른 테이블을 찾고 스키마-한정 이름을 사용하세요."})
			return
		}
		if alias == "" {
			alias = t.Name
		}
		refs = append(refs, tableRefInternal{Table: t, Alias: alias, Raw: rawName})
		aliasMap[alias] = t
		aliasMap[t.Name] = t
		aliasMap[t.FQN] = t
	}
	for _, m := range re.FindAllStringSubmatchIndex(masked, -1) {
		keyword := strings.ToUpper(masked[m[2]:m[3]])
		rawName := masked[m[4]:m[5]]
		rawAlias := ""
		if m[6] >= 0 {
			rawAlias = masked[m[6]:m[7]]
		}
		addRef(rawName, rawAlias)
		// comma-separated FROM list: FROM t1 a, t2 b, ...
		if keyword == "FROM" {
			pos := m[1]
			for {
				sub := commaRe.FindStringSubmatchIndex(masked[pos:])
				if sub == nil {
					break
				}
				name := masked[pos+sub[2] : pos+sub[3]]
				alias := ""
				if sub[4] >= 0 {
					alias = masked[pos+sub[4] : pos+sub[5]]
				}
				addRef(name, alias)
				pos += sub[1]
			}
		}
	}
	return refs, aliasMap, issues
}

func (c *Catalog) extractColumnRefs(masked string, aliasMap map[string]*Table) ([]ColumnRef, []ValidationIssue) {
	schemas := map[string]bool{}
	for _, t := range c.Tables {
		schemas[t.Schema] = true
	}
	cteNames := extractCTENames(masked)
	derived := extractDerivedAliases(masked)
	matches := qualifiedRE.FindAllStringSubmatch(masked, -1)
	var refs []ColumnRef
	var issues []ValidationIssue
	seen := map[string]bool{}
	for _, m := range matches {
		alias := cleanIdent(m[1])
		colName := cleanIdent(m[2])
		if schemas[alias] {
			continue
		}
		t, known := aliasMap[alias]
		if t == nil {
			if known || isLikelyFunctionOrPackage(alias) || cteNames[alias] || derived[alias] {
				continue // CTE or inline-view columns cannot be checked against the catalog
			}
			key := alias + "." + colName
			if !seen[key] {
				issues = append(issues, ValidationIssue{Level: "warning", Code: "UNKNOWN_ALIAS", Message: "column qualifier is not a known table alias", Column: key})
				seen[key] = true
			}
			continue
		}
		key := t.FQN + "." + colName
		if seen[key] {
			continue
		}
		seen[key] = true
		if t.ColumnMap[colName] == nil {
			issues = append(issues, ValidationIssue{
				Level:   "error",
				Code:    "UNKNOWN_COLUMN",
				Message: "column not found in referenced table",
				Table:   t.FQN,
				Column:  colName,
				Hint:    "Use get_schema_context for the table and choose a listed column verbatim.",
			})
			continue
		}
		refs = append(refs, ColumnRef{Table: t.FQN, Alias: alias, Column: colName})
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].Table == refs[j].Table {
			return refs[i].Column < refs[j].Column
		}
		return refs[i].Table < refs[j].Table
	})
	return refs, issues
}

func (c *Catalog) requiredFilterWarnings(masked string, ref tableRefInternal) []ValidationIssue {
	t := ref.Table
	alias := ref.Alias
	var out []ValidationIssue
	has := func(col string) bool { return t.ColumnMap[col] != nil }
	refsCol := func(col string) bool {
		re := regexp.MustCompile(`(?i)(\b` + regexp.QuoteMeta(alias) + `\s*\.\s*)?\b` + regexp.QuoteMeta(col) + `\b`)
		return re.MatchString(masked)
	}
	if c.Overrides == nil {
		return out
	}
	// a schema-hint rule with a note acts as a per-schema caution (e.g. "this
	// schema is a snapshot; don't invent the other schema's history columns")
	for _, rule := range c.Overrides.SchemaHints {
		if rule.Note == "" || !contains(rule.Schemas, t.Schema) {
			continue
		}
		for _, pair := range c.Overrides.SegmentHistoryColumnPairs {
			if regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(alias)+`\s*\.\s*`+regexp.QuoteMeta(pair.Start)+`\b`).MatchString(masked) ||
				regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(alias)+`\s*\.\s*`+regexp.QuoteMeta(pair.End)+`\b`).MatchString(masked) {
				out = append(out, ValidationIssue{Level: "error", Code: "SCHEMA_HISTORY_PREDICATE_MISMATCH", Message: rule.Note, Table: t.FQN})
			}
		}
	}
	for _, pair := range c.Overrides.SegmentHistoryColumnPairs {
		if has(pair.Start) && has(pair.End) && (!refsCol(pair.Start) || !refsCol(pair.End)) {
			out = append(out, ValidationIssue{Level: "warning", Code: "MISSING_PIT_FILTER", Message: "segment history columns exist but point-in-time filter is missing", Table: t.FQN, Hint: "Add " + pair.Start + " <= 기준일 AND " + pair.End + " > 기준일."})
		}
	}
	for _, col := range c.Overrides.ValidityFlagColumns {
		if has(col) && !refsCol(col) {
			out = append(out, ValidationIssue{Level: "warning", Code: "MISSING_VALIDITY_FILTER", Message: col + " exists but validity filter is missing", Table: t.FQN, Column: col, Hint: "Add COALESCE(" + col + ", 'Y') <> 'N' when querying valid rows."})
		}
	}
	for _, col := range c.Overrides.SoftDeleteColumns {
		if has(col) && !refsCol(col) {
			out = append(out, ValidationIssue{Level: "warning", Code: "MISSING_DEL_FILTER", Message: col + " exists but deletion filter is missing", Table: t.FQN, Column: col, Hint: "Add " + col + " IS NULL when deleted rows should be excluded."})
		}
	}
	for _, col := range t.Columns {
		for _, prefix := range c.Overrides.ExclusionColumnPrefixes {
			if prefix != "" && strings.HasPrefix(col.Name, prefix) && !refsCol(col.Name) {
				out = append(out, ValidationIssue{Level: "warning", Code: "MISSING_EXCL_FILTER", Message: "analytics exclusion column exists but filter is missing", Table: t.FQN, Column: col.Name, Hint: "Add " + col.Name + " IS NULL unless exclusions are intentionally included."})
			}
		}
	}
	return out
}

func (c *Catalog) joinWarnings(masked string, refs []tableRefInternal, aliasMap map[string]*Table) []ValidationIssue {
	if len(refs) < 2 || !regexp.MustCompile(`(?i)\bJOIN\b`).MatchString(masked) {
		return nil
	}
	var out []ValidationIssue
	for i := 1; i < len(refs); i++ {
		prev := refs[i-1].Table
		cur := refs[i].Table
		if p := c.findJoinPath(prev.FQN, cur.FQN, 2); p.Found {
			if p.Confidence > 0 && p.Confidence < 0.7 {
				out = append(out, ValidationIssue{
					Level:   "warning",
					Code:    "LOW_CONFIDENCE_JOIN",
					Message: "join path exists but confidence is low",
					Table:   prev.FQN + " -> " + cur.FQN,
					Hint:    "get_join_paths의 caution/description을 확인하고 업무적으로 맞는 조인인지 검증하세요.",
				})
			}
			continue
		}
		if c.haveCommonJoinColumn(prev, cur) {
			continue
		}
		out = append(out, ValidationIssue{
			Level:   "warning",
			Code:    "UNVERIFIED_JOIN",
			Message: "no catalog relation path found for adjacent JOIN tables",
			Table:   prev.FQN + " -> " + cur.FQN,
			Hint:    "Call get_join_paths before generating the final SQL.",
		})
	}
	return out
}

func (c *Catalog) haveCommonJoinColumn(a, b *Table) bool {
	if c.Overrides == nil {
		return false
	}
	for _, name := range c.Overrides.JoinKeyCandidateColumns {
		if a.ColumnMap[name] != nil && b.ColumnMap[name] != nil {
			return true
		}
	}
	return false
}

func validationHints(res ValidationResult) []string {
	hints := []string{}
	if !res.Valid {
		hints = append(hints, "Fix errors before executing or explaining the query.")
	}
	if len(res.Warnings) > 0 {
		hints = append(hints, "Review warnings; most are metadata-driven safety checks for temporal validity, deletion flags, and join confidence.")
	}
	if res.BoundedSQL != "" {
		hints = append(hints, "Use bounded_sql for safe preview execution.")
	}
	return hints
}

func maskSQL(sql string) string {
	var b strings.Builder
	inSingle := false
	inDouble := false
	inLineComment := false
	inBlockComment := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		next := byte(0)
		if i+1 < len(sql) {
			next = sql[i+1]
		}
		switch {
		case inLineComment:
			if ch == '\n' {
				inLineComment = false
				b.WriteByte(ch)
			} else {
				b.WriteByte(' ')
			}
		case inBlockComment:
			if ch == '*' && next == '/' {
				inBlockComment = false
				b.WriteString("  ")
				i++
			} else {
				b.WriteByte(' ')
			}
		case inSingle:
			if ch == '\'' {
				if next == '\'' {
					b.WriteString("  ")
					i++
				} else {
					inSingle = false
					b.WriteByte(' ')
				}
			} else {
				b.WriteByte(' ')
			}
		case inDouble:
			if ch == '"' {
				inDouble = false
			}
			b.WriteByte(ch)
		case ch == '-' && next == '-':
			inLineComment = true
			b.WriteString("  ")
			i++
		case ch == '/' && next == '*':
			inBlockComment = true
			b.WriteString("  ")
			i++
		case ch == '\'':
			inSingle = true
			b.WriteByte(' ')
		case ch == '"':
			inDouble = true
			b.WriteByte(ch)
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

func hasRowBound(upper string) bool {
	for _, marker := range []string{" FETCH FIRST ", " LIMIT ", " OFFSET "} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// addRowBound appends a LIMIT clause (valid on postgres, mysql, and mariadb).
func addRowBound(sql string, limit int) string {
	trimmed := strings.TrimSpace(sql)
	trimmed = strings.TrimSuffix(trimmed, ";")
	if hasRowBound(strings.ToUpper(trimmed)) {
		return trimmed
	}
	return trimmed + "\nLIMIT " + itoa(limit)
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func isSQLClause(s string) bool {
	switch strings.ToUpper(s) {
	case "", "WHERE", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "ON", "GROUP", "ORDER", "HAVING", "FETCH", "UNION", "LIMIT":
		return true
	default:
		return false
	}
}

func isLikelyFunctionOrPackage(s string) bool {
	switch strings.ToUpper(s) {
	case "DBMS_RANDOM", "SYS", "STANDARD":
		return true
	default:
		return false
	}
}
