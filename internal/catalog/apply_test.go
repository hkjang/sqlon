package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJSON(t *testing.T, dir, file string, v any) {
	t.Helper()
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSONT(t *testing.T, dir, file string, dst any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatal(err)
	}
}

// seedDecisions writes approved decisions of all kinds into dataDir.
func seedDecisions(t *testing.T, c *Catalog) {
	t.Helper()
	recs := map[string]ReviewRecord{
		"ln-1": {ID: "ln-1", Kind: "logical_name", Table: "S.T", Column: "CUST_NO",
			Suggested: "고객 번호", Status: "approved", DecidedAt: "2026-07-12T00:00:00Z"},
		"st-1": {ID: "st-1", Kind: "semantic_type", Table: "S.T", Column: "CUST_NO",
			Suggested: "IDENTIFIER", Status: "approved", DecidedAt: "2026-07-12T00:00:00Z"},
		"mt-1": {ID: "mt-1", Kind: "metric", Table: "S.T", Column: "TOT_AMT",
			Suggested: map[string]any{"name": "sum_tot_amt", "expression": "SUM(S.T.TOT_AMT)", "aggregation": "SUM"},
			Status:    "approved", DecidedAt: "2026-07-12T00:00:00Z"},
		"rl-1": {ID: "rl-1", Kind: "relation", Table: "S.T", Column: "CUST_NO",
			Suggested: map[string]any{"base_table": "S.T", "base_column": "CUST_NO",
				"reference_table": "S.CUSTOMER", "reference_column": "CUST_NO", "cardinality": "many-to-one"},
			Status: "approved", DecidedAt: "2026-07-12T00:00:00Z"},
		"cd-1": {ID: "cd-1", Kind: "code_dict", Table: "S.T", Column: "STATUS_CD",
			Suggested: map[string]any{"code_dict": "t_status_cd",
				"entries": []any{map[string]any{"code": "A", "label": ""}, map[string]any{"code": "B", "label": "보류"}}},
			Status: "approved", DecidedAt: "2026-07-12T00:00:00Z"},
		"rj-1": {ID: "rj-1", Kind: "metric", Table: "S.T", Column: "X",
			Suggested: map[string]any{"name": "never"}, Status: "rejected", DecidedAt: "2026-07-12T00:00:00Z"},
	}
	if err := c.saveReviewRecords(recs); err != nil {
		t.Fatal(err)
	}
}

func TestApplyApprovedMergesAllKinds(t *testing.T) {
	dir := t.TempDir()
	c := &Catalog{DataDir: dir}
	seedDecisions(t, c)
	// pre-existing files: overrides has a curated logical_name that must win;
	// metrics has an unrelated metric.
	writeJSON(t, dir, "overrides.json", map[string]any{
		"columns": []any{map[string]any{"table": "S.T", "column": "CUST_NO", "logical_name": "수기 논리명"}},
	})
	writeJSON(t, dir, "metrics.json", []any{map[string]any{"name": "existing_metric", "expression": "COUNT(*)"}})

	res := c.ApplyApproved(dir, time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC))
	if res["error"] != nil {
		t.Fatalf("apply failed: %v", res["error"])
	}
	if res["applied"].(int) != 5 {
		t.Fatalf("expected 5 applied, got %v", res["applied"])
	}

	// overrides: curated logical_name kept, semantic_type added
	var ov map[string]any
	readJSONT(t, dir, "overrides.json", &ov)
	cols := ov["columns"].([]any)
	if len(cols) != 1 {
		t.Fatalf("expected 1 merged column entry, got %d", len(cols))
	}
	entry := cols[0].(map[string]any)
	if entry["logical_name"] != "수기 논리명" {
		t.Fatalf("curated value must win, got %v", entry["logical_name"])
	}
	if entry["semantic_type"] != "IDENTIFIER" {
		t.Fatalf("semantic_type not merged: %v", entry)
	}

	// metrics: appended without clobbering
	var ms []map[string]any
	readJSONT(t, dir, "metrics.json", &ms)
	if len(ms) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(ms))
	}

	// relations: created with split schema/table
	var rels []map[string]any
	readJSONT(t, dir, "topology_relations.json", &rels)
	if len(rels) != 1 || rels[0]["base_schema"] != "S" || rels[0]["base_table"] != "T" {
		t.Fatalf("relation not written correctly: %+v", rels)
	}

	// code dict: empty label falls back to code
	var cds []map[string]any
	readJSONT(t, dir, "meta_code_dict.json", &cds)
	if len(cds) != 1 || cds[0]["code_dict_txt"] != "A:A, B:보류" {
		t.Fatalf("code dict wrong: %+v", cds)
	}

	// idempotency: second apply is a no-op
	res2 := c.ApplyApproved(dir, time.Date(2026, 7, 12, 2, 0, 0, 0, time.UTC))
	if res2["applied"].(int) != 0 {
		t.Fatalf("re-apply must be a no-op, got %v", res2["applied"])
	}
	readJSONT(t, dir, "metrics.json", &ms)
	if len(ms) != 2 {
		t.Fatalf("re-apply duplicated metrics: %d", len(ms))
	}

	// rejected record was never applied
	stored, _ := c.loadReviewRecords()
	if stored["rj-1"].AppliedAt != "" {
		t.Fatal("rejected record must not be stamped applied")
	}
	if stored["mt-1"].AppliedAt == "" {
		t.Fatal("approved record must be stamped applied")
	}
}

func TestApplyApprovedNoDecisions(t *testing.T) {
	dir := t.TempDir()
	c := &Catalog{DataDir: dir}
	res := c.ApplyApproved(dir, time.Now())
	if res["applied"].(int) != 0 {
		t.Fatal("no decisions → applied 0")
	}
}
