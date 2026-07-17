package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"sqlon/internal/meta"
)

// Authentication model
//
//	Meta == nil (standalone): everything behaves exactly as before — the
//	  optional AdminToken guards mutations, no login exists.
//	Meta != nil: requests are resolved to a user via (in order)
//	  1. master admin token (break-glass, acts as a synthetic admin)
//	  2. session cookie (browser console)
//	  3. MCP key: Authorization: Bearer ssk_... or X-MCP-Key
//	/mcp over HTTP requires authentication; stdio stays local-trusted.

type ctxKeyUser struct{}
type ctxKeyHTTPAdmin struct{}
type ctxKeySession struct{}

// withSession threads the MCP session id (Mcp-Session-Id) so activity records
// can correlate a driving prompt with the SQL later generated in the same
// conversation.
func withSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeySession{}, id)
}

func sessionFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeySession{}).(string)
	return id
}

// withHTTPAdmin marks (standalone HTTP) requests as having passed the master
// token check, so mutating/DB tools over MCP honor -admin-token.
func withHTTPAdmin(ctx context.Context, ok bool) context.Context {
	return context.WithValue(ctx, ctxKeyHTTPAdmin{}, ok)
}

// checkMasterToken reports whether the request carries the correct master
// token (constant-time). Only meaningful when a token is configured.
func (s *Server) checkMasterToken(r *http.Request) bool {
	t := strings.TrimSpace(s.Options.AdminToken)
	if t == "" {
		return true
	}
	got := r.Header.Get("X-Admin-Token")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1
}

// toolActorIsAdmin decides whether the MCP caller may run admin-only or
// DB-executing tools. Meta mode → admin role; standalone → the master-token
// flag (absent flag, e.g. stdio, means locally trusted).
func (s *Server) toolActorIsAdmin(ctx context.Context) bool {
	if s.authEnabled() {
		u := userFrom(ctx)
		return u != nil && u.IsAdmin()
	}
	if v, ok := ctx.Value(ctxKeyHTTPAdmin{}).(bool); ok {
		return v
	}
	return true // stdio / no token configured
}

// toolActorIsDBA decides whether the MCP caller may run privileged DBA tools.
// Meta mode → dba or admin role. Standalone → the same master-token flag as
// admin (locally trusted when no token is configured). DBA mutation is strictly
// more dangerous than catalog admin, so callers ALSO require the profile to opt
// in via DBAConfig (checked at the exec layer).
func (s *Server) toolActorIsDBA(ctx context.Context) bool {
	if s.authEnabled() {
		u := userFrom(ctx)
		return u != nil && u.IsDBA()
	}
	if v, ok := ctx.Value(ctxKeyHTTPAdmin{}).(bool); ok {
		return v
	}
	return true // stdio / no token configured
}

// newMasterUser returns a fresh synthetic identity for the -admin-token
// break-glass. A new copy per request avoids sharing a mutable pointer across
// concurrent requests.
func newMasterUser() *meta.User {
	return &meta.User{ID: "master", Username: "master-token", Role: meta.RoleAdmin, IsActive: true, Provider: "token"}
}

func withUser(ctx context.Context, u *meta.User) context.Context {
	return context.WithValue(ctx, ctxKeyUser{}, u)
}

// userFrom returns the authenticated user, or nil (standalone/stdio).
func userFrom(ctx context.Context) *meta.User {
	u, _ := ctx.Value(ctxKeyUser{}).(*meta.User)
	return u
}

// authEnabled reports whether the meta DB (and therefore auth) is active.
func (s *Server) authEnabled() bool { return s.Meta != nil }

func sessionToken(r *http.Request) string {
	for _, name := range []string{meta.SessionCookie, meta.LegacySessionCookie} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// authenticate resolves the request to a user. Never called in standalone.
func (s *Server) authenticate(r *http.Request) (*meta.User, error) {
	// 1. master token (constant-time compare)
	if t := strings.TrimSpace(s.Options.AdminToken); t != "" {
		got := r.Header.Get("X-Admin-Token")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			return newMasterUser(), nil
		}
	}
	// 2. session cookie
	if token := sessionToken(r); token != "" {
		if u, err := s.Meta.Authenticate(r.Context(), token); err == nil {
			return u, nil
		}
	}
	// 3. MCP key
	key := r.Header.Get("X-MCP-Key")
	if key == "" {
		if b := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); meta.IsMCPKeyFormat(b) {
			key = b
		}
	}
	if key != "" {
		u, _, err := s.Meta.AuthenticateKey(r.Context(), key)
		if err != nil {
			return nil, err
		}
		return u, nil
	}
	return nil, meta.ErrUnauthorized
}

// requireActor authenticates any active user (or acts as anonymous admin in
// standalone mode). Writes 401 on failure.
func (s *Server) requireActor(w http.ResponseWriter, r *http.Request) (*meta.User, bool) {
	if !s.authEnabled() {
		// standalone: AdminToken (if set) still guards mutating endpoints via
		// requireAdmin; actor-level endpoints are open as before.
		return nil, true
	}
	u, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "authentication required",
			"hint":  "세션 쿠키(로그인), MCP 키(Authorization: Bearer ssk_... 또는 X-MCP-Key), 또는 X-Admin-Token 중 하나가 필요합니다. 로그인: /auth/login",
		})
		return nil, false
	}
	return u, true
}

// actorName renders the acting identity for audit entries.
func actorName(u *meta.User) string {
	if u == nil {
		return ""
	}
	return u.Username
}

// isAdminActor: standalone → token check result decides; meta → role check.
func (s *Server) isAdminActor(u *meta.User) bool {
	if !s.authEnabled() {
		return true // standalone requireAdmin already validated the token
	}
	return u.IsAdmin()
}

// ---- HTTP surface ----

func (s *Server) registerAuth(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", s.serveWebUI("webui/login.html", "text/html; charset=utf-8"))
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/me", s.handleMe)
	mux.HandleFunc("PUT /auth/password", s.handleChangePassword)
	mux.HandleFunc("PUT /auth/profile", s.handleUpdateProfile)
	mux.HandleFunc("GET /auth/sso/login", s.handleSSOLogin)
	mux.HandleFunc("GET /auth/sso/callback", s.handleSSOCallback)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"auth_enabled": false, "authenticated": false,
			"version": Version,
			"note":    "meta DB 미설정 — 단독 모드(인증 비활성)입니다. -meta-db 로 활성화하세요."})
		return
	}
	u, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"auth_enabled": true, "authenticated": false,
			"version": Version, "sso_enabled": s.OIDC != nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth_enabled": true, "authenticated": true,
		"version": Version, "user": u, "sso_enabled": s.OIDC != nil})
}

// handleUpdateProfile lets a logged-in local user edit their own display name
// and email. Role, active state, and password are deliberately untouched here —
// role/active changes go through /api/users (admin only), password through
// /auth/password.
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireActor(w, r)
	if !ok {
		return
	}
	if u == nil || u.ID == "master" {
		writeAPIError(w, http.StatusBadRequest, errEmpty("개인정보 변경은 로그인한 로컬/SSO 계정에서만 가능합니다"))
		return
	}
	var req struct {
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	cur, err := s.Meta.Store.GetUserByID(r.Context(), u.ID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err)
		return
	}
	cur.DisplayName = strings.TrimSpace(req.DisplayName)
	cur.Email = strings.TrimSpace(req.Email)
	cur.PasswordHash = "" // NULLIF keeps the stored hash
	if err := s.Meta.Store.UpdateUser(r.Context(), cur); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	s.authAudit(r, "profile_update", u.Username)
	reloaded, _ := s.Meta.Store.GetUserByID(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": reloaded})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeAPIError(w, http.StatusServiceUnavailable, errEmpty("auth is disabled (no meta db configured)"))
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	u, token, err := s.Meta.Login(r.Context(), req.Username, req.Password, remoteIP(r), r.UserAgent())
	if err != nil {
		s.authAudit(r, "login_failed", req.Username)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "아이디 또는 비밀번호가 올바르지 않습니다."})
		return
	}
	s.setSessionCookie(w, r, token)
	s.authAudit(r, "login", u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.authEnabled() {
		if token := sessionToken(r); token != "" {
			_ = s.Meta.Logout(r.Context(), token)
		}
	}
	for _, name := range []string{meta.SessionCookie, meta.LegacySessionCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	s.authAudit(r, "logout", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireActor(w, r)
	if !ok {
		return
	}
	if u == nil || u.ID == "master" {
		writeAPIError(w, http.StatusBadRequest, errEmpty("password change requires a logged-in local account"))
		return
	}
	var req struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if u.Provider != meta.ProviderLocal {
		writeAPIError(w, http.StatusBadRequest, errEmpty("SSO 계정의 비밀번호는 Keycloak에서 관리됩니다"))
		return
	}
	if err := s.Meta.VerifyPassword(r.Context(), u, req.Old); err != nil {
		writeAPIError(w, http.StatusUnauthorized, errEmpty("현재 비밀번호가 올바르지 않습니다"))
		return
	}
	hash, err := meta.HashPassword(req.New)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	u.PasswordHash = hash
	if err := s.Meta.Store.UpdateUser(r.Context(), u); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	s.authAudit(r, "password_changed", u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// requestIsHTTPS reports whether the original client connection was TLS,
// honoring a TLS-terminating reverse proxy's X-Forwarded-Proto.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: meta.SessionCookie, Value: token, Path: "/",
		MaxAge: int(meta.SessionTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: requestIsHTTPS(r),
	})
}

// guardPage protects an authenticated HTML page: with auth enabled and no
// valid session, redirect to login.
func (s *Server) guardPage(next http.HandlerFunc) http.HandlerFunc {
	return s.guard(next, false)
}

// guardAdminPage additionally requires the admin role; non-admins are sent to
// the DB console (their default landing) rather than shown an admin shell.
func (s *Server) guardAdminPage(next http.HandlerFunc) http.HandlerFunc {
	return s.guard(next, true)
}

func (s *Server) guard(next http.HandlerFunc, adminOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authEnabled() {
			var u *meta.User
			if token := sessionToken(r); token != "" {
				u, _ = s.Meta.Authenticate(r.Context(), token)
			}
			if u == nil {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.Path), http.StatusFound)
				return
			}
			if adminOnly && !u.IsAdmin() {
				http.Redirect(w, r, "/admin/db", http.StatusFound)
				return
			}
		}
		next(w, r)
	}
}

func remoteIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		return strings.TrimSpace(strings.Split(xf, ",")[0])
	}
	return r.RemoteAddr
}

// authAudit records auth events into the shared audit JSONL.
func (s *Server) authAudit(r *http.Request, action, subject string) {
	s.adminAudit(r, "auth_"+action, subject, nil)
}

// ---- Keycloak OIDC (authorization code + userinfo) ----

// OIDCProvider implements the confidential-client authorization code flow.
// Token validation is delegated to the provider's userinfo endpoint (the
// access token is verified server-side by Keycloak), avoiding local JWT/JWKS
// handling entirely.
type OIDCProvider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string // e.g. https://host:9797/auth/sso/callback

	mu   sync.Mutex
	disc *oidcDiscovery
	hc   *http.Client
}

type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

func (p *OIDCProvider) client() *http.Client {
	if p.hc == nil {
		p.hc = &http.Client{Timeout: 10 * time.Second}
	}
	return p.hc
}

func (p *OIDCProvider) discover(ctx context.Context) (*oidcDiscovery, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disc != nil {
		return p.disc, nil
	}
	u := strings.TrimRight(p.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("oidc discovery returned %d", res.StatusCode)
	}
	var d oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&d); err != nil {
		return nil, err
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.UserinfoEndpoint == "" {
		return nil, errors.New("oidc discovery document is missing endpoints")
	}
	p.disc = &d
	return p.disc, nil
}

func (s *Server) handleSSOLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.OIDC == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errEmpty("SSO is not configured (set SQLON_OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET/REDIRECT_URL)"))
		return
	}
	d, err := s.OIDC.discover(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err)
		return
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	state := hex.EncodeToString(b[:])
	http.SetCookie(w, &http.Cookie{Name: "sqlon_oauth_state", Value: state, Path: "/auth/sso",
		MaxAge: 300, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: requestIsHTTPS(r)})
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {s.OIDC.ClientID},
		"redirect_uri":  {s.OIDC.RedirectURL},
		"scope":         {"openid profile email"},
		"state":         {state},
	}
	http.Redirect(w, r, d.AuthorizationEndpoint+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.OIDC == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errEmpty("SSO is not configured"))
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "SSO 오류: "+e+" — "+r.URL.Query().Get("error_description"), http.StatusBadGateway)
		return
	}
	stateCookie, err := r.Cookie("sqlon_oauth_state")
	if err != nil {
		stateCookie, err = r.Cookie("jamypg_oauth_state")
	}
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		http.Error(w, "SSO state 불일치 — 로그인 페이지에서 다시 시도하세요", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sqlon_oauth_state", Value: "", Path: "/auth/sso", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "jamypg_oauth_state", Value: "", Path: "/auth/sso", MaxAge: -1})
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}
	d, err := s.OIDC.discover(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err)
		return
	}
	// token exchange (client_secret_post)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.OIDC.RedirectURL},
		"client_id":     {s.OIDC.ClientID},
		"client_secret": {s.OIDC.ClientSecret},
	}
	tokReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRes, err := s.OIDC.client().Do(tokReq)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, fmt.Errorf("token exchange failed: %w", err))
		return
	}
	defer tokRes.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(tokRes.Body, 1<<20)).Decode(&tok); err != nil || tok.AccessToken == "" {
		http.Error(w, "token exchange failed: "+tok.Error+" "+tok.ErrorDesc, http.StatusBadGateway)
		return
	}
	// userinfo — validates the access token at the provider
	uiReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, d.UserinfoEndpoint, nil)
	uiReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uiRes, err := s.OIDC.client().Do(uiReq)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, fmt.Errorf("userinfo failed: %w", err))
		return
	}
	defer uiRes.Body.Close()
	if uiRes.StatusCode != 200 {
		http.Error(w, fmt.Sprintf("userinfo returned %d", uiRes.StatusCode), http.StatusBadGateway)
		return
	}
	var claims struct {
		Sub               string `json:"sub"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Email             string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(uiRes.Body, 1<<20)).Decode(&claims); err != nil || claims.Sub == "" {
		http.Error(w, "userinfo response missing sub", http.StatusBadGateway)
		return
	}
	u, err := s.Meta.UpsertSSOUser(r.Context(), claims.Sub, claims.PreferredUsername, claims.Name, claims.Email)
	if err != nil {
		s.authAudit(r, "sso_login_failed", claims.PreferredUsername)
		writeAPIError(w, http.StatusForbidden, err)
		return
	}
	token, err := s.Meta.IssueSession(r.Context(), u.ID, remoteIP(r), r.UserAgent())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	_ = s.Meta.Store.TouchLogin(r.Context(), u.ID, time.Now())
	s.setSessionCookie(w, r, token)
	s.authAudit(r, "sso_login", u.Username)
	http.Redirect(w, r, "/admin", http.StatusFound)
}
