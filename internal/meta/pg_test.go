package meta

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPGStore runs the full Store contract against a real Postgres when
// SQLON_TEST_PG (or the one-release JAMYPG_TEST_PG alias) is set (e.g. a
// throwaway docker container in CI). Skipped
// otherwise so `go test ./...` stays hermetic.
func TestPGStore(t *testing.T) {
	dsn := os.Getenv("SQLON_TEST_PG")
	if dsn == "" {
		dsn = os.Getenv("JAMYPG_TEST_PG")
	}
	if dsn == "" {
		t.Skip("set SQLON_TEST_PG to run Postgres integration tests")
	}
	ctx := context.Background()
	store, err := OpenPG(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	defer store.Close()
	var schemaTable string
	if err := store.db.QueryRowContext(ctx, `SELECT to_regclass('sqlon_meta.users')::text`).Scan(&schemaTable); err != nil || schemaTable == "" {
		t.Fatalf("SQLON meta schema missing: table=%q err=%v", schemaTable, err)
	}
	// clean slate for a deterministic run
	svc := NewService(store)

	u, err := svc.CreateLocalUser(ctx, "pg-admin", "password1", RoleAdmin, "PG Admin", "a@x.com")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateLocalUser(ctx, "pg-admin", "password1", RoleAdmin, "", ""); err == nil {
		t.Fatal("duplicate username must fail")
	}
	// login + session round-trip
	_, token, err := svc.Login(ctx, "pg-admin", "password1", "1.2.3.4", "test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if got, err := svc.Authenticate(ctx, token); err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %v", err)
	}
	if err := svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, token); err == nil {
		t.Fatal("revoked session must fail")
	}
	// MCP key round-trip
	raw, k, err := svc.CreateMCPKey(ctx, u.ID, "pg-key", time.Hour)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, _, err := svc.AuthenticateKey(ctx, raw); err != nil {
		t.Fatalf("auth key: %v", err)
	}
	newRaw, nk, err := svc.RotateMCPKey(ctx, k.ID)
	if err != nil || nk.RotatedFrom != k.ID {
		t.Fatalf("rotate: %v", err)
	}
	if _, _, err := svc.AuthenticateKey(ctx, raw); err == nil {
		t.Fatal("rotated-away key must fail")
	}
	if _, _, err := svc.AuthenticateKey(ctx, newRaw); err != nil {
		t.Fatalf("rotated key must work: %v", err)
	}
	// profile + grant round-trip
	rec := &ProfileRecord{ID: "pg-prof", OwnerID: u.ID, Definition: []byte(`{"id":"pg-prof"}`), Visibility: VisibilityPrivate}
	if err := store.UpsertProfile(ctx, rec, true); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	other, _ := svc.CreateLocalUser(ctx, "pg-user", "password1", RoleUser, "", "")
	if err := store.SetGrant(ctx, Grant{ProfileID: "pg-prof", UserID: other.ID, Permission: PermManage, GrantedBy: u.ID}); err != nil {
		t.Fatalf("set grant: %v", err)
	}
	grants, _ := store.ListGrants(ctx, "pg-prof")
	if len(grants) != 1 || !CanManageProfile(other, *rec, grants) {
		t.Fatalf("grant not effective: %+v", grants)
	}
	// idempotent migrations: reopening must not error
	store2, err := OpenPG(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen (idempotent migrations): %v", err)
	}
	store2.Close()

	// cleanup
	_ = store.DeleteProfile(ctx, "pg-prof")
}
