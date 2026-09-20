package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sqlon/internal/catalog"
	"sqlon/internal/dbconn"
)

func seedCatalogReadWorkspace(t *testing.T, s *Server, id string) {
	t.Helper()
	dir := s.profileCatalogDir(id)
	if err := ensureWorkspaceScaffold(dir); err != nil {
		t.Fatal(err)
	}
	physical := fmt.Sprintf(`[{"schema_name":"public","table_name":%q,"column_order":"1","column_name":"id","data_type":"BIGINT","is_pk":"Y","is_fk":"N","description":"catalog-read-fixture"}]`, id)
	if err := os.WriteFile(filepath.Join(dir, "meta_physical_models.json"), []byte(physical), 0o644); err != nil {
		t.Fatal(err)
	}
}

func catalogReadJSON(t *testing.T, mux *http.ServeMux, path string, headers map[string]string, status int) map[string]json.RawMessage {
	t.Helper()
	rec := doReq(t, mux, "GET", path, "", headers)
	if rec.Code != status {
		t.Fatalf("GET %s: status=%d want=%d", path, rec.Code, status)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("GET %s: non-JSON response", path)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		if len(body["error"]) == 0 {
			t.Fatalf("GET %s: missing error", path)
		}
		for key := range body {
			if key != "error" && key != "hint" {
				t.Fatalf("GET %s: denied response exposes %s", path, key)
			}
		}
	}
	return body
}

func checkCatalogReadAllowed(t *testing.T, s *Server, mux *http.ServeMux, id string, headers map[string]string) {
	t.Helper()
	base := "/api/profile-catalogs/" + id
	body := catalogReadJSON(t, mux, base, headers, http.StatusOK)
	var summary catalog.CatalogSummary
	if err := json.Unmarshal(body["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	if len(body["error"]) != 0 || string(body["workspace"]) != "true" || summary.TableCount != 1 || summary.ColumnCount != 1 || summary.DataDir != s.profileCatalogDir(id) {
		t.Fatalf("catalog summary is not the fixture: %s", body)
	}
	body = catalogReadJSON(t, mux, base+"/dataset/physical_models", headers, http.StatusOK)
	var rows []struct {
		Table       string `json:"table_name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body["content"], &rows); err != nil {
		t.Fatal(err)
	}
	if len(body["error"]) != 0 || len(rows) != 1 || rows[0].Table != id || rows[0].Description != "catalog-read-fixture" {
		t.Fatalf("dataset is not the fixture: %s", body)
	}
	// Reset previous connection failures so every actor reaches the real driver.
	s.DB.Invalidate(id)
	// The real PostgreSQL driver must attempt the missing local Unix socket.
	// This proves authorized dispatch, not successful live schema discovery.
	body = catalogReadJSON(t, mux, base+"/schemas", headers, http.StatusOK)
	var message string
	if err := json.Unmarshal(body["error"], &message); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "dial unix") || !strings.Contains(message, "no such file or directory") {
		t.Fatalf("expected bounded driver connection failure, got %q", message)
	}
}

func checkCatalogReadList(t *testing.T, mux *http.ServeMux, headers map[string]string, want map[string]bool) {
	t.Helper()
	body := catalogReadJSON(t, mux, "/api/profile-catalogs", headers, http.StatusOK)
	var profiles []struct {
		Profile string `json:"profile"`
		Tables  int    `json:"tables"`
	}
	var count int
	if err := json.Unmarshal(body["profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body["count"], &count); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range profiles {
		if got[p.Profile] || p.Tables != 1 {
			t.Fatalf("duplicate or invalid workspace: %+v", p)
		}
		got[p.Profile] = true
	}
	if count != len(profiles) || !reflect.DeepEqual(got, want) {
		t.Fatalf("list=%v count=%d want=%v", got, count, want)
	}
}

func TestProfileCatalogReadAuthorization(t *testing.T) {
	s, mux, adminCookie, aliceCookie := newAuthServer(t)
	t.Cleanup(s.DB.Close)
	s.Options.AdminToken = "catalog-read-test-master"
	// Active catalog information requires authentication even when the user has no profiles.
	checkCatalogReadList(t, mux, withCookie(aliceCookie), map[string]bool{})
	active := catalogReadJSON(t, mux, "/api/profile-catalogs/active", withCookie(aliceCookie), http.StatusOK)
	if string(active["is_default"]) != "true" || len(active["operational_dir"]) == 0 {
		t.Fatalf("active catalog unavailable without profile grants: %s", active)
	}
	ids := []string{"alice-owned", "admin-granted", "admin-shared", "admin-private"}
	for _, id := range ids {
		cookie, visibility := adminCookie, "private"
		if id == "alice-owned" {
			cookie = aliceCookie
		}
		if id == "admin-shared" {
			visibility = "shared"
		}
		// A nonexistent socket under TempDir cannot reach an operational DB.
		body, err := json.Marshal(map[string]any{"id": id, "type": "postgres", "connect_string": "postgres:///catalog?host=" + filepath.Join(t.TempDir(), "missing") + "&connect_timeout=1", "username": "fixture", "password_ref": "plain:fixture", "visibility": visibility, "policy": map[string]int{"query_timeout_seconds": 1, "connection_test_timeout_seconds": 1}})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReq(t, mux, "POST", "/api/db-profiles", string(body), withCookie(cookie))
		if rec.Code != http.StatusOK {
			t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
		}
		seedCatalogReadWorkspace(t, s, id)
	}
	rec := doReq(t, mux, "PUT", "/api/db-profiles/admin-granted/grants", `{"username":"alice","permission":"use"}`, withCookie(adminCookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "POST", "/api/mcp-keys", `{"name":"catalog-read","ttl_hours":0}`, withCookie(aliceCookie))
	var issued struct {
		Key  string `json:"key"`
		Info struct {
			ID string `json:"id"`
		} `json:"key_info"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &issued) != nil || !strings.HasPrefix(issued.Key, "ssk_") {
		t.Fatal("MCP key issuance failed")
	}
	paths := []string{"/api/profile-catalogs", "/api/profile-catalogs/active", "/api/profile-catalogs/alice-owned", "/api/profile-catalogs/alice-owned/schemas", "/api/profile-catalogs/alice-owned/dataset/physical_models"}
	for name, headers := range map[string]map[string]string{
		"anonymous": nil, "invalid-cookie": withCookie("invalid"), "invalid-key": {"X-MCP-Key": "ssk_invalid"}, "invalid-master": {"X-Admin-Token": "invalid"}, "invalid-bearer": {"Authorization": "Bearer invalid"},
	} {
		for _, path := range paths {
			t.Run(name+path, func(t *testing.T) { catalogReadJSON(t, mux, path, headers, http.StatusUnauthorized) })
		}
	}
	aliceProfiles := map[string]bool{"alice-owned": true, "admin-granted": true, "admin-shared": true}
	allProfiles := map[string]bool{"alice-owned": true, "admin-granted": true, "admin-shared": true, "admin-private": true}
	for _, actor := range []struct {
		name    string
		headers map[string]string
		allowed map[string]bool
	}{
		{"alice-cookie", withCookie(aliceCookie), aliceProfiles},
		{"alice-key", map[string]string{"Authorization": "Bearer " + issued.Key}, aliceProfiles},
		{"admin-cookie", withCookie(adminCookie), allProfiles},
		{"master", map[string]string{"X-Admin-Token": s.Options.AdminToken}, allProfiles},
	} {
		t.Run(actor.name, func(t *testing.T) {
			t.Run("list", func(t *testing.T) { checkCatalogReadList(t, mux, actor.headers, actor.allowed) })
			t.Run("active", func(t *testing.T) {
				body := catalogReadJSON(t, mux, "/api/profile-catalogs/active", actor.headers, http.StatusOK)
				expected, err := json.Marshal(s.activeCatalogInfo())
				if err != nil {
					t.Fatal(err)
				}
				var want map[string]json.RawMessage
				if err := json.Unmarshal(expected, &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("active response changed: %s", body)
				}
			})
			for _, id := range ids {
				t.Run(id, func(t *testing.T) {
					if actor.allowed[id] {
						checkCatalogReadAllowed(t, s, mux, id, actor.headers)
						return
					}
					for _, suffix := range []string{"", "/schemas", "/dataset/physical_models"} {
						catalogReadJSON(t, mux, "/api/profile-catalogs/"+id+suffix, actor.headers, http.StatusForbidden)
					}
				})
			}
		})
	}
	// A real issued key must stop authenticating every read route after revocation.
	rec = doReq(t, mux, "DELETE", "/api/mcp-keys/"+issued.Info.ID, "", withCookie(aliceCookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke key: status=%d", rec.Code)
	}
	for _, path := range paths {
		t.Run("revoked-key"+path, func(t *testing.T) {
			catalogReadJSON(t, mux, path, map[string]string{"Authorization": "Bearer " + issued.Key}, http.StatusUnauthorized)
		})
	}

}

func TestProfileCatalogReadStandaloneCompatibility(t *testing.T) {
	for _, token := range []string{"", "catalog-read-test-master"} {
		t.Run(fmt.Sprintf("token-configured=%t", token != ""), func(t *testing.T) {
			s, mux := newAdminMux(t, token)
			t.Cleanup(s.DB.Close)
			id := "standalone"
			profiles, err := dbconn.UpsertProfile(s.opDir(), dbconn.Profile{ID: id, Type: "postgres", ConnectString: "postgres:///catalog?host=" + filepath.Join(t.TempDir(), "missing") + "&connect_timeout=1", Username: "fixture", PasswordRef: "plain:fixture"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := dbconn.SaveProfiles(s.opDir(), profiles); err != nil {
				t.Fatal(err)
			}
			seedCatalogReadWorkspace(t, s, id)
			checkCatalogReadList(t, mux, nil, map[string]bool{id: true})
			checkCatalogReadAllowed(t, s, mux, id, nil)
			body := catalogReadJSON(t, mux, "/api/profile-catalogs/active", nil, http.StatusOK)
			if string(body["is_default"]) != "true" || string(body["can_activate"]) != "true" {
				t.Fatalf("standalone active response changed: %s", body)
			}
		})
	}
}
