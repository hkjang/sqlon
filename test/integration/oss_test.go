//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/catalog"
	"sqlon/internal/dbconn"
)

// Famous open-source service schemas seeded into all three engines by
// deploy/test/gen_oss_testenv.py: sakila (DVD rental), northwind (orders),
// wordpress (blog). Each golden query carries a python-computed expected
// first row, asserted identically against postgres, mysql, and mariadb.

type ossGolden struct {
	ID               int            `json:"id"`
	Question         string         `json:"question"`
	ExpectedTables   []string       `json:"expected_tables"`
	ExpectedSQL      string         `json:"expected_sql"`
	ExpectedFirstRow map[string]any `json:"expected_first_row"`
}

var ossDatasets = []string{"sakila", "northwind", "wordpress"}

func ossProfiles(name string) []string {
	return []string{"pg-" + name, "mysql-" + name, "mariadb-" + name}
}

// expectStr normalizes JSON-decoded expected values (float64 for numbers) and
// DB-returned values to comparable strings.
func expectStr(v any) string {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return fmt.Sprint(int64(f))
	}
	return fmt.Sprint(v)
}

func TestOSSSchemasText2SQL(t *testing.T) {
	for _, name := range ossDatasets {
		name := name
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(repoRoot(t), "data", name)
			c, err := catalog.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, issue := range c.Issues {
				if issue.Level == "error" {
					t.Fatalf("catalog error: %+v", issue)
				}
			}
			if c.Dialect != "postgres" {
				t.Fatalf("dialect = %q", c.Dialect)
			}
			m := dbconn.NewManager(dir)
			t.Cleanup(m.Close)

			// all three engines reachable through this dataset's profiles
			for _, id := range ossProfiles(name) {
				if res := m.Ping(ctxT(t), id); !res.OK {
					t.Fatalf("%s ping: %s", id, res.Error)
				}
			}

			var golden []ossGolden
			b, err := os.ReadFile(filepath.Join(dir, "golden_queries.json"))
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
					// 1) retrieval: expected tables surface in schema search
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
						got := make([]string, 0, len(resp.Results))
						for _, h := range resp.Results {
							got = append(got, h.Table)
						}
						t.Fatalf("search missed %v for %q (got %v)", g.ExpectedTables, g.Question, got)
					}
					// 2) catalog validation passes
					vres := c.ValidateSQL(catalog.ValidateRequest{SQL: g.ExpectedSQL})
					if !vres.Valid {
						t.Fatalf("golden SQL invalid: %+v", vres.Errors)
					}
					// 3) execution returns the same known answer on all engines
					for _, id := range ossProfiles(name) {
						res, err := m.Execute(ctxT(t), id, g.ExpectedSQL, dbconn.ExecOptions{})
						if err != nil {
							t.Fatalf("%s: execute: %v", id, err)
						}
						if len(res.Rows) == 0 {
							t.Fatalf("%s: no rows", id)
						}
						for col, want := range g.ExpectedFirstRow {
							got, ok := res.Rows[0][col]
							if !ok {
								t.Fatalf("%s: column %q missing in %v", id, col, res.Rows[0])
							}
							if expectStr(got) != expectStr(want) {
								t.Fatalf("%s: %s = %v, want %v (row %v)", id, col, got, want, res.Rows[0])
							}
						}
					}
				})
			}
		})
	}
}

// TestOSSGuardAndPlan spot-checks the guard and live EXPLAIN on the OSS
// schemas: writes stay blocked and joins produce analyzable plans everywhere.
func TestOSSGuardAndPlan(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "data", "sakila")
	m := dbconn.NewManager(dir)
	t.Cleanup(m.Close)
	for _, id := range ossProfiles("sakila") {
		if _, err := m.Execute(ctxT(t), id, "DELETE FROM sakila.rental", dbconn.ExecOptions{}); err == nil {
			t.Fatalf("%s: DELETE must be blocked", id)
		}
		plan, err := m.ExplainPlan(ctxT(t), id,
			"SELECT c.name, COUNT(*) FROM sakila.film_category fc JOIN sakila.category c ON fc.category_id = c.category_id GROUP BY c.name")
		if err != nil {
			t.Fatalf("%s explain: %v", id, err)
		}
		if len(plan.Steps) == 0 || plan.Risk == "" {
			t.Fatalf("%s: empty plan %+v", id, plan)
		}
		t.Logf("%s: plan steps=%d risk=%s", id, len(plan.Steps), plan.Risk)
	}
	// PII: customer.email is flagged, selecting it must invalidate
	c, err := catalog.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := c.ValidateSQL(catalog.ValidateRequest{SQL: "SELECT T1.email FROM sakila.customer T1"})
	if res.Valid {
		t.Fatalf("PII column selection must be invalid: %+v", res)
	}
}
