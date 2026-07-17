package dbconn

import (
	"strings"
	"testing"
)

func TestASTGuardAllowsReadOnlyQueries(t *testing.T) {
	ok := map[string][]string{
		"postgres": {
			"SELECT 1",
			"SELECT a, COUNT(*) FROM s.t WHERE b = 'x' GROUP BY a ORDER BY 2 DESC LIMIT 10",
			"WITH x AS (SELECT id FROM t) SELECT * FROM x JOIN u ON u.id = x.id",
			"SELECT a, ROW_NUMBER() OVER (PARTITION BY b ORDER BY c) FROM t",
			"SELECT * FROM (SELECT * FROM t WHERE d > CURRENT_DATE - INTERVAL '3 months') sub",
			"SELECT a FROM t UNION ALL SELECT a FROM u",
			"SELECT COALESCE(a, 'Y') FROM t WHERE b IN (SELECT b FROM u)",
		},
		"mysql": {
			"SELECT 1",
			"SELECT a, COUNT(*) FROM s.t WHERE b = 'x' GROUP BY a ORDER BY 2 DESC LIMIT 10",
			"WITH x AS (SELECT id FROM t) SELECT * FROM x JOIN u ON u.id = x.id",
			"SELECT a, ROW_NUMBER() OVER (PARTITION BY b ORDER BY c) FROM t",
			"SELECT a FROM t UNION ALL SELECT a FROM u",
			"SELECT @@transaction_read_only",
		},
		"mariadb": {
			"SELECT 1",
			"WITH x AS (SELECT id FROM t) SELECT * FROM x",
			"SELECT @@tx_read_only",
		},
	}
	for dialect, queries := range ok {
		for _, q := range queries {
			if err := ValidateReadOnlyAST(dialect, q); err != nil {
				t.Errorf("%s: expected ok for %q, got %v", dialect, q, err)
			}
		}
	}
}

func TestASTGuardBlocksWritesAndEvasions(t *testing.T) {
	bad := map[string][]string{
		"postgres": {
			"UPDATE t SET a = 1",
			"DELETE FROM t",
			"INSERT INTO t VALUES (1)",
			"DROP TABLE t",
			"TRUNCATE t",
			// the classic regex-evasion: a SELECT statement whose CTE deletes
			"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x",
			"WITH x AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM x",
			"WITH a AS (SELECT 1), b AS (UPDATE t SET c = 2 RETURNING *) SELECT * FROM b",
			// SELECT variants with side effects
			"SELECT * INTO newtab FROM t",
			"SELECT * FROM t FOR UPDATE",
			"SELECT * FROM t FOR SHARE",
			// non-query statements
			"DO $$ BEGIN PERFORM 1; END $$",
			"EXPLAIN SELECT 1",
			"SET search_path TO public",
			"COPY t TO '/tmp/x'",
			"VACUUM t",
			"CREATE TABLE x AS SELECT * FROM t",
			// multi-statement
			"SELECT 1; SELECT 2",
			"SELECT 1; DROP TABLE t",
		},
		"mysql": {
			"UPDATE t SET a = 1",
			"DELETE FROM t",
			"INSERT INTO t VALUES (1)",
			"REPLACE INTO t VALUES (1)",
			"DROP TABLE t",
			"SELECT * FROM t INTO OUTFILE '/tmp/x'",
			"SELECT * FROM t INTO DUMPFILE '/tmp/x'",
			"SELECT * FROM t FOR UPDATE",
			"SELECT * FROM t LOCK IN SHARE MODE",
			"SELECT @a := 1",
			"SET @a = 1",
			"PREPARE stmt FROM 'SELECT 1'",
			"SELECT 1; SELECT 2",
			"SELECT 1; DROP TABLE t",
		},
		"mariadb": {
			"DELETE FROM t",
			"SELECT * FROM t INTO OUTFILE '/tmp/x'",
			"SELECT * FROM t FOR UPDATE",
			"SELECT @a := 1",
			"SELECT 1; DROP TABLE t",
		},
	}
	for dialect, queries := range bad {
		for _, q := range queries {
			if err := ValidateReadOnlyAST(dialect, q); err == nil {
				t.Errorf("%s: expected rejection for %q", dialect, q)
			}
		}
	}
}

func TestASTGuardFailsClosedOnUnparseable(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		err := ValidateReadOnlyAST(dialect, "SELECT FROM WHERE ((")
		if err == nil {
			t.Fatalf("%s: unparseable SQL must be rejected", dialect)
		}
		if !strings.Contains(err.Error(), "파싱") {
			t.Fatalf("%s: expected parse-failure message, got %v", dialect, err)
		}
	}
	if err := ValidateReadOnlyAST("oracle", "SELECT 1"); err != nil {
		t.Fatalf("safe Oracle SELECT rejected: %v", err)
	}
	if err := ValidateReadOnlyAST("oracle", "SELECT FROM WHERE (("); err == nil {
		t.Fatal("ambiguous Oracle SQL must be rejected")
	}
}

func TestValidateReadOnlyCombinesKeywordAndAST(t *testing.T) {
	pg, _ := DialectFor("postgres")
	// keyword guard catches the denied function even though the AST is a valid SELECT
	if err := ValidateReadOnly(pg, "SELECT pg_sleep(10)", pg.DeniedExtras()); err == nil {
		t.Fatal("pg_sleep must still be denied by the keyword layer")
	}
	// AST guard catches the data-modifying CTE that the keyword guard would
	// also flag (DELETE keyword) — but this variant hides it behind quoting
	// tricks the AST cannot be fooled by
	if err := ValidateReadOnly(pg, `WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x`, nil); err == nil {
		t.Fatal("data-modifying CTE must be rejected")
	}
	// clean query passes both layers
	if err := ValidateReadOnly(pg, "SELECT COUNT(*) FROM public.jamypg_users WHERE is_active = TRUE", pg.DeniedExtras()); err != nil {
		t.Fatalf("clean query must pass: %v", err)
	}
	my, _ := DialectFor("mysql")
	if err := ValidateReadOnly(my, "SELECT * FROM t INTO OUTFILE '/x'", my.DeniedExtras()); err == nil {
		t.Fatal("INTO OUTFILE must be rejected")
	}
}
