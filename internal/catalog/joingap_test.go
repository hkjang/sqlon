package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSuggestJoinRelations(t *testing.T) {
	c := loadTestCatalog(t)

	// the checked-in golden set must be fully covered by the join graph
	clean, err := c.SuggestJoinRelations("")
	if err != nil {
		t.Fatal(err)
	}
	if n := clean["gap_count"].(int); n != 0 {
		t.Fatalf("metadb golden set should have no join gaps, got %d: %+v", n, clean["gaps"])
	}

	// jamypg_settings has no relation in the fixture graph; a golden case that
	// pairs it with jamypg_users must surface as a gap with a paste-ready
	// suggestion on the shared UPDATED_AT column.
	golden := `[{"question":"설정을 마지막으로 수정한 사용자 목록",
	 "expected_tables":["PUBLIC.JAMYPG_SETTINGS","PUBLIC.JAMYPG_USERS"]}]`
	path := filepath.Join(t.TempDir(), "golden.json")
	if err := os.WriteFile(path, []byte(golden), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := c.SuggestJoinRelations(path)
	if err != nil {
		t.Fatal(err)
	}
	gaps := out["gaps"].([]JoinGap)
	if len(gaps) == 0 {
		t.Fatal("expected a join gap for the unlinked settings table")
	}
	for i, g := range gaps {
		if g.From == "" || g.To == "" || len(g.Questions) == 0 {
			t.Fatalf("gap %d malformed: %+v", i, g)
		}
	}
	found := false
	for _, g := range gaps {
		if g.From != "PUBLIC.JAMYPG_SETTINGS" && g.To != "PUBLIC.JAMYPG_SETTINGS" {
			continue
		}
		found = true
		hasSharedKey := false
		for _, cand := range g.Candidates {
			if cand == "UPDATED_AT" {
				hasSharedKey = true
			}
		}
		if !hasSharedKey {
			t.Fatalf("expected shared column UPDATED_AT as candidate join key, got %+v", g.Candidates)
		}
		if g.Suggested == nil {
			t.Fatalf("gap %s->%s has candidates but no suggested_relation", g.From, g.To)
		}
	}
	if !found {
		t.Fatalf("expected a gap touching PUBLIC.JAMYPG_SETTINGS, got %+v", gaps)
	}
	// every gap with shared columns must carry a paste-ready suggestion
	for _, g := range gaps {
		if len(g.Candidates) > 0 && g.Suggested == nil {
			t.Fatalf("gap %s->%s has candidates but no suggested_relation", g.From, g.To)
		}
	}
}
