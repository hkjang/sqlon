package catalog

import (
	"testing"
	"time"
)

func TestCandidateIDStableAndContentAddressed(t *testing.T) {
	a := candidateID("semantic_type", "S.T", "C", "CODE")
	b := candidateID("semantic_type", "s.t", "c", "CODE") // case-insensitive target
	if a != b {
		t.Fatalf("id should be case-insensitive on target: %s vs %s", a, b)
	}
	c := candidateID("semantic_type", "S.T", "C", "AMOUNT") // changed suggestion
	if a == c {
		t.Fatal("changed suggestion must produce a different id")
	}
	if a[:2] != "st" {
		t.Fatalf("id should be prefixed by kind short code, got %s", a)
	}
}

func TestReviewDecideRoundTrip(t *testing.T) {
	c := loadTestCatalog(t)
	c.DataDir = t.TempDir() // decisions persist under a throwaway dir

	// list current candidates; must have at least one to decide on
	cands := c.collectCandidates(nil, nil)
	if len(cands) == 0 {
		t.Skip("no candidates in metadb to review")
	}
	target := cands[0]

	// approve the first candidate
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	res := c.DecideCandidates([]DecideCandidate{{ID: target.ID, Decision: "approve", Notes: "ok"}}, "tester", now)
	if res["applied"].(int) != 1 {
		t.Fatalf("expected 1 applied, got %v (%v)", res["applied"], res["unknown_ids"])
	}

	// it should now be reflected in the approved status filter
	approved := c.ReviewCandidates(nil, nil, "approved")
	summary, _ := approved["summary"].(map[string]int)
	if summary["approved"] < 1 {
		t.Fatalf("expected an approved item, summary=%v", summary)
	}

	// and it should appear in the compiled overrides for its kind
	ov := c.ApprovedOverrides()
	counts, _ := ov["counts"].(map[string]int)
	total := counts["overrides_columns"] + counts["metrics"] + counts["relations"] + counts["code_dicts"]
	if total < 1 {
		t.Fatalf("approved candidate should compile into a fragment, counts=%v", counts)
	}

	// an unknown id is reported, not applied
	res2 := c.DecideCandidates([]DecideCandidate{{ID: "zz-deadbeef", Decision: "approved"}}, "tester", now)
	if res2["applied"].(int) != 0 {
		t.Fatalf("unknown id must not apply, got %v", res2["applied"])
	}
	if res2["unknown_ids"] == nil {
		t.Fatal("unknown id should be reported")
	}

	// bad decision verb is rejected
	res3 := c.DecideCandidates([]DecideCandidate{{ID: target.ID, Decision: "maybe"}}, "tester", now)
	if res3["applied"].(int) != 0 {
		t.Fatal("bad decision verb must not apply")
	}
}

func TestApprovedOverridesEmptyWhenNoDecisions(t *testing.T) {
	c := loadTestCatalog(t)
	c.DataDir = t.TempDir()
	ov := c.ApprovedOverrides()
	counts, _ := ov["counts"].(map[string]int)
	if counts["overrides_columns"]+counts["metrics"]+counts["relations"]+counts["code_dicts"] != 0 {
		t.Fatal("no decisions → empty overrides")
	}
}
