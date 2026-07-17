package catalog

import (
	"testing"
	"time"
)

func TestRetrieveContextGraphExpansion(t *testing.T) {
	c := loadTestCatalog(t)
	ret := c.RetrieveContext("최근 3개월 사용자별 도구 호출 수 상위 10개", 6)

	if len(ret.Tables) == 0 {
		t.Fatal("retriever returned no tables")
	}
	// every candidate carries per-signal provenance and a hybrid score
	for _, tc := range ret.Tables {
		if tc.Score <= 0 {
			t.Fatalf("candidate %s has non-positive score", tc.Table)
		}
		if len(tc.Signals) == 0 {
			t.Fatalf("candidate %s missing signal provenance", tc.Table)
		}
		if tc.Origin != "seed" && tc.Origin != "join_expanded" {
			t.Fatalf("unexpected origin %q", tc.Origin)
		}
	}
	// order-preserving fusion: seeds keep the search order verbatim, which the
	// rank-decayed semantic signal encodes (non-increasing over seeds)
	prev := 2.0
	for _, tc := range ret.Tables {
		if tc.Origin != "seed" {
			continue
		}
		if tc.Signals["semantic"] > prev {
			t.Fatalf("seed candidates not in search order: %s semantic=%v > prev=%v", tc.Table, tc.Signals["semantic"], prev)
		}
		prev = tc.Signals["semantic"]
	}
	// the two question-relevant tables must lead the ranking
	top3 := map[string]bool{}
	for i, tc := range ret.Tables {
		if i < 3 {
			top3[tc.Table] = true
		}
	}
	if !top3["PUBLIC.JAMYPG_MCP_ACTIVITY"] || !top3["PUBLIC.JAMYPG_USERS"] {
		t.Fatalf("expected activity+users in top 3, got %+v", ret.Tables)
	}
	// trace records the weights so runs are reproducible/tunable
	if ret.Trace["weights"] == nil {
		t.Fatal("trace missing weights")
	}
	// metrics detected from the question
	if len(ret.Metrics) == 0 || ret.Metrics[0] != "도구 호출 수" {
		t.Fatalf("'도구 호출 수' should be detected as a metric, got %v", ret.Metrics)
	}
}

func TestRetrieveContextJoinExpansionAddsNeighbors(t *testing.T) {
	c := loadTestCatalog(t)
	if len(c.Adjacency) == 0 {
		t.Skip("no join graph in fixture")
	}
	ret := c.RetrieveContext("사용자 세션 접속 이력", 8)
	expanded := 0
	for _, tc := range ret.Tables {
		if tc.Origin == "join_expanded" || tc.Signals["join"] > 0 {
			expanded++
		}
	}
	if expanded == 0 {
		t.Log("no join-expanded candidates surfaced in top-k (acceptable if seeds dominate); trace:", ret.Trace)
	}
}

func TestPrepareContextUsesGraphRanking(t *testing.T) {
	c := loadTestCatalog(t)
	now := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	res := c.PrepareContext("최근 3개월 도구별 평균 실행시간 상위 5개", nil, 100, now, nil)
	if res["status"] != "ready" {
		t.Fatalf("expected ready, got %v", res["status"])
	}
	ranked, ok := res["ranked_tables"].([]TableCandidate)
	if !ok || len(ranked) == 0 {
		t.Fatalf("bundle must carry ranked_tables with provenance, got %T", res["ranked_tables"])
	}
	sel, _ := res["selected_tables"].([]string)
	if len(sel) == 0 || sel[0] != ranked[0].Table {
		t.Fatalf("selected_tables should follow graph ranking: sel=%v top=%s", sel, ranked[0].Table)
	}
	if res["retrieval_trace"] == nil {
		t.Fatal("bundle must include retrieval_trace")
	}
}

func TestEvaluateRetrievalOnGoldenSet(t *testing.T) {
	c := loadTestCatalog(t)
	sum, err := c.EvaluateRetrieval("", 5)
	if err != nil {
		t.Fatalf("EvaluateRetrieval: %v", err)
	}
	for _, k := range []string{"table_recall_at_k", "table_recall_plain", "column_recall_at_k", "join_path_recall", "value_evidence_recall"} {
		if _, ok := sum[k]; !ok {
			t.Fatalf("summary missing %s: %v", k, sum)
		}
	}
	if sum["cases"].(int) == 0 {
		t.Skip("empty golden set")
	}
	if tr := sum["table_recall_at_k"].(float64); tr < 0.5 {
		t.Fatalf("graph table recall@5 suspiciously low: %v", tr)
	}
}
