package mcp

import (
	"testing"

	"sqlon/internal/catalog"
)

func TestPIIHeuristicMatchesKoreanAndEnglish(t *testing.T) {
	cases := []struct {
		col  *catalog.Column
		want bool
	}{
		{&catalog.Column{Name: "jumin_no"}, true},
		{&catalog.Column{Name: "resident_reg_no", LogicalName: "주민등록번호"}, true},
		{&catalog.Column{Name: "email"}, true},
		{&catalog.Column{Name: "customer_email_address"}, true},
		{&catalog.Column{Name: "mobile_phone"}, true},
		{&catalog.Column{Name: "card_no"}, true},
		{&catalog.Column{Name: "account_no"}, true},
		{&catalog.Column{Name: "col1", LogicalName: "고객명"}, true},
		{&catalog.Column{Name: "birth_date"}, true},
		{&catalog.Column{Name: "order_total"}, false},
		{&catalog.Column{Name: "status"}, false},
		{&catalog.Column{Name: "x", SemanticType: "PII"}, true},
	}
	for _, c := range cases {
		got, reason := piiHeuristic(c.col)
		if got != c.want {
			t.Errorf("piiHeuristic(%q/%q)=%v (reason %q), want %v", c.col.Name, c.col.LogicalName, got, reason, c.want)
		}
	}
}

func TestPIIExposureSeparatesTaggedFromUntagged(t *testing.T) {
	cat := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"public.customer": {FQN: "public.customer", Columns: []*catalog.Column{
			{Name: "id"},
			{Name: "ssn", PII: true},                 // tagged → protected
			{Name: "email"},                          // heuristic, untagged → exposed
			{Name: "mobile_phone"},                   // heuristic, untagged → exposed
			{Name: "created_at"},                     // not PII
		}},
	}}
	rep := piiExposureReport(cat, "active")
	sum := rep["summary"].(map[string]any)
	if sum["tagged_pii"].(int) != 1 {
		t.Fatalf("expected 1 tagged pii, got %v", sum["tagged_pii"])
	}
	if sum["exposed_candidates"].(int) != 2 {
		t.Fatalf("expected 2 exposed candidates (email, mobile_phone), got %v", sum["exposed_candidates"])
	}
	if rep["status"] != "exposed_risk" {
		t.Fatalf("untagged PII candidates must set exposed_risk, got %v", rep["status"])
	}
	// the tagged column must be reported as masked
	prot := rep["protected"].([]piiColumn)
	if len(prot) != 1 || !prot[0].Masked || prot[0].Column != "ssn" {
		t.Fatalf("tagged ssn must be protected+masked: %+v", prot)
	}
}

func TestPIIExposureCleanCatalogIsOK(t *testing.T) {
	cat := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"public.orders": {FQN: "public.orders", Columns: []*catalog.Column{
			{Name: "id"}, {Name: "amount"}, {Name: "status"},
		}},
	}}
	rep := piiExposureReport(cat, "active")
	if rep["status"] != "ok" {
		t.Fatalf("no PII → status ok, got %v", rep["status"])
	}
}
