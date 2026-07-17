package catalog

import (
	"reflect"
	"strings"
	"testing"
)

func TestMetricResolverExactNameRanksBeforeBusinessNameAndAlias(t *testing.T) {
	c := &Catalog{Metrics: []MetricDef{
		{Name: "기준 순매출", BusinessName: "순매출", Aliases: []string{"net revenue"}, Expression: "SUM(BASE_NET)"},
		{Name: "순매출", BusinessName: "승인 순매출", Expression: "SUM(NET)"},
	}}

	got := c.LookupMetrics("순매출")
	if len(got) != 2 {
		t.Fatalf("LookupMetrics exact match count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "순매출" {
		t.Fatalf("exact name must rank before another metric's business name: %+v", got)
	}
	alias := c.LookupMetrics("net revenue")
	if len(alias) != 1 || alias[0].Name != "기준 순매출" {
		t.Fatalf("exact alias resolution failed: %+v", alias)
	}

	matches := c.resolveMetricMatches("순매출")
	if matches[0].Match.MatchType != "exact" || matches[0].Match.MatchedField != "name" || matches[0].Match.Confidence != 1 {
		t.Fatalf("unexpected exact-match evidence: %+v", matches[0].Match)
	}
}

func TestMetricResolverUsesGlossaryAndTokenOverlapConsistently(t *testing.T) {
	c := semanticMetricTestCatalog()
	question := "회원별 판매액 합계를 보여줘"

	lookup := c.LookupMetrics(question)
	if len(lookup) != 1 || lookup[0].Name != "고객 매출 합계" {
		t.Fatalf("LookupMetrics semantic result = %+v", lookup)
	}
	if got := c.MetricNamesInQuestion(question); !reflect.DeepEqual(got, []string{"고객 매출 합계"}) {
		t.Fatalf("MetricNamesInQuestion = %v", got)
	}
	byTable := c.metricsInQuestion(question)
	if got := byTable["PUBLIC.SALES"]; !reflect.DeepEqual(got, []string{"고객 매출 합계"}) {
		t.Fatalf("metricsInQuestion = %+v", byTable)
	}

	matches := c.resolveMetricMatches(question)
	if len(matches) != 1 {
		t.Fatalf("resolveMetricMatches = %+v", matches)
	}
	match := matches[0].Match
	if match.MatchType != "semantic" || match.Confidence < metricSemanticConfidenceThreshold || match.Confidence >= 0.97 {
		t.Fatalf("semantic confidence/type = %+v", match)
	}
	evidence := strings.Join(match.Evidence, " | ")
	for _, want := range []string{"glossary '고객'", "glossary '매출'", "token overlap: 3/3", "token window: 3"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("match evidence %q missing %q", evidence, want)
		}
	}

	definition := c.MetricDefinition(question, 5)
	if definition["source"] != "dictionary" {
		t.Fatalf("MetricDefinition source = %v", definition["source"])
	}
	if definition["confidence_threshold"] != metricSemanticConfidenceThreshold {
		t.Fatalf("confidence threshold = %v", definition["confidence_threshold"])
	}
	matchEvidence, ok := definition["match_evidence"].([]MetricMatchEvidence)
	if !ok || len(matchEvidence) != 1 || matchEvidence[0].MetricName != "고객 매출 합계" {
		t.Fatalf("MetricDefinition evidence = %T %+v", definition["match_evidence"], definition["match_evidence"])
	}

	analysis := c.AnalyzeQuestion(AnalyzeRequest{Question: question})
	if got := analysis["expected_output_columns"].([]string); !reflect.DeepEqual(got, []string{"고객 매출 합계"}) {
		t.Fatalf("AnalyzeQuestion did not use semantic resolver: %v", got)
	}
}

func TestMetricResolverRejectsPartialAndDistantTokenFalsePositives(t *testing.T) {
	table := &Table{Schema: "PUBLIC", Name: "ACTIVITY", FQN: "PUBLIC.ACTIVITY", ColumnMap: map[string]*Column{}}
	c := &Catalog{
		Tables: map[string]*Table{table.FQN: table},
		ByName: map[string][]*Table{table.Name: {table}},
		Glossary: &Glossary{Entries: []GlossaryEntry{
			{Term: "사용자", Synonyms: []string{"회원"}},
			{Term: "도구", Synonyms: []string{"툴"}},
		}},
		Metrics: []MetricDef{
			{Name: "사용자 수", Aliases: []string{"사용 인원"}, Expression: "COUNT(USER_ID)", Tables: []string{table.FQN}},
			{Name: "도구 호출 수", Expression: "COUNT(*)", Tables: []string{table.FQN}},
			{Name: "전환율", Aliases: []string{"율"}, Expression: "AVG(CONVERSION_RATE)", Tables: []string{table.FQN}},
		},
	}

	for _, question := range []string{
		"사용자 상태와 이름을 조회해줘", // only 1/2 label tokens
		"도구 호출 목록을 보여줘",    // missing the metric's count concept
		"확률 분포를 보여줘",       // must not substring-match the short alias "율"
		"수",                // generic partial label
	} {
		if got := c.LookupMetrics(question); len(got) != 0 {
			t.Fatalf("%q produced false-positive metrics: %+v", question, got)
		}
	}

	// "사용자" and the final "수" are outside the resolver's local token
	// window; only the exact "도구 호출 수" phrase may match.
	question := "사용자와 도구 호출 수"
	got := c.MetricNamesInQuestion(question)
	if !reflect.DeepEqual(got, []string{"도구 호출 수"}) {
		t.Fatalf("distant tokens caused a false positive: %v", got)
	}
}

func TestMetricResolverSupportsCompactKoreanLabel(t *testing.T) {
	c := &Catalog{Metrics: []MetricDef{{Name: "사용자 수", Expression: "COUNT(*)"}}}
	matches := c.resolveMetricMatches("사용자수를 알려줘")
	if len(matches) != 1 || matches[0].Match.MatchType != "exact_phrase" {
		t.Fatalf("compact Korean label was not matched exactly: %+v", matches)
	}
}

func semanticMetricTestCatalog() *Catalog {
	table := &Table{Schema: "PUBLIC", Name: "SALES", FQN: "PUBLIC.SALES", ColumnMap: map[string]*Column{}}
	return &Catalog{
		Tables: map[string]*Table{table.FQN: table},
		ByName: map[string][]*Table{table.Name: {table}},
		Glossary: &Glossary{Entries: []GlossaryEntry{
			{Term: "고객", Synonyms: []string{"회원", "customer"}},
			{Term: "매출", Synonyms: []string{"판매액", "revenue"}},
		}},
		Metrics: []MetricDef{{
			Name: "고객 매출 합계", BusinessName: "고객별 총 매출액",
			Expression: "SUM(SALES_AMOUNT)", Tables: []string{table.FQN},
		}},
	}
}
