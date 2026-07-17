package meta

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

func TestPGMigrationsConvergeOnSQLONMetaSchema(t *testing.T) {
	all := strings.Join(migrations, "\n")
	for _, table := range []string{
		"users", "sessions", "mcp_keys", "database_profiles",
		"profile_grants", "settings", "datasets", "mcp_activity",
	} {
		if !strings.Contains(all, "sqlon_meta."+table) {
			t.Errorf("migration does not create or reference sqlon_meta.%s", table)
		}
	}
	for _, legacy := range []string{
		"jamypg_users", "jamypg_sessions", "jamypg_mcp_keys",
		"jamypg_db_profiles", "jamypg_profile_grants", "jamypg_settings",
		"jamypg_datasets", "jamypg_mcp_activity",
	} {
		if !strings.Contains(all, "ARRAY['"+legacy+"'") {
			t.Errorf("legacy table %s has no explicit migration mapping", legacy)
		}
	}
	if !strings.Contains(all, "both public.% and sqlon_meta.% exist") {
		t.Error("ambiguous dual-schema state must fail closed")
	}
	if !strings.Contains(all, "'admin','dba','user'") {
		t.Error("SQLON DBA role is missing from the database constraint")
	}
}

// TestPGLegacySchemaMigration requires an explicitly disposable database. It
// proves that an existing JAMYPG user table and row are moved, not recreated.
func TestPGLegacySchemaMigration(t *testing.T) {
	dsn := os.Getenv("SQLON_TEST_PG_LEGACY")
	if dsn == "" {
		t.Skip("set SQLON_TEST_PG_LEGACY to an empty disposable Postgres database")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS sqlon_meta CASCADE`,
		`DROP TABLE IF EXISTS public.jamypg_users CASCADE`,
		`CREATE TABLE public.jamypg_users (
			id TEXT PRIMARY KEY, username TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '', password_hash TEXT,
			role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin','user')),
			provider TEXT NOT NULL DEFAULT 'local' CHECK (provider IN ('local','keycloak')),
			provider_subject TEXT, is_active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			last_login_at TIMESTAMPTZ)`,
		`INSERT INTO public.jamypg_users (id, username) VALUES ('legacy-user', 'legacy-admin')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed legacy schema: %v", err)
		}
	}

	store, err := OpenPG(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPG migration: %v", err)
	}
	defer store.Close()
	var username string
	if err := store.db.QueryRowContext(ctx, `SELECT username FROM sqlon_meta.users WHERE id='legacy-user'`).Scan(&username); err != nil {
		t.Fatalf("legacy row was not preserved: %v", err)
	}
	if username != "legacy-admin" {
		t.Fatalf("legacy row changed: %q", username)
	}
	var oldTable sql.NullString
	if err := store.db.QueryRowContext(ctx, `SELECT to_regclass('public.jamypg_users')::text`).Scan(&oldTable); err != nil {
		t.Fatal(err)
	}
	if oldTable.Valid {
		t.Fatalf("legacy table still exists: %s", oldTable.String)
	}
	if _, err := NewService(store).CreateLocalUser(ctx, "sqlon-dba", "password1", RoleDBA, "", ""); err != nil {
		t.Fatalf("DBA role rejected after migration: %v", err)
	}
}
