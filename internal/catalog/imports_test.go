package catalog

import (
	"testing"
	"time"
)

func importTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	dir := t.TempDir()
	col := &Column{Name: "CUST_NO"}                          // no logical name/description/pii → all gaps
	filled := &Column{Name: "STATUS", LogicalName: "상태(수기)"} // curated logical name present
	tb := &Table{
		Schema: "PUBLIC", Name: "CUSTOMER", FQN: "PUBLIC.CUSTOMER",
		Columns:   []*Column{col, filled},
		ColumnMap: map[string]*Column{"CUST_NO": col, "STATUS": filled},
	}
	c := &Catalog{
		DataDir: dir,
		Tables:  map[string]*Table{"PUBLIC.CUSTOMER": tb},
		ByName:  map[string][]*Table{"CUSTOMER": {tb}},
	}
	return c
}

func TestImportExternalPreviewOnlyGaps(t *testing.T) {
	c := importTestCatalog(t)
	imp := ExternalImport{
		Source: "openmetadata",
		Columns: []ExternalColumnMeta{
			{Table: "public.customer", Column: "cust_no", LogicalName: "고객번호", Description: "고객 식별자", PII: true},
			{Table: "public.customer", Column: "status", LogicalName: "상태(OM)"}, // sqlon already has one → skip
			{Table: "public.unknown", Column: "x", LogicalName: "y"},            // unknown table → skipped
		},
		Glossary: []ExternalGlossaryTerm{{Term: "고객", Synonyms: []string{"customer"}}},
	}
	res := c.ImportExternalMetadata(imp, false, time.Now())
	if res["applied"].(bool) {
		t.Fatal("preview must not apply")
	}
	cols := res["column_candidates"].([]importColEntry)
	if len(cols) != 1 || cols[0].Column != "CUST_NO" {
		t.Fatalf("only the gap column should be proposed: %+v", cols)
	}
	e := cols[0]
	if e.LogicalName != "고객번호" || e.Description != "고객 식별자" || e.PII == nil || !*e.PII || e.SemanticType != "PII" {
		t.Fatalf("gap column candidate wrong: %+v", e)
	}
	counts := res["counts"].(map[string]int)
	if counts["glossary"] != 1 {
		t.Fatalf("glossary count wrong: %+v", counts)
	}
	skipped := res["skipped_tables"].([]string)
	if len(skipped) != 1 || skipped[0] != "PUBLIC.UNKNOWN" {
		t.Fatalf("unknown table should be reported skipped: %+v", skipped)
	}
}

func TestImportExternalApplyWritesAndProtects(t *testing.T) {
	c := importTestCatalog(t)
	// pre-existing overrides curation that must be protected
	writeJSON(t, c.DataDir, "overrides.json", map[string]any{
		"columns": []any{map[string]any{"table": "PUBLIC.CUSTOMER", "column": "CUST_NO", "description": "수기 설명"}},
	})
	imp := ExternalImport{
		Source: "openmetadata",
		Columns: []ExternalColumnMeta{
			{Table: "public.customer", Column: "cust_no", LogicalName: "고객번호", Description: "OM 설명", PII: true},
		},
		Glossary: []ExternalGlossaryTerm{{Term: "고객", Synonyms: []string{"customer"}}},
	}
	res := c.ImportExternalMetadata(imp, true, time.Now())
	if !res["applied"].(bool) {
		t.Fatalf("apply failed: %v", res["error"])
	}

	var ov map[string]any
	readJSONT(t, c.DataDir, "overrides.json", &ov)
	entry := ov["columns"].([]any)[0].(map[string]any)
	// curated description kept, logical name + pii added
	if entry["description"] != "수기 설명" {
		t.Fatalf("curated description must be protected: %v", entry["description"])
	}
	if entry["logical_name"] != "고객번호" {
		t.Fatalf("logical name not merged: %v", entry)
	}
	if entry["pii"] != true {
		t.Fatalf("pii not merged: %v", entry)
	}

	var gl Glossary
	readJSONT(t, c.DataDir, "glossary.json", &gl)
	if len(gl.Entries) != 1 || gl.Entries[0].Term != "고객" {
		t.Fatalf("glossary not written: %+v", gl.Entries)
	}

	// re-apply is a no-op (all present now)
	res2 := c.ImportExternalMetadata(imp, true, time.Now())
	w2, _ := res2["written"].(map[string]int)
	if w2["overrides.json"] != 0 && w2["glossary.json"] != 0 {
		t.Fatalf("re-apply should write nothing: %+v", w2)
	}
}

func TestDiffExternalMetadataClassifies(t *testing.T) {
	c := importTestCatalog(t) // CUST_NO empty; STATUS has logical name "상태(수기)"
	imp := ExternalImport{
		Source: "openmetadata",
		Columns: []ExternalColumnMeta{
			{Table: "public.customer", Column: "cust_no", LogicalName: "고객번호", PII: true}, // sqlon empty → sqlon_gap (name + pii)
			{Table: "public.customer", Column: "status", LogicalName: "상태(OM)"},           // both differ → conflict
		},
	}
	res := c.DiffExternalMetadata(imp)
	gaps := res["sqlon_gaps"].([]driftItem)
	conflicts := res["conflicts"].([]driftItem)

	var nameGap, piiGap bool
	for _, g := range gaps {
		if g.Column == "CUST_NO" && g.Field == "logical_name" && g.ExtValue == "고객번호" {
			nameGap = true
		}
		if g.Column == "CUST_NO" && g.Field == "pii" && g.ExtValue == "true" {
			piiGap = true
		}
	}
	if !nameGap || !piiGap {
		t.Fatalf("expected logical_name + pii gaps for CUST_NO: %+v", gaps)
	}
	var statusConflict bool
	for _, cf := range conflicts {
		if cf.Column == "STATUS" && cf.Field == "logical_name" && cf.SqlonValue == "상태(수기)" && cf.ExtValue == "상태(OM)" {
			statusConflict = true
		}
	}
	if !statusConflict {
		t.Fatalf("expected a STATUS logical_name conflict: %+v", conflicts)
	}
	counts := res["counts"].(map[string]int)
	if counts["conflicts"] < 1 || counts["sqlon_gaps"] < 2 {
		t.Fatalf("counts wrong: %+v", counts)
	}
}

func TestStageExternalImportFeedsReviewQueue(t *testing.T) {
	c := importTestCatalog(t) // CUST_NO empty; STATUS has a logical name
	imp := ExternalImport{
		Source: "openmetadata",
		Columns: []ExternalColumnMeta{
			{Table: "public.customer", Column: "cust_no", LogicalName: "고객번호", Description: "고객 식별자", PII: true},
			{Table: "public.customer", Column: "status", LogicalName: "상태(OM)"}, // sqlon already set → skip
		},
	}
	res := c.StageExternalImport(imp)
	if res["staged"].(int) != 2 { // logical_name + description for CUST_NO
		t.Fatalf("expected 2 staged (ln+desc for the gap column), got %v", res["staged"])
	}

	// staged candidates must appear in the review queue as pending, source=openmetadata
	rq := c.ReviewCandidates(nil, nil, "pending")
	found := 0
	for _, it := range c.collectCandidates(nil, nil) {
		if it.Source == "openmetadata" && it.Table == "PUBLIC.CUSTOMER" && it.Column == "CUST_NO" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expected 2 openmetadata candidates in queue, got %d", found)
	}
	if rq["error"] != nil {
		t.Fatalf("review queue error: %v", rq["error"])
	}

	// idempotent: staging again adds nothing
	res2 := c.StageExternalImport(imp)
	if res2["staged"].(int) != 0 {
		t.Fatalf("re-stage should be a no-op, got %v", res2["staged"])
	}
}
