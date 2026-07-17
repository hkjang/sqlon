package catalog

import (
	"strings"
	"testing"
	"time"
)

func TestDiagnoseZeroRowsHints(t *testing.T) {
	c := loadTestCatalog(t)

	// find a table+column with a code dictionary to ground the test
	var tbl, col, dict string
	for _, tb := range c.Tables {
		for _, cl := range tb.Columns {
			if cl.CodeDict != "" && strings.Contains(cl.CodeDict, ":") {
				tbl, col, dict = tb.FQN, cl.Name, cl.CodeDict
				break
			}
		}
		if tbl != "" {
			break
		}
	}
	if tbl == "" {
		t.Skip("no code-dict column in fixture")
	}
	_ = dict
	sql := "SELECT * FROM " + tbl + " WHERE " + col + " = 'ZZ_NOT_A_CODE' AND BS_YR_MON BETWEEN '209901' AND '209912'"
	hints := c.DiagnoseZeroRows(sql)
	joined := strings.Join(hints, "\n")
	if !strings.Contains(joined, "ZZ_NOT_A_CODE") {
		t.Fatalf("expected code-dict hint for unknown value, got: %s", joined)
	}
	if !strings.Contains(joined, "기간") {
		t.Fatalf("expected time-range hint, got: %s", joined)
	}
}

func TestPrepareFollowupMergesContext(t *testing.T) {
	c := loadTestCatalog(t)
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	prev := "최근 3개월 회원사별 연체율 상위 10개"
	res := c.PrepareFollowup("그중 서울만", prev, "SELECT 1 FROM DUAL", nil, 0, now, nil)
	if res["status"] != "ready" {
		t.Fatalf("short follow-up must not hit too_vague, got %v (clar=%v)", res["status"], res["clarifications"])
	}
	if res["followup"] != true {
		t.Fatal("followup flag missing")
	}
	if res["previous_sql"] != "SELECT 1 FROM DUAL" {
		t.Fatal("previous_sql must ride the bundle")
	}
	q, _ := res["question"].(string)
	if !strings.Contains(q, prev) || !strings.Contains(q, "서울") {
		t.Fatalf("merged question should contain both turns, got %q", q)
	}
}
