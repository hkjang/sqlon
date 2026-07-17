package mcp

import (
	"context"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		dialect, in, want string
		wantErr           bool
	}{
		{"postgres", "app_user", `"app_user"`, false},
		{"postgres", `we"ird`, `"we""ird"`, false}, // embedded double-quote doubled
		{"mysql", "app_user", "`app_user`", false},
		{"mariadb", "ba`d", "`ba``d`", false}, // embedded backtick doubled
		{"postgres", "", "", true},
		{"postgres", "bad\nname", "", true}, // newline rejected
		{"postgres", "nul\x00", "", true},   // NUL rejected
	}
	for _, c := range cases {
		got, err := quoteIdent(c.dialect, c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("quoteIdent(%q,%q) expected error", c.dialect, c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("quoteIdent(%q,%q) unexpected error: %v", c.dialect, c.in, err)
		}
		if got != c.want {
			t.Errorf("quoteIdent(%q,%q)=%q want %q", c.dialect, c.in, got, c.want)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	// a password containing a quote must be escaped so it can't break out
	if got := quoteLiteral("p'; DROP ROLE x; --"); got != `'p''; DROP ROLE x; --'` {
		t.Fatalf("quoteLiteral escaping wrong: %s", got)
	}
	if got := quoteLiteral("simple"); got != "'simple'" {
		t.Fatalf("quoteLiteral(simple)=%s", got)
	}
}

func TestBoolWord(t *testing.T) {
	if boolWord(true, "LOGIN", "NOLOGIN") != "LOGIN" || boolWord(false, "LOGIN", "NOLOGIN") != "NOLOGIN" {
		t.Fatal("boolWord mapping wrong")
	}
}

func TestLegacyDirectDBAExecutionIsDisabled(t *testing.T) {
	s, _ := newFixtureServer(t)
	result := s.dbaExec(context.Background(), "pg", "execute", "CREATE TABLE x(id int)", "CREATE TABLE x(id int)")
	if result["status"] != "error" {
		t.Fatalf("direct DBA execution must be blocked: %#v", result)
	}
}
