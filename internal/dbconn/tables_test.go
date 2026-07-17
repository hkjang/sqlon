package dbconn

import (
	"sort"
	"strings"
	"testing"
)

func refsToStrings(refs []TableRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.String())
	}
	sort.Strings(out)
	return out
}

func TestExtractTables(t *testing.T) {
	cases := []struct {
		dialect, sql string
		want         []string
	}{
		{"postgres", "SELECT * FROM sales.orders o JOIN sales.customers c ON o.cid = c.id",
			[]string{"sales.customers", "sales.orders"}},
		{"postgres", "SELECT count(*) FROM public.users WHERE active",
			[]string{"public.users"}},
		{"postgres", "WITH recent AS (SELECT * FROM hr.employees) SELECT * FROM recent r JOIN hr.depts d ON r.dept = d.id",
			[]string{"hr.depts", "hr.employees"}}, // CTE name 'recent' excluded
		{"postgres", "SELECT * FROM t1, t2 WHERE t1.a = t2.b",
			[]string{"t1", "t2"}},
		{"mysql", "SELECT * FROM shop.products p JOIN shop.orders o ON p.id = o.pid",
			[]string{"shop.orders", "shop.products"}},
		{"mysql", "WITH x AS (SELECT id FROM app.users) SELECT * FROM x JOIN app.logs l ON x.id = l.uid",
			[]string{"app.logs", "app.users"}}, // CTE 'x' excluded
		{"mariadb", "SELECT COUNT(*) FROM analytics.events",
			[]string{"analytics.events"}},
	}
	for _, c := range cases {
		refs, err := ExtractTables(c.dialect, c.sql)
		if err != nil {
			t.Fatalf("%s: %q: %v", c.dialect, c.sql, err)
		}
		got := refsToStrings(refs)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %q → %v, want %v", c.dialect, c.sql, got, c.want)
		}
	}
}

func TestExtractTablesParseError(t *testing.T) {
	if _, err := ExtractTables("postgres", "SELECT FROM WHERE (("); err == nil {
		t.Fatal("expected parse error")
	}
	if _, err := ExtractTables("oracle", "SELECT 1"); err == nil {
		t.Fatal("unknown dialect must error")
	}
}
