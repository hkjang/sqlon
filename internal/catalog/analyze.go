package catalog

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

type AnalyzeRequest struct {
	Question string `json:"question"`
}

var (
	reTopN    = regexp.MustCompile(`(?:상위|top)\s*(\d+)`)
	reLimitN  = regexp.MustCompile(`(\d+)\s*(?:개만|건만|명만|개까지|건까지)`)
	reCompare = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(?:점|원|건|명|%|퍼센트)?\s*(이상|이하|초과|미만)`)
)

// IntentSignature maps a question onto the same intent vocabulary used by
// sql_datasets.json target_intent (agg_count_distinct, cond_range, ...), so
// few-shot retrieval can rank examples by *structural* similarity instead of
// lexical overlap alone.
func (c *Catalog) IntentSignature(question string) []string {
	q := strings.ToLower(question)
	var sig []string
	add := func(tok string) { sig = appendUnique(sig, tok) }
	if hasAny(q, "고객 수", "고객수", "회원 수", "회원수", "명수", "몇 명", "기관수", "기관 수") {
		add("agg_count_distinct")
		add("agg_count")
	}
	if hasAny(q, "건수", "몇 건", "개수", "count", "총건수") {
		add("agg_count")
	}
	if hasAny(q, "합계", "총액", "sum", "금액의 합", "총 ", "합쳐") {
		add("agg_sum")
	}
	if hasAny(q, "평균", "avg") {
		add("agg_avg")
	}
	if hasAny(q, "최대", "최고", "최장", "max") {
		add("agg_max")
	}
	if hasAny(q, "최소", "최저", "min") {
		add("agg_min")
	}
	if hasAny(q, "별", "별로", "그룹", "기준으로 집계", "단위로") {
		add("agg_groupby")
	}
	if hasAny(q, "높은 순", "낮은 순", "많은 순", "적은 순", "순서대로", "정렬", "순으로") {
		add("sort_order")
	}
	if reTopN.MatchString(q) || reLimitN.MatchString(q) {
		add("limit_topn")
		add("sort_order")
	}
	for _, tr := range ParseTimeExpressions(question, time.Now()) {
		if tr.Start != "" {
			add("cond_range")
			break
		}
	}
	if hasAny(q, "이내", "사이", "부터", "까지") {
		add("cond_range")
	}
	if reCompare.MatchString(q) || hasAny(q, "보다 큰", "보다 작은", "0보다") {
		add("cond_compare")
	}
	if hasAny(q, "미해제", "유효", "삭제 제외", "해제되지 않은", "해지되지 않은", "미해지", "제외하고", "정상 거래") {
		add("cond_null")
	}
	if hasAny(q, "값이 있는", "존재하는", "등록된") {
		add("cond_not_null")
	}
	if hasAny(q, "포함", "로 시작", "으로 시작", "이 들어간") {
		add("cond_like")
	}
	if hasAny(q, "상태인", "구분이", "코드가", "유형이", "업종에서", "업권") {
		add("cond_eq")
	}
	if hasAny(q, "조인", "연계", "포함해서", "매핑") {
		add("join_left")
	}
	// two or more distinct conditions usually means logical composition
	condCount := 0
	for _, tok := range sig {
		if strings.HasPrefix(tok, "cond_") {
			condCount++
		}
	}
	if condCount >= 2 {
		add("cond_logic")
	}
	return sig
}

// AnalyzeQuestion decomposes a natural-language question into a structured
// plan: intent, metrics (dictionary-checked), dimensions, filters, time
// range, sort/limit/comparison, ambiguities, and applied defaults. Callers
// must run this before SQL generation.
func (c *Catalog) AnalyzeQuestion(req AnalyzeRequest) map[string]any {
	q := strings.ToLower(req.Question)
	now := time.Now()
	intent := []string{}
	dimensions := []string{}
	filters := []string{}
	domains := []string{}
	schemas := []string{}
	ambiguities := []string{}
	appliedDefaults := []string{}
	expectedOutput := []string{}

	if hasAny(q, "몇 명", "고객 수", "회원 수", "건수", "count", "몇 건", "명수") {
		intent = append(intent, "aggregation.count")
	}
	if hasAny(q, "합계", "총액", "sum", "총 ") {
		intent = append(intent, "aggregation.sum")
	}
	if hasAny(q, "평균", "avg") {
		intent = append(intent, "aggregation.avg")
	}
	if hasAny(q, "최대", "max", "최고") {
		intent = append(intent, "aggregation.max")
	}
	if hasAny(q, "최소", "min", "최저") {
		intent = append(intent, "aggregation.min")
	}
	if hasAny(q, "높은 순", "낮은 순", "상위", "top", "순위", "많은 순") {
		intent = append(intent, "sort.rank")
	}
	if hasAny(q, "별", "별로", "그룹", "분포") {
		intent = append(intent, "group_by")
	}
	if hasAny(q, "추이", "변화", "트렌드") {
		intent = append(intent, "trend")
	}
	if hasAny(q, "목록", "리스트", "내역", "조회") && len(intent) == 0 {
		intent = append(intent, "list")
	}

	// --- metrics: dictionary first ---
	type metricAnalysis struct {
		Term        string      `json:"term"`
		Source      string      `json:"source"` // dictionary | unknown
		Definitions []MetricDef `json:"definitions,omitempty"`
	}
	metricTerms := c.MetricNamesInQuestion(req.Question)
	metrics := []metricAnalysis{}
	for _, term := range metricTerms {
		metrics = append(metrics, metricAnalysis{Term: term, Source: "dictionary", Definitions: c.LookupMetrics(term)})
		expectedOutput = append(expectedOutput, term)
	}
	// metric-looking phrases without a dictionary entry
	for _, kw := range []string{"평점", "점수", "잔액", "금액", "비율", "율"} {
		if strings.Contains(q, kw) && len(c.LookupMetrics(kw)) == 0 {
			found := false
			for _, m := range metrics {
				if strings.Contains(m.Term, kw) {
					found = true
					break
				}
			}
			if !found {
				metrics = append(metrics, metricAnalysis{Term: kw, Source: "unknown"})
				ambiguities = append(ambiguities, "지표 '"+kw+"'가 지표 사전에 없습니다. get_metric_definition으로 후보를 확인하고 계산식을 사용자와 확정하세요.")
			}
		}
	}

	// --- dimensions ---
	for _, d := range []string{"회원사", "기관", "월", "일자", "고객", "등급", "성별", "연령대", "지역", "시도", "상품", "업권", "직업"} {
		if strings.Contains(q, d) {
			dimensions = append(dimensions, d)
		}
	}
	for _, pair := range [][2]string{
		{"카드", "card"}, {"대출", "loan"}, {"연체", "delinquency"}, {"보증", "guarantee"},
		{"평점", "score"}, {"점수", "score"}, {"등급", "grade"}, {"자산", "asset"}, {"주택", "asset"},
		{"개인사업자", "soho"}, {"소득", "income"},
	} {
		if strings.Contains(q, pair[0]) {
			domains = append(domains, pair[1])
		}
	}
	// operator-configured keyword→schema hints (Overrides.SchemaHints); no-op
	// unless the dataset configures them
	hintRules, hintedSchemas := c.MatchSchemaHints(req.Question)
	schemas = append(schemas, hintedSchemas...)

	// --- time range ---
	timeRanges := ParseTimeExpressions(req.Question, now)
	comparison := ""
	aggregationLevel := ""
	hasRange := false
	for _, tr := range timeRanges {
		if tr.Comparison != "" {
			comparison = tr.Comparison
		}
		if tr.Start != "" {
			hasRange = true
			filters = append(filters, "temporal: "+tr.Expression+" ("+tr.Start+"~"+tr.End+")")
		}
		if tr.Granularity != "" && strings.HasSuffix(tr.Expression, "별") {
			aggregationLevel = tr.Granularity
		}
	}
	if !hasRange && (len(intent) > 0 && intent[0] != "list") {
		ambiguities = append(ambiguities, "기간이 명시되지 않았습니다. 기본값으로 최신 스냅샷/최근 데이터 기준을 적용하되 최종 응답에 가정을 표시하세요.")
		appliedDefaults = append(appliedDefaults, "기간 미지정 → 최신 기준월/기준일 데이터 사용")
	}

	// --- comparison filters (이상/이하/초과/미만) ---
	for _, m := range reCompare.FindAllStringSubmatch(q, -1) {
		filters = append(filters, "comparison: "+m[1]+" "+m[2])
	}
	if hasAny(q, "삭제 제외", "유효", "사용중") {
		filters = append(filters, "validity filters requested")
	}

	// --- sort & limit ---
	var sortCond map[string]any
	if strings.Contains(q, "높은 순") || strings.Contains(q, "많은 순") || strings.Contains(q, "내림차순") {
		sortCond = map[string]any{"direction": "DESC"}
	} else if strings.Contains(q, "낮은 순") || strings.Contains(q, "적은 순") || strings.Contains(q, "오름차순") {
		sortCond = map[string]any{"direction": "ASC"}
	}
	limit := 0
	if m := reTopN.FindStringSubmatch(q); len(m) == 2 {
		limit, _ = strconv.Atoi(m[1])
		if sortCond == nil {
			sortCond = map[string]any{"direction": "DESC"}
		}
	} else if m := reLimitN.FindStringSubmatch(q); len(m) == 2 {
		limit, _ = strconv.Atoi(m[1])
	}
	if limit == 0 {
		appliedDefaults = append(appliedDefaults, "limit 미지정 → LIMIT 1000 기본 적용")
	}

	// --- aggregation level ---
	if aggregationLevel == "" {
		switch {
		case strings.Contains(q, "고객별") || strings.Contains(q, "고객 별"):
			aggregationLevel = "customer"
		case strings.Contains(q, "계좌별"):
			aggregationLevel = "account"
		case strings.Contains(q, "기관별") || strings.Contains(q, "회원사별"):
			aggregationLevel = "agency"
		case len(dimensions) > 0 && hasAny(q, "별", "별로"):
			aggregationLevel = dimensions[0]
		}
	}
	for _, d := range dimensions {
		expectedOutput = appendUnique(expectedOutput, d)
	}

	// --- value literals -> filter column candidates ---
	valueTokens := []string{}
	for _, tok := range tokenize(req.Question) {
		if len([]rune(tok)) >= 2 && !regexp.MustCompile(`^\d+$`).MatchString(tok) {
			valueTokens = append(valueTokens, tok)
		}
	}
	valueFilters := c.FindFilterColumns(valueTokens, nil, 6)

	// when the question matches more than one operator-configured schema-hint
	// rule group, surface each rule's note so the LLM knows how to prioritize
	if len(hintRules) > 1 {
		for _, rule := range hintRules {
			if rule.Note != "" {
				ambiguities = appendUnique(ambiguities, rule.Note)
			}
		}
	}

	search := c.SearchSchema(SearchRequest{Question: req.Question, TopK: 5, IncludeColumns: true, MaxColumns: 6})
	examples := c.SearchSamples(req.Question, 3, "")
	feedbackExamples := c.SuccessfulFeedbackExamples(req.Question, 2)

	res := map[string]any{
		"question":                req.Question,
		"intent":                  unique(intent),
		"intent_signature":        c.IntentSignature(req.Question),
		"patterns":                c.MatchPatterns(req.Question),
		"target_metrics":          metrics,
		"dimensions":              unique(dimensions),
		"filters":                 unique(filters),
		"value_filter_candidates": valueFilters["candidates"],
		"time_range":              timeRanges,
		"comparison":              comparison,
		"aggregation_level":       aggregationLevel,
		"limit":                   limit,
		"expected_output_columns": unique(expectedOutput),
		"ambiguities":             ambiguities,
		"applied_defaults":        appliedDefaults,
		"domains":                 unique(domains),
		"schema_hints":            unique(schemas),
		"top_schema_hits":         search.Results,
		"fewshot_hits":            examples["examples"],
	}
	if sortCond != nil {
		res["sort"] = sortCond
	}
	if len(feedbackExamples) > 0 {
		res["feedback_examples"] = feedbackExamples
	}
	if len(ambiguities) > 0 {
		res["guidance"] = "모호한 요소가 있습니다. 명확한 기본값이 있으면 적용하고 최종 응답에 가정을 표시하되, 기본값이 없는 항목은 SQL 생성 전에 사용자에게 확인하세요."
	}
	return res
}

func hasAny(s string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(s, strings.ToLower(v)) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func looksMetricColumn(name string) bool {
	for _, suffix := range []string{"_AMT", "_CNT", "_COUNT", "_SCORE", "_SCR", "_VAL", "_RT", "_RATE", "_GRAD"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// metricExpression infers a default aggregation expression for a column from
// generic naming-suffix conventions, plus operator-configured entity-key
// columns (Overrides.EntityKeyColumns) for distinct-count metrics.
func (c *Catalog) metricExpression(t *Table, col *Column) (string, []string) {
	name := col.Name
	notes := []string{}
	entityKey := ""
	if c.Overrides != nil && len(c.Overrides.EntityKeyColumns) > 0 {
		entityKey = c.Overrides.EntityKeyColumns[0]
	}
	switch {
	case strings.HasSuffix(name, "_AMT"):
		return "SUM(" + name + ")", notes
	case strings.HasSuffix(name, "_CNT") || strings.HasSuffix(name, "_COUNT"):
		return "SUM(" + name + ")", notes
	case strings.Contains(name, "SCORE") || strings.HasSuffix(name, "_SCR") || strings.HasSuffix(name, "_VAL"):
		return "AVG(" + name + ")", notes
	case strings.HasSuffix(name, "_GRAD") && entityKey != "" && t.ColumnMap[entityKey] != nil:
		return name + " as dimension; COUNT(DISTINCT " + entityKey + ") when " + entityKey + " exists", notes
	case entityKey != "" && name == entityKey:
		return "COUNT(DISTINCT " + entityKey + ")", notes
	}
	return "", nil
}

func metricNameBonus(metricName string, col *Column) float64 {
	l := strings.ToLower(metricName + " " + col.LogicalName + " " + col.Description)
	score := 0.0
	if strings.Contains(l, "금액") && strings.HasSuffix(col.Name, "_AMT") {
		score += 8
	}
	if strings.Contains(l, "등급") && strings.HasSuffix(col.Name, "_GRAD") {
		score += 8
	}
	if (strings.Contains(l, "점수") || strings.Contains(l, "평점")) && (strings.Contains(col.Name, "SCORE") || strings.HasSuffix(col.Name, "_SCR")) {
		score += 8
	}
	return score
}
