package catalog

import (
	"path/filepath"
	"strings"
)

// Retrieval-stage evaluation (design §8): measure the retriever in isolation —
// Table Recall@k, Column Recall@k, Join Path Recall, Value Evidence recall —
// so a generation failure can be attributed to retrieval vs SQL synthesis.
// It also runs plain search side-by-side with the graph-expanded retriever so
// weight changes show their recall delta directly.

type RetrievalCaseResult struct {
	Question         string   `json:"question"`
	GraphTableRecall float64  `json:"graph_table_recall"`
	PlainTableRecall float64  `json:"plain_table_recall"`
	GraphTableRank   int      `json:"graph_table_rank,omitempty"` // best rank of any expected table
	ColumnRecall     float64  `json:"column_recall"`
	JoinRecall       *bool    `json:"join_recall,omitempty"`
	ValueEvidence    *bool    `json:"value_evidence,omitempty"`
	Missing          []string `json:"missing,omitempty"`
}

// EvaluateRetrieval scores the retrieval layer against the golden set.
func (c *Catalog) EvaluateRetrieval(goldenPath string, topK int) (map[string]any, error) {
	if goldenPath == "" {
		goldenPath = filepath.Join(c.DataDir, "golden_queries.json")
	}
	if topK <= 0 {
		topK = 5
	}
	var golden []GoldenQuery
	if err := readJSON(goldenPath, &golden); err != nil {
		return nil, err
	}

	results := make([]RetrievalCaseResult, 0, len(golden))
	var gtSum, ptSum, colSum float64
	joinTotal, joinHit, valTotal, valHit := 0, 0, 0, 0

	for _, g := range golden {
		r := RetrievalCaseResult{Question: g.Question}
		ret := c.RetrieveContext(g.Question, topK)
		plain := c.SearchSchema(SearchRequest{Question: g.Question, TopK: topK})

		resolve := func(want string) string {
			if t, ok := c.ResolveTable(want); ok {
				return t.FQN
			}
			return strings.ToUpper(strings.TrimSpace(want))
		}

		// ---- table recall@k: graph vs plain ----
		if len(g.ExpectedTables) > 0 {
			gHit, pHit := 0, 0
			for _, want := range g.ExpectedTables {
				fqn := resolve(want)
				for i, tc := range ret.Tables {
					if tc.Table == fqn {
						gHit++
						if r.GraphTableRank == 0 || i+1 < r.GraphTableRank {
							r.GraphTableRank = i + 1
						}
						break
					}
				}
				found := false
				for _, res := range plain.Results {
					if res.Table == fqn {
						found = true
						break
					}
				}
				if found {
					pHit++
				}
				if gHit == 0 {
					r.Missing = appendUnique(r.Missing, "table:"+want)
				}
			}
			r.GraphTableRecall = round(float64(gHit) / float64(len(g.ExpectedTables)))
			r.PlainTableRecall = round(float64(pHit) / float64(len(g.ExpectedTables)))
		} else {
			r.GraphTableRecall, r.PlainTableRecall = 1, 1
		}

		// ---- column recall@k over retrieved candidate columns ----
		if len(g.ExpectedColumns) > 0 {
			found := 0
			for _, want := range g.ExpectedColumns {
				wc := cleanIdent(want)
				if i := strings.LastIndex(wc, "."); i >= 0 {
					wc = wc[i+1:]
				}
				hit := false
				for _, tc := range ret.Tables {
					for _, col := range tc.Columns {
						if strings.EqualFold(col, wc) {
							hit = true
							break
						}
					}
				}
				if hit {
					found++
				} else {
					r.Missing = appendUnique(r.Missing, "column:"+want)
				}
			}
			r.ColumnRecall = round(float64(found) / float64(len(g.ExpectedColumns)))
		} else {
			r.ColumnRecall = 1
		}

		// ---- join path recall: every expected multi-table pair reachable ----
		if len(g.ExpectedTables) > 1 {
			joinTotal++
			ok := true
			if out, err := c.GetJoinPaths(JoinPathRequest{Tables: g.ExpectedTables}); err != nil {
				ok = false
			} else if paths, has := out["join_paths"].([]JoinPathResult); has {
				for _, p := range paths {
					if !p.Found {
						ok = false
						r.Missing = appendUnique(r.Missing, "join:"+p.From+"->"+p.To)
					}
				}
			}
			r.JoinRecall = &ok
			if ok {
				joinHit++
			}
		}

		// ---- value evidence: a question literal linked to a real column ----
		if len(ret.Trace["value_tokens"].([]string)) > 0 {
			valTotal++
			ok := len(ret.Values) > 0
			r.ValueEvidence = &ok
			if ok {
				valHit++
			}
		}

		gtSum += r.GraphTableRecall
		ptSum += r.PlainTableRecall
		colSum += r.ColumnRecall
		results = append(results, r)
	}

	n := max(1, len(golden))
	return map[string]any{
		"golden_path":              goldenPath,
		"top_k":                    topK,
		"cases":                    len(golden),
		"table_recall_at_k":        round(gtSum / float64(n)),
		"table_recall_plain":       round(ptSum / float64(n)),
		"table_recall_gain":        round((gtSum - ptSum) / float64(n)),
		"column_recall_at_k":       round(colSum / float64(n)),
		"join_path_recall":         ratio(joinHit, max(1, joinTotal)),
		"value_evidence_recall":    ratio(valHit, max(1, valTotal)),
		"value_evidence_evaluated": valTotal,
		"results":                  results,
		"note":                     "검색 단계만 분리 측정: table/column/join/value recall. table_recall_gain은 그래프 확장 재순위가 순수 검색 대비 얻은 recall 차이.",
	}, nil
}
