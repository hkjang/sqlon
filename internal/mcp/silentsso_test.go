package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sqlon/internal/meta"
)

// Silent SSO (prompt=none) — the whole point is never to loop: a refused
// silent attempt must land on the login page with a marker, and the server
// must ignore prompt=none unless the administrator turned auto_login on.

// withOIDC installs a provider whose discovery document is pre-resolved so no
// network is touched. endpoints may be "" for tests that never reach them.
func withOIDC(s *Server, autoLogin bool, endpoints string) {
	s.OIDC = &OIDCProvider{Issuer: "https://kc/realms/x", ClientID: "c", ClientSecret: "s",
		RedirectURL: "https://h/auth/sso/callback", AutoLogin: autoLogin,
		disc: &oidcDiscovery{AuthorizationEndpoint: endpoints + "/auth", TokenEndpoint: endpoints + "/token", UserinfoEndpoint: endpoints + "/userinfo"}}
}

// cookieByName extracts a Set-Cookie from a recorded response.
func cookieByName(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ctxCookieValues decodes the round-trip context cookie.
func ctxCookieValues(t *testing.T, rec *httptest.ResponseRecorder) url.Values {
	t.Helper()
	c := cookieByName(rec, oauthCtxCookie)
	if c == nil {
		t.Fatalf("no %s cookie: %v", oauthCtxCookie, rec.Header()["Set-Cookie"])
	}
	v, err := url.ParseQuery(c.Value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSafeReturnTo(t *testing.T) {
	ok := []string{"/", "/admin", "/admin/db?profile=x&y=1", "/a/b#c"}
	bad := []string{"", "//evil.com", "//evil.com/x", "https://evil.com", "admin", "/x\r\nSet-Cookie: a=b", "/\\evil.com", "\\\\evil"}
	for _, v := range ok {
		if !safeReturnTo(v) {
			t.Errorf("safeReturnTo(%q) should be accepted", v)
		}
	}
	for _, v := range bad {
		if safeReturnTo(v) {
			t.Errorf("safeReturnTo(%q) must be rejected", v)
		}
	}
}

func TestSilentSSOLoginHonoursAutoLoginSetting(t *testing.T) {
	s, mux, _, _ := newAuthServer(t)

	// auto_login off (the default): ?prompt=none is quietly downgraded to an
	// ordinary login so a crafted URL cannot change the flow.
	withOIDC(s, false, "https://kc")
	rec := doReq(t, mux, "GET", "/auth/sso/login?prompt=none&return_to=%2Fadmin%2Fdb%3Fx%3D1", "", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("sso login: %d %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("prompt") != "" {
		t.Fatalf("auto_login off must not forward prompt=none: %s", loc)
	}
	if v := ctxCookieValues(t, rec); v.Get("silent") != "" || v.Get("return_to") != "/admin/db?x=1" {
		t.Fatalf("ctx cookie with auto_login off: %v", v)
	}

	// auto_login on: prompt=none reaches the provider and the leg is marked silent
	withOIDC(s, true, "https://kc")
	rec = doReq(t, mux, "GET", "/auth/sso/login?prompt=none&return_to=%2Fadmin%2Fdb", "", nil)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("prompt") != "none" || loc.Query().Get("state") == "" {
		t.Fatalf("auto_login on must forward prompt=none with state: %s", loc)
	}
	if v := ctxCookieValues(t, rec); v.Get("silent") != "1" || v.Get("return_to") != "/admin/db" {
		t.Fatalf("ctx cookie with auto_login on: %v", v)
	}

	// an ordinary (non-silent) login with auto_login on stays ordinary
	rec = doReq(t, mux, "GET", "/auth/sso/login", "", nil)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("prompt") != "" {
		t.Fatalf("plain login must not add prompt=none: %s", loc)
	}

	// unsafe return_to values are dropped rather than carried
	for _, bad := range []string{"//evil.com", "https://evil.com/x", "admin"} {
		rec = doReq(t, mux, "GET", "/auth/sso/login?return_to="+url.QueryEscape(bad), "", nil)
		if v := ctxCookieValues(t, rec); v.Get("return_to") != "" {
			t.Fatalf("return_to %q must be rejected, got %v", bad, v)
		}
	}
}

func TestSilentSSOCallbackRefusalLandsOnLoginWithMarker(t *testing.T) {
	s, mux, _, _ := newAuthServer(t)
	withOIDC(s, true, "https://kc")
	ctx := url.Values{"silent": {"1"}, "return_to": {"/admin/db?x=1"}}.Encode()
	hdr := func(ctxVal string) map[string]string {
		return map[string]string{"Cookie": "sqlon_oauth_state=abc; " + oauthCtxCookie + "=" + ctxVal}
	}

	// login_required after prompt=none is a normal answer: go to the login
	// page with the do-not-retry marker and keep the deep link as next=
	rec := doReq(t, mux, "GET", "/auth/sso/callback?error=login_required&state=abc", "", hdr(ctx))
	if rec.Code != http.StatusFound {
		t.Fatalf("silent refusal must redirect: %d %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Path != "/auth/login" || loc.Query().Get("sso") != "none" || loc.Query().Get("next") != "/admin/db?x=1" {
		t.Fatalf("silent refusal location: %s", loc)
	}
	if c := cookieByName(rec, oauthCtxCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("ctx cookie must be cleared on callback: %v", c)
	}
	for _, e := range []string{"interaction_required", "consent_required"} {
		rec = doReq(t, mux, "GET", "/auth/sso/callback?error="+e+"&state=abc", "", hdr(ctx))
		loc, _ = url.Parse(rec.Header().Get("Location"))
		if rec.Code != http.StatusFound || loc.Query().Get("sso") != "none" {
			t.Fatalf("%s must be treated as a refusal: %d %s", e, rec.Code, loc)
		}
	}

	// any other provider error on a silent leg still stops retrying (sso=error)
	rec = doReq(t, mux, "GET", "/auth/sso/callback?error=server_error&state=abc", "", hdr(ctx))
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc.Query().Get("sso") != "error" {
		t.Fatalf("silent provider error: %d %s", rec.Code, loc)
	}

	// an unsafe return_to smuggled into the cookie is not propagated
	evil := url.Values{"silent": {"1"}, "return_to": {"//evil.com"}}.Encode()
	rec = doReq(t, mux, "GET", "/auth/sso/callback?error=login_required&state=abc", "", hdr(evil))
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("next") != "" {
		t.Fatalf("unsafe return_to leaked into next: %s", loc)
	}

	// a non-silent leg keeps the explicit error page
	plain := url.Values{"return_to": {"/admin"}}.Encode()
	rec = doReq(t, mux, "GET", "/auth/sso/callback?error=login_required&state=abc", "", hdr(plain))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("non-silent error must stay an error: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/auth/sso/callback?error=login_required&state=abc", "", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("error without ctx cookie must stay an error: %d", rec.Code)
	}
}

func TestSilentSSODeepLinkReturnsToOrigin(t *testing.T) {
	s, mux, _, _ := newAuthServer(t)
	// a stand-in provider that hands out a token and userinfo for any code
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at"})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer at" {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sub": "sub-1", "preferred_username": "kc-user", "name": "KC User", "email": "kc@example.com"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer idp.Close()
	withOIDC(s, true, idp.URL)

	roundTrip := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		start := doReq(t, mux, "GET", "/auth/sso/login"+query, "", nil)
		if start.Code != http.StatusFound {
			t.Fatalf("start: %d %s", start.Code, start.Body.String())
		}
		loc, _ := url.Parse(start.Header().Get("Location"))
		var cookies []string
		for _, c := range start.Result().Cookies() {
			cookies = append(cookies, c.Name+"="+c.Value)
		}
		return doReq(t, mux, "GET", "/auth/sso/callback?code=xyz&state="+loc.Query().Get("state"), "",
			map[string]string{"Cookie": strings.Join(cookies, "; ")})
	}

	// silent success with a deep link lands on that exact link with a session
	rec := roundTrip("?prompt=none&return_to=" + url.QueryEscape("/admin/db?profile=p1"))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/db?profile=p1" {
		t.Fatalf("deep link return: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if cookieByName(rec, meta.SessionCookie) == nil {
		t.Fatal("silent success must issue a session cookie")
	}
	if u, err := s.Meta.Store.GetUserByUsername(t.Context(), "kc-user"); err != nil || u.Provider == meta.ProviderLocal {
		t.Fatalf("SSO user not provisioned: %+v %v", u, err)
	}

	// without return_to the console root remains the landing page
	rec = roundTrip("")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin" {
		t.Fatalf("default landing: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSSOAutoLoginSettingAndMe(t *testing.T) {
	s, mux, adminTok, _ := newAuthServer(t)

	// no OIDC → flags off
	rec := doReq(t, mux, "GET", "/auth/me", "", nil)
	if !strings.Contains(rec.Body.String(), `"sso_auto_login":false`) {
		t.Fatalf("me without OIDC: %s", rec.Body.String())
	}

	// OIDC configured through settings: auto_login stays off by default
	doReq(t, mux, "PUT", "/api/settings",
		`{"oidc_issuer":"https://kc/realms/x","oidc_client_id":"c","oidc_client_secret":"s","oidc_redirect_url":"https://h/auth/sso/callback"}`,
		withCookie(adminTok))
	if s.OIDC == nil || s.OIDC.AutoLogin {
		t.Fatalf("auto_login must default to off: %+v", s.OIDC)
	}
	rec = doReq(t, mux, "GET", "/auth/me", "", nil)
	if !strings.Contains(rec.Body.String(), `"sso_enabled":true`) || !strings.Contains(rec.Body.String(), `"sso_auto_login":false`) {
		t.Fatalf("me with OIDC default: %s", rec.Body.String())
	}

	// only an explicit affirmative turns it on, and it is visible to the browser
	doReq(t, mux, "PUT", "/api/settings", `{"oidc_auto_login":"true"}`, withCookie(adminTok))
	if !s.OIDC.AutoLogin {
		t.Fatal("oidc_auto_login=true not applied at runtime")
	}
	rec = doReq(t, mux, "GET", "/auth/me", "", nil)
	if !strings.Contains(rec.Body.String(), `"sso_auto_login":true`) {
		t.Fatalf("me should publish auto_login: %s", rec.Body.String())
	}
	for _, off := range []string{`"false"`, `"nope"`, `null`} {
		doReq(t, mux, "PUT", "/api/settings", `{"oidc_auto_login":`+off+`}`, withCookie(adminTok))
		if s.OIDC.AutoLogin {
			t.Fatalf("oidc_auto_login=%s must read as off", off)
		}
	}
	// the setting is listed for the admin console
	rec = doReq(t, mux, "GET", "/api/settings", "", withCookie(adminTok))
	if !strings.Contains(rec.Body.String(), `"key":"oidc_auto_login"`) {
		t.Fatalf("settings view missing oidc_auto_login: %s", rec.Body.String())
	}
}

func TestGuardRedirectKeepsDeepLinkQuery(t *testing.T) {
	_, mux, _, _ := newAuthServer(t)
	rec := doReq(t, mux, "GET", "/admin/db?profile=p1", "", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("anon page: %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Path != "/auth/login" || loc.Query().Get("next") != "/admin/db?profile=p1" {
		t.Fatalf("guard must carry path+query as next: %s", loc)
	}
}

func TestSSOAutoLoginBootDefault(t *testing.T) {
	s, _ := newFixtureServer(t)
	svc := meta.NewService(meta.NewMemStore())
	s.EnableMeta(svc, &OIDCProvider{Issuer: "https://kc/realms/x", ClientID: "c", ClientSecret: "s", RedirectURL: "https://h/cb", AutoLogin: true})
	if err := s.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.OIDC == nil || !s.OIDC.AutoLogin {
		t.Fatalf("flag/env auto_login should survive ApplySettings: %+v", s.OIDC)
	}
	// a stored explicit "false" overrides the boot value
	if err := svc.ApplySetting(t.Context(), meta.SetOIDCAutoLogin, "false", "t"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.OIDC.AutoLogin {
		t.Fatal("stored false must win over boot true")
	}
}
