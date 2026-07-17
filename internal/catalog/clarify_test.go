package catalog

import (
	"strings"
	"testing"
	"time"
)

func TestPrepareContextWithholdsSkeletonOnVagueQuestion(t *testing.T) {
	c := loadTestCatalog(t)
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// too short, no metric, no time → blocking clarification, no skeleton
	res := c.PrepareContext("잔액", nil, 0, now, nil)
	if res["status"] != "needs_clarification" {
		t.Fatalf("vague question should need clarification, got status=%v", res["status"])
	}
	if _, hasSkeleton := res["skeleton"]; hasSkeleton {
		t.Fatal("skeleton must be withheld while blocking clarifications exist")
	}
	cls, ok := res["clarifications"].([]Clarification)
	if !ok || len(cls) == 0 {
		t.Fatalf("expected structured clarifications, got %T", res["clarifications"])
	}
	for _, cl := range cls {
		if cl.Severity != SeverityBlocking {
			t.Fatalf("clarifications list must be blocking-only, got %s (%s)", cl.Severity, cl.ID)
		}
		if cl.Question == "" {
			t.Fatalf("clarification %s has no question text", cl.ID)
		}
	}
}

func TestPrepareContextResolvesClarificationsAndProceeds(t *testing.T) {
	c := loadTestCatalog(t)
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	first := c.PrepareContext("잔액", nil, 0, now, nil)
	if first["status"] != "needs_clarification" {
		t.Skipf("fixture no longer flags this question: %v", first["status"])
	}
	cls := first["clarifications"].([]Clarification)

	// answer every blocking item with something concrete
	answers := map[string]string{}
	for _, cl := range cls {
		if len(cl.Options) > 0 {
			answers[cl.ID] = cl.Options[0].Key
		} else {
			answers[cl.ID] = "최근 3개월 회원사별 대출 잔액 합계"
		}
	}
	second := c.PrepareContext("잔액", nil, 0, now, answers)
	if second["status"] != "ready" {
		t.Fatalf("answered clarifications should unlock the bundle, got %v (clar=%v)",
			second["status"], second["clarifications"])
	}
	if _, ok := second["skeleton"]; !ok {
		t.Fatal("resolved call must include the skeleton")
	}
	if rq, _ := second["resolved_question"].(string); rq == "" || !strings.Contains(rq, "잔액") {
		t.Fatalf("resolved_question should carry the merged question, got %q", rq)
	}
}

func TestPrepareContextClearQuestionIsReady(t *testing.T) {
	c := loadTestCatalog(t)
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	res := c.PrepareContext("최근 3개월 도구별 평균 실행시간 상위 5개", nil, 100, now, nil)
	if res["status"] != "ready" {
		t.Fatalf("clear question should be ready, got %v (clarifications=%v)",
			res["status"], res["clarifications"])
	}
	if _, ok := res["skeleton"]; !ok {
		t.Fatal("ready bundle must include skeleton")
	}
}

func TestExplicitTablesBypassBlockingGate(t *testing.T) {
	c := loadTestCatalog(t)
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	var anyTable string
	for _, t2 := range c.Tables {
		anyTable = t2.FQN
		break
	}
	if anyTable == "" {
		t.Skip("no tables in fixture")
	}
	res := c.PrepareContext("잔액", []string{anyTable}, 0, now, nil)
	if res["status"] == "needs_clarification" {
		t.Fatal("caller-supplied tables should take responsibility and bypass the table gate")
	}
}

func TestResolveClarificationsExpandsOptionKeys(t *testing.T) {
	cls := []Clarification{{
		ID: "metric:잔액", Severity: SeverityBlocking, Question: "q",
		Options: []ClarOption{{Key: "a", Label: "대출잔액 합계"}, {Key: "b", Label: "예금잔액 합계"}},
	}}
	got := ResolveClarifications("잔액 알려줘", cls, map[string]string{"metric:잔액": "b"})
	if !strings.Contains(got, "예금잔액 합계") {
		t.Fatalf("option key should expand to label, got %q", got)
	}
	free := ResolveClarifications("잔액 알려줘", cls, map[string]string{"metric:잔액": "요구불예금 기준"})
	if !strings.Contains(free, "요구불예금 기준") {
		t.Fatalf("free text answer should be appended, got %q", free)
	}
}
