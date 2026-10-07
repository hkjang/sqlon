package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

	// the default landing off: sent to the first page they can open, quietly
	// (nobody asked for the root), while a page someone did ask for says why
	putMenus(t, mux, `{"menus":{"fleet":{"enabled":false}}}`, admin)
	expectPage(t, mux, "/", alice, "/admin/alerts")

	// the switches survive a restart, and the file holds only decisions
	s.menuLoaded = false
	if h := strings.Join(hiddenFor(t, mux, alice), ","); h != "fleet" {
		t.Fatalf("after reload alice hidden = %s", h)
	}
	raw, err := os.ReadFile(s.menuConfigPath())
	var file menuConfig
	if err == nil {
		err = json.Unmarshal(raw, &file)
	}
	if err != nil || len(file.Menus) != 1 || !file.Menus["fleet"].off() || file.UpdatedBy != "admin" {
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
	expectPage(t, mux, "/", admin, "/admin/menus") // 메뉴 관리 is never switched off
	expectPage(t, mux, "/admin/sessions", admin, "/admin/menus?menu_off=sessions")
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

// Two admins editing at once: the second save names the revision it started
// from and is refused instead of silently undoing the first.
func TestMenuSaveRefusesAStaleRevision(t *testing.T) {
	_, mux, adminTok, _ := newAuthServer(t)
	admin := withCookie(adminTok)
	var got struct {
		Revision int `json:"revision"`
	}
	_ = json.Unmarshal(doReq(t, mux, "GET", "/api/console/menus", "", admin).Body.Bytes(), &got)
	if got.Revision != 0 {
		t.Fatalf("fresh revision = %d", got.Revision)
	}
	out := putMenus(t, mux, `{"revision":0,"menus":{"openmetadata":{"enabled":false}}}`, admin)
	if out["revision"].(float64) != 1 {
		t.Fatalf("revision after a save = %v", out["revision"])
	}
	rec := doReq(t, mux, "PUT", "/api/console/menus", `{"revision":0,"menus":{"ask":{"roles":["admin"]}}}`, admin)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "먼저 저장했습니다") || !strings.Contains(rec.Body.String(), `"revision":1`) {
		t.Fatalf("stale save: %d %s", rec.Code, rec.Body.String())
	}
	// the stale save changed nothing
	var now struct {
		Menus []menuView `json:"menus"`
	}
	_ = json.Unmarshal(doReq(t, mux, "GET", "/api/console/menus", "", admin).Body.Bytes(), &now)
	for _, m := range now.Menus {
		if m.Key == "ask" && m.RolesLimited {
			t.Fatal("a refused save was applied")
		}
	}
	// a save that changes nothing is not a new revision
	out = putMenus(t, mux, `{"revision":1,"menus":{"openmetadata":{"enabled":false}}}`, admin)
	if out["unchanged"] != true || out["revision"].(float64) != 1 {
		t.Fatalf("no-op save: %v", out)
	}
	// omitting the revision is an explicit "save regardless"
	if out := putMenus(t, mux, `{"menus":{}}`, admin); out["revision"].(float64) != 2 {
		t.Fatalf("save without revision: %v", out["revision"])
	}
}

// The page lists what each save changed, and the audit log records the change
// rather than the resulting state.
func TestMenuHistoryRecordsChanges(t *testing.T) {
	s, mux, adminTok, _ := newAuthServer(t)
	admin := withCookie(adminTok)
	putMenus(t, mux, `{"menus":{"openmetadata":{"enabled":false},"ask":{"roles":["admin","dba"]}}}`, admin)
	putMenus(t, mux, `{"menus":{"ask":{"roles":["admin","dba"]}}}`, admin)
	var got struct {
		History []menuHistoryEntry `json:"history"`
	}
	_ = json.Unmarshal(doReq(t, mux, "GET", "/api/console/menus", "", admin).Body.Bytes(), &got)
	b, _ := json.Marshal(got.History)
	h := got.History
	if len(h) != 2 || h[0].Revision != 2 || h[0].By != "admin" || h[0].At == "" ||
		!strings.Contains(string(b), `"changes":[{"key":"openmetadata","from":{"enabled":false},"to":{}}]`) ||
		!strings.Contains(string(b), `"changes":[{"key":"ask","from":{},"to":{"roles":["admin","dba"]}},{"key":"openmetadata","from":{},"to":{"enabled":false}}]`) {
		t.Fatalf("history = %s", b)
	}
	audit, _ := filepath.Glob(filepath.Join(s.opDir(), "audit", "audit-*.jsonl"))
	if len(audit) == 0 {
		t.Fatal("no audit file")
	}
	raw, _ := os.ReadFile(audit[0])
	if !strings.Contains(string(raw), "rev 1: ask roles all→admin+dba; openmetadata on→off by admin") ||
		!strings.Contains(string(raw), "rev 2: openmetadata off→on by admin") {
		t.Fatalf("audit lines:\n%s", raw)
	}
	// history is bounded; the audit log keeps the rest
	for i := 0; i < menuHistoryKeep+5; i++ {
		body := `{"menus":{}}`
		if i%2 == 0 {
			body = `{"menus":{"stats":{"enabled":false}}}`
		}
		putMenus(t, mux, body, admin)
	}
	if h := s.menus().History; len(h) != menuHistoryKeep || h[0].Revision != menuHistoryKeep+7 {
		t.Fatalf("history kept %d, newest rev %d", len(h), h[0].Revision)
	}
}

// A hand-edited or restored file is picked up without a restart, and one that
// breaks the rules is ignored (every menu on) with the reason on the page.
func TestMenuFileIsValidatedAndReloaded(t *testing.T) {
	s, mux, adminTok, aliceTok := newAuthServer(t)
	admin, alice := withCookie(adminTok), withCookie(aliceTok)
	if h := hiddenFor(t, mux, alice); len(h) != 0 {
		t.Fatalf("hidden = %v", h)
	}
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(s.menuConfigPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.menuConfigPath(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		s.menuChecked = time.Time{} // skip the once-a-second throttle
	}
	write(`{"revision":4,"menus":{"ask":{"roles":["admin","dba"]}}}`)
	if h := strings.Join(hiddenFor(t, mux, alice), ","); h != "ask" {
		t.Fatalf("after an outside edit alice hidden = %q", h)
	}
	// "users" is a typo for "user": it would hide SQL Lab from every role
	write(`{"revision":5,"menus":{"ask":{"roles":["users"]}}}`)
	if h := hiddenFor(t, mux, alice); len(h) != 0 {
		t.Fatalf("an invalid file must leave every menu on, hidden = %v", h)
	}
	rec := doReq(t, mux, "GET", "/api/console/menus", "", admin)
	body := rec.Body.String()
	if !strings.Contains(body, `"load_error"`) || !strings.Contains(body, "users 역할에게 열 수 없습니다") || strings.Contains(body, s.opDir()) || !strings.Contains(body, `"revision":5`) {
		t.Fatalf("menus with a bad file: %s", body)
	}
	// saving from the page repairs the file, even with nothing else changed
	out := putMenus(t, mux, `{"revision":5,"menus":{}}`, admin)
	if out["unchanged"] == true || out["revision"].(float64) != 6 {
		t.Fatalf("repair save: %v", out)
	}
	if strings.Contains(doReq(t, mux, "GET", "/api/console/menus", "", admin).Body.String(), "load_error") {
		t.Fatal("load_error survives a save")
	}
	if h := s.menus().History; len(h) == 0 || !h[0].Repaired {
		t.Fatalf("repair not recorded: %+v", h)
	}
}
