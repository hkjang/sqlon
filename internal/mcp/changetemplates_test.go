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
