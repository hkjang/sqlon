package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormQuestionAndSQL(t *testing.T) {
	if normQuestion("  활성   사용자\t수를\n알려줘 ") != "활성 사용자 수를 알려줘" {
		t.Fatalf("question normalization: %q", normQuestion("  활성   사용자\t수를\n알려줘 "))
	}
	if normSQL("SELECT  *\nFROM T ;") != "select * from t" {
		t.Fatalf("sql normalization: %q", normSQL("SELECT  *\nFROM T ;"))
	}
}

// writeFeedback writes one JSONL feedback record into <dir>/feedback.
func writeFeedback(t *testing.T, dir string, rec FeedbackRecord) {
	t.Helper()
	fdir := filepath.Join(dir, "feedback")
	if err := os.MkdirAll(fdir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(fdir, "fb.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := json.Marshal(rec)
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func approvedSuccessRec(id, q, sql string) FeedbackRecord {
	yes := true
	return FeedbackRecord{
		SchemaVersion: FeedbackSchemaVersion, ID: id, Question: q, FinalSQL: sql,
		Outcome: "success", Executed: &yes, RecordedAt: "2026-07-12T00:00:00Z",
		Fingerprint: "fp-" + id, TrustStatus: FeedbackTrustTrusted, ReviewStatus: FeedbackReviewApproved,
		Tables: []string{"public.t"},
	}
}

func TestSuggestGoldenFromFeedbackFiltersAndDedups(t *testing.T) {
	dir := t.TempDir()
	c := &Catalog{DataDir: dir} // FeedbackDatasetID = base dir name; records must match
	dsid := c.FeedbackDatasetID()

	ok := approvedSuccessRec("f1", "활성 사용자 수를 알려줘", "SELECT count(*) FROM public.t")
	ok.DatasetID = dsid
	writeFeedback(t, dir, ok)

	// pending (not approved) → excluded
	pend := approvedSuccessRec("f2", "다른 질문", "SELECT 1 FROM public.t")
	pend.DatasetID = dsid
	pend.ReviewStatus = FeedbackReviewPending
	writeFeedback(t, dir, pend)

	// failure outcome → excluded
	fail := approvedSuccessRec("f3", "실패 질문", "SELECT 2 FROM public.t")
	fail.DatasetID = dsid
	fail.Outcome = "failure"
	writeFeedback(t, dir, fail)

	res := c.SuggestGoldenFromFeedback(50)
	cands, _ := res["candidates"].([]GoldenCandidate)
	if len(cands) != 1 || cands[0].FeedbackID != "f1" {
		t.Fatalf("expected only f1, got %+v", cands)
	}

	// now seed golden set with the same question → next suggest must dedup it out
	writeJSON(t, dir, "golden_queries.json", []any{
		map[string]any{"id": 1, "question": "활성 사용자 수를 알려줘", "expected_sql": "SELECT count(*) FROM public.t"},
	})
	res2 := c.SuggestGoldenFromFeedback(50)
	if res2["count"].(int) != 0 {
		t.Fatalf("duplicate should be deduped, got %v", res2["count"])
	}
}

func TestPromoteGoldenAppendsAndDedups(t *testing.T) {
	dir := t.TempDir()
	c := &Catalog{DataDir: dir}
	dsid := c.FeedbackDatasetID()
	rec := approvedSuccessRec("f1", "월별 매출", "SELECT date_trunc('month',dt), SUM(amt) FROM public.t GROUP BY 1")
	rec.DatasetID = dsid
	writeFeedback(t, dir, rec)

	res := c.PromoteGolden([]string{"f1"}, time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC))
	if res["applied"].(int) != 1 {
		t.Fatalf("expected 1 applied, got %v (%v)", res["applied"], res["error"])
	}
	var gq []map[string]any
	readJSONT(t, dir, "golden_queries.json", &gq)
	if len(gq) != 1 || gq[0]["question"] != "월별 매출" {
		t.Fatalf("golden not written: %+v", gq)
	}

	// promoting again is a no-op (now present in golden set)
	res2 := c.PromoteGolden([]string{"f1"}, time.Now())
	if res2["applied"].(int) != 0 {
		t.Fatalf("re-promote must dedup, got %v", res2["applied"])
	}

	// unknown id reported
	res3 := c.PromoteGolden([]string{"nope"}, time.Now())
	if res3["applied"].(int) != 0 || res3["unknown_ids"] == nil {
		t.Fatalf("unknown id must be reported: %+v", res3)
	}
}
