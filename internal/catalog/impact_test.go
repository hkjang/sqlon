package catalog

import "testing"

func TestContainsWord(t *testing.T) {
	cases := []struct {
		hay, word string
		want      bool
	}{
		{"select customer_id from t", "customer_id", true},
		{"select customer_id from t", "customer", false}, // sub-token must not match
		{"join customer c", "customer", true},
		{"from orders", "order", false},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := containsWord(c.hay, c.word); got != c.want {
			t.Errorf("containsWord(%q,%q)=%v want %v", c.hay, c.word, got, c.want)
		}
	}
}

func TestImpactLevel(t *testing.T) {
	if impactLevel(map[string]int{"metric": 1}) != "high" {
		t.Fatal("metric dep → high")
	}
	if impactLevel(map[string]int{"relation": 1}) != "medium" {
		t.Fatal("relation dep → medium")
	}
	if impactLevel(map[string]int{"override": 1}) != "low" {
		t.Fatal("override-only → low")
	}
	if impactLevel(map[string]int{}) != "none" {
		t.Fatal("no deps → none")
	}
}

func TestAnalyzeImpactUnknownTable(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.AnalyzeImpact("public.does_not_exist", "")
	if res["error"] == nil {
		t.Fatal("expected error for unknown table")
	}
}

func TestAnalyzeImpactStructure(t *testing.T) {
	c := loadTestCatalog(t)
	// pick any real table
	var any string
	for fqn := range c.Tables {
		any = fqn
		break
	}
	res := c.AnalyzeImpact(any, "")
	if res["target"] != any {
		t.Fatalf("target mismatch: %v", res["target"])
	}
	if _, ok := res["dependents"].([]ImpactRef); !ok {
		t.Fatalf("dependents not a []ImpactRef: %T", res["dependents"])
	}
	if _, ok := res["impact_level"].(string); !ok {
		t.Fatal("impact_level missing")
	}
}

func TestAnalyzeImpactFindsRelation(t *testing.T) {
	// catalog stores identifiers uppercased (cleanIdent), so keys/FQNs match.
	base := &Table{Schema: "S", Name: "ORDERS", FQN: "S.ORDERS",
		ColumnMap: map[string]*Column{"CUSTOMER_ID": {Name: "CUSTOMER_ID"}},
		Columns:   []*Column{{Name: "CUSTOMER_ID"}}}
	ref := &Table{Schema: "S", Name: "CUSTOMER", FQN: "S.CUSTOMER",
		ColumnMap: map[string]*Column{"CUSTOMER_ID": {Name: "CUSTOMER_ID", IsPK: true}}}
	c := &Catalog{
		Tables:    map[string]*Table{"S.ORDERS": base, "S.CUSTOMER": ref},
		Relations: []Relation{{BaseSchema: "S", BaseTable: "ORDERS", BaseColumn: "CUSTOMER_ID", ReferenceSchema: "S", ReferenceTable: "CUSTOMER", ReferenceColumn: "CUSTOMER_ID"}},
	}
	res := c.AnalyzeImpact("S.CUSTOMER", "CUSTOMER_ID")
	byKind, _ := res["count_bykind"].(map[string]int)
	if byKind["relation"] < 1 {
		t.Fatalf("expected a relation dependent, got %+v (res=%v)", byKind, res["error"])
	}
}
