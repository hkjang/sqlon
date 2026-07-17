package catalog

import (
	"strings"
	"testing"
	"time"
)

func TestIntentSignature(t *testing.T) {
	c := loadTestCatalog(t)
	sig := c.IntentSignature("최근 6개월간 이용금액이 0보다 큰 고객 수를 회원사별로 많은 순으로 상위 10개")
	want := []string{"agg_count_distinct", "cond_range", "cond_compare", "agg_groupby", "sort_order", "limit_topn", "cond_logic"}
	have := map[string]bool{}
	for _, s := range sig {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Fatalf("expected intent %q in signature %v", w, sig)
		}
	}
}

func TestSearchSamplesIntentBoost(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.SearchSamples("최근 3개월 카드 이용 고객 수", 5, "")
	examples := res["examples"]
	if examples == nil {
		t.Fatal("expected examples")
	}
	if res["intent_signature"] == nil {
		t.Fatal("expected intent_signature in response")
	}
}

func TestCodeValueValidation(t *testing.T) {
	c := loadTestCatalog(t)
	// STATUS code dict: ok:정상 실행, error:실행 오류
	bad := c.ValidateSQL(ValidateRequest{SQL: "SELECT T1.ID FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1 WHERE T1.STATUS = 'done' LIMIT 10"})
	found := false
	for _, e := range bad.Errors {
		if e.Code == "CODE_VALUE_UNKNOWN" && strings.Contains(e.Hint, "OK:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected CODE_VALUE_UNKNOWN error, got %+v", bad.Errors)
	}
	good := c.ValidateSQL(ValidateRequest{SQL: "SELECT T1.ID FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1 WHERE T1.STATUS = 'ok' LIMIT 10"})
	for _, e := range good.Errors {
		if e.Code == "CODE_VALUE_UNKNOWN" {
			t.Fatalf("valid code flagged: %+v", e)
		}
	}
	inList := c.ValidateSQL(ValidateRequest{SQL: "SELECT T1.ID FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1 WHERE T1.STATUS IN ('ok', 'done') LIMIT 10"})
	found = false
	for _, e := range inList.Errors {
		if e.Code == "CODE_VALUE_UNKNOWN" && strings.Contains(e.Message, "'done'") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected CODE_VALUE_UNKNOWN for IN-list item, got %+v", inList.Errors)
	}
}

func TestExpectedOutputValidation(t *testing.T) {
	c := loadTestCatalog(t)
	missing := c.ValidateSQL(ValidateRequest{
		SQL:             "SELECT COUNT(DISTINCT T1.ID) AS CNT FROM PUBLIC.JAMYPG_USERS T1 LIMIT 10",
		ExpectedOutputs: []string{"역할"},
	})
	found := false
	for _, w := range missing.Warnings {
		if w.Code == "EXPECTED_OUTPUT_MISSING" && strings.Contains(w.Hint, "ROLE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EXPECTED_OUTPUT_MISSING with ROLE hint, got %+v", missing.Warnings)
	}
	covered := c.ValidateSQL(ValidateRequest{
		SQL:             "SELECT T1.ROLE, COUNT(DISTINCT T1.ID) AS CNT FROM PUBLIC.JAMYPG_USERS T1 GROUP BY T1.ROLE LIMIT 10",
		ExpectedOutputs: []string{"역할"},
	})
	for _, w := range covered.Warnings {
		if w.Code == "EXPECTED_OUTPUT_MISSING" {
			t.Fatalf("covered output flagged: %+v", w)
		}
	}
}

func TestCTEAndInlineViewValidation(t *testing.T) {
	c := loadTestCatalog(t)
	sql := `WITH per_user AS (
  SELECT T1.USER_ID, COUNT(*) AS CALLS
  FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1
  WHERE T1.STATUS = 'ok'
  GROUP BY T1.USER_ID
)
SELECT m.USER_ID, m.CALLS
FROM per_user m
ORDER BY m.CALLS DESC
LIMIT 10`
	res := c.ValidateSQL(ValidateRequest{SQL: sql})
	for _, e := range res.Errors {
		if e.Code == "UNKNOWN_TABLE" {
			t.Fatalf("CTE name flagged as unknown table: %+v", e)
		}
	}
	for _, w := range res.Warnings {
		if w.Code == "UNKNOWN_ALIAS" && strings.HasPrefix(w.Column, "M.") {
			t.Fatalf("CTE alias columns flagged: %+v", w)
		}
	}
	if !res.Valid {
		t.Fatalf("CTE query should be valid, errors: %+v", res.Errors)
	}

	inline := `SELECT X.USER_ID
FROM (
  SELECT T1.USER_ID FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1 WHERE T1.STATUS = 'ok'
) X
LIMIT 10`
	res2 := c.ValidateSQL(ValidateRequest{SQL: inline})
	for _, w := range res2.Warnings {
		if w.Code == "UNKNOWN_ALIAS" {
			t.Fatalf("inline view alias flagged: %+v", w)
		}
	}
	// real tables inside the CTE/subquery must still be validated
	badInner := `WITH x AS (SELECT T1.NOPE_COL FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1) SELECT * FROM x LIMIT 5`
	res3 := c.ValidateSQL(ValidateRequest{SQL: badInner})
	found := false
	for _, e := range res3.Errors {
		if e.Code == "UNKNOWN_COLUMN" && e.Column == "NOPE_COL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected UNKNOWN_COLUMN inside CTE body, got %+v", res3.Errors)
	}
}

func TestMatchPatterns(t *testing.T) {
	c := loadTestCatalog(t)
	got := c.MatchPatterns("전월 대비 카드 이용금액 증감률과 등급별 비율")
	names := map[string]bool{}
	for _, p := range got {
		names[p.Name] = true
	}
	if !names["mom_change"] || !names["ratio"] {
		t.Fatalf("expected mom_change and ratio patterns, got %v", names)
	}
}

func TestBuildSQLSkeleton(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.BuildSQLSkeleton("2026년 7월 사용자별 도구 호출 수", []string{"PUBLIC.JAMYPG_MCP_ACTIVITY", "PUBLIC.JAMYPG_USERS"}, 100,
		time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC))
	skeleton, _ := res["skeleton_sql"].(string)
	if skeleton == "" {
		t.Fatalf("expected skeleton_sql, got %+v", res)
	}
	if !strings.Contains(skeleton, "JOIN PUBLIC.JAMYPG_USERS") || !strings.Contains(skeleton, "T1.USER_ID = T2.ID") {
		t.Fatalf("expected catalog join with aliases in skeleton:\n%s", skeleton)
	}
	if !strings.Contains(skeleton, "T1.CREATED_AT") || !strings.Contains(skeleton, "2026-07-01") {
		t.Fatalf("expected rendered TIMESTAMP time condition in skeleton:\n%s", skeleton)
	}
	if !strings.Contains(skeleton, `COUNT(*) AS "도구 호출 수"`) {
		t.Fatalf("expected dictionary metric expression in skeleton:\n%s", skeleton)
	}
	if !strings.Contains(skeleton, "LIMIT 100") {
		t.Fatalf("expected row bound in skeleton:\n%s", skeleton)
	}
}
