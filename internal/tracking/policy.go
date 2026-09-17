package tracking

import "strings"

// ReportPath receives the browser's policy violation reports while tracking
// is on. It is what turns a silent console error into a line on the admin
// screen.
const ReportPath = "/api/tracking/csp-report"

// Policy is the pair of Content-Security-Policy headers a tracked page
// carries. The connection and image sources are enforced. The script source
// is report-only for now: the console's pages still use inline event
// handlers (onclick="…"), which a nonce cannot cover, and the alternative —
// 'unsafe-inline' — is exactly what this package exists to avoid. Reporting
// still names every script origin the snippet would need, so the day the
// handlers are gone the same header can be enforced without surprises.
type Policy struct {
	Enforced   string
	ReportOnly string
}

// PolicyFor assembles the headers for a page. An inactive configuration
// returns empty headers, which leaves the page exactly as it was before
// tracking existed.
func PolicyFor(c Config, path, nonce string) Policy {
	if !c.Active(path) {
		return Policy{}
	}
	scripts := []string{"'self'", "'nonce-" + nonce + "'"}
	connects := []string{"'self'", "ws:", "wss:"}
	images := []string{"'self'", "data:", "blob:"}
	extraScripts, extraConnects, extraImages := c.PolicySources()
	scripts = append(scripts, extraScripts...)
	connects = append(connects, extraConnects...)
	images = append(images, extraImages...)
	return Policy{
		Enforced: "img-src " + strings.Join(images, " ") +
			"; connect-src " + strings.Join(connects, " ") +
			"; report-uri " + ReportPath,
		ReportOnly: "script-src " + strings.Join(scripts, " ") +
			"; report-uri " + ReportPath,
	}
}
