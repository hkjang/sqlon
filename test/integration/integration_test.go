//go:build integration

// Package integration exercises the full text2sql path against live
// PostgreSQL, MySQL, and MariaDB containers loaded with the jamypg meta
// schema (deploy/test/docker-compose.yml):
//
//	docker compose -f deploy/test/docker-compose.yml up -d --wait
//	go test -tags integration ./test/integration -v
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sqlon/internal/catalog"
	"sqlon/internal/dbconn"
)

var profiles = []string{"pg-meta", "mysql-meta", "mariadb-meta"}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func newManager(t *testing.T) *dbconn.Manager {
	t.Helper()
	m := dbconn.NewManager(filepath.Join(repoRoot(t), "data", "metadb"))
	t.Cleanup(m.Close)
	return m
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPingAllDialects(t *testing.T) {
	m := newManager(t)
	for _, id := range profiles {
		res := m.Ping(ctxT(t), id)
		if !res.OK {
			t.Fatalf("%s ping failed: %s (%s)", id, res.Error, res.ErrorCode)
		}
		t.Logf("%s: ping ok in %dms", id, res.ElapsedMs)
	}
}

func TestReadOnlySessionEnforced(t *testing.T) {
	m := newManager(t)
	queries := map[string]string{
		"pg-meta":      "SELECT current_setting('transaction_read_only') AS ro",
		"mysql-meta":   "SELECT @@transaction_read_only AS ro",
		"mariadb-meta": "SELECT @@tx_read_only AS ro",
	}
	want := map[string]string{"pg-meta": "on", "mysql-meta": "1", "mariadb-meta": "1"}
	for id, q := range queries {
		res, err := m.Execute(ctxT(t), id, q, dbconn.ExecOptions{})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("%s: rows %d", id, len(res.Rows))
		}
		got := fmt.Sprint(res.Rows[0]["ro"])
		if got != want[id] {
			t.Fatalf("%s: session not read-only: %q", id, got)
		}
	}
}

func TestGuardBlocksWrites(t *testing.T) {
	m := newManager(t)
	bad := []string{
		"INSERT INTO public.jamypg_settings (setting_key, setting_value) VALUES ('x','y')",
		"DELETE FROM public.jamypg_users",
		"DROP TABLE public.jamypg_users",
		"SELECT 1; DELETE FROM public.jamypg_users",
	}
	for _, id := range profiles {
		for _, q := range bad {
			if _, err := m.Execute(ctxT(t), id, q, dbconn.ExecOptions{}); err == nil {
				t.Fatalf("%s: guard must block %q", id, q)
			}
		}
	}
	// dialect-specific dangerous functions
	if _, err := m.Execute(ctxT(t), "pg-meta", "SELECT pg_sleep(5)", dbconn.ExecOptions{}); err == nil {
		t.Fatal("pg_sleep must be blocked")
	}
	if _, err := m.Execute(ctxT(t), "mysql-meta", "SELECT SLEEP(5)", dbconn.ExecOptions{}); err == nil {
		t.Fatal("SLEEP must be blocked on mysql")
	}
}

func TestExecuteLimitAndTruncation(t *testing.T) {
	m := newManager(t)
	for _, id := range profiles {
		res, err := m.Execute(ctxT(t), id, "SELECT id, tool, status FROM public.jamypg_mcp_activity ORDER BY id", dbconn.ExecOptions{MaxRows: 3})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if res.RowCount != 3 || !res.Truncated {
			t.Fatalf("%s: expected 3 truncated rows, got %d truncated=%v", id, res.RowCount, res.Truncated)
		}
		if len(res.Columns) != 3 {
			t.Fatalf("%s: columns %+v", id, res.Columns)
		}
	}
}

func TestExecuteWithCTE(t *testing.T) {
	m := newManager(t)
	q := "WITH per_user AS (SELECT user_id, COUNT(*) AS calls FROM public.jamypg_mcp_activity GROUP BY user_id) SELECT COUNT(*) AS users_with_calls FROM per_user"
	for _, id := range profiles {
		res, err := m.Execute(ctxT(t), id, q, dbconn.ExecOptions{})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if fmt.Sprint(res.Rows[0]["users_with_calls"]) != "5" {
			t.Fatalf("%s: users_with_calls = %v", id, res.Rows[0])
		}
	}
}

func TestCountRowsAndMetadata(t *testing.T) {
	m := newManager(t)
	q := "SELECT username, role FROM public.jamypg_users WHERE is_active = TRUE"
	for _, id := range profiles {
		n, err := m.CountRows(ctxT(t), id, q)
		if err != nil {
			t.Fatalf("%s count: %v", id, err)
		}
		if n != 8 {
			t.Fatalf("%s: active users = %d, want 8", id, n)
		}
		cols, err := m.Metadata(ctxT(t), id, q)
		if err != nil {
			t.Fatalf("%s metadata: %v", id, err)
		}
		if len(cols) != 2 {
			t.Fatalf("%s: metadata cols %+v", id, cols)
		}
	}
}

func TestExplainPlanAllDialects(t *testing.T) {
	m := newManager(t)
	q := "SELECT u.username, COUNT(*) AS calls FROM public.jamypg_mcp_activity a JOIN public.jamypg_users u ON a.user_id = u.id GROUP BY u.username"
	for _, id := range profiles {
		plan, err := m.ExplainPlan(ctxT(t), id, q)
		if err != nil {
			t.Fatalf("%s explain: %v", id, err)
		}
		if len(plan.Steps) == 0 {
			t.Fatalf("%s: empty plan steps", id)
		}
		if plan.Risk == "" {
			t.Fatalf("%s: missing risk", id)
		}
		t.Logf("%s: %d steps, risk=%s cost=%d maxrows=%d", id, len(plan.Steps), plan.Risk, plan.TotalCost, plan.MaxCardinality)
	}
}

func TestErrorCodesPerDialect(t *testing.T) {
	m := newManager(t)
	_, err := m.Execute(ctxT(t), "pg-meta", "SELECT nope FROM public.jamypg_users", dbconn.ExecOptions{})
	if err == nil {
		t.Fatal("expected pg column error")
	}
	_, err = m.Execute(ctxT(t), "mysql-meta", "SELECT nope FROM public.jamypg_users", dbconn.ExecOptions{})
	if err == nil {
		t.Fatal("expected mysql column error")
	}
	hist := m.History(10)
	var sawPG, sawMY bool
	for _, h := range hist {
		if h.ErrorCode == "PG-42703" {
			sawPG = true
		}
		if h.ErrorCode == "MY-1054" {
			sawMY = true
		}
	}
	if !sawPG || !sawMY {
		t.Fatalf("expected PG-42703 and MY-1054 in history, got %+v", hist)
	}
}

// ---- text2sql flow: catalog search → skeleton → validate → execute ----

type goldenQuery struct {
	ID             int      `json:"id"`
	Question       string   `json:"question"`
	ExpectedTables []string `json:"expected_tables"`
	ExpectedSQL    string   `json:"expected_sql"`
}

func loadCatalogT(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load(filepath.Join(repoRoot(t), "data", "metadb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range c.Issues {
		if issue.Level == "error" {
			t.Fatalf("catalog error: %+v", issue)
		}
	}
	return c
}

func TestText2SQLFlowOnMetaDB(t *testing.T) {
	c := loadCatalogT(t)
	m := newManager(t)

	var golden []goldenQuery
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "data", "metadb", "golden_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) < 5 {
		t.Fatalf("golden set too small: %d", len(golden))
	}

	for _, g := range golden {
		g := g
		t.Run(fmt.Sprintf("golden-%02d", g.ID), func(t *testing.T) {
			// 1) retrieval: the expected table must rank in schema search
			resp := c.SearchSchema(catalog.SearchRequest{Question: g.Question, TopK: 5})
			found := false
			for _, h := range resp.Results {
				for _, want := range g.ExpectedTables {
					if strings.EqualFold(h.Table, want) {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("search did not surface %v for %q", g.ExpectedTables, g.Question)
			}
			// 2) validation: the golden SQL must pass the catalog guard
			vres := c.ValidateSQL(catalog.ValidateRequest{SQL: g.ExpectedSQL})
			if !vres.Valid {
				t.Fatalf("golden SQL invalid: %+v", vres.Errors)
			}
			// 3) execution on all three engines with identical SQL
			results := map[string]string{}
			for _, id := range profiles {
				res, err := m.Execute(ctxT(t), id, g.ExpectedSQL, dbconn.ExecOptions{})
				if err != nil {
					t.Fatalf("%s: execute: %v", id, err)
				}
				rows, _ := json.Marshal(res.Rows)
				results[id] = string(rows)
			}
			t.Logf("q%02d %q → %s rows(pg)=%s", g.ID, g.Question, "ok", results["pg-meta"])
		})
	}
}

func TestText2SQLKnownAnswers(t *testing.T) {
	m := newManager(t)
	cases := []struct {
		sql, col, want string
	}{
		{"SELECT COUNT(*) AS n FROM public.jamypg_users WHERE is_active = TRUE", "n", "8"},
		{"SELECT COUNT(*) AS n FROM public.jamypg_db_profiles WHERE visibility = 'shared'", "n", "3"},
		{"SELECT COUNT(*) AS n FROM public.jamypg_users WHERE role = 'admin'", "n", "2"},
	}
	for _, tc := range cases {
		for _, id := range profiles {
			res, err := m.Execute(ctxT(t), id, tc.sql, dbconn.ExecOptions{})
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if got := fmt.Sprint(res.Rows[0][tc.col]); got != tc.want {
				t.Fatalf("%s: %q = %s, want %s", id, tc.sql, got, tc.want)
			}
		}
	}
	// top tool must agree across engines
	q := "SELECT tool, COUNT(*) AS calls FROM public.jamypg_mcp_activity GROUP BY tool ORDER BY calls DESC LIMIT 1"
	var top string
	for _, id := range profiles {
		res, err := m.Execute(ctxT(t), id, q, dbconn.ExecOptions{})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		got := fmt.Sprint(res.Rows[0]["tool"])
		if top == "" {
			top = got
		} else if got != top {
			t.Fatalf("%s: top tool %q != %q", id, got, top)
		}
	}
	if top != "run_sql_safely" {
		t.Fatalf("top tool = %q", top)
	}
}

func TestSkeletonAndDialectValidation(t *testing.T) {
	c := loadCatalogT(t)
	if c.Dialect != "postgres" {
		t.Fatalf("dataset dialect = %q", c.Dialect)
	}
	// Oracle-isms must be rejected
	res := c.ValidateSQL(catalog.ValidateRequest{SQL: "SELECT NVL(username, '-') FROM public.jamypg_users WHERE ROWNUM <= 5"})
	if res.Valid {
		t.Fatal("Oracle syntax must be rejected")
	}
	// PII columns must be flagged
	res = c.ValidateSQL(catalog.ValidateRequest{SQL: "SELECT T1.email, T1.password_hash FROM public.jamypg_users T1"})
	if res.Valid {
		t.Fatalf("PII selection must be invalid: %+v", res)
	}
	// mysql dialect mode: FETCH FIRST rejected, || warned
	c.Dialect = "mysql"
	res = c.ValidateSQL(catalog.ValidateRequest{SQL: "SELECT username FROM public.jamypg_users FETCH FIRST 5 ROWS ONLY"})
	if res.Valid {
		t.Fatal("FETCH FIRST must be invalid for mysql")
	}
	c.Dialect = "postgres"
	// unbounded query gets a LIMIT suggestion
	res = c.ValidateSQL(catalog.ValidateRequest{SQL: "SELECT username FROM public.jamypg_users", Limit: 50})
	if res.BoundedSQL == "" || res.BoundedSQL == "SELECT username FROM public.jamypg_users" {
		t.Fatalf("expected bounded SQL, got %q", res.BoundedSQL)
	}
}
