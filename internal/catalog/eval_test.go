package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGoldenEvaluation(t *testing.T) {
	c := loadTestCatalog(t)
	path := filepath.Join("..", "..", "data", "metadb", "golden_queries.json")
	if !FileExists(path) {
		t.Skip("golden_queries.json not present")
	}
	res, err := c.RunEvaluation(path, 5)
	if err != nil {
		t.Fatalf("RunEvaluation() error = %v", err)
	}
	// the 8-case metadb golden set currently scores 1.0 on every axis; table,
	// join, and SQL validity must stay perfect, column recall gets a small
	// margin so minor scoring shifts do not flake the suite.
	if acc := res["table_selection_acc"].(float64); acc < 1.0 {
		t.Fatalf("table selection accuracy %.2f below 1.0: %+v", acc, res["results"])
	}
	if acc := res["column_recall_avg"].(float64); acc < 0.9 {
		t.Fatalf("column recall %.2f below 0.9: %+v", acc, res["results"])
	}
	if acc := res["metric_lookup_acc"].(float64); acc < 1.0 {
		t.Fatalf("metric lookup accuracy %.2f below 1.0", acc)
	}
	if acc := res["join_path_acc"].(float64); acc < 1.0 {
		t.Fatalf("join path accuracy %.2f below 1.0: %+v", acc, res["results"])
	}
	if acc := res["expected_sql_valid"].(float64); acc < 1.0 {
		t.Fatalf("expected SQL validity %.2f below 1.0: %+v", acc, res["results"])
	}
	if n := res["cases"].(int); n < 8 {
		t.Fatalf("golden set shrank to %d cases; expected at least 8", n)
	}
}

func TestMetricDictionaryLookup(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.MetricDefinition("평균 실행시간", 5)
	if res["source"] != "dictionary" {
		t.Fatalf("expected dictionary source, got %v", res["source"])
	}
	defs := res["definitions"].([]MetricDef)
	if len(defs) == 0 || defs[0].Expression == "" {
		t.Fatalf("expected curated definition, got %+v", defs)
	}
	unknown := c.MetricDefinition("존재하지않는지표XYZ", 5)
	if unknown["source"] != "inferred" {
		t.Fatalf("expected inferred source for unknown metric, got %v", unknown["source"])
	}
}

func TestParseTimeExpressions(t *testing.T) {
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct{ start, end string }{
		"어제 가입한 고객":        {"20260701", "20260701"},
		"지난달 카드 이용금액":      {"20260601", "20260630"},
		"최근 3개월 실적":        {"20260401", "20260702"},
		"2025년 6월 평균 신용점수": {"20250601", "20250630"},
		"2025년 상반기 대출 잔액":  {"20250101", "20250630"},
	}
	for q, want := range cases {
		got := ParseTimeExpressions(q, now)
		if len(got) == 0 {
			t.Fatalf("%q: no time range parsed", q)
		}
		if got[0].Start != want.start || got[0].End != want.end {
			t.Fatalf("%q: got %s~%s want %s~%s", q, got[0].Start, got[0].End, want.start, want.end)
		}
	}
}

func TestRenderTimeConditionBySemanticType(t *testing.T) {
	tr := TimeRange{Expression: "2025년 6월", Start: "20250601", End: "20250630"}
	monCol := &Column{Name: "BS_YR_MON", SemanticType: "MONTH_YYYYMM"}
	if got := RenderTimeCondition(monCol, tr, ""); got != "BS_YR_MON = '202506'" {
		t.Fatalf("month render = %q", got)
	}
	dtCol := &Column{Name: "D3_REG_DT", SemanticType: "DATE_YYYYMMDD"}
	if got := RenderTimeCondition(dtCol, tr, "T1"); got != "T1.D3_REG_DT >= '20250601' AND T1.D3_REG_DT <= '20250630'" {
		t.Fatalf("date render = %q", got)
	}
	dateCol := &Column{Name: "CHG_DT", SemanticType: "DATE"}
	if got := RenderTimeCondition(dateCol, tr, ""); got != "CHG_DT >= DATE '2025-06-01' AND CHG_DT < DATE '2025-07-01'" {
		t.Fatalf("DATE render = %q", got)
	}
}

func TestValidatePIIAndDialect(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.ValidateSQL(ValidateRequest{SQL: "SELECT T1.EMAIL FROM PUBLIC.JAMYPG_USERS T1 LIMIT 10"})
	if res.Valid {
		t.Fatalf("expected PII violation to invalidate SQL: %+v", res)
	}
	foundPII := false
	for _, e := range res.Errors {
		if e.Code == "PII_COLUMN" {
			foundPII = true
		}
	}
	if !foundPII {
		t.Fatalf("expected PII_COLUMN error, got %+v", res.Errors)
	}
	res = c.ValidateSQL(ValidateRequest{SQL: "SELECT NVL(T1.USERNAME, '-') FROM PUBLIC.JAMYPG_USERS T1 WHERE ROWNUM <= 10"})
	foundDialect := false
	for _, e := range res.Errors {
		if e.Code == "DIALECT_FUNCTION" {
			foundDialect = true
		}
	}
	if !foundDialect {
		t.Fatalf("expected DIALECT_FUNCTION error for Oracle-only syntax, got %+v", res.Errors)
	}
	if len(res.FixHints) == 0 {
		t.Fatal("expected structured fix hints")
	}
}

func TestJoinPathConfidenceAndGuidance(t *testing.T) {
	c := loadTestCatalog(t)
	out, err := c.GetJoinPaths(JoinPathRequest{Tables: []string{"PUBLIC.JAMYPG_MCP_ACTIVITY", "PUBLIC.JAMYPG_USERS"}})
	if err != nil {
		t.Fatal(err)
	}
	paths := out["join_paths"].([]JoinPathResult)
	if len(paths) != 1 || !paths[0].Found {
		t.Fatalf("expected found path, got %+v", paths)
	}
	if paths[0].Confidence <= 0 {
		t.Fatalf("expected confidence > 0, got %+v", paths[0])
	}
	if len(paths[0].Edges) == 0 || paths[0].Edges[0].Condition == "" {
		t.Fatalf("expected rendered join condition, got %+v", paths[0].Edges)
	}
}

func TestAnalyzeQuestionDecomposition(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.AnalyzeQuestion(AnalyzeRequest{Question: "2025년 6월 등급별 평균 신용점수 상위 5개"})
	if lim, _ := res["limit"].(int); lim != 5 {
		t.Fatalf("expected limit 5, got %v", res["limit"])
	}
	trs := res["time_range"].([]TimeRange)
	if len(trs) == 0 || trs[0].Start != "20250601" {
		t.Fatalf("expected time range for 2025년 6월, got %+v", trs)
	}
	metrics := res["target_metrics"]
	if metrics == nil {
		t.Fatal("expected target_metrics")
	}
}

func TestCatalogHealth(t *testing.T) {
	c := loadTestCatalog(t)
	h := c.Health()
	if h["status"] == "" {
		t.Fatal("expected health status")
	}
	// the generated metadb fixture must load cleanly: every relation points at
	// real tables/columns, so no error-level topology issues may surface.
	for _, issue := range c.Issues {
		if issue.Source == "topology_relations.json" && issue.Level == "error" {
			t.Fatalf("metadb fixture has a broken relation: %+v", issue)
		}
	}
	if n := h["metric_definitions"].(int); n < 4 {
		t.Fatalf("expected >=4 metric definitions, got %d", n)
	}
	if h["pii_columns"] == nil {
		t.Fatal("expected pii_columns list")
	}
}

func TestExecutionBasedEvaluation(t *testing.T) {
	c := loadTestCatalog(t)
	golden := `[
	 {"question":"실행 성공+범위 통과","expected_tables":["PUBLIC.JAMYPG_USERS"],
	  "expected_sql":"SELECT COUNT(*) AS CNT FROM PUBLIC.JAMYPG_USERS T1 WHERE T1.IS_ACTIVE = TRUE",
	  "expected_min_rows":1,"expected_max_rows":100},
	 {"question":"범위 미달","expected_tables":["PUBLIC.JAMYPG_USERS"],
	  "expected_sql":"SELECT COUNT(*) AS CNT FROM PUBLIC.JAMYPG_USERS T1 WHERE T1.IS_ACTIVE = TRUE",
	  "expected_min_rows":1000},
	 {"question":"실행 오류","expected_tables":["PUBLIC.JAMYPG_USERS"],
	  "expected_sql":"SELECT T1.USERNAME FROM PUBLIC.JAMYPG_USERS T1 LIMIT 1"}
	]`
	path := filepath.Join(t.TempDir(), "golden.json")
	if err := os.WriteFile(path, []byte(golden), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	counter := func(_ context.Context, sql string) (int64, error) {
		calls++
		if calls == 3 {
			return 0, fmt.Errorf("PG-42P01: relation does not exist")
		}
		return 42, nil
	}
	res, err := c.RunEvaluationExec(context.Background(), path, 5, counter)
	if err != nil {
		t.Fatal(err)
	}
	if res["execution_checked"].(int) != 3 {
		t.Fatalf("execution_checked = %v", res["execution_checked"])
	}
	if rate := res["execution_success_rate"].(float64); rate < 0.66 || rate > 0.67 {
		t.Fatalf("execution_success_rate = %v", rate)
	}
	if res["row_sanity_checked"].(int) != 2 {
		t.Fatalf("row_sanity_checked = %v", res["row_sanity_checked"])
	}
	if rate := res["row_sanity_rate"].(float64); rate != 0.5 {
		t.Fatalf("row_sanity_rate = %v", rate)
	}
	results := res["results"].([]EvalCaseResult)
	if results[0].RowSanityOK == nil || !*results[0].RowSanityOK || *results[0].ExecutedRows != 42 {
		t.Fatalf("case0: %+v", results[0])
	}
	if results[1].RowSanityOK == nil || *results[1].RowSanityOK {
		t.Fatalf("case1 should fail sanity: %+v", results[1])
	}
	if results[2].ExecError == "" {
		t.Fatalf("case2 should carry exec error: %+v", results[2])
	}
	// counter 없이 호출하면 실행 지표가 아예 없어야 함
	res2, _ := c.RunEvaluationExec(context.Background(), path, 5, nil)
	if _, ok := res2["execution_checked"]; ok {
		t.Fatal("no counter → no execution metrics")
	}
}

func TestClassifyMisses(t *testing.T) {
	results := []EvalCaseResult{
		{Missing: nil}, // clean
		{Missing: []string{"table:public.x", "column:public.x.c"}},
		{Missing: []string{"join:a->b"}},
		{Missing: []string{"table:public.y"}},
	}
	mb := classifyMisses(results)
	if mb["clean_cases"].(int) != 1 || mb["failing_cases"].(int) != 3 {
		t.Fatalf("clean/failing wrong: %+v", mb)
	}
	by := mb["by_category"].(map[string]int)
	if by["table_miss"] != 2 || by["column_miss"] != 1 || by["join_broken"] != 1 {
		t.Fatalf("category counts wrong: %+v", by)
	}
	pri := mb["priority"].([]MissCatRank)
	if len(pri) == 0 || pri[0].Category != "table_miss" {
		t.Fatalf("table_miss should rank first: %+v", pri)
	}
	if _, ok := mb["recommendation"].(string); !ok {
		t.Fatal("recommendation missing")
	}
}

func TestMissCategoryMapping(t *testing.T) {
	cases := map[string]string{
		"table:x": "table_miss", "column:x": "column_miss", "metric:x": "metric_miss",
		"join:a->b": "join_broken", "sql_error:x": "sql_invalid", "exec:x": "exec_error",
		"rows:5": "row_sanity", "weird": "other",
	}
	for in, want := range cases {
		if got := missCategory(in); got != want {
			t.Errorf("missCategory(%q)=%q want %q", in, got, want)
		}
	}
}
