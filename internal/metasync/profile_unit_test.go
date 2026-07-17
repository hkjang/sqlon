package metasync

import "testing"

func TestSensitiveClassification(t *testing.T) {
	tb := TableAsset{Schema: "s", Name: "users"}
	cases := []struct {
		col  string
		want bool
	}{
		{"email", true}, {"user_email", true}, {"phone_no", true}, {"ssn", true},
		{"password_hash", true}, {"token", true}, {"birth_dt", true}, {"주민번호", true},
		{"ip_addr", true}, {"home_address", true},
		{"status_cd", false}, {"amount", false}, {"order_id", false}, {"created_at", false},
	}
	extra := parsePIISpecs([]string{"*.secret_col", "s.users.legacy_id"})
	for _, c := range cases {
		got := isSensitive(tb, ColumnAsset{Name: c.col}, extra)
		if got != c.want {
			t.Errorf("isSensitive(%q) = %v, want %v", c.col, got, c.want)
		}
	}
	// explicit config additions
	if !isSensitive(tb, ColumnAsset{Name: "secret_col"}, extra) {
		t.Error("*.secret_col must be sensitive via config")
	}
	if !isSensitive(tb, ColumnAsset{Name: "legacy_id"}, extra) {
		t.Error("s.users.legacy_id must be sensitive via config")
	}
	if isSensitive(TableAsset{Schema: "s", Name: "other"}, ColumnAsset{Name: "legacy_id"}, extra) {
		t.Error("legacy_id on a different table must NOT be sensitive (fully-qualified spec)")
	}
}

func TestCharClassPattern(t *testing.T) {
	cases := map[string]string{
		"20250131":      "9{8}",
		"ABC":           "A{3}",
		"A1B2":          "A9A9",
		"2025-01-31":    "9{4}.9{2}.9{2}",
		"user@host.com": "A{4}.A{4}.A{3}",
	}
	for in, want := range cases {
		if got := charClassPattern(in); got != want {
			t.Errorf("charClassPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoter(t *testing.T) {
	pg := newQuoter("postgres")
	if pg.ident("Order Date") != `"Order Date"` {
		t.Errorf("pg ident: %s", pg.ident("Order Date"))
	}
	if pg.sample("s", "t", 100) != `(SELECT * FROM "s"."t" LIMIT 100) "sqlon_sample"` {
		t.Errorf("pg sample: %s", pg.sample("s", "t", 100))
	}
	my := newQuoter("mysql")
	if my.ident("key") != "`key`" {
		t.Errorf("mysql ident: %s", my.ident("key"))
	}
	if my.sample("s", "t", 0) != "`s`.`t`" {
		t.Errorf("mysql no-limit sample: %s", my.sample("s", "t", 0))
	}
}

func TestOrderableAndStringType(t *testing.T) {
	if !isOrderable("integer") || !isOrderable("timestamp without time zone") || !isOrderable("numeric") {
		t.Error("numeric/date types must be orderable")
	}
	if isOrderable("json") || isOrderable("bytea") {
		t.Error("json/bytea must not be orderable")
	}
	if !isStringType("character varying") || !isStringType("varchar") || !isStringType("text") {
		t.Error("char types must be string types")
	}
	if isStringType("integer") {
		t.Error("integer is not a string type")
	}
}
