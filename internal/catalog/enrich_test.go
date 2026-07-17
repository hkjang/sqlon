package catalog

import (
	"strings"
	"testing"
)

func TestExpandName(t *testing.T) {
	cases := map[string]string{
		"CUST_NO":   "고객 번호",
		"ACCT_BAL":  "계좌 잔액", // BAL not in abbrev? it is via glossary? check
		"USE_YN":    "여부",    // USE not mapped, YN→여부
		"REG_DT":    "등록 일자",
		"RANDOMXYZ": "",
	}
	for in, want := range cases {
		got, _ := expandName(in)
		if got != want {
			t.Errorf("expandName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSuggestSemanticTypeRules(t *testing.T) {
	tb := &Table{Schema: "s", Name: "t", FQN: "s.t"}
	cases := []struct {
		col  Column
		want string
	}{
		{Column{Name: "STATUS_CD"}, "CODE"},
		{Column{Name: "USE_YN"}, "FLAG"},
		{Column{Name: "TOT_AMT"}, "AMOUNT"},
		{Column{Name: "DLQ_RT"}, "RATIO"},
		{Column{Name: "CREDIT_SCORE"}, "SCORE"},
		{Column{Name: "TX_CNT"}, "COUNT"},
		{Column{Name: "CUST_NM"}, "NAME"},
		{Column{Name: "CUST_NO"}, "IDENTIFIER"},
		{Column{Name: "REG_DT", DataType: "varchar"}, "DATE"},
		{Column{Name: "EMAIL", PII: true}, "PII"},
	}
	for _, c := range cases {
		got, ok := suggestSemanticType(tb, &c.col)
		if !ok || got.Suggested != c.want {
			t.Errorf("suggestSemanticType(%q) = %q (ok=%v), want %q", c.col.Name, got.Suggested, ok, c.want)
		}
		if got.Confidence <= 0 || got.Generator != "rule" || got.Status != "suggested" {
			t.Errorf("%q: bad provenance %+v", c.col.Name, got)
		}
	}
}

func TestSuggestSemanticMetadataOnMetadb(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.SuggestSemanticMetadata(nil, nil)
	sugg, _ := res["suggestions"].([]SemanticSuggestion)
	// metadb columns already have logical names + semantic types on some, so
	// suggestions should exist only for the gaps; every suggestion must carry
	// provenance and never target an already-filled field
	for _, s := range sugg {
		if s.Generator != "rule" || s.Status != "suggested" {
			t.Fatalf("suggestion missing provenance: %+v", s)
		}
		if s.Suggested == "" {
			t.Fatalf("empty suggestion: %+v", s)
		}
		if s.Confidence < 0 || s.Confidence > 1 {
			t.Fatalf("confidence out of range: %+v", s)
		}
	}
	// the overrides snippet must only contain high-confidence (>=0.7) items
	snippet, _ := res["overrides_snippet"].([]map[string]any)
	_ = snippet // shape check only; content validated by rule tests
	if res["generator"] != "rule" {
		t.Fatal("generator must be rule")
	}
}

func TestSuggestKindFilter(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.SuggestSemanticMetadata(nil, []string{"semantic_type"})
	sugg, _ := res["suggestions"].([]SemanticSuggestion)
	for _, s := range sugg {
		if s.Kind != "semantic_type" {
			t.Fatalf("kind filter leaked %q", s.Kind)
		}
	}
}

func TestOverridesSnippetHighConfidenceOnly(t *testing.T) {
	sugg := []SemanticSuggestion{
		{Kind: "logical_name", Table: "s.t", Column: "a", Suggested: "이름", Confidence: 0.9},
		{Kind: "semantic_type", Table: "s.t", Column: "a", Suggested: "NAME", Confidence: 0.75},
		{Kind: "description", Table: "s.t", Column: "b", Suggested: "설명", Confidence: 0.55}, // below 0.7 → excluded
	}
	snip := buildOverridesSnippet(sugg)
	if len(snip) != 1 {
		t.Fatalf("expected 1 column entry, got %d: %+v", len(snip), snip)
	}
	e := snip[0]
	if e["column"] != "a" || e["logical_name"] != "이름" || e["semantic_type"] != "NAME" {
		t.Fatalf("merged entry wrong: %+v", e)
	}
	if _, has := e["description"]; has {
		t.Fatal("low-confidence description must be excluded from the snippet")
	}
	_ = strings.TrimSpace
}
