package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sqlon/internal/catalog"
)

func TestSanitizeProfileID(t *testing.T) {
	cases := map[string]string{
		"pg-meta":       "pg-meta",
		"a/b\\c":        "a_b_c",
		"../etc/passwd": ".._etc_passwd",
		"":              "_",
		"..":            "_",
	}
	for in, want := range cases {
		if got := sanitizeProfileID(in); got != want {
			t.Errorf("sanitizeProfileID(%q)=%q want %q", in, got, want)
		}
	}
}

func newPCServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{}
	s.setCatalog(&catalog.Catalog{DataDir: t.TempDir(), Tables: map[string]*catalog.Table{}})
	return s
}

func TestProfileCatalogWorkspaceLifecycle(t *testing.T) {
	s := newPCServer(t)
	dir := s.profileCatalogDir("pg-prod")
	if !filepath.IsAbs(dir) && dir == "" {
		t.Fatal("bad workspace dir")
	}

	// no workspace yet
	res := s.getProfileCatalog("pg-prod")
	if res["workspace"].(bool) {
		t.Fatalf("expected no workspace, got %+v", res)
	}

	// seed a workspace with a minimal physical model (as build_profile_catalog would)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	phys := []map[string]any{
		{"schema_name": "public", "table_name": "orders", "column_order": "1",
			"column_name": "id", "data_type": "BIGINT", "is_pk": "Y", "is_fk": "N", "description": "", "version": 1},
	}
	b, _ := json.MarshalIndent(phys, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta_physical_models.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// logical model is a required dataset; an empty one is valid
	if err := os.WriteFile(filepath.Join(dir, "meta_logical_models.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}

	// now it loads
	res = s.getProfileCatalog("pg-prod")
	if !res["workspace"].(bool) {
		t.Fatalf("workspace should exist: %+v", res)
	}
	if res["summary"] == nil || res["datasets"] == nil {
		t.Fatalf("summary/datasets missing: %+v", res)
	}

	// put a dataset (overrides) into the workspace, then read it back
	overrides := json.RawMessage(`{"columns":[{"table":"public.orders","column":"id","logical_name":"주문번호"}]}`)
	pr := s.putProfileDataset("pg-prod", "overrides", overrides)
	if !pr["applied"].(bool) {
		t.Fatalf("put failed: %+v", pr)
	}
	got := s.getProfileDataset("pg-prod", "overrides")
	if got["error"] != nil {
		t.Fatalf("get dataset failed: %v", got["error"])
	}
	raw, _ := got["content"].(json.RawMessage)
	if raw == nil || !contains(string(raw), "주문번호") {
		t.Fatalf("overrides content missing: %s", string(raw))
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestWorkspaceCatalogCacheAndCatalogFor(t *testing.T) {
	s := newPCServer(t)
	// no workspace → catalogFor falls back to active
	c, src := s.catalogFor("pg-x")
	if src != "active" || c != s.cat() {
		t.Fatalf("no workspace should fall back to active, got %q", src)
	}

	// create a workspace
	dir := s.profileCatalogDir("pg-x")
	if err := ensureWorkspaceScaffold(dir); err != nil {
		t.Fatal(err)
	}
	phys := `[{"schema_name":"public","table_name":"orders","column_order":"1","column_name":"id","data_type":"BIGINT","is_pk":"Y","is_fk":"N","description":"","version":1}]`
	if err := os.WriteFile(filepath.Join(dir, "meta_physical_models.json"), []byte(phys), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, ok := s.workspaceCatalog("pg-x")
	if !ok || len(ws.Tables) != 1 {
		t.Fatalf("workspace catalog should load with 1 table, ok=%v", ok)
	}
	// cached: same pointer on second call
	ws2, _ := s.workspaceCatalog("pg-x")
	if ws2 != ws {
		t.Fatal("expected cached catalog pointer")
	}

	// edit the workspace → fingerprint changes → reload
	phys2 := `[{"schema_name":"public","table_name":"orders","column_order":"1","column_name":"id","data_type":"BIGINT","is_pk":"Y","is_fk":"N","description":"","version":1},
{"schema_name":"public","table_name":"users","column_order":"1","column_name":"id","data_type":"BIGINT","is_pk":"Y","is_fk":"N","description":"","version":1}]`
	time.Sleep(10 * time.Millisecond) // ensure mtime tick
	if err := os.WriteFile(filepath.Join(dir, "meta_physical_models.json"), []byte(phys2), 0o644); err != nil {
		t.Fatal(err)
	}
	ws3, ok := s.workspaceCatalog("pg-x")
	if !ok || len(ws3.Tables) != 2 {
		t.Fatalf("edited workspace should reload with 2 tables, got %d", len(ws3.Tables))
	}

	// catalogFor now picks the workspace
	c2, src2 := s.catalogFor("pg-x")
	if src2 != "profile-workspace:pg-x" || c2 != ws3 {
		t.Fatalf("catalogFor should pick the workspace, got %q", src2)
	}
}

func TestActiveCatalogInfoAdvertisesActivationCapability(t *testing.T) {
	s := newPCServer(t)
	info := s.activeCatalogInfo()
	if got, ok := info["can_activate"].(bool); !ok || !got {
		t.Fatalf("standalone catalog should advertise activation capability: %+v", info)
	}
	if info["activation_note"] == "" {
		t.Fatalf("activation guidance missing: %+v", info)
	}
}
