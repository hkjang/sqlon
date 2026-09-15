package tracking

import (
	"strings"
	"testing"
	"time"
)

func TestReadConfigDefaultsOff(t *testing.T) {
	c := ReadConfig(map[string]string{})
	if c.Enabled || c.Provider != ProviderNone || c.Placement != "head" || !c.MomentoProxy {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Active("/") || c.Active("/admin") {
		t.Fatal("fresh config must not be active anywhere")
	}
	if p := PolicyFor(c, "/", "n"); p.Enforced != "" || p.ReportOnly != "" {
		t.Fatalf("inactive config must yield empty headers: %+v", p)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("empty config must validate: %v", err)
	}
}

func TestMomentoProxySnippetAndPolicy(t *testing.T) {
	c := ReadConfig(map[string]string{
		SetEnabled: "true", SetProvider: "momento",
		SetMomentoURL: "https://momento.internal:8443/", SetMomentoSiteID: "sqlon",
	})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.UsesProxy() {
		t.Fatal("proxy should be the default for momento")
	}
	snip := c.Snippet("n0nce")
	for _, want := range []string{`src="/momento/tracker.js"`, `data-endpoint="/momento"`, `data-site-id="sqlon"`, `nonce="n0nce"`, `data-contract-version="1"`} {
		if !strings.Contains(snip, want) {
			t.Fatalf("snippet missing %q: %s", want, snip)
		}
	}
	if strings.Contains(snip, "momento.internal") {
		t.Fatalf("proxied snippet must not name the collector: %s", snip)
	}
	p := PolicyFor(c, "/", "n0nce")
	if strings.Contains(p.Enforced, "momento.internal") || strings.Contains(p.ReportOnly, "momento.internal") {
		t.Fatalf("proxied policy must not name the collector: %+v", p)
	}
	if !strings.Contains(p.ReportOnly, "script-src 'self' 'nonce-n0nce'") {
		t.Fatalf("nonce missing from script-src: %s", p.ReportOnly)
	}
	if strings.Contains(p.Enforced+p.ReportOnly, "unsafe-inline") {
		t.Fatal("'unsafe-inline' must never appear")
	}
	if !strings.Contains(p.Enforced, "report-uri "+ReportPath) || !strings.Contains(p.ReportOnly, "report-uri "+ReportPath) {
		t.Fatalf("report-uri missing: %+v", p)
	}
}

func TestMomentoDirectNamesCollector(t *testing.T) {
	c := ReadConfig(map[string]string{
		SetEnabled: "true", SetProvider: "momento", SetMomentoProxy: "false",
		SetMomentoURL: "https://momento.internal:8443", SetMomentoSiteID: "sqlon",
	})
	if c.UsesProxy() {
		t.Fatal("proxy explicitly off")
	}
	snip := c.Snippet("n")
	if !strings.Contains(snip, `src="https://momento.internal:8443/tracker.js"`) || strings.Contains(snip, "data-endpoint") {
		t.Fatalf("direct snippet wrong: %s", snip)
	}
	p := PolicyFor(c, "/", "n")
	for _, h := range []string{p.Enforced, p.ReportOnly} {
		if !strings.Contains(h, "https://momento.internal:8443") {
			t.Fatalf("collector origin missing from policy: %s", h)
		}
	}
}

func TestValidateRequiresProviderFields(t *testing.T) {
	cases := []map[string]string{
		{SetEnabled: "true", SetProvider: "momento"},
		{SetEnabled: "true", SetProvider: "momento", SetMomentoURL: "not a url", SetMomentoSiteID: "x"},
		{SetEnabled: "true", SetProvider: "ga4"},
		{SetEnabled: "true", SetProvider: "matomo", SetMatomoURL: "https://m.example.com"},
		{SetEnabled: "true", SetProvider: "custom"},
		{SetEnabled: "true", SetProvider: "piwik"},
	}
	for i, values := range cases {
		if err := ReadConfig(values).Validate(); err == nil {
			t.Fatalf("case %d should fail: %v", i, values)
		}
	}
	// disabled: fields may be incomplete, but an oversized snippet is refused regardless
	if err := ReadConfig(map[string]string{SetProvider: "momento"}).Validate(); err != nil {
		t.Fatalf("disabled config should validate: %v", err)
	}
	big := strings.Repeat("x", MaxSnippetBytes+1)
	if err := ReadConfig(map[string]string{SetCustomSnippet: big}).Validate(); err == nil {
		t.Fatal("snippet over 8KB must be refused even when disabled")
	}
}

func TestAdminPagesExcludedUnlessIncluded(t *testing.T) {
	values := map[string]string{SetEnabled: "true", SetProvider: "ga4", SetMeasurementID: "G-1"}
	c := ReadConfig(values)
	if !c.Active("/") || !c.Active("/auth/login") || !c.Active("/docs") {
		t.Fatal("public pages should be tracked")
	}
	if c.Active("/admin") || c.Active("/admin/db") {
		t.Fatal("admin pages excluded by default")
	}
	values[SetIncludeAdmin] = "true"
	if !ReadConfig(values).Active("/admin/db") {
		t.Fatal("include_admin should track admin pages")
	}
	// /administrator-like paths outside the console are not admin pages
	if IsAdminPath("/adminx") {
		t.Fatal("/adminx is not the console")
	}
}

func TestWithNonceSurvivesLengthChangingRunes(t *testing.T) {
	// U+0130 folds from 2 bytes to 3, U+212A from 3 to 1: a ToLower-derived
	// index would land inside the tag name.
	for _, snippet := range []string{
		"İİİİ<script>1</script>",
		"KK<SCRIPT src=\"https://x.example.com/a.js\"></SCRIPT>",
		"<script nonce=\"keep\">a</script><script>b</script>",
	} {
		out := withNonce(snippet, "abc")
		if strings.Contains(out, "<sc nonce") || strings.Contains(out, "<SC nonce") {
			t.Fatalf("nonce broke the tag: %s", out)
		}
		if strings.Contains(out, "<script>") || strings.Contains(out, "<SCRIPT>") {
			t.Fatalf("a script tag is missing its nonce: %s", out)
		}
	}
	out := withNonce("<script nonce=\"keep\">a</script><script>b</script>", "abc")
	if strings.Count(out, "nonce=") != 2 || !strings.Contains(out, `nonce="keep"`) || !strings.Contains(out, `nonce="abc"`) {
		t.Fatalf("existing nonce must be kept, missing one added: %s", out)
	}
}

func TestSnippetOriginsAndPolicySources(t *testing.T) {
	snippet := `<script async src="HTTPS://cdn.Example.com/t.js"></script>
<script>fetch('https://collect.example.com/api/hit');new Image().src="http://px.example.com/p.gif?u="+location.href;var same='https://cdn.example.com/x';</script>`
	got := SnippetOrigins(snippet)
	want := []string{"https://cdn.example.com", "https://collect.example.com", "http://px.example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("origins = %v, want %v", got, want)
	}
	c := ReadConfig(map[string]string{SetEnabled: "true", SetProvider: "custom", SetCustomSnippet: snippet, SetAllowedHosts: "https://extra.example.com, https://*.wild.example.com"})
	scripts, connects, images := c.PolicySources()
	for _, group := range [][]string{scripts, connects, images} {
		joined := strings.Join(group, " ")
		for _, origin := range append(want, "https://extra.example.com", "https://*.wild.example.com") {
			if !strings.Contains(joined, origin) {
				t.Fatalf("%q missing from %v", origin, group)
			}
		}
	}
	p := PolicyFor(c, "/", "n")
	if !strings.Contains(p.Enforced, "connect-src 'self' ws: wss: https://cdn.example.com") {
		t.Fatalf("connect-src wrong: %s", p.Enforced)
	}
}

func TestInjectPlacement(t *testing.T) {
	page := []byte("<!DOCTYPE html><HTML><HEAD><title>x</title></HEAD><BODY>hi</BODY></HTML>")
	head := string(Inject(page, "<script>1</script>", "head"))
	if !strings.Contains(head, "<script>1</script>\n</HEAD>") {
		t.Fatalf("head placement: %s", head)
	}
	body := string(Inject(page, "<script>1</script>", "body"))
	if !strings.Contains(body, "<script>1</script>\n</BODY>") {
		t.Fatalf("body placement: %s", body)
	}
	if got := string(Inject([]byte("no closing tags"), "<s>", "head")); !strings.HasSuffix(got, "\n<s>") {
		t.Fatalf("fallback append: %q", got)
	}
	if got := Inject(page, "", "head"); string(got) != string(page) {
		t.Fatal("empty snippet must not touch the page")
	}
}

func TestRecorderDistinctOriginsAndBound(t *testing.T) {
	r := NewRecorder()
	now := time.Unix(1000, 0)
	r.now = func() time.Time { now = now.Add(time.Second); return now }
	r.Record("https://a.example.com/x.js", "script-src-elem", "report", "/")
	r.Record("https://a.example.com/y.js", "script-src-elem", "report", "/docs")
	r.Record("https://a.example.com/beacon", "connect-src", "enforce", "/")
	r.Record("inline", "script-src-attr", "report", "/")
	r.Record("data", "img-src", "", "/")
	r.Record("chrome-extension://abc/x.js", "script-src", "", "/")
	list := r.List(Config{})
	if len(list) != 2 {
		t.Fatalf("want 2 distinct (directive, origin) entries, got %+v", list)
	}
	if list[0].Directive != "connect-src" || list[0].Disposition != "enforce" {
		t.Fatalf("most recent first: %+v", list)
	}
	if list[1].Count != 2 || list[1].Page != "/docs" {
		t.Fatalf("same origin should accumulate: %+v", list[1])
	}
	// the allow-list marks entries the policy already covers
	list = r.List(ReadConfig(map[string]string{SetAllowedHosts: "https://a.example.com"}))
	for _, v := range list {
		if !v.Allowed {
			t.Fatalf("should be marked allowed: %+v", v)
		}
	}
	// bounded
	for i := 0; i < MaxViolations+20; i++ {
		r.Record("https://h"+strings.Repeat("x", i%50)+string(rune('a'+i%26))+".example.com", "connect-src", "", "/")
	}
	if n := len(r.List(Config{})); n > MaxViolations {
		t.Fatalf("recorder unbounded: %d", n)
	}
	r.Forget()
	if len(r.List(Config{})) != 0 {
		t.Fatal("forget should clear")
	}
}

func TestAddAllowedHost(t *testing.T) {
	if got := AddAllowedHost("", "https://a.example.com/"); got != "https://a.example.com" {
		t.Fatal(got)
	}
	if got := AddAllowedHost("https://a.example.com", "HTTPS://a.example.com"); got != "https://a.example.com" {
		t.Fatal(got)
	}
	if got := AddAllowedHost("https://a.example.com", "https://b.example.com"); got != "https://a.example.com, https://b.example.com" {
		t.Fatal(got)
	}
}
