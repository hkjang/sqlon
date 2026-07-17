package catalog

import "testing"

func TestSuggestCodeDictFromTopValues(t *testing.T) {
	c := &Catalog{Tables: map[string]*Table{}}
	col := &Column{Name: "STATUS_CD", SemanticType: "CODE", Stats: &ColumnStatData{
		DistinctCount: 3,
		TopValues:     []TopValue{{Value: "A"}, {Value: "B"}, {Value: "C"}},
	}}
	tb := &Table{Schema: "s", Name: "acct", FQN: "s.acct", Columns: []*Column{col}}
	cand, ok := c.suggestCodeDict(tb, col)
	if !ok {
		t.Fatal("expected a code_dict candidate")
	}
	if cand.Kind != "code_dict" || cand.Confidence < 0.7 {
		t.Fatalf("bad candidate: %+v", cand)
	}
	entries, _ := cand.Suggested["entries"].([]map[string]any)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	// a column that already has a dictionary is skipped
	col.CodeDict = "existing"
	if _, ok := c.suggestCodeDict(tb, col); ok {
		t.Fatal("should skip a column that already has a code_dict")
	}
	// PII columns never get a value list
	col.CodeDict, col.PII = "", true
	if _, ok := c.suggestCodeDict(tb, col); ok {
		t.Fatal("PII column must not get a code_dict candidate")
	}
}

func TestSuggestMetricAggregation(t *testing.T) {
	c := &Catalog{Tables: map[string]*Table{}}
	tb := &Table{Schema: "s", Name: "sales", FQN: "s.sales"}
	cases := map[string]string{"TOT_AMT": "SUM", "DLQ_RT": "AVG", "CREDIT_SCORE": "AVG", "TX_CNT": "SUM"}
	for name, wantAgg := range cases {
		col := &Column{Name: name}
		cand, ok := c.suggestMetric(tb, col, map[string]bool{})
		if !ok {
			t.Fatalf("%s: expected metric candidate", name)
		}
		if cand.Suggested["aggregation"] != wantAgg {
			t.Fatalf("%s: agg=%v want %v", name, cand.Suggested["aggregation"], wantAgg)
		}
	}
	// a non-measure column yields nothing
	if _, ok := c.suggestMetric(tb, &Column{Name: "CUST_NM"}, map[string]bool{}); ok {
		t.Fatal("name column should not become a metric")
	}
	// an already-covered column is skipped
	if _, ok := c.suggestMetric(tb, &Column{Name: "TOT_AMT"}, map[string]bool{"tot_amt": true}); ok {
		t.Fatal("covered column should be skipped")
	}
}

func TestSuggestRelationByNaming(t *testing.T) {
	cust := &Table{Schema: "s", Name: "customer", FQN: "s.customer",
		PrimaryKeys: []string{"customer_id"},
		ColumnMap:   map[string]*Column{"customer_id": {Name: "customer_id", DataType: "bigint", IsPK: true}}}
	order := &Table{Schema: "s", Name: "orders", FQN: "s.orders",
		Columns: []*Column{{Name: "customer_id", DataType: "bigint"}}}
	c := &Catalog{Tables: map[string]*Table{"s.customer": cust, "s.orders": order}}

	pkIndex := c.pkColumnIndex()
	nameIndex := c.tableStemIndex()
	existing := c.relationCoverage()

	cand, ok := c.suggestRelation(order, order.Columns[0], existing, pkIndex, nameIndex)
	if !ok {
		t.Fatal("expected a relation candidate for customer_id → customer")
	}
	if cand.Target != "s.customer" || cand.Suggested["reference_column"] != "customer_id" {
		t.Fatalf("bad relation: %+v", cand.Suggested)
	}
	if cand.Confidence < 0.75 {
		t.Fatalf("PK-name match should be high confidence, got %v", cand.Confidence)
	}
	// once a relation already exists, do not re-propose it
	c.Relations = []Relation{{BaseSchema: "s", BaseTable: "orders", BaseColumn: "customer_id",
		ReferenceSchema: "s", ReferenceTable: "customer", ReferenceColumn: "customer_id"}}
	existing = c.relationCoverage()
	if _, ok := c.suggestRelation(order, order.Columns[0], existing, pkIndex, nameIndex); ok {
		t.Fatal("existing relation must not be re-proposed")
	}
}

func TestTypeFamilyCompatible(t *testing.T) {
	if !typeFamilyCompatible("bigint", "integer") {
		t.Fatal("bigint/integer should be compatible")
	}
	if typeFamilyCompatible("varchar(20)", "bigint") {
		t.Fatal("varchar/bigint should be incompatible")
	}
}

func TestSuggestModelCandidatesOnMetadb(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.SuggestModelCandidates(nil, nil)
	cands, _ := res["candidates"].([]ModelCandidate)
	for _, cand := range cands {
		if cand.Generator != "rule" || cand.Status != "suggested" {
			t.Fatalf("candidate missing provenance: %+v", cand)
		}
		if cand.Kind == "" || cand.Suggested == nil {
			t.Fatalf("empty candidate: %+v", cand)
		}
	}
	if res["generator"] != "rule" {
		t.Fatal("generator must be rule")
	}
}
