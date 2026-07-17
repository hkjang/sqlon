package catalog

import (
	"strings"
	"testing"
)

func rewritesByRule(rs []RewriteSuggestion) map[string]RewriteSuggestion {
	m := map[string]RewriteSuggestion{}
	for _, r := range rs {
		m[r.Rule] = r
	}
	return m
}

func TestSuggestRewritesExpandsSelectStarFromCatalog(t *testing.T) {
	c := lintTestCatalog()
	got := rewritesByRule(c.SuggestRewrites("SELECT * FROM S.USERS"))
	star, ok := got["select_star"]
	if !ok {
		t.Fatalf("select_star rewrite missing: %+v", got)
	}
	if !star.AutoApplicable {
		t.Fatalf("single-table SELECT * expansion should be auto-applicable: %+v", star)
	}
	// Every real column, no star, correct table.
	for _, col := range []string{"ID", "EMAIL", "STATUS", "CREATED"} {
		if !strings.Contains(star.After, col) {
			t.Fatalf("expanded SQL missing column %s: %q", col, star.After)
		}
	}
	if strings.Contains(star.After, "*") || !strings.Contains(star.After, "FROM S.USERS") {
		t.Fatalf("bad expansion: %q", star.After)
	}
}

func TestSuggestRewritesSelectStarAmbiguousStaysTemplate(t *testing.T) {
	c := lintTestCatalog()
	// A self-join (one FQN but two references) is ambiguous → no auto-expand.
	got := rewritesByRule(c.SuggestRewrites("SELECT * FROM S.USERS a JOIN S.USERS b ON a.id = b.id"))
	star, ok := got["select_star"]
	if !ok {
		t.Fatalf("select_star rewrite missing")
	}
	if star.AutoApplicable {
		t.Fatalf("multi-table SELECT * must not be auto-expanded: %+v", star)
	}
}

func TestSuggestRewritesSemanticTransformsAreTemplatesNotAuto(t *testing.T) {
	c := lintTestCatalog()
	got := rewritesByRule(c.SuggestRewrites("SELECT id FROM S.USERS WHERE id NOT IN (SELECT uid FROM S.USERS)"))
	ni, ok := got["not_in_subquery"]
	if !ok {
		t.Fatalf("not_in_subquery rewrite missing: %+v", got)
	}
	if ni.AutoApplicable {
		t.Fatalf("semantic transform must require human review, not auto: %+v", ni)
	}
	if ni.Before == "" || ni.After == "" || !strings.Contains(ni.After, "NOT EXISTS") {
		t.Fatalf("expected NOT EXISTS template: %+v", ni)
	}
}

func TestSuggestRewritesOnlyForRulesWithTemplates(t *testing.T) {
	c := lintTestCatalog()
	// A clean query yields no rewrite suggestions.
	if got := c.SuggestRewrites("SELECT id FROM S.USERS WHERE id = 1"); len(got) != 0 {
		t.Fatalf("clean query should have no rewrites: %+v", got)
	}
}
