package catalog

import (
	"strings"
	"testing"
)

func TestRankCandidates(t *testing.T) {
	c := loadTestCatalog(t)
	good := "SELECT T1.TOOL, AVG(T1.ELAPSED_MS) AS AVG_MS FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1 WHERE T1.STATUS = 'ok' GROUP BY T1.TOOL LIMIT 100"
	badColumn := "SELECT T1.TOOL, AVG(T1.NO_SUCH_COL) FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1 GROUP BY T1.TOOL LIMIT 100"
	noBound := "SELECT T1.TOOL, AVG(T1.ELAPSED_MS) AS AVG_MS FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1 GROUP BY T1.TOOL"
	res := c.RankCandidates("도구별 평균 실행시간", []string{badColumn, noBound, good},
		[]string{"도구"}, []string{"평균 실행시간"}, 100)
	if res["best_index"].(int) != 2 {
		t.Fatalf("expected best_index 2 (valid+bounded), got %v: %+v", res["best_index"], res["ranking"])
	}
	if !strings.Contains(res["best_sql"].(string), "LIMIT") {
		t.Fatalf("best sql should be the bounded one")
	}

	allBad := c.RankCandidates("q", []string{badColumn}, nil, nil, 0)
	if g, _ := allBad["guidance"].(string); !strings.Contains(g, "검증에 실패") {
		t.Fatalf("expected all-invalid guidance, got %v", allBad["guidance"])
	}
}

func TestSuggestJoins(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.SuggestJoins(nil, 30)
	suggestions := res["suggestions"].([]JoinSuggestion)
	if len(suggestions) == 0 {
		t.Fatal("expected join suggestions from single-column-PK masters")
	}
	for _, s := range suggestions {
		if s.FromColumn != s.ToColumn {
			t.Fatalf("suggestion should join on the same key column: %+v", s)
		}
		if s.Confidence > 0.85 {
			t.Fatalf("suggestion confidence must not exceed 0.85: %+v", s)
		}
		if len(s.Evidence) == 0 {
			t.Fatalf("suggestion must carry evidence: %+v", s)
		}
		// suggested edges must not already exist in the graph
		for _, e := range c.Adjacency[s.FromTable] {
			if e.To == s.ToTable {
				t.Fatalf("suggested edge already exists: %+v", s)
			}
		}
	}
	// scoped call only returns edges touching the scope table
	scoped := c.SuggestJoins([]string{"PUBLIC.JAMYPG_MCP_KEYS"}, 10)
	scopedSuggestions := scoped["suggestions"].([]JoinSuggestion)
	if len(scopedSuggestions) == 0 {
		t.Fatal("expected scoped suggestions for PUBLIC.JAMYPG_MCP_KEYS")
	}
	for _, s := range scopedSuggestions {
		if s.FromTable != "PUBLIC.JAMYPG_MCP_KEYS" && s.ToTable != "PUBLIC.JAMYPG_MCP_KEYS" {
			t.Fatalf("scoped suggestion outside scope: %+v", s)
		}
	}
}
