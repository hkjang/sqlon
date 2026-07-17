package catalog

import (
	"strings"
	"testing"
)

func lintTestCatalog() *Catalog {
	users := &Table{
		Schema: "S", Name: "USERS", FQN: "S.USERS", LogicalName: "사용자",
		Columns: []*Column{
			{Name: "ID", IsPK: true},
			{Name: "EMAIL"},   // indexed via idx below
			{Name: "STATUS"},  // not indexed
			{Name: "CREATED"}, // not indexed
		},
		ColumnMap: map[string]*Column{
			"ID": {Name: "ID", IsPK: true}, "EMAIL": {Name: "EMAIL"},
			"STATUS": {Name: "STATUS"}, "CREATED": {Name: "CREATED"},
		},
		Indexes: []IndexDef{{IndexName: "idx_email", ColumnName: "EMAIL", Seq: 1}},
	}
	return &Catalog{
		Tables: map[string]*Table{"S.USERS": users},
		ByName: map[string][]*Table{"USERS": {users}},
	}
}

func rulesHit(fs []LintFinding) map[string]LintFinding {
	m := map[string]LintFinding{}
	for _, f := range fs {
		m[f.Rule] = f
	}
	return m
}

func TestLintSQL_SelectStarAndOrderByNoLimit(t *testing.T) {
	c := lintTestCatalog()
	got := rulesHit(c.LintSQL("SELECT * FROM S.USERS ORDER BY created"))
	if _, ok := got["select_star"]; !ok {
		t.Error("expected select_star")
	}
	if _, ok := got["order_by_no_limit"]; !ok {
		t.Error("expected order_by_no_limit")
	}
	// with LIMIT the order_by rule should not fire
	got = rulesHit(c.LintSQL("SELECT id FROM S.USERS ORDER BY created LIMIT 10"))
	if _, ok := got["order_by_no_limit"]; ok {
		t.Error("order_by_no_limit must not fire when LIMIT present")
	}
}

func TestLintSQL_NonSargableOnlyForIndexed(t *testing.T) {
	c := lintTestCatalog()
	// EMAIL is indexed → function wrap should flag non_sargable
	got := rulesHit(c.LintSQL("SELECT id FROM S.USERS WHERE UPPER(email) = 'X'"))
	if _, ok := got["non_sargable_function"]; !ok {
		t.Error("expected non_sargable_function on indexed EMAIL")
	}
	// STATUS is NOT indexed → no non_sargable noise
	got = rulesHit(c.LintSQL("SELECT id FROM S.USERS WHERE LOWER(status) = 'a'"))
	if _, ok := got["non_sargable_function"]; ok {
		t.Error("non_sargable_function must not fire on un-indexed STATUS")
	}
}

func TestLintSQL_LeadingWildcardAndNotIn(t *testing.T) {
	c := lintTestCatalog()
	got := rulesHit(c.LintSQL("SELECT id FROM S.USERS WHERE email LIKE '%@x.com'"))
	if _, ok := got["leading_wildcard_like"]; !ok {
		t.Error("expected leading_wildcard_like")
	}
	// a trailing wildcard must NOT trip the rule
	got = rulesHit(c.LintSQL("SELECT id FROM S.USERS WHERE email LIKE 'abc%'"))
	if _, ok := got["leading_wildcard_like"]; ok {
		t.Error("trailing wildcard must not trip leading_wildcard_like")
	}
	got = rulesHit(c.LintSQL("SELECT id FROM S.USERS WHERE id NOT IN (SELECT id FROM S.USERS)"))
	if _, ok := got["not_in_subquery"]; !ok {
		t.Error("expected not_in_subquery")
	}
}

func TestLintSQL_NoFalsePositiveInStringLiteral(t *testing.T) {
	c := lintTestCatalog()
	// the '*' and 'LIKE %' live inside a string literal → masked out
	got := rulesHit(c.LintSQL("SELECT id FROM S.USERS WHERE status = 'a * b LIKE %x'"))
	if _, ok := got["select_star"]; ok {
		t.Error("select_star must not fire on a literal containing '*'")
	}
	if _, ok := got["leading_wildcard_like"]; ok {
		t.Error("leading_wildcard_like must not fire on a literal")
	}
}

func TestExplainSQLWords(t *testing.T) {
	c := lintTestCatalog()
	r := c.ExplainSQLWords("SELECT COUNT(*) FROM S.USERS WHERE status = 'a' GROUP BY status ORDER BY status LIMIT 5")
	if r["statement"] != "SELECT" {
		t.Fatalf("statement=%v", r["statement"])
	}
	aggs, _ := r["aggregates"].([]string)
	if len(aggs) != 1 || aggs[0] != "COUNT" {
		t.Fatalf("aggregates=%v", r["aggregates"])
	}
	if r["has_filter"] != true {
		t.Error("expected has_filter true")
	}
	if r["limit"] != "5" {
		t.Errorf("limit=%v", r["limit"])
	}
	sum, _ := r["summary"].(string)
	if !strings.Contains(sum, "사용자") {
		t.Errorf("summary should use the table's logical name '사용자': %q", sum)
	}
}
