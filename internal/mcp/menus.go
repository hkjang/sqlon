package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"sqlon/internal/meta"
)

// Console menu switches. An admin can turn any console menu off for everyone
// or narrow it to some roles. The page behind a hidden menu is refused as
// well, so hiding a menu does not leave it one bookmark away. This governs the
// console only: REST and MCP access stay with roles and keys.
//
// consoleMenus mirrors GROUPS in webui/nav.js (order, key, path, rule);
// TestConsoleMenusMatchNav keeps the two from drifting.

type consoleMenu struct {
	Key  string
	Path string // page path the menu opens (fragment dropped)
	// Rule is nav.js `show`: always | auth (login mode) | admin | dba
	// (dba/admin, or standalone) | manage (admin, or standalone).
	Rule string
	// Locked menus cannot be switched off: the menu page itself, so an admin
	// can always undo what they did.
	Locked bool
}

var consoleMenus = []consoleMenu{
	{Key: "fleet", Path: "/", Rule: "always"},
	{Key: "alerts", Path: "/admin/alerts", Rule: "always"},
	{Key: "sessions", Path: "/admin/sessions", Rule: "always"},
	{Key: "workload", Path: "/admin/workload", Rule: "always"},
	{Key: "capacity", Path: "/admin/workload", Rule: "always"},
	{Key: "availability", Path: "/admin/availability", Rule: "always"},
	{Key: "maintenance", Path: "/admin/maintenance", Rule: "always"},
	{Key: "dba", Path: "/admin/dba", Rule: "always"},
	{Key: "security", Path: "/admin/security", Rule: "always"},
	{Key: "compliance", Path: "/admin/compliance", Rule: "always"},
	{Key: "changes", Path: "/admin/changes", Rule: "dba"},
	{Key: "dba-console", Path: "/admin/dba-console", Rule: "dba"},
	{Key: "ask", Path: "/admin/ask", Rule: "always"},
	{Key: "history", Path: "/admin/history", Rule: "auth"},
	{Key: "stats", Path: "/admin/stats", Rule: "auth"},
	{Key: "datasets", Path: "/admin", Rule: "always"},
	{Key: "editor", Path: "/admin/editor", Rule: "always"},
	{Key: "reviews", Path: "/admin/reviews", Rule: "always"},
	{Key: "quality", Path: "/admin/quality", Rule: "always"},
	{Key: "profcat", Path: "/admin/profile-catalogs", Rule: "always"},
	{Key: "openmetadata", Path: "/admin/openmetadata", Rule: "always"},
	{Key: "db", Path: "/admin/db", Rule: "always"},
	{Key: "users", Path: "/admin/users", Rule: "admin"},
	{Key: "settings", Path: "/admin/settings", Rule: "admin"},
	{Key: "menus", Path: "/admin/menus", Rule: "manage", Locked: true},
	{Key: "keys", Path: "/admin/keys", Rule: "auth"},
}

var consoleRoles = []string{meta.RoleAdmin, meta.RoleDBA, meta.RoleUser}

// allowedRoles is who a menu's rule lets see it in login mode; a role
// setting can only narrow this, never widen it.
func (m consoleMenu) allowedRoles() []string {
	switch m.Rule {
	case "admin", "manage":
		return []string{meta.RoleAdmin}
	case "dba":
		return []string{meta.RoleAdmin, meta.RoleDBA}
	default:
		return append([]string(nil), consoleRoles...)
	}
}

func findConsoleMenu(key string) (consoleMenu, bool) {
	for _, m := range consoleMenus {
		if m.Key == key {
			return m, true
		}
	}
	return consoleMenu{}, false
}

// menuOverride is one admin decision. A menu without one is on for every
// role its rule allows.
type menuOverride struct {
	Enabled *bool    `json:"enabled,omitempty"` // nil = on
	Roles   []string `json:"roles,omitempty"`   // nil = every allowed role
}

type menuConfig struct {
	Menus     map[string]menuOverride `json:"menus"`
	UpdatedAt string                  `json:"updated_at,omitempty"`
	UpdatedBy string                  `json:"updated_by,omitempty"`
}

func (s *Server) menuConfigPath() string {
	return filepath.Join(s.opDir(), "operations", "console", "menus.json")
}

// menus returns the current switches, reading the file on first use. A file
// that cannot be read leaves every menu on (a broken file must not lock the
// console) and the error is reported on the menu page.
func (s *Server) menus() menuConfig {
	s.menuMu.RLock()
	if s.menuLoaded {
		cfg := s.menuCfg
		s.menuMu.RUnlock()
		return cfg
	}
	s.menuMu.RUnlock()
	s.menuMu.Lock()
	defer s.menuMu.Unlock()
	if !s.menuLoaded {
		s.menuCfg, s.menuLoadErr = readMenuConfig(s.menuConfigPath())
		if s.menuLoadErr != nil {
			log.Printf("console menus: %v (every menu stays on)", s.menuLoadErr)
		}
		s.menuLoaded = true
	}
	return s.menuCfg
}

func readMenuConfig(path string) (menuConfig, error) {
	cfg := menuConfig{Menus: map[string]menuOverride{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	var got menuConfig
	if err := json.Unmarshal(b, &got); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if got.Menus == nil {
		got.Menus = map[string]menuOverride{}
	}
	return got, nil
}

// baseVisible applies the menu's own rule, before any admin decision.
func (s *Server) baseVisible(m consoleMenu, u *meta.User) bool {
	if !s.authEnabled() {
		return m.Rule != "auth" && m.Rule != "admin"
	}
	switch m.Rule {
	case "admin", "manage":
		return u.IsAdmin()
	case "dba":
		return u.IsDBA()
	default: // always, auth
		return u != nil
	}
}

// switchedOff reports whether the admin's decision hides the menu from u.
// Roles only apply in login mode; standalone has no roles.
func (s *Server) switchedOff(m consoleMenu, u *meta.User, cfg menuConfig) bool {
	if m.Locked {
		return false
	}
	o, ok := cfg.Menus[m.Key]
	if !ok {
		return false
	}
	if o.Enabled != nil && !*o.Enabled {
		return true
	}
	if s.authEnabled() && o.Roles != nil {
		return u == nil || !slices.Contains(o.Roles, u.Role)
	}
	return false
}

func (s *Server) menuVisible(m consoleMenu, u *meta.User, cfg menuConfig) bool {
	return s.baseVisible(m, u) && !s.switchedOff(m, u, cfg)
}

// hiddenMenusFor lists the menus the admin's switches hide from u; nav.js
// drops them from the sidebar and the command palette.
func (s *Server) hiddenMenusFor(u *meta.User) []string {
	cfg := s.menus()
	out := []string{}
	for _, m := range consoleMenus {
		if s.switchedOff(m, u, cfg) {
			out = append(out, m.Key)
		}
	}
	return out
}

// menuGate refuses a console page whose every menu is hidden from u: it sends
// the viewer to the first page they can open, naming the refused menu so the
// shell can say why, or answers 403 when nothing is left to open.
func (s *Server) menuGate(w http.ResponseWriter, r *http.Request, u *meta.User) bool {
	cfg := s.menus()
	var refused string
	for _, m := range consoleMenus {
		if m.Path != r.URL.Path {
			continue
		}
		if s.menuVisible(m, u, cfg) {
			return true
		}
		if refused == "" {
			refused = m.Key
		}
	}
	if refused == "" {
		return true // not a menu page
	}
	for _, m := range consoleMenus {
		if m.Path != r.URL.Path && s.menuVisible(m, u, cfg) {
			http.Redirect(w, r, m.Path+"?menu_off="+url.QueryEscape(refused), http.StatusFound)
			return false
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(noMenuPage))
	return false
}

// landingFor is where a viewer goes from a page their role cannot open: DB
// 연결 as before, or the first page the menu switches leave them.
func (s *Server) landingFor(u *meta.User) string {
	cfg := s.menus()
	if m, ok := findConsoleMenu("db"); ok && s.menuVisible(m, u, cfg) {
		return m.Path
	}
	for _, m := range consoleMenus {
		if s.menuVisible(m, u, cfg) {
			return m.Path
		}
	}
	return "/"
}

const noMenuPage = `<!DOCTYPE html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>열 수 있는 화면 없음 · sqlon</title>
<link rel="stylesheet" href="/admin/ui.css"><script src="/admin/theme.js"></script>
<style>main{max-width:520px;margin:12vh auto;padding:0 16px}h1{font-size:20px;margin:0 0 8px}p{color:var(--muted)}</style>
</head><body><main><h1>열 수 있는 화면이 없습니다</h1>
<p>관리자가 이 계정의 역할에 보이는 콘솔 메뉴를 모두 꺼 두었습니다. 필요한 화면이 있으면 관리자에게 <b>메뉴 관리</b>에서 켜 달라고 요청하세요.</p>
<p><a href="/auth/logout" onclick="fetch('/auth/logout',{method:'POST'}).then(function(){location.href='/auth/login'});return false">로그아웃</a></p>
</main></body></html>`

// ---- REST ----

type menuView struct {
	Key          string   `json:"key"`
	Path         string   `json:"path"`
	Rule         string   `json:"rule"`
	Locked       bool     `json:"locked"`
	Enabled      bool     `json:"enabled"`
	Roles        []string `json:"roles"` // effective roles in login mode
	RolesLimited bool     `json:"roles_limited"`
	AllowedRoles []string `json:"allowed_roles"`
}

func (s *Server) menuViews() map[string]any {
	cfg := s.menus()
	views := make([]menuView, 0, len(consoleMenus))
	for _, m := range consoleMenus {
		v := menuView{Key: m.Key, Path: m.Path, Rule: m.Rule, Locked: m.Locked, Enabled: true,
			Roles: m.allowedRoles(), AllowedRoles: m.allowedRoles()}
		if o, ok := cfg.Menus[m.Key]; ok && !m.Locked {
			if o.Enabled != nil {
				v.Enabled = *o.Enabled
			}
			if o.Roles != nil {
				v.Roles, v.RolesLimited = o.Roles, true
			}
		}
		views = append(views, v)
	}
	out := map[string]any{
		"auth_enabled": s.authEnabled(),
		"roles":        consoleRoles,
		"menus":        views,
		"updated_at":   cfg.UpdatedAt,
		"updated_by":   cfg.UpdatedBy,
		"file":         s.menuConfigPath(),
	}
	s.menuMu.RLock()
	if s.menuLoadErr != nil {
		out["load_error"] = s.menuLoadErr.Error()
	}
	s.menuMu.RUnlock()
	return out
}

// normalizeMenus validates a requested set of switches and keeps only the
// ones that differ from the default, so the file lists real decisions.
func normalizeMenus(in map[string]menuOverride) (map[string]menuOverride, error) {
	out := map[string]menuOverride{}
	for key, o := range in {
		m, ok := findConsoleMenu(key)
		if !ok {
			return nil, fmt.Errorf("알 수 없는 메뉴: %q", key)
		}
		off := o.Enabled != nil && !*o.Enabled
		if m.Locked && (off || o.Roles != nil) {
			return nil, fmt.Errorf("%q 메뉴는 끄거나 역할을 좁힐 수 없습니다 (메뉴 설정으로 돌아올 길이 사라집니다)", key)
		}
		var roles []string
		if o.Roles != nil {
			if len(o.Roles) == 0 {
				return nil, fmt.Errorf("%q: 역할을 하나 이상 고르거나 메뉴를 끄세요", key)
			}
			allowed := m.allowedRoles()
			seen := map[string]bool{}
			for _, r := range o.Roles {
				if !slices.Contains(allowed, r) {
					return nil, fmt.Errorf("%q 메뉴는 %s 역할에게 열 수 없습니다 (허용: %s)", key, r, strings.Join(allowed, ", "))
				}
				if !seen[r] {
					seen[r] = true
					roles = append(roles, r)
				}
			}
			sort.Slice(roles, func(i, j int) bool { return roleOrder(roles[i]) < roleOrder(roles[j]) })
			if len(roles) == len(allowed) {
				roles = nil // every allowed role = no narrowing
			}
		}
		if !off && roles == nil {
			continue
		}
		n := menuOverride{Roles: roles}
		if off {
			f := false
			n.Enabled = &f
		}
		out[key] = n
	}
	return out, nil
}

func roleOrder(r string) int {
	for i, x := range consoleRoles {
		if x == r {
			return i
		}
	}
	return len(consoleRoles)
}

func (s *Server) registerMenus(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/menus", s.guardAdminPage(s.serveWebUI("webui/menus.html", "text/html; charset=utf-8")))
	mux.HandleFunc("GET /api/console/menus", func(w http.ResponseWriter, r *http.Request) {
		if s.authEnabled() && !s.requireAdmin(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, s.menuViews())
	})
	mux.HandleFunc("PUT /api/console/menus", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		var req struct {
			Menus map[string]menuOverride `json:"menus"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		menus, err := normalizeMenus(req.Menus)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		by := "admin-token"
		if s.authEnabled() {
			if u, err := s.authenticate(r); err == nil {
				by = actorName(u)
			}
		}
		next := menuConfig{Menus: menus, UpdatedAt: time.Now().Format(time.RFC3339), UpdatedBy: by}
		s.menuMu.Lock()
		err = writeJSONFileAtomic(s.menuConfigPath(), next)
		if err == nil {
			s.menuCfg, s.menuLoadErr, s.menuLoaded = next, nil, true
		}
		s.menuMu.Unlock()
		s.adminAudit(r, "console_menus_update", describeMenus(menus)+" by "+by, err)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		out := s.menuViews()
		var u *meta.User
		if s.authEnabled() {
			u, _ = s.authenticate(r)
		}
		out["hidden_for_you"] = s.hiddenMenusFor(u)
		writeJSON(w, http.StatusOK, out)
	})
}

func describeMenus(menus map[string]menuOverride) string {
	if len(menus) == 0 {
		return "all menus on"
	}
	var off, narrowed []string
	for _, m := range consoleMenus {
		o, ok := menus[m.Key]
		if !ok {
			continue
		}
		if o.Enabled != nil && !*o.Enabled {
			off = append(off, m.Key)
		} else if o.Roles != nil {
			narrowed = append(narrowed, m.Key+"="+strings.Join(o.Roles, "+"))
		}
	}
	parts := []string{}
	if len(off) > 0 {
		parts = append(parts, "off: "+strings.Join(off, ","))
	}
	if len(narrowed) > 0 {
		parts = append(parts, "roles: "+strings.Join(narrowed, ","))
	}
	return strings.Join(parts, "; ")
}

func writeJSONFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
