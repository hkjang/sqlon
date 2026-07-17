package catalog

import (
	"fmt"
	"strings"
	"time"
)

// BuildSQLSkeleton assembles a draft SQL frame (dialect-neutral: postgres/mysql/mariadb) from vetted parts only:
// catalog join conditions, dictionary metric expressions, semantic-type-aware
// time predicates, and policy/default filters. The LLM fills the /* SLOT */
// comments instead of inventing structure, which removes the most common
// complex-query failure modes (invented joins, missing PIT filters).
func (c *Catalog) BuildSQLSkeleton(question string, tableNames []string, limit int, now time.Time) map[string]any {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if len(tableNames) == 0 && question != "" {
		search := c.SearchSchema(SearchRequest{Question: question, TopK: 5})
		// A caller that didn't pin tableNames is asking the catalog to guess
		// the table itself — so if the top candidates are a near-tie, refuse
		// to guess and hand back the same clarification prepare_sql_context
		// would raise, instead of silently building a skeleton against
		// whichever table happened to rank first.
		if cl, ambiguous := DetectTableChoiceAmbiguity(search); ambiguous {
			return map[string]any{
				"status":         "needs_clarification",
				"question":       question,
				"clarifications": []Clarification{cl},
				"guidance":       "테이블 후보가 여러 개 비슷한 점수로 검색되었습니다. 답변을 받은 뒤 tableNames를 명시적으로 지정해 다시 호출하세요.",
			}
		}
		for i, r := range search.Results {
			if i >= 2 {
				break
			}
			tableNames = append(tableNames, r.Table)
		}
	}
	var resolved []*Table
	var notes []string
	for _, name := range tableNames {
		t, ok := c.ResolveTable(name)
		if !ok {
			notes = append(notes, "table not found and skipped: "+name)
			continue
		}
		resolved = append(resolved, t)
	}
	if len(resolved) == 0 {
		return map[string]any{"error": "no resolvable tables", "question": question}
	}

	// alias assignment; join-path intermediates get aliases too
	alias := map[string]string{}
	order := []*Table{}
	assign := func(t *Table) string {
		if a, ok := alias[t.FQN]; ok {
			return a
		}
		a := fmt.Sprintf("T%d", len(order)+1)
		alias[t.FQN] = a
		order = append(order, t)
		return a
	}
	assign(resolved[0])

	type joinLine struct {
		Table     string  `json:"table"`
		Alias     string  `json:"alias"`
		JoinType  string  `json:"join_type"`
		Condition string  `json:"condition"`
		Conf      float64 `json:"confidence"`
		Caution   string  `json:"caution,omitempty"`
	}
	var joins []joinLine
	missingJoins := []string{}
	for i := 1; i < len(resolved); i++ {
		if fj, bad := c.IsForbiddenJoin(resolved[i-1].FQN, resolved[i].FQN); bad {
			missingJoins = append(missingJoins, resolved[i].FQN+" (forbidden: "+fj.Reason+")")
			continue
		}
		p := c.findJoinPath(resolved[i-1].FQN, resolved[i].FQN, 3)
		if !p.Found {
			missingJoins = append(missingJoins, resolved[i].FQN+" (no catalog join path)")
			continue
		}
		for _, e := range p.Edges {
			fromT, _ := c.ResolveTable(e.From)
			toT, _ := c.ResolveTable(e.To)
			fromAlias := assign(fromT)
			toAlias := assign(toT)
			cond := e.Condition
			cond = replaceTableWithAlias(cond, e.From, fromAlias)
			cond = replaceTableWithAlias(cond, e.To, toAlias)
			joins = append(joins, joinLine{
				Table: toT.FQN, Alias: toAlias,
				JoinType: nonEmpty(e.JoinType, "INNER"), Condition: cond,
				Conf: e.Confidence, Caution: e.Caution,
			})
		}
	}

	// time predicate: pick the best date column per selected table
	timeRanges := ParseTimeExpressions(question, now)
	var timeConds []string
	timeAlternatives := []map[string]any{}
	for _, t := range order {
		col := c.preferredDateColumn(t, timeRanges)
		if col == nil {
			continue
		}
		for _, tr := range timeRanges {
			if tr.Start == "" {
				continue
			}
			if cond := RenderTimeCondition(col, tr, alias[t.FQN]); cond != "" {
				timeConds = append(timeConds, cond)
				break // one primary predicate per table
			}
		}
		for _, other := range t.Columns {
			if other.SemanticType != "" && other != col && !c.hasExcludedDatePrefix(other.Name) {
				timeAlternatives = append(timeAlternatives, map[string]any{
					"table": t.FQN, "column": other.Name, "logical_name": other.LogicalName, "semantic_type": other.SemanticType,
				})
			}
		}
	}

	// policy + operator default filters, only for columns that exist. The
	// column/pair names are all operator-configured (Overrides) rather than
	// hardcoded, so a dataset with no domain policy configured gets none of
	// these auto-injected conditions.
	var o Overrides
	if c.Overrides != nil {
		o = *c.Overrides
	}
	var policyConds []string
	for _, t := range order {
		a := alias[t.FQN]
		for _, pair := range o.SegmentHistoryColumnPairs {
			if pair.Start == "" || pair.End == "" {
				continue
			}
			if t.ColumnMap[pair.Start] != nil && t.ColumnMap[pair.End] != nil {
				policyConds = append(policyConds, a+"."+pair.Start+" <= '{기준일:YYYYMMDD}' AND "+a+"."+pair.End+" > '{기준일:YYYYMMDD}' /* SLOT: point-in-time 기준일 */")
			}
		}
		for _, col := range o.SoftDeleteColumns {
			if t.ColumnMap[col] != nil {
				policyConds = append(policyConds, a+"."+col+" IS NULL")
			}
		}
		for _, col := range o.ValidityFlagColumns {
			if t.ColumnMap[col] != nil {
				policyConds = append(policyConds, "COALESCE("+a+"."+col+", 'Y') <> 'N'")
			}
		}
		for _, col := range t.Columns {
			for _, prefix := range o.ExclusionColumnPrefixes {
				if prefix != "" && strings.HasPrefix(col.Name, prefix) {
					policyConds = append(policyConds, a+"."+col.Name+" IS NULL")
				}
			}
		}
		for _, df := range c.DefaultFiltersFor(t.FQN) {
			cond := df.Condition
			if !strings.Contains(cond, ".") {
				cond = a + "." + cond
			}
			policyConds = appendUnique(policyConds, cond)
		}
	}
	policyConds = unique(policyConds)

	// dictionary metrics mentioned in the question
	metricDefs := []MetricDef{}
	metricLines := []string{}
	seenMetric := map[string]bool{}
	for _, names := range c.metricsInQuestion(question) {
		for _, n := range names {
			if seenMetric[n] {
				continue
			}
			seenMetric[n] = true
			for _, m := range c.Metrics {
				if m.Name == n {
					metricDefs = append(metricDefs, m)
					metricLines = append(metricLines, m.Expression+" AS \""+m.Name+"\" /* 지표사전: "+m.Name+"; 컬럼에 테이블 alias를 붙이세요 */")
				}
			}
		}
	}

	var b strings.Builder
	b.WriteString("SELECT /* SLOT: 출력 컬럼 (차원 먼저, 지표 다음) */")
	if len(metricLines) > 0 {
		b.WriteString("\n       " + strings.Join(metricLines, ",\n       "))
	}
	b.WriteString("\n")
	b.WriteString("FROM " + order[0].FQN + " " + alias[order[0].FQN] + "\n")
	for _, j := range joins {
		b.WriteString(j.JoinType + " JOIN " + j.Table + " " + j.Alias + " ON " + j.Condition)
		if j.Caution != "" {
			b.WriteString(" /* 주의: " + j.Caution + " */")
		}
		b.WriteString("\n")
	}
	where := append(append([]string{}, timeConds...), policyConds...)
	if len(where) == 0 {
		where = []string{"/* SLOT: 필터 조건 */"}
	} else {
		where = append(where, "/* SLOT: 추가 필터 조건 */")
	}
	b.WriteString("WHERE " + strings.Join(where, "\n  AND ") + "\n")
	b.WriteString("/* SLOT: GROUP BY 차원 (SELECT의 비집계 컬럼과 동일해야 함) */\n")
	b.WriteString("/* SLOT: ORDER BY */\n")
	b.WriteString(fmt.Sprintf("LIMIT %d", limit))

	aliasOut := map[string]string{}
	for fqn, a := range alias {
		aliasOut[a] = fqn
	}
	res := map[string]any{
		"question":          question,
		"skeleton_sql":      b.String(),
		"aliases":           aliasOut,
		"join_lines":        joins,
		"time_conditions":   timeConds,
		"time_alternatives": timeAlternatives,
		"policy_filters":    policyConds,
		"metrics":           metricDefs,
		"patterns":          c.MatchPatterns(question),
		"rules": []string{
			"skeleton_sql의 구조(FROM/JOIN/정책 필터)는 유지하고 SLOT 주석만 채우세요.",
			"조인 조건을 추가·변경하려면 get_join_paths를 다시 호출하세요.",
			"지표 expression은 그대로 두고 컬럼에 alias만 붙이세요.",
			"완성 후 validate_sql로 검증하세요.",
		},
	}
	if len(missingJoins) > 0 {
		res["missing_joins"] = missingJoins
		res["guidance"] = "조인 경로가 없는 테이블은 skeleton에서 제외되었습니다. 사용자에게 연결 기준을 확인하거나 단일 테이블 쿼리로 분리하세요."
	}
	if len(notes) > 0 {
		res["notes"] = notes
	}
	return res
}

// preferredDateColumn picks the column a time predicate should target.
// Month columns are preferred for month-grained ranges; ties break on how
// often golden examples for this table actually filter on the column.
//
// Exclusion/preference rules (which naming conventions mean "history",
// "audit", or "entity attribute date" rather than "event/snapshot date") are
// entirely operator-configured via Overrides — see
// DateColumnExclude{Prefixes,Names,Substrings}, WellKnownDateColumns, and
// DateColumnEligibleSuffixes. With no such config, every semantically-typed
// date/timestamp column on the table is eligible: this keeps the feature
// useful out of the box for a freshly onboarded dataset with few or no
// golden examples, while still letting an operator with legacy naming
// conventions (e.g. history-table B_-prefixed columns) restrict it tightly.
func (c *Catalog) preferredDateColumn(t *Table, ranges []TimeRange) *Column {
	monthGrain := false
	for _, tr := range ranges {
		if tr.Granularity == "month" && tr.Start != "" {
			monthGrain = true
		}
	}
	var o Overrides
	if c.Overrides != nil {
		o = *c.Overrides
	}
	wellKnown := map[string]bool{}
	for _, n := range o.WellKnownDateColumns {
		wellKnown[n] = true
	}
	hasEligibilityConfig := len(wellKnown) > 0 || len(o.DateColumnEligibleSuffixes) > 0
	eligibleSuffix := func(name string) bool {
		for _, suf := range o.DateColumnEligibleSuffixes {
			if suf != "" && strings.HasSuffix(name, suf) {
				return true
			}
		}
		return false
	}

	var best *Column
	bestScore := -1
	for _, col := range t.Columns {
		if col.SemanticType == "" || c.hasExcludedDatePrefix(col.Name) {
			continue
		}
		if exactMatch(o.DateColumnExcludeNames, col.Name) {
			continue
		}
		if substringMatch(o.DateColumnExcludeSubstrings, col.Name) {
			continue
		}
		// with no eligibility config, every semantic-typed column is
		// eligible; with config, only well-known/eligible-suffix columns or
		// ones with proven golden-example usage qualify
		if hasEligibilityConfig && !wellKnown[col.Name] && !eligibleSuffix(col.Name) && c.columnSampleUsage(t, col.Name) < 3 {
			continue
		}
		score := 0
		if monthGrain == (col.SemanticType == "MONTH_YYYYMM") {
			score += 100
		}
		if wellKnown[col.Name] {
			score += 50
		}
		score += c.columnSampleUsage(t, col.Name)
		if score > bestScore {
			bestScore = score
			best = col
		}
	}
	return best
}

// hasExcludedDatePrefix reports whether a column name starts with one of the
// operator-configured DateColumnExcludePrefixes.
func (c *Catalog) hasExcludedDatePrefix(name string) bool {
	if c.Overrides == nil {
		return false
	}
	for _, prefix := range c.Overrides.DateColumnExcludePrefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func exactMatch(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func substringMatch(list []string, v string) bool {
	for _, x := range list {
		if x != "" && strings.Contains(v, x) {
			return true
		}
	}
	return false
}

// columnSampleUsage counts golden examples that target this table and
// reference the column in their SQL.
func (c *Catalog) columnSampleUsage(t *Table, colName string) int {
	n := 0
	for _, s := range c.Samples {
		if strings.Contains(strings.ToUpper(s.TargetTable), t.Name) && strings.Contains(strings.ToUpper(s.TargetSQL), colName) {
			n++
		}
	}
	return n
}

func replaceTableWithAlias(cond, fqn, alias string) string {
	return strings.ReplaceAll(cond, fqn+".", alias+".")
}
