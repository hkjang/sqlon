package mcp

import (
	"context"
	"encoding/json"

	"testing"
)

// TestRunSQLSafelyBlockedByPendingClarifications verifies the session-level
// execution gate: after prepare_sql_context withholds the skeleton, the same
// session cannot execute SQL until the re-question is answered.
func TestRunSQLSafelyBlockedByPendingClarifications(t *testing.T) {
	s, _ := newFixtureServer(t)
	ctx := withSession(context.Background(), "clar-sess-1")

	call := func(name, args string) map[string]any {
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(args)})
		res, err := s.callTool(ctx, params)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m, ok := res.(map[string]any)
		if !ok {
			b, _ := json.Marshal(res)
			_ = json.Unmarshal(b, &m)
		}
		return m
	}

	// vague question → withheld skeleton + pending gate armed
	prep := call("prepare_sql_context", `{"question":"잔액"}`)
	if prep["status"] != "needs_clarification" {
		t.Skipf("fixture no longer flags this question: %v", prep["status"])
	}

	// execution with a profile must now be refused
	run := call("run_sql_safely", `{"sql":"SELECT 1 FROM DUAL","profile":"dev-01"}`)
	if run["status"] != "clarification_required" {
		t.Fatalf("execute during pending clarification must be refused, got %v", run["status"])
	}

	// answer the round → gate clears
	answers := map[string]string{}
	if cls, ok := prep["clarifications"].([]any); ok {
		for _, raw := range cls {
			if cl, ok := raw.(map[string]any); ok {
				answers[cl["id"].(string)] = "최근 3개월 회원사별 대출 잔액 합계"
			}
		}
	} else {
		b, _ := json.Marshal(prep["clarifications"])
		var list []map[string]any
		_ = json.Unmarshal(b, &list)
		for _, cl := range list {
			answers[cl["id"].(string)] = "최근 3개월 회원사별 대출 잔액 합계"
		}
	}
	ab, _ := json.Marshal(answers)
	prep2 := call("prepare_sql_context", `{"question":"잔액","clarifications":`+string(ab)+`}`)
	if prep2["status"] != "ready" {
		t.Fatalf("answered round should be ready, got %v", prep2["status"])
	}
	run2 := call("run_sql_safely", `{"sql":"SELECT 1 FROM DUAL","profile":"dev-01"}`)
	if run2["status"] == "clarification_required" {
		t.Fatal("gate must clear after clarifications are answered")
	}
	// different session was never gated
	other := withSession(context.Background(), "clar-sess-2")
	params, _ := json.Marshal(map[string]any{"name": "run_sql_safely",
		"arguments": json.RawMessage(`{"sql":"SELECT 1 FROM DUAL","profile":"dev-01"}`)})
	res, err := s.callTool(other, params)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := res.(map[string]any); ok && m["status"] == "clarification_required" {
		t.Fatal("other sessions must not be affected")
	}
}
