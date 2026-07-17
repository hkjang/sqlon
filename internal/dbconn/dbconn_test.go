package dbconn

import (
	"strings"
	"testing"
)

func TestDialectFor(t *testing.T) {
	cases := []struct {
		in, name, driver string
		wantErr          bool
	}{
		{"", "postgres", "pgx", false},
		{"postgres", "postgres", "pgx", false},
		{"PostgreSQL", "postgres", "pgx", false},
		{"pg", "postgres", "pgx", false},
		{"mysql", "mysql", "mysql", false},
		{"MySQL", "mysql", "mysql", false},
		{"mariadb", "mariadb", "mysql", false},
		{"oracle", "oracle", "godror", false},
		{"sqlite", "", "", true},
	}
	for _, c := range cases {
		d, err := DialectFor(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("DialectFor(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("DialectFor(%q): %v", c.in, err)
		}
		if d.Name() != c.name || d.DriverName() != c.driver {
			t.Fatalf("DialectFor(%q) = %s/%s, want %s/%s", c.in, d.Name(), d.DriverName(), c.name, c.driver)
		}
	}
}

func TestPostgresDSN(t *testing.T) {
	d, _ := DialectFor("postgres")
	p := Profile{ConnectString: "localhost:5432/appdb", Username: "ro_user"}
	dsn, err := d.BuildDSN(p, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"postgres://", "ro_user:s3cret@localhost:5432/appdb", "default_transaction_read_only=on", "connect_timeout=5"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("dsn %q missing %q", dsn, want)
		}
	}
	// URL form keeps params, injects credentials and read-only
	p.ConnectString = "postgres://h:5433/db2?sslmode=require"
	dsn, err = d.BuildDSN(p, "pw")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sslmode=require", "default_transaction_read_only=on", "ro_user:pw@h:5433"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("dsn %q missing %q", dsn, want)
		}
	}
	if _, err := d.BuildDSN(Profile{ConnectString: "not a dsn"}, "x"); err == nil {
		t.Fatal("expected error for malformed connect string")
	}
}

func TestMySQLDSN(t *testing.T) {
	my, _ := DialectFor("mysql")
	ma, _ := DialectFor("mariadb")
	p := Profile{ConnectString: "localhost:3306/appdb", Username: "ro_user"}
	dsn, err := my.BuildDSN(p, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ro_user:s3cret@tcp(localhost:3306)/appdb", "parseTime=true", "transaction_read_only=1"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("mysql dsn %q missing %q", dsn, want)
		}
	}
	dsn, err = ma.BuildDSN(p, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "tx_read_only=1") {
		t.Fatalf("mariadb dsn %q missing tx_read_only", dsn)
	}
	if strings.Contains(dsn, "transaction_read_only=1") {
		t.Fatalf("mariadb dsn %q must not use mysql read-only var", dsn)
	}
	// driver DSN passthrough keeps host/db but overrides credentials
	p.ConnectString = "olduser:oldpw@tcp(db:3307)/other"
	dsn, err = my.BuildDSN(p, "npw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "ro_user:npw@tcp(db:3307)/other") {
		t.Fatalf("mysql dsn %q must override credentials", dsn)
	}
}

func TestWrapLimit(t *testing.T) {
	pg, _ := DialectFor("postgres")
	my, _ := DialectFor("mysql")
	if got := pg.WrapLimit("SELECT * FROM t;", 101); got != "SELECT * FROM (SELECT * FROM t) AS sqlon_q LIMIT 101" {
		t.Fatalf("pg wrap: %q", got)
	}
	if got := my.WrapLimit("SELECT * FROM t", 5); got != "SELECT * FROM (SELECT * FROM t) AS sqlon_q LIMIT 5" {
		t.Fatalf("mysql wrap: %q", got)
	}
	// WITH on mysql: appended LIMIT, no derived-table wrap
	got := my.WrapLimit("WITH c AS (SELECT 1 AS x) SELECT x FROM c", 7)
	if strings.Contains(got, "sqlon_q") || !strings.HasSuffix(got, "LIMIT 7") {
		t.Fatalf("mysql WITH wrap: %q", got)
	}
	// WITH + existing trailing LIMIT stays untouched
	in := "WITH c AS (SELECT 1 AS x) SELECT x FROM c LIMIT 3"
	if got := my.WrapLimit(in, 7); got != in {
		t.Fatalf("mysql WITH+LIMIT wrap: %q", got)
	}
	// pg wraps WITH queries fine
	if got := pg.WrapLimit("WITH c AS (SELECT 1) SELECT * FROM c", 2); !strings.HasPrefix(got, "SELECT * FROM (WITH") {
		t.Fatalf("pg WITH wrap: %q", got)
	}
}

func TestCountWrap(t *testing.T) {
	pg, _ := DialectFor("postgres")
	if got := pg.CountWrap("SELECT a FROM t;"); got != "SELECT COUNT(*) FROM (SELECT a FROM t) AS sqlon_q" {
		t.Fatalf("count wrap: %q", got)
	}
}

func TestValidateReadOnlySQL(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT /* UPDATE in comment */ col FROM t WHERE name = 'DELETE'",
		"SELECT col FROM t;",
	}
	for _, q := range ok {
		if err := ValidateReadOnlySQL(q, nil); err != nil {
			t.Fatalf("expected ok for %q: %v", q, err)
		}
	}
	bad := []string{
		"",
		"UPDATE t SET a = 1",
		"SELECT 1; DROP TABLE t",
		"INSERT INTO t VALUES (1)",
		"SELECT * FROM t; SELECT * FROM u",
	}
	for _, q := range bad {
		if err := ValidateReadOnlySQL(q, nil); err == nil {
			t.Fatalf("expected error for %q", q)
		}
	}
	// dialect extras: pg blocks pg_sleep, mysql blocks LOAD_FILE/SLEEP
	pg, _ := DialectFor("postgres")
	if err := ValidateReadOnlySQL("SELECT pg_sleep(10)", pg.DeniedExtras()); err == nil {
		t.Fatal("pg_sleep must be denied for postgres")
	}
	my, _ := DialectFor("mysql")
	if err := ValidateReadOnlySQL("SELECT LOAD_FILE('/etc/passwd')", my.DeniedExtras()); err == nil {
		t.Fatal("LOAD_FILE must be denied for mysql")
	}
	if err := ValidateReadOnlySQL("SELECT SLEEP(10)", my.DeniedExtras()); err == nil {
		t.Fatal("SLEEP must be denied for mysql")
	}
	// profile extras still merge
	if err := ValidateReadOnlySQL("SELECT secret_col FROM t", []string{"SECRET_COL"}); err == nil {
		t.Fatal("profile denied keyword must apply")
	}
}

func TestProfileValidateAndDefaults(t *testing.T) {
	p := Profile{ID: "pg-01", ConnectString: "h:5432/db", Username: "u", PasswordRef: "env:PW"}
	if err := p.Validate(); err != nil {
		t.Fatalf("default type must validate: %v", err)
	}
	p.Type = "mariadb"
	if err := p.Validate(); err != nil {
		t.Fatalf("mariadb must validate: %v", err)
	}
	got := ApplyDefaults(p)
	if got.Type != "mariadb" || got.Driver != "mysql" {
		t.Fatalf("defaults: %+v", got)
	}
	p.Type = "oracle"
	if err := p.Validate(); err != nil {
		t.Fatalf("oracle profile shape must validate before its optional driver build: %v", err)
	}
	p.Type = "mysql"
	p.Driver = "pgx"
	if err := p.Validate(); err == nil {
		t.Fatal("mismatched driver must be rejected")
	}
}

func TestProductionProfileRequiresSecretReferenceAndMasksFleetContext(t *testing.T) {
	p := Profile{
		ID: "prod-01", Type: "postgres", ConnectString: "db:5432/app",
		Username: "monitor", PasswordRef: "plain:secret", Environment: "production",
		ServiceName: "payments", Criticality: "critical", Role: "primary",
		OwnerTeam: "dba", Location: "seoul", Maintenance: "sat 02:00-04:00",
		Tags: []string{"tier-1", "pci"},
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "plain: is forbidden") {
		t.Fatalf("production plain secret must be rejected, got %v", err)
	}
	p.PasswordRef = "file:/run/secrets/db_password"
	if err := p.Validate(); err != nil {
		t.Fatalf("production file secret must validate: %v", err)
	}
	got := ApplyDefaults(p)
	view := got.Masked()
	if view["service_name"] != "payments" || view["criticality"] != "critical" || view["maintenance_window"] != "sat 02:00-04:00" {
		t.Fatalf("fleet context missing from API-safe view: %#v", view)
	}
	if view["password_ref"] != "file:/run/secrets/db_password" {
		t.Fatalf("secret reference pointer should be preserved: %#v", view["password_ref"])
	}

	p.DBA = &DBAConfig{Enabled: true, Username: "admin", PasswordRef: "plain:secret"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "production DBA") {
		t.Fatalf("production DBA plain secret must be rejected, got %v", err)
	}
}

func TestAnalyzePostgresPlanJSON(t *testing.T) {
	raw := []byte(`[{"Plan":{
		"Node Type":"Nested Loop","Total Cost":250000.5,"Plan Rows":2500000,
		"Plans":[
			{"Node Type":"Seq Scan","Relation Name":"big_tab","Plan Rows":2000000,"Total Cost":90000,"Filter":"(a > 1)"},
			{"Node Type":"Seq Scan","Relation Name":"other_tab","Plan Rows":500,"Total Cost":10}
		]}}]`)
	res, err := AnalyzePostgresPlanJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Risk != "high" {
		t.Fatalf("expected high risk, got %s (score %d, factors %v)", res.Risk, res.RiskScore, res.RiskFactors)
	}
	if res.TotalCost != 250000 || res.MaxCardinality != 2500000 {
		t.Fatalf("cost/card: %d/%d", res.TotalCost, res.MaxCardinality)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("steps: %d", len(res.Steps))
	}
	// cartesian: no join filter, no index conds anywhere
	hasCartesian := false
	for _, f := range res.RiskFactors {
		if strings.Contains(f, "cartesian") {
			hasCartesian = true
		}
	}
	if !hasCartesian {
		t.Fatalf("expected cartesian factor: %v", res.RiskFactors)
	}

	// small indexed plan → low risk
	raw = []byte(`[{"Plan":{"Node Type":"Index Scan","Relation Name":"t","Index Name":"t_pk","Plan Rows":10,"Total Cost":8.3,"Index Cond":"(id = 1)"}}]`)
	res, err = AnalyzePostgresPlanJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Risk != "low" {
		t.Fatalf("expected low risk, got %s: %v", res.Risk, res.RiskFactors)
	}
}

func TestAnalyzeMySQLPlanJSON(t *testing.T) {
	// MySQL 8 shape
	raw := []byte(`{"query_block":{"select_id":1,"cost_info":{"query_cost":"205000.40"},
		"nested_loop":[
			{"table":{"table_name":"big_tab","access_type":"ALL","rows_examined_per_scan":1500000,"attached_condition":"(a > 1)"}},
			{"table":{"table_name":"other","access_type":"ALL","rows_examined_per_scan":20000,"using_join_buffer":"hash join"}}
		]}}`)
	res, err := AnalyzeMySQLPlanJSON(raw, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if res.Risk != "high" {
		t.Fatalf("expected high risk, got %s (score %d, %v)", res.Risk, res.RiskScore, res.RiskFactors)
	}
	if res.TotalCost != 205000 {
		t.Fatalf("cost: %d", res.TotalCost)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("steps: %d", len(res.Steps))
	}

	// MariaDB shape (no cost_info, "rows", block-nl-join wrapper)
	raw = []byte(`{"query_block":{"select_id":1,
		"table":{"table_name":"t1","access_type":"ALL","rows":120},
		"block-nl-join":{"table":{"table_name":"t2","access_type":"ALL","rows":50}}}}`)
	res, err = AnalyzeMySQLPlanJSON(raw, "mariadb")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("mariadb steps: %d (%+v)", len(res.Steps), res.Steps)
	}
	if res.Risk != "low" {
		t.Fatalf("small tables must be low risk, got %s: %v", res.Risk, res.RiskFactors)
	}
	// indexed lookup → low
	raw = []byte(`{"query_block":{"table":{"table_name":"t","access_type":"eq_ref","key":"PRIMARY","rows":1}}}`)
	res, err = AnalyzeMySQLPlanJSON(raw, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if res.Risk != "low" || len(res.Steps) != 1 {
		t.Fatalf("indexed: %s %d", res.Risk, len(res.Steps))
	}
}

func TestPasswordRefAndMask(t *testing.T) {
	t.Setenv("JAMYPG_TEST_PW", "pw1")
	if v, err := ResolvePassword("env:JAMYPG_TEST_PW"); err != nil || v != "pw1" {
		t.Fatalf("env ref: %v %q", err, v)
	}
	if _, err := ResolvePassword("bogus"); err == nil {
		t.Fatal("schemeless ref must fail")
	}
	if MaskedRef("plain:hunter2") != "plain:****" {
		t.Fatal("plain ref must be masked")
	}
	if MaskedRef("env:NAME") != "env:NAME" {
		t.Fatal("env ref is a pointer, not a secret")
	}
}
