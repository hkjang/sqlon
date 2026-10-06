package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAdminMux(t *testing.T, token string) (*Server, *http.ServeMux) {
	t.Helper()
	s, _ := newFixtureServer(t)
	s.Options.AdminToken = token
	mux := http.NewServeMux()
	s.Register(mux)
	return s, mux
}

func doReq(t *testing.T, mux *http.ServeMux, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAdminAPILifecycle(t *testing.T) {
	s, mux := newAdminMux(t, "")

	// list
	rec := doReq(t, mux, "GET", "/api/datasets", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "physical_models") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String()[:200])
	}
	// detail
	rec = doReq(t, mux, "GET", "/api/datasets/physical_models?sample_rows=1", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "total_entries") {
		t.Fatalf("detail: %d", rec.Code)
	}
	// content of absent optional dataset -> 204
	rec = doReq(t, mux, "GET", "/api/datasets/glossary/content", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("absent content should be 204, got %d", rec.Code)
	}
	// put glossary
	rec = doReq(t, mux, "PUT", "/api/datasets/glossary",
		`{"entries":[{"term":"고객","synonyms":["cust_no"],"category":"entity"}]}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"applied":true`) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.cat().Glossary.Entries) != 1 {
		t.Fatal("catalog not hot-swapped via REST put")
	}
	// content now present
	rec = doReq(t, mux, "GET", "/api/datasets/glossary/content", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "고객") {
		t.Fatalf("content after put: %d", rec.Code)
	}
	// second put creates a backup of the first
	rec = doReq(t, mux, "PUT", "/api/datasets/glossary",
		`{"entries":[{"term":"잔액","synonyms":["bal"],"category":"metric"}]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("second put: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/api/datasets/glossary/backups", "", nil)
	var bl struct {
		Backups []struct{ Name string } `json:"backups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bl); err != nil || len(bl.Backups) == 0 {
		t.Fatalf("backups: %d %s", rec.Code, rec.Body.String())
	}
	// restore the first version
	rec = doReq(t, mux, "POST", "/api/datasets/glossary/restore",
		`{"backup":"`+bl.Backups[0].Name+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	if s.cat().Glossary.Entries[0].Term != "고객" {
		t.Fatalf("restore did not bring back first version: %+v", s.cat().Glossary.Entries)
	}
	// restore with traversal-ish name refused
	rec = doReq(t, mux, "POST", "/api/datasets/glossary/restore", `{"backup":"../glossary.json"}`, nil)
	if rec.Code != 400 {
		t.Fatalf("traversal restore must be 400, got %d", rec.Code)
	}
	// delete
	rec = doReq(t, mux, "DELETE", "/api/datasets/glossary", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"removed":true`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	// delete required refused
	rec = doReq(t, mux, "DELETE", "/api/datasets/physical_models", "", nil)
	if rec.Code != 400 {
		t.Fatalf("required delete must be 400, got %d", rec.Code)
	}
	// reload
	rec = doReq(t, mux, "POST", "/api/reload", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reloaded":true`) {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	// health
	rec = doReq(t, mux, "GET", "/api/health", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "status") {
		t.Fatalf("health: %d", rec.Code)
	}
}

func TestAdminTokenEnforcedOnMutations(t *testing.T) {
	_, mux := newAdminMux(t, "sekrit")
	// reads stay open
	if rec := doReq(t, mux, "GET", "/api/datasets", "", nil); rec.Code != 200 {
		t.Fatalf("read should not require token: %d", rec.Code)
	}
	// mutation without token -> 401
	rec := doReq(t, mux, "PUT", "/api/datasets/glossary", `{"entries":[]}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	// wrong token -> 401
	rec = doReq(t, mux, "PUT", "/api/datasets/glossary", `{"entries":[]}`, map[string]string{"X-Admin-Token": "nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %d", rec.Code)
	}
	// correct token via header and via bearer
	rec = doReq(t, mux, "PUT", "/api/datasets/glossary", `{"entries":[]}`, map[string]string{"X-Admin-Token": "sekrit"})
	if rec.Code != 200 {
		t.Fatalf("expected 200 with token, got %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "POST", "/api/reload", "", map[string]string{"Authorization": "Bearer sekrit"})
	if rec.Code != 200 {
		t.Fatalf("expected 200 with bearer, got %d", rec.Code)
	}
}

func TestAdminStaticPages(t *testing.T) {
	_, mux := newAdminMux(t, "")
	for path, needle := range map[string]string{
		"/admin":                     "<h1>데이터셋",
		"/admin/editor":              "<h1>테이블 편집",
		"/admin/changes":             "변경 관리",
		"/admin/availability":        "복제 · 백업",
		"/admin/security":            "보안 · 권한",
		"/docs":                      "swagger-ui",
		"/openapi.json":              "SQLON AI Database Operations API",
		"/docs/swagger-ui.css":       "swagger-ui",
		"/docs/swagger-ui-bundle.js": "webpack",
	} {
		rec := doReq(t, mux, "GET", path, "", nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), needle) {
			t.Fatalf("%s: code=%d, needle %q not found", path, rec.Code, needle)
		}
	}
	var spec map[string]any
	rec := doReq(t, mux, "GET", "/openapi.json", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	if spec["openapi"] != "3.0.3" {
		t.Fatalf("unexpected openapi version: %v", spec["openapi"])
	}
}

// Every console page carries the design system: the stylesheet, and the
// head script that sets the theme before the first paint. A page that
// forgets them renders unstyled or flashes the wrong theme.
func TestConsolePagesLoadTheDesignSystem(t *testing.T) {
	_, mux := newAdminMux(t, "")
	for _, c := range []struct{ path, ctype, needle string }{
		{"/admin/ui.css", "text/css", "--accent-solid"},
		{"/admin/theme.js", "application/javascript", "sqlon-theme"},
		{"/admin/nav.js", "application/javascript", "openPalette"},
	} {
		rec := doReq(t, mux, "GET", c.path, "", nil)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), c.ctype) || !strings.Contains(rec.Body.String(), c.needle) {
			t.Fatalf("%s: %d %q", c.path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	entries, err := webuiFS.ReadDir("webui")
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".html") || e.Name() == "docs.html" {
			continue
		}
		b, _ := webuiFS.ReadFile("webui/" + e.Name())
		html := string(b)
		pages++
		if !strings.Contains(html, `<link rel="stylesheet" href="/admin/ui.css">`) || !strings.Contains(html, `<script src="/admin/theme.js"`) {
			t.Errorf("%s does not load ui.css and theme.js", e.Name())
		}
		if strings.Contains(html, "/admin/nav.js") && !strings.Contains(html, `<script src="/admin/theme.js" data-shell>`) {
			t.Errorf("%s mounts the shell but its theme.js lacks data-shell (the sidebar would jump in after paint)", e.Name())
		}
		// shell pages were written with their own element defaults, which
		// the design system has to override (login/landing build on it)
		if strings.Contains(html, "/admin/nav.js") && strings.Index(html, "/admin/ui.css") < strings.Index(html, "</style>") {
			t.Errorf("%s links ui.css before its own <style>; the design system must come after to win", e.Name())
		}
	}
	if pages < 25 {
		t.Fatalf("expected the console pages, found %d", pages)
	}
}
