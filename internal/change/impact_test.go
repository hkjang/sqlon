package change

import "testing"

func TestPredictPostgresCreateIndex(t *testing.T) {
	plain := PredictImpact("postgres", "CREATE INDEX idx ON orders(cust_id)")
	if plain.Blocking != "writes" || plain.Severity != "warning" || plain.OnlineOption == "" {
		t.Fatalf("plain CREATE INDEX should block writes with an online option: %+v", plain)
	}
	conc := PredictImpact("postgres", "CREATE INDEX CONCURRENTLY idx ON orders(cust_id)")
	if conc.Blocking != "none" || conc.Severity != "info" {
		t.Fatalf("CONCURRENTLY should not block: %+v", conc)
	}
}

func TestPredictPostgresAlterTypeIsCriticalRewrite(t *testing.T) {
	p := PredictImpact("postgres", "ALTER TABLE orders ALTER COLUMN amount TYPE bigint")
	if !p.Rewrite || p.LockLevel != "ACCESS EXCLUSIVE" || p.Severity != "critical" {
		t.Fatalf("type change must be a critical rewrite under ACCESS EXCLUSIVE: %+v", p)
	}
}

func TestPredictPostgresAddColumnConstantDefaultIsCheap(t *testing.T) {
	// PG11+ constant default → simple add (info, no rewrite flagged).
	p := PredictImpact("postgres", "ALTER TABLE orders ADD COLUMN flag boolean DEFAULT false")
	if p.Rewrite || p.Severity != "info" {
		t.Fatalf("constant DEFAULT add should be cheap: %+v", p)
	}
}

func TestPredictPostgresAddColumnVolatileDefaultRewrites(t *testing.T) {
	p := PredictImpact("postgres", "ALTER TABLE orders ADD COLUMN uid uuid DEFAULT gen_random_uuid()")
	if !p.Rewrite || p.Severity != "warning" {
		t.Fatalf("function DEFAULT should be flagged as a potential rewrite: %+v", p)
	}
}

func TestPredictPostgresNotNullNoDefaultRewrites(t *testing.T) {
	p := PredictImpact("postgres", "ALTER TABLE orders ADD COLUMN c int NOT NULL")
	if !p.Rewrite {
		t.Fatalf("NOT NULL without DEFAULT should be flagged as a rewrite: %+v", p)
	}
}

func TestPredictPostgresForeignKeyValidating(t *testing.T) {
	valid := PredictImpact("postgres", "ALTER TABLE a ADD CONSTRAINT fk FOREIGN KEY (b) REFERENCES c(id)")
	if !valid.Rewrite || valid.OnlineOption == "" && valid.Recommendation == "" {
		t.Fatalf("validating FK should scan and suggest NOT VALID: %+v", valid)
	}
	notValid := PredictImpact("postgres", "ALTER TABLE a ADD CONSTRAINT fk FOREIGN KEY (b) REFERENCES c(id) NOT VALID")
	if notValid.Rewrite {
		t.Fatalf("NOT VALID FK should not be flagged as a scan/rewrite: %+v", notValid)
	}
}

func TestPredictMySQLColumnTypeChangeIsCopy(t *testing.T) {
	p := PredictImpact("mysql", "ALTER TABLE orders MODIFY COLUMN amount BIGINT")
	if !p.Rewrite || p.Severity != "critical" || p.OnlineOption == "" {
		t.Fatalf("MODIFY COLUMN should be a critical table-copy with an online option: %+v", p)
	}
}

func TestPredictMySQLAddColumnIsInstant(t *testing.T) {
	p := PredictImpact("mariadb", "ALTER TABLE orders ADD COLUMN note varchar(20)")
	if p.Blocking != "none" || p.Rewrite {
		t.Fatalf("ADD COLUMN should be online on MySQL/MariaDB 8.0.12+: %+v", p)
	}
}

func TestPredictUnknownEngineIsExplicit(t *testing.T) {
	p := PredictImpact("oracle", "ALTER TABLE t ADD (c NUMBER)")
	if p.Operation != "unknown" || len(p.Notes) == 0 {
		t.Fatalf("unknown engine must be explicit, not a false all-clear: %+v", p)
	}
}

func TestPredictEmptyStatement(t *testing.T) {
	if p := PredictImpact("postgres", "   "); p.Operation != "unknown" {
		t.Fatalf("empty statement handled: %+v", p)
	}
}
