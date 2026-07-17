package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"sqlon/internal/meta"
)

func TestClarificationSuggestionsAggregation(t *testing.T) {
	mk := func(id, ans string, at time.Time) *meta.MCPActivity {
		params, _ := json.Marshal(map[string]any{
			"arguments": map[string]any{"clarifications": map[string]string{id: ans}},
		})
		return &meta.MCPActivity{Kind: meta.ActivityPrompt, Params: params, CreatedAt: at}
	}
	now := time.Now()
	acts := []*meta.MCPActivity{
		mk("metric:잔액", "대출잔액 합계", now),
		mk("metric:잔액", "대출잔액 합계", now.Add(-time.Hour)),
		mk("metric:잔액", "예금잔액", now.Add(-2*time.Hour)),
		mk("too_vague", "한 번만", now), // single occurrence → filtered out
		{Kind: meta.ActivityExecute, SQL: "SELECT 1"},
	}
	sug := clarificationSuggestions(acts)
	if len(sug) != 1 {
		t.Fatalf("expected 1 suggestion (2+ occurrences), got %d: %+v", len(sug), sug)
	}
	s0 := sug[0]
	if s0["clarification_id"] != "metric:잔액" || s0["occurrences"].(int) != 3 {
		t.Fatalf("wrong aggregation: %+v", s0)
	}
	if s0["top_answer"] != "대출잔액 합계" || s0["top_answer_count"].(int) != 2 {
		t.Fatalf("wrong top answer: %+v", s0)
	}
}

func TestOraHintMapping(t *testing.T) {
	if h := dbHint(`ERROR: relation "foo" does not exist (SQLSTATE 42P01)`); h == "" {
		t.Fatal("SQLSTATE 42P01 should map to a hint")
	}
	if h := dbHint("Error 1146 (42S02): Table 'db.foo' doesn't exist"); h == "" {
		t.Fatal("mysql errno 1146 should map to a hint")
	}
	if h := dbHint("some non-oracle error"); h != "" {
		t.Fatalf("unknown errors should have no hint, got %q", h)
	}
}
