package mcp

import (
	"testing"

	"sqlon/internal/catalog"
)

func TestDBErrCodeOf(t *testing.T) {
	cases := map[string]string{
		"ERROR: relation \"x\" does not exist (SQLSTATE 42P01)": "PG-42P01",
		"Error 1146 (42S02): Table 'db.x' doesn't exist":        "MY-1146",
		"context deadline exceeded: query TIMEOUT":              "TIMEOUT",
		"driver: bad connection":                                "",
	}
	for in, want := range cases {
		if got := dbErrCodeOf(in); got != want {
			t.Errorf("dbErrCodeOf(%q)=%q want %q", in, got, want)
		}
	}
}

func TestReferencedTableNamesDedup(t *testing.T) {
	v := catalog.ValidationResult{ReferencedTables: []catalog.TableRef{
		{Table: "s.a"}, {Table: "S.A"}, {Table: "s.b"}, {Table: ""},
	}}
	got := referencedTableNames(v)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique tables, got %v", got)
	}
}

func TestRepairKitValidationPhase(t *testing.T) {
	s := &Server{}
	s.setCatalog(&catalog.Catalog{Tables: map[string]*catalog.Table{}})
	v := catalog.ValidationResult{
		Valid:    false,
		Errors:   []catalog.ValidationIssue{{Level: "error", Code: "NO_TABLE", Message: "x"}},
		FixHints: []catalog.FixHint{{Code: "NO_TABLE", Suggestion: "use catalog names"}},
	}
	kit := s.repairKit(s.cat(), "validation", "", v)
	if kit["phase"] != "validation" {
		t.Fatal("phase")
	}
	if kit["fix_hints"] == nil || kit["errors"] == nil {
		t.Fatalf("validation kit must carry errors + fix_hints: %+v", kit)
	}
}

func TestRepairKitExecutionPhaseCarriesCode(t *testing.T) {
	s := &Server{}
	s.setCatalog(&catalog.Catalog{Tables: map[string]*catalog.Table{}})
	kit := s.repairKit(s.cat(), "execution", "ERROR: column \"foo\" does not exist (SQLSTATE 42703)", catalog.ValidationResult{})
	if kit["error_code"] != "PG-42703" {
		t.Fatalf("expected PG-42703, got %v", kit["error_code"])
	}
	if kit["hint"] == nil {
		t.Fatal("execution kit should carry a hint for a known code")
	}
}
