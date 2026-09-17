package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"sqlon/internal/tracking"
)

func putSettings(t *testing.T, mux *http.ServeMux, adminTok, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, mux, "PUT", "/api/settings", body, withCookie(adminTok))
}

var nonceAttr = regexp.MustCompile(`nonce="([^"]+)"`)

func TestTrackingOffByDefault(t *testing.T) {
	_, mux, adminTok, _ := newAuthServer(t)
	for _, path := range []string{"/", "/auth/login", "/admin/db", "/docs"} {
		rec := doReq(t, mux, "GET", path, "", withCookie(adminTok))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if rec.Header().Get("Content-Security-Policy") != "" || rec.Header().Get("Content-Security-Policy-Report-Only") != "" {
			t.Fatalf("%s: fresh install must not carry a policy", path)
		}
		if strings.Contains(rec.Body.String(), "tracker.js") || strings.Contains(rec.Body.String(), "googletagmanager") {
			t.Fatalf("%s: snippet present while off", path)
		}
	}
	// the report sink and proxy are inert while off
	if rec := doReq(t, mux, "POST", tracking.ReportPath, `{"csp-report":{"blocked-uri":"https://x.example.com/a.js"}}`, nil); rec.Code != 204 {
		t.Fatalf("report while off: %d", rec.Code)
	}
	if rec := doReq(t, mux, "GET", "/momento/tracker.js", "", nil); rec.Code != 404 {
		t.Fatalf("proxy while off: %d", rec.Code)
	}
	rec := doReq(t, mux, "GET", "/api/tracking/violations", "", withCookie(adminTok))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) || !strings.Contains(rec.Body.String(), `"violations":[]`) {
		t.Fatalf("violations while off: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTrackingMomentoProxyEndToEnd(t *testing.T) {
	_, mux, adminTok, userTok := newAuthServer(t)

	// A stand-in collector: records what reached it.
	var gotPath, gotCookie, gotHost string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCookie, gotHost = r.URL.Path, r.Header.Get("Cookie"), r.Host
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("/* tracker */"))
	}))
	defer collector.Close()

	rec := putSettings(t, mux, adminTok, `{"tracking_enabled":"true","tracking_provider":"momento","tracking_momento_url":"`+collector.URL+`","tracking_momento_site_id":"sqlon"}`)
	if rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}

	// public page: snippet in <head>, nonce on the tag and in script-src
	rec = doReq(t, mux, "GET", "/auth/login", "", nil)
	body := rec.Body.String()
	head := body[:strings.Index(strings.ToLower(body), "</head>")]
	if !strings.Contains(head, `src="/momento/tracker.js"`) || !strings.Contains(head, `data-endpoint="/momento"`) {
		t.Fatalf("snippet not in head: %s", head)
	}
	m := nonceAttr.FindStringSubmatch(head)
	if m == nil {
		t.Fatalf("no nonce on snippet: %s", head)
	}
	nonce := m[1]
	if ro := rec.Header().Get("Content-Security-Policy-Report-Only"); !strings.Contains(ro, "script-src 'self' 'nonce-"+nonce+"'") {
		t.Fatalf("script-src lacks the page nonce: %s", ro)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "report-uri "+tracking.ReportPath) {
		t.Fatalf("enforced policy: %s", csp)
	}
	if strings.Contains(csp, collector.URL) || strings.Contains(rec.Header().Get("Content-Security-Policy-Report-Only"), collector.URL) {
		t.Fatal("proxied collector must not appear in the policy")
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Fatal("'unsafe-inline' is forbidden")
	}
	// a second request gets a different nonce
	rec2 := doReq(t, mux, "GET", "/auth/login", "", nil)
	if m2 := nonceAttr.FindStringSubmatch(rec2.Body.String()); m2 == nil || m2[1] == nonce {
		t.Fatal("nonce must be per request")
	}

	// admin pages excluded by default; non-page paths untouched
	rec = doReq(t, mux, "GET", "/admin/db", "", withCookie(adminTok))
	if strings.Contains(rec.Body.String(), "tracker.js") || rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("admin page tracked without include_admin")
	}
	rec = doReq(t, mux, "GET", "/api/health", "", withCookie(adminTok))
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("api path must not carry the page policy")
	}
	rec = doReq(t, mux, "GET", "/admin/nav.js", "", withCookie(adminTok))
	if strings.Contains(rec.Body.String(), "tracker.js") || rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("script asset must not be modified")
	}

	// include_admin + body placement
	if rec := putSettings(t, mux, adminTok, `{"tracking_include_admin":"true","tracking_placement":"body"}`); rec.Code != 200 {
		t.Fatalf("include_admin: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "GET", "/admin/db", "", withCookie(userTok))
	body = rec.Body.String()
	at := strings.Index(body, "tracker.js")
	if at < 0 || at < strings.Index(strings.ToLower(body), "</head>") {
		t.Fatalf("body placement on admin page failed (at=%d)", at)
	}

	// the proxy forwards to the collector without the session cookie
	rec = doReq(t, mux, "GET", "/momento/tracker.js?v=1", "", withCookie(adminTok))
	if rec.Code != 200 || rec.Body.String() != "/* tracker */" {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/tracker.js" || gotCookie != "" || gotHost == "" {
		t.Fatalf("proxied request path=%q cookie=%q host=%q", gotPath, gotCookie, gotHost)
	}

	// turning it off restores the page and drops the headers
	if rec := putSettings(t, mux, adminTok, `{"tracking_enabled":"false"}`); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "GET", "/auth/login", "", nil)
	if strings.Contains(rec.Body.String(), "tracker.js") || rec.Header().Get("Content-Security-Policy") != "" || rec.Header().Get("Content-Security-Policy-Report-Only") != "" {
		t.Fatal("tracking still present after disable")
	}
	if rec := doReq(t, mux, "GET", "/momento/tracker.js", "", nil); rec.Code != 404 {
		t.Fatalf("proxy after disable: %d", rec.Code)
	}
}

func TestTrackingSettingsValidation(t *testing.T) {
	s, mux, adminTok, _ := newAuthServer(t)

	// enabling momento without an address stores nothing
	rec := putSettings(t, mux, adminTok, `{"tracking_enabled":"true","tracking_provider":"momento"}`)
	if rec.Code != 400 {
		t.Fatalf("incomplete momento accepted: %d %s", rec.Code, rec.Body.String())
	}
	stored, _ := s.Meta.Store.GetSettings(t.Context())
	if stored["tracking_enabled"] != "" || stored["tracking_provider"] != "" {
		t.Fatalf("partial change stored: %v", stored)
	}

	// an oversized snippet is refused even while disabled
	big := strings.Repeat("a", tracking.MaxSnippetBytes+1)
	rec = putSettings(t, mux, adminTok, `{"tracking_custom_snippet":"`+big+`"}`)
	if rec.Code != 400 {
		t.Fatalf("oversized snippet accepted: %d", rec.Code)
	}
	stored, _ = s.Meta.Store.GetSettings(t.Context())
	if stored["tracking_custom_snippet"] != "" {
		t.Fatal("oversized snippet stored")
	}

	// bad provider / bad url
	if rec := putSettings(t, mux, adminTok, `{"tracking_enabled":"true","tracking_provider":"piwik"}`); rec.Code != 400 {
		t.Fatalf("unknown provider accepted: %d", rec.Code)
	}
	if rec := putSettings(t, mux, adminTok, `{"tracking_matomo_url":"ftp://x"}`); rec.Code != 400 {
		t.Fatalf("bad url accepted: %d", rec.Code)
	}
	// a per-key format error refuses the whole request: the valid key beside
	// it must not land either, whatever order the map is walked in
	for i := 0; i < 16; i++ {
		if rec := putSettings(t, mux, adminTok, `{"tracking_enabled":"false","tracking_matomo_url":"ftp://x"}`); rec.Code != 400 {
			t.Fatalf("bad url beside valid key accepted: %d", rec.Code)
		}
		stored, _ = s.Meta.Store.GetSettings(t.Context())
		if _, ok := stored["tracking_enabled"]; ok {
			t.Fatalf("partial change stored beside bad url: %v", stored)
		}
		if rec := putSettings(t, mux, adminTok, `{"tracking_placement":"body","no_such_setting":"1"}`); rec.Code != 400 {
			t.Fatalf("unknown key beside valid key accepted: %d", rec.Code)
		}
		stored, _ = s.Meta.Store.GetSettings(t.Context())
		if _, ok := stored["tracking_placement"]; ok {
			t.Fatalf("partial change stored beside unknown key: %v", stored)
		}
	}

	// settings view exposes the group with control types for the console
	rec = doReq(t, mux, "GET", "/api/settings", "", withCookie(adminTok))
	var view struct {
		Settings []struct {
			Key     string   `json:"key"`
			Group   string   `json:"group"`
			Type    string   `json:"type"`
			Options []string `json:"options"`
			Default string   `json:"default"`
		} `json:"settings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	found := map[string]string{}
	defaults := map[string]string{}
	for _, s := range view.Settings {
		if s.Group == "방문 추적" {
			found[s.Key] = s.Type
			defaults[s.Key] = s.Default
			if s.Key == "tracking_provider" && (len(s.Options) == 0 || s.Options[1] != "momento") {
				t.Fatalf("momento must be the first real provider: %v", s.Options)
			}
		}
	}
	if found["tracking_enabled"] != "bool" || found["tracking_custom_snippet"] != "multiline" || found["tracking_provider"] != "select" {
		t.Fatalf("control types: %v", found)
	}
	// the console draws a blank bool as its server-side default: the proxy is
	// on unless explicitly turned off, the others are off unless turned on
	if defaults["tracking_momento_proxy"] != "true" || defaults["tracking_enabled"] != "" || defaults["tracking_include_admin"] != "" {
		t.Fatalf("bool defaults: %v", defaults)
	}
}

func TestTrackingCustomSnippetAndViolations(t *testing.T) {
	_, mux, adminTok, userTok := newAuthServer(t)
	snippet := `<script async src=\"https://cdn.example.com/t.js\"></script>\n<script>fetch('https://collect.example.com/hit')</script>`
	rec := putSettings(t, mux, adminTok, `{"tracking_enabled":"true","tracking_provider":"custom","tracking_custom_snippet":"`+snippet+`"}`)
	if rec.Code != 200 {
		t.Fatalf("enable custom: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "GET", "/", "", withCookie(userTok))
	body := rec.Body.String()
	if strings.Count(body, `nonce="`) < 2 {
		t.Fatalf("every script tag of the snippet needs the nonce: %s", body[:400])
	}
	nonces := nonceAttr.FindAllStringSubmatch(body, -1)
	for _, m := range nonces {
		if m[1] != nonces[0][1] {
			t.Fatal("all tags must share the request nonce")
		}
	}
	ro := rec.Header().Get("Content-Security-Policy-Report-Only")
	if !strings.Contains(ro, "'nonce-"+nonces[0][1]+"'") || !strings.Contains(ro, "https://cdn.example.com") || !strings.Contains(ro, "https://collect.example.com") {
		t.Fatalf("script-src should carry the nonce and the snippet's origins: %s", ro)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' ws: wss: https://cdn.example.com https://collect.example.com") {
		t.Fatalf("connect-src: %s", csp)
	}

	// the browser reports what it refused; the same origin does not pile up
	report := `{"csp-report":{"document-uri":"http://sqlon.local/","blocked-uri":"https://px.example.com/p.gif?x=1","effective-directive":"img-src","disposition":"enforce"}}`
	for i := 0; i < 3; i++ {
		if rec := doReq(t, mux, "POST", tracking.ReportPath, report, map[string]string{"Content-Type": "application/csp-report"}); rec.Code != 204 {
			t.Fatalf("report: %d", rec.Code)
		}
	}
	doReq(t, mux, "POST", tracking.ReportPath, `{"csp-report":{"blocked-uri":"inline","violated-directive":"script-src-attr","disposition":"report"}}`, nil)

	rec = doReq(t, mux, "GET", "/api/tracking/violations", "", withCookie(userTok))
	if rec.Code != 403 {
		t.Fatalf("non-admin must not read violations: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/api/tracking/violations", "", withCookie(adminTok))
	var out struct {
		Enabled    bool                 `json:"enabled"`
		Violations []tracking.Violation `json:"violations"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Enabled || len(out.Violations) != 1 {
		t.Fatalf("violations: %s", rec.Body.String())
	}
	v := out.Violations[0]
	if v.Origin != "https://px.example.com" || v.Directive != "img-src" || v.Count != 3 || v.Page != "/" || v.Allowed {
		t.Fatalf("violation: %+v", v)
	}

	// one click: allow it, and the policy picks it up
	rec = doReq(t, mux, "POST", "/api/tracking/allow", `{"origin":"https://px.example.com"}`, withCookie(adminTok))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"allowed_hosts":"https://px.example.com"`) {
		t.Fatalf("allow: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "GET", "/api/tracking/violations", "", withCookie(adminTok))
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Violations) != 1 || !out.Violations[0].Allowed {
		t.Fatalf("should be marked allowed: %s", rec.Body.String())
	}
	if rec := doReq(t, mux, "GET", "/", "", withCookie(userTok)); !strings.Contains(rec.Header().Get("Content-Security-Policy"), "img-src 'self' data: blob: https://cdn.example.com https://collect.example.com https://px.example.com") {
		t.Fatalf("allowed host missing from policy: %s", rec.Header().Get("Content-Security-Policy"))
	}
	if rec := doReq(t, mux, "POST", "/api/tracking/allow", `{"origin":"javascript:alert(1)"}`, withCookie(adminTok)); rec.Code != 400 {
		t.Fatalf("non-http origin accepted: %d", rec.Code)
	}

	if rec := doReq(t, mux, "DELETE", "/api/tracking/violations", "", withCookie(adminTok)); rec.Code != 200 {
		t.Fatalf("forget: %d", rec.Code)
	}
	rec = doReq(t, mux, "GET", "/api/tracking/violations", "", withCookie(adminTok))
	if !strings.Contains(rec.Body.String(), `"violations":[]`) {
		t.Fatalf("not cleared: %s", rec.Body.String())
	}
}
