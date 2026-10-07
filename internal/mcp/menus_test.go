package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"sqlon/internal/meta"
)

// The server refuses pages by consoleMenus and the sidebar draws GROUPS in
// nav.js; if they drift, a menu is hidden without its page being refused (or
// the other way round).
func TestConsoleMenusMatchNav(t *testing.T) {
	b, err := webuiFS.ReadFile("webui/nav.js")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\{ key: '([^']+)', href: '([^']+)', icon: '[^']+', label: '[^']+', show: '([^']+)'`)
	var nav []consoleMenu
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		nav = append(nav, consoleMenu{Key: m[1], Path: strings.SplitN(m[2], "#", 2)[0], Rule: m[3]})
	}
	if len(nav) != len(consoleMenus) {
		t.Fatalf("nav.js has %d menus, consoleMenus %d", len(nav), len(consoleMenus))
	}
	for i, m := range consoleMenus {
		if nav[i].Key != m.Key || nav[i].Path != m.Path || nav[i].Rule != m.Rule {
			t.Errorf("menu %d: nav.js %+v, consoleMenus {%s %s %s}", i, nav[i], m.Key, m.Path, m.Rule)
		}
	}
}

func putMenus(t *testing.T, mux *http.ServeMux, body string, hdr map[string]string) map[string]any {
	t.Helper()
	rec := doReq(t, mux, "PUT", "/api/console/menus", body, hdr)
	if rec.Code != 200 {
		t.Fatalf("PUT menus %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func hiddenFor(t *testing.T, mux *http.ServeMux, hdr map[string]string) []string {
	t.Helper()
	var me struct {
		Hidden []string `json:"hidden_menus"`
	}
	rec := doReq(t, mux, "GET", "/auth/me", "", hdr)
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.Hidden == nil {
		t.Fatalf("/auth/me hidden_menus: %s", rec.Body.String())
	}
	return me.Hidden
}

// expectPage asserts a page answers 200, or redirects to want.
func expectPage(t *testing.T, mux *http.ServeMux, path string, hdr map[string]string, want string) {
	t.Helper()
	rec := doReq(t, mux, "GET", path, "", hdr)
	if want == "" {
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s, want 200", path, rec.Code, rec.Header().Get("Location"))
		}
		return
	}
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
		t.Fatalf("GET %s: %d → %q, want 302 → %q", path, rec.Code, rec.Header().Get("Location"), want)
	}
}

func TestMenuSwitchesLoginMode(t *testing.T) {
	s, mux, adminTok, aliceTok := newAuthServer(t)
	if _, err := s.Meta.CreateLocalUser(context.Background(), "bob", "bobpass12", meta.RoleDBA, "Bob", ""); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, mux, "POST", "/auth/login", `{"username":"bob","password":"bobpass12"}`, nil)
	var bobTok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == meta.SessionCookie {
			bobTok = c.Value
		}
	}
	admin, alice, bob := withCookie(adminTok), withCookie(aliceTok), withCookie(bobTok)

	// default: everything on
	if h := hiddenFor(t, mux, alice); len(h) != 0 {
		t.Fatalf("hidden before any switch: %v", h)
	}
	expectPage(t, mux, "/admin/openmetadata", alice, "")

	// only an admin reads or changes the switches
	if rec := doReq(t, mux, "GET", "/api/console/menus", "", alice); rec.Code != 403 {
		t.Fatalf("user GET menus: %d", rec.Code)
	}
	if rec := doReq(t, mux, "PUT", "/api/console/menus", `{"menus":{}}`, alice); rec.Code != 403 {
		t.Fatalf("user PUT menus: %d", rec.Code)
	}
	expectPage(t, mux, "/admin/menus", alice, "/admin/db")

	out := putMenus(t, mux, `{"menus":{"openmetadata":{"enabled":false},"ask":{"roles":["dba","admin"]},"workload":{"enabled":false}}}`, admin)
	if got := out["hidden_for_you"]; len(got.([]any)) != 2 { // openmetadata, workload
		t.Fatalf("hidden_for_you for admin: %v", got)
	}

	// off for everyone, narrowed to roles, and a page two menus share
	if h := strings.Join(hiddenFor(t, mux, alice), ","); h != "workload,ask,openmetadata" {
		t.Fatalf("alice hidden = %s", h)
	}
	expectPage(t, mux, "/admin/ask", alice, "/?menu_off=ask")
	expectPage(t, mux, "/admin/openmetadata", alice, "/?menu_off=openmetadata")
	expectPage(t, mux, "/admin/openmetadata", admin, "/?menu_off=openmetadata")
	expectPage(t, mux, "/admin/ask", admin, "")
	expectPage(t, mux, "/admin/ask", bob, "")
	expectPage(t, mux, "/admin/workload", alice, "") // 객체 · 용량 is still on

	// an admin-only page sends a user to DB 연결, or past it when it is off
	putMenus(t, mux, `{"menus":{"db":{"roles":["admin","dba"]}}}`, admin)
	expectPage(t, mux, "/admin/users", alice, "/")
	expectPage(t, mux, "/admin/users", bob, "/admin/db")

	// the default landing off: sent to the first page they can open
	putMenus(t, mux, `{"menus":{"fleet":{"enabled":false}}}`, admin)
	expectPage(t, mux, "/", alice, "/admin/alerts?menu_off=fleet")

	// the switches survive a restart, and the file holds only decisions
	s.menuLoaded = false
	if h := strings.Join(hiddenFor(t, mux, alice), ","); h != "fleet" {
		t.Fatalf("after reload alice hidden = %s", h)
	}
	raw, err := os.ReadFile(s.menuConfigPath())
	if err != nil || !strings.Contains(string(raw), `"fleet"`) || strings.Contains(string(raw), `"ask"`) || !strings.Contains(string(raw), `"updated_by": "admin"`) {
		t.Fatalf("menus.json: %v %s", err, raw)
	}

	// nothing left for a role: an explanation instead of a redirect loop
	all := map[string]menuOverride{}
	f := false
	for _, m := range consoleMenus {
		if !m.Locked {
			all[m.Key] = menuOverride{Enabled: &f}
		}
	}
	body, _ := json.Marshal(map[string]any{"menus": all})
	putMenus(t, mux, string(body), admin)
	rec = doReq(t, mux, "GET", "/", "", alice)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "열 수 있는 화면이 없습니다") {
		t.Fatalf("alice with no menus: %d %s", rec.Code, rec.Body.String())
	}
	expectPage(t, mux, "/", admin, "/admin/menus?menu_off=fleet") // 메뉴 관리 is never switched off
}

func TestMenuSwitchesRejectBadRequests(t *testing.T) {
	_, mux, adminTok, _ := newAuthServer(t)
	admin := withCookie(adminTok)
	for body, want := range map[string]string{
		`{"menus":{"nope":{"enabled":false}}}`:    "알 수 없는 메뉴",
		`{"menus":{"menus":{"enabled":false}}}`:   "끄거나 역할을 좁힐 수 없습니다",
		`{"menus":{"menus":{"roles":["admin"]}}}`: "끄거나 역할을 좁힐 수 없습니다",
		`{"menus":{"users":{"roles":["user"]}}}`:  "user 역할에게 열 수 없습니다",
		`{"menus":{"ask":{"roles":[]}}}`:          "역할을 하나 이상",
		`{"menus":{"ask":{"roles":["root"]}}}`:    "root 역할에게 열 수 없습니다",
	} {
		rec := doReq(t, mux, "PUT", "/api/console/menus", body, admin)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s, want 400 %q", body, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestMenuSwitchesStandalone(t *testing.T) {
	_, mux := newAdminMux(t, "tok")
	tok := map[string]string{"X-Admin-Token": "tok"}
	rec := doReq(t, mux, "GET", "/api/console/menus", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"auth_enabled":false`) {
		t.Fatalf("standalone GET menus: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "PUT", "/api/console/menus", `{"menus":{}}`, nil); rec.Code != 401 {
		t.Fatalf("standalone PUT without the admin token: %d", rec.Code)
	}
	expectPage(t, mux, "/admin/menus", nil, "")

	// no roles here: a role narrowing has nothing to apply to
	putMenus(t, mux, `{"menus":{"sessions":{"enabled":false},"ask":{"roles":["admin"]}}}`, tok)
	if h := strings.Join(hiddenFor(t, mux, nil), ","); h != "sessions" {
		t.Fatalf("standalone hidden = %s", h)
	}
	expectPage(t, mux, "/admin/sessions", nil, "/?menu_off=sessions")
	expectPage(t, mux, "/admin/ask", nil, "")
}

func TestNormalizeMenusKeepsOnlyDecisions(t *testing.T) {
	tr, f := true, false
	got, err := normalizeMenus(map[string]menuOverride{
		"fleet":   {Enabled: &tr},                                  // default
		"changes": {Roles: []string{"dba", "admin"}},               // every allowed role
		"ask":     {Roles: []string{"user", "admin", "user"}},      // dedupe + order
		"stats":   {Enabled: &f, Roles: []string{"admin"}},         // off wins, roles kept
		"keys":    {Enabled: &tr, Roles: []string{"admin", "dba"}}, // narrowed
		"history": {Enabled: &tr, Roles: []string{"admin", "dba", "user"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	want := `{"ask":{"roles":["admin","user"]},"keys":{"roles":["admin","dba"]},"stats":{"enabled":false,"roles":["admin"]}}`
	if string(b) != want {
		t.Fatalf("normalized = %s\nwant        %s", b, want)
	}
}
