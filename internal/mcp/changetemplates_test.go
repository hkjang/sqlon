package mcp

import (
	"strings"
	"testing"

	"sqlon/internal/change"
)

func TestBuildChangeStepReversibleActionsQuoteAndValidate(t *testing.T) {
	cases := []struct {
		name, dialect, action string
		args                  map[string]any
		wantCommand           string
		wantCompensation      string
	}{
		{
			name: "postgres create_user", dialect: "postgres", action: "create_user",
			args:        map[string]any{"username": "app_user"},
			wantCommand: `CREATE ROLE "app_user" NOLOGIN`, wantCompensation: `DROP ROLE IF EXISTS "app_user"`,
		},
		{
			name: "mysql create_user wildcard host", dialect: "mysql", action: "create_user",
			args:        map[string]any{"username": "app_user"},
			wantCommand: "CREATE USER 'app_user'@'%'", wantCompensation: "DROP USER IF EXISTS 'app_user'@'%'",
		},
		{
			name: "postgres create_database with owner", dialect: "postgres", action: "create_database",
			args:        map[string]any{"name": "analytics", "owner": "app_user"},
			wantCommand: `CREATE DATABASE "analytics" OWNER "app_user"`, wantCompensation: `DROP DATABASE IF EXISTS "analytics"`,
		},
		{
			name: "postgres grant then revoke compensation", dialect: "postgres", action: "grant",
			args:        map[string]any{"privileges": "SELECT", "object": "public.orders", "grantee": "app_user"},
			wantCommand: `GRANT SELECT ON public.orders TO "app_user"`, wantCompensation: `REVOKE SELECT ON public.orders FROM "app_user"`,
		},
		{
			name: "revoke then grant compensation", dialect: "postgres", action: "revoke",
			args:        map[string]any{"privileges": "INSERT", "object": "public.orders", "grantee": "app_user"},
			wantCommand: `REVOKE INSERT ON public.orders FROM "app_user"`, wantCompensation: `GRANT INSERT ON public.orders TO "app_user"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, err := buildChangeStep(tc.dialect, tc.action, 2, tc.args)
			if err != nil {
				t.Fatalf("buildChangeStep: %v", err)
			}
			if step.Order != 2 || step.Command != tc.wantCommand || step.Compensation != tc.wantCompensation {
				t.Fatalf("step mismatch:\n got command=%q comp=%q\nwant command=%q comp=%q", step.Command, step.Compensation, tc.wantCommand, tc.wantCompensation)
			}
			if strings.TrimSpace(step.Verification) == "" {
				t.Fatalf("verification must be non-empty: %+v", step)
			}
			// A generated step must satisfy the change-domain step contract so it
			// can go straight into a plan.
			p := change.Plan{ID: "t", ProfileID: "p", Target: "x", Reason: "r", Risk: change.Medium, Steps: []change.Step{{Order: 1, Command: step.Command, Verification: step.Verification, Compensation: step.Compensation}}}
			if err := p.Validate(); err != nil {
				t.Fatalf("generated step fails plan validation: %v", err)
			}
		})
	}
}

func TestBuildChangeStepCreateIndexIsReversibleAndQuoted(t *testing.T) {
	cases := []struct {
		name, dialect      string
		args               map[string]any
		wantCommand        string
		wantCompContains   string
		wantVerifyContains string
	}{
		{
			name: "postgres qualified table single column", dialect: "postgres",
			args:               map[string]any{"table": "public.orders", "columns": []any{"created_at"}},
			wantCommand:        `CREATE INDEX "idx_orders_created_at" ON "public"."orders" ("created_at")`,
			wantCompContains:   `DROP INDEX IF EXISTS "public"."idx_orders_created_at"`,
			wantVerifyContains: `pg_indexes WHERE indexname = 'idx_orders_created_at'`,
		},
		{
			name: "mysql multi-column comma string with explicit name", dialect: "mysql",
			args:               map[string]any{"table": "orders", "columns": "cust_id,created_at", "index": "ix_orders_cust_created"},
			wantCommand:        "CREATE INDEX `ix_orders_cust_created` ON `orders` (`cust_id`, `created_at`)",
			wantCompContains:   "DROP INDEX `ix_orders_cust_created` ON `orders`",
			wantVerifyContains: "INDEX_NAME = 'ix_orders_cust_created' AND TABLE_NAME = 'orders'",
		},
		{
			name: "oracle unique single column", dialect: "oracle",
			args:               map[string]any{"table": "APP.ORDERS", "column": "STATUS", "unique": true, "index": "UX_ORDERS_STATUS"},
			wantCommand:        `CREATE UNIQUE INDEX "UX_ORDERS_STATUS" ON "APP"."ORDERS" ("STATUS")`,
			wantCompContains:   `DROP INDEX "UX_ORDERS_STATUS"`,
			wantVerifyContains: `all_indexes WHERE index_name = 'UX_ORDERS_STATUS'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, err := buildChangeStep(tc.dialect, "create_index", 1, tc.args)
			if err != nil {
				t.Fatalf("buildChangeStep: %v", err)
			}
			if step.Command != tc.wantCommand {
				t.Fatalf("command\n got %q\nwant %q", step.Command, tc.wantCommand)
			}
			if !strings.Contains(step.Compensation, tc.wantCompContains) {
				t.Fatalf("compensation %q missing %q", step.Compensation, tc.wantCompContains)
			}
			if !strings.Contains(step.Verification, tc.wantVerifyContains) {
				t.Fatalf("verification %q missing %q", step.Verification, tc.wantVerifyContains)
			}
			p := change.Plan{ID: "t", ProfileID: "p", Target: "x", Reason: "r", Risk: change.High, Steps: []change.Step{{Order: 1, Command: step.Command, Verification: step.Verification, Compensation: step.Compensation}}}
			if err := p.Validate(); err != nil {
				t.Fatalf("generated index step fails plan validation: %v", err)
			}
		})
	}
}

func TestBuildChangeStepCreateIndexRejectsMissingAndInjection(t *testing.T) {
	if _, err := buildChangeStep("postgres", "create_index", 1, map[string]any{"columns": []any{"c"}}); err == nil {
		t.Fatal("missing table must be refused")
	}
	if _, err := buildChangeStep("postgres", "create_index", 1, map[string]any{"table": "t"}); err == nil {
		t.Fatal("missing columns must be refused")
	}
	// Injection attempt in the column is neutralized: the embedded double-quote
	// is doubled so the payload stays inside one quoted identifier.
	step, err := buildChangeStep("postgres", "create_index", 1, map[string]any{"table": "public.t", "columns": []any{`c") ; DROP TABLE t; --`}})
	if err != nil {
		t.Fatalf("quoting should neutralize, not error: %v", err)
	}
	if !strings.Contains(step.Command, `("c"") ; DROP TABLE t; --")`) {
		t.Fatalf("column not safely quoted (expected doubled quote): %q", step.Command)
	}
}

func TestBuildChangeStepRefusesPasswordAndInjection(t *testing.T) {
	if _, err := buildChangeStep("postgres", "create_user", 1, map[string]any{"username": "x", "password": "secret"}); err == nil {
		t.Fatal("password in a persisted plan must be refused")
	}
	// Identifier injection attempt is neutralized by quoting: the whole name
	// becomes one quoted identifier with its embedded double-quote doubled, so
	// the payload can never break out into a second statement.
	step, err := buildChangeStep("postgres", "create_user", 1, map[string]any{"username": `evil"; DROP TABLE users; --`})
	if err != nil {
		t.Fatalf("quoting should neutralize, not error: %v", err)
	}
	if !strings.HasPrefix(step.Command, `CREATE ROLE "evil""; DROP TABLE users; --" `) {
		t.Fatalf("identifier not safely quoted: %q", step.Command)
	}
	// The compensation quotes it identically — a single quoted identifier.
	if step.Compensation != `DROP ROLE IF EXISTS "evil""; DROP TABLE users; --"` {
		t.Fatalf("compensation not safely quoted: %q", step.Compensation)
	}
	// Newline/control chars in an identifier are rejected outright.
	if _, err := buildChangeStep("postgres", "create_user", 1, map[string]any{"username": "bad\nname"}); err == nil {
		t.Fatal("control character in identifier must be rejected")
	}
}

func TestBuildChangeStepRejectsIrreversibleAndUnknownActions(t *testing.T) {
	for _, action := range []string{"drop_user", "drop_database", "dba_execute", "truncate", ""} {
		if _, err := buildChangeStep("postgres", action, 1, map[string]any{"name": "x", "username": "x"}); err == nil {
			t.Fatalf("irreversible/unknown action %q must be refused (author it by hand)", action)
		}
	}
}

func TestBuildChangeStepGrantRejectsMetaCharacters(t *testing.T) {
	if _, err := buildChangeStep("postgres", "grant", 1, map[string]any{"privileges": "SELECT; DROP TABLE t", "object": "public.t", "grantee": "u"}); err == nil {
		t.Fatal("privileges with statement separator must be refused")
	}
}

func TestBuildChangeStepDropIndexIsReversible(t *testing.T) {
	// A redundant-index cleanup: DROP is the command, the exact CREATE INDEX is
	// the compensation so the drop is fully reversible.
	step, err := buildChangeStep("postgres", "drop_index", 1, map[string]any{
		"index": "idx_orders_cust", "table": "public.orders", "columns": []any{"cust_id"},
	})
	if err != nil {
		t.Fatalf("buildChangeStep drop_index: %v", err)
	}
	if !strings.Contains(step.Command, `DROP INDEX IF EXISTS "public"."idx_orders_cust"`) {
		t.Fatalf("command must DROP the index: %q", step.Command)
	}
	if !strings.Contains(step.Compensation, `CREATE INDEX "idx_orders_cust" ON "public"."orders" ("cust_id")`) {
		t.Fatalf("compensation must recreate the index: %q", step.Compensation)
	}
	p := change.Plan{ID: "t", ProfileID: "p", Target: "x", Reason: "r", Risk: change.Medium,
		Steps: []change.Step{{Order: 1, Command: step.Command, Verification: step.Verification, Compensation: step.Compensation}}}
	if err := p.Validate(); err != nil {
		t.Fatalf("generated drop-index step fails plan validation: %v", err)
	}
}

func TestBuildChangeStepDropIndexRequiresColumnsForReversibility(t *testing.T) {
	// Without columns there is no way to build the recreate compensation, so the
	// template must refuse rather than emit an irreversible drop.
	if _, err := buildChangeStep("postgres", "drop_index", 1, map[string]any{"index": "i", "table": "t"}); err == nil {
		t.Fatal("drop_index without columns must be refused")
	}
	if _, err := buildChangeStep("postgres", "drop_index", 1, map[string]any{"table": "t", "columns": []any{"c"}}); err == nil {
		t.Fatal("drop_index without index name must be refused")
	}
}
