// Package tracking injects a visitor tracking snippet into the served pages.
//
// The snippet is configured from the admin console (settings stored in the
// meta DB) so a closed-network deployment can point at its own collector
// without a rebuild. Every request gets a fresh nonce that is written into
// each <script> tag of the snippet and into the page's Content-Security-Policy,
// so the policy never has to be loosened with 'unsafe-inline'. The origins the
// snippet needs are read out of the snippet itself and added to the policy.
package tracking

import (
	"fmt"
	"html"
	"net"
	"net/url"
	"strings"
	"unicode"
)

const (
	ProviderNone   = "none"
	ProviderGA4    = "ga4"
	ProviderGTM    = "gtm"
	ProviderMatomo = "matomo"
	ProviderCustom = "custom"
	// ProviderMomento is the in-house, self-hosted collector — the only choice
	// that keeps visitor data inside the network, so it is listed first.
	ProviderMomento = "momento"

	// MaxSnippetBytes bounds a pasted snippet; anything larger is refused.
	MaxSnippetBytes = 8 * 1024

	// ProxyPath is the same-origin path the app forwards to the Momento
	// collector. With the proxy, no external origin appears in the policy.
	ProxyPath = "/momento"

	// The only two collector paths the tracker reaches through the proxy
	// (momento contract version 1): the script itself, and the batch endpoint
	// tracker.js derives from data-endpoint as `${endpoint}/collect/v1/events`.
	// The collector serves its console, admin API and login on the same
	// origin, so everything else must stay unreachable from here.
	ProxyTrackerPath = "/tracker.js"
	ProxyCollectPath = "/collect/v1/events"
)

// Providers lists the accepted provider values in display order.
var Providers = []string{ProviderNone, ProviderMomento, ProviderGA4, ProviderGTM, ProviderMatomo, ProviderCustom}

// Config is the effective tracking configuration.
type Config struct {
	Enabled       bool
	Provider      string
	MomentoURL    string
	MomentoSiteID string
	// MomentoProxy serves the tracker and its beacons through ProxyPath on
	// this origin instead of naming the collector in the policy.
	MomentoProxy  bool
	MeasurementID string
	MatomoURL     string
	MatomoSiteID  string
	CustomSnippet string
	AllowedHosts  string
	IncludeAdmin  bool
	Placement     string
}

// Setting keys as stored in the meta DB settings table.
const (
	SetEnabled       = "tracking_enabled"
	SetProvider      = "tracking_provider"
	SetMomentoURL    = "tracking_momento_url"
	SetMomentoSiteID = "tracking_momento_site_id"
	SetMomentoProxy  = "tracking_momento_proxy"
	SetMeasurementID = "tracking_measurement_id"
	SetMatomoURL     = "tracking_matomo_url"
	SetMatomoSiteID  = "tracking_matomo_site_id"
	SetCustomSnippet = "tracking_custom_snippet"
	SetAllowedHosts  = "tracking_allowed_hosts"
	SetIncludeAdmin  = "tracking_include_admin"
	SetPlacement     = "tracking_placement"
)

// ReadConfig maps stored string settings onto the configuration. Every value
// is a string in the settings table, so booleans are "true"/"false".
func ReadConfig(values map[string]string) Config {
	c := Config{
		Enabled:       IsTrue(values[SetEnabled]),
		Provider:      strings.ToLower(strings.TrimSpace(values[SetProvider])),
		MomentoURL:    strings.TrimSpace(values[SetMomentoURL]),
		MomentoSiteID: strings.TrimSpace(values[SetMomentoSiteID]),
		// The proxy is the default: it is the path that keeps the policy free
		// of external origins. Only an explicit "false" turns it off.
		MomentoProxy:  !IsFalse(values[SetMomentoProxy]),
		MeasurementID: strings.TrimSpace(values[SetMeasurementID]),
		MatomoURL:     strings.TrimSpace(values[SetMatomoURL]),
		MatomoSiteID:  strings.TrimSpace(values[SetMatomoSiteID]),
		CustomSnippet: values[SetCustomSnippet],
		AllowedHosts:  values[SetAllowedHosts],
		IncludeAdmin:  IsTrue(values[SetIncludeAdmin]),
		Placement:     strings.ToLower(strings.TrimSpace(values[SetPlacement])),
	}
	if c.Provider == "" {
		c.Provider = ProviderNone
	}
	if c.Placement != "body" {
		c.Placement = "head"
	}
	return c
}

// IsTrue accepts the spellings an administrator is likely to type.
func IsTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// IsFalse is the explicit negative; blank is neither true nor false.
func IsFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off", "n":
		return true
	}
	return false
}

// IsAdminPath reports the paths that belong to the management console. In
// this app almost every screen lives under /admin; the console traffic is
// rarely the visitor data anybody wants, so it is excluded by default.
func IsAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

// Active reports whether a page at path should carry the snippet.
func (c Config) Active(path string) bool {
	if !c.Enabled || c.Provider == ProviderNone || c.Provider == "" {
		return false
	}
	if !c.IncludeAdmin && IsAdminPath(path) {
		return false
	}
	return strings.TrimSpace(c.Snippet("")) != ""
}

// UsesProxy reports whether the Momento proxy at ProxyPath should be live.
func (c Config) UsesProxy() bool {
	return c.Enabled && c.Provider == ProviderMomento && c.MomentoProxy && originOf(c.MomentoURL) != ""
}

// Validate reports what is missing or wrong for the chosen provider. A
// disabled configuration is always valid so an administrator can fill the
// fields in any order and switch it on last.
func (c Config) Validate() error {
	if len(c.CustomSnippet) > MaxSnippetBytes {
		return fmt.Errorf("추적 코드는 %d바이트를 넘을 수 없습니다", MaxSnippetBytes)
	}
	if !c.Enabled {
		return nil
	}
	switch c.Provider {
	case ProviderNone, "":
		return nil
	case ProviderMomento:
		if c.MomentoURL == "" || c.MomentoSiteID == "" {
			return fmt.Errorf("%s 와 %s 가 필요합니다", SetMomentoURL, SetMomentoSiteID)
		}
		if originOf(c.MomentoURL) == "" {
			return fmt.Errorf("%s 이 올바른 주소가 아닙니다", SetMomentoURL)
		}
	case ProviderGA4, ProviderGTM:
		if c.MeasurementID == "" {
			return fmt.Errorf("%s 가 필요합니다", SetMeasurementID)
		}
	case ProviderMatomo:
		if c.MatomoURL == "" || c.MatomoSiteID == "" {
			return fmt.Errorf("%s 과 %s 가 필요합니다", SetMatomoURL, SetMatomoSiteID)
		}
		if originOf(c.MatomoURL) == "" {
			return fmt.Errorf("%s 이 올바른 주소가 아닙니다", SetMatomoURL)
		}
	case ProviderCustom:
		if strings.TrimSpace(c.CustomSnippet) == "" {
			return fmt.Errorf("%s 이 비어 있습니다", SetCustomSnippet)
		}
	default:
		return fmt.Errorf("%s 는 %s 중 하나여야 합니다", SetProvider, strings.Join(Providers, ", "))
	}
	return nil
}

// Snippet renders the markup to inject. The nonce is applied to every script
// tag so the page policy can stay strict.
func (c Config) Snippet(nonce string) string {
	switch c.Provider {
	case ProviderMomento:
		site := html.EscapeString(c.MomentoSiteID)
		if site == "" {
			return ""
		}
		if c.MomentoProxy {
			if originOf(c.MomentoURL) == "" {
				return ""
			}
			return withNonce(fmt.Sprintf(`<script async src="%s%s" data-site-id="%s" data-environment="prd" data-contract-version="1" data-endpoint="%s"></script>`, ProxyPath, ProxyTrackerPath, site, ProxyPath), nonce)
		}
		base := strings.TrimRight(c.MomentoURL, "/")
		if originOf(base) == "" {
			return ""
		}
		return withNonce(fmt.Sprintf(`<script async src="%s/tracker.js" data-site-id="%s" data-environment="prd" data-contract-version="1"></script>`, html.EscapeString(base), site), nonce)
	case ProviderGA4:
		id := html.EscapeString(c.MeasurementID)
		if id == "" {
			return ""
		}
		return withNonce(fmt.Sprintf(`<script async src="https://www.googletagmanager.com/gtag/js?id=%s"></script>
<script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments);}gtag('js',new Date());gtag('config','%s');</script>`, id, id), nonce)
	case ProviderGTM:
		id := html.EscapeString(c.MeasurementID)
		if id == "" {
			return ""
		}
		return withNonce(fmt.Sprintf(`<script>(function(w,d,s,l,i){w[l]=w[l]||[];w[l].push({'gtm.start':new Date().getTime(),event:'gtm.js'});var f=d.getElementsByTagName(s)[0],j=d.createElement(s),dl=l!='dataLayer'?'&l='+l:'';j.async=true;j.src='https://www.googletagmanager.com/gtm.js?id='+i+dl;f.parentNode.insertBefore(j,f);})(window,document,'script','dataLayer','%s');</script>`, id), nonce)
	case ProviderMatomo:
		base := strings.TrimRight(c.MatomoURL, "/")
		site := html.EscapeString(c.MatomoSiteID)
		if originOf(base) == "" || site == "" {
			return ""
		}
		return withNonce(fmt.Sprintf(`<script>var _paq=window._paq=window._paq||[];_paq.push(['trackPageView']);_paq.push(['enableLinkTracking']);(function(){var u="%s/";_paq.push(['setTrackerUrl',u+'matomo.php']);_paq.push(['setSiteId','%s']);var d=document,g=d.createElement('script'),s=d.getElementsByTagName('script')[0];g.async=true;g.src=u+'matomo.js';s.parentNode.insertBefore(g,s);})();</script>`, html.EscapeString(base), site), nonce)
	case ProviderCustom:
		return withNonce(strings.TrimSpace(c.CustomSnippet), nonce)
	}
	return ""
}

// Inject places the snippet into an HTML page: before </head> for the head
// placement, before </body> for the body placement. A page without the
// closing tag gets the snippet appended so it is never silently dropped.
func Inject(page []byte, snippet, placement string) []byte {
	if snippet == "" {
		return page
	}
	closing := "</head>"
	if placement == "body" {
		closing = "</body>"
	}
	at := indexFold(string(page), closing)
	if at < 0 {
		return append(append(page, '\n'), snippet...)
	}
	out := make([]byte, 0, len(page)+len(snippet)+1)
	out = append(out, page[:at]...)
	out = append(out, snippet...)
	out = append(out, '\n')
	out = append(out, page[at:]...)
	return out
}

// indexFold finds sub in s ignoring ASCII case and returns an index into s.
//
// strings.ToLower is the obvious way and the wrong one: it changes byte
// lengths for some runes — U+212A KELVIN SIGN is three bytes and folds to a
// one-byte 'k', U+0130 'İ' is two and folds to three — so an index taken from
// the folded copy lands somewhere else in the original and the nonce ends up
// inside the tag name. Every needle here is ASCII, and folding only ASCII
// keeps every byte in place.
func indexFold(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if foldASCII(s[i+j]) != foldASCII(sub[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func containsFold(s, sub string) bool { return indexFold(s, sub) >= 0 }

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && indexFold(s[:len(prefix)], prefix) == 0
}

func foldASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// withNonce adds the nonce to every script tag that does not already carry
// one, which is what lets a pasted snippet run under a strict policy unchanged.
func withNonce(snippet, nonce string) string {
	if nonce == "" || snippet == "" {
		return snippet
	}
	var b strings.Builder
	remaining := snippet
	for {
		at := indexFold(remaining, "<script")
		if at < 0 {
			b.WriteString(remaining)
			return b.String()
		}
		end := at + len("<script")
		b.WriteString(remaining[:end])
		tag := remaining[end:]
		if closing := strings.IndexByte(tag, '>'); closing >= 0 {
			tag = tag[:closing]
		}
		if !containsFold(tag, "nonce=") {
			b.WriteString(` nonce="` + html.EscapeString(nonce) + `"`)
		}
		remaining = remaining[end:]
	}
}

// PolicySources lists the extra origins the snippet needs for script-src,
// connect-src and img-src, derived from the provider so a common setup needs
// no policy knowledge at all.
func (c Config) PolicySources() (scripts, connects, images []string) {
	add := func(origin string) {
		scripts = append(scripts, origin)
		connects = append(connects, origin)
		images = append(images, origin)
	}
	switch c.Provider {
	case ProviderMomento:
		// Through the proxy the collector is same-origin and 'self' covers it.
		if !c.MomentoProxy {
			if origin := originOf(c.MomentoURL); origin != "" {
				add(origin)
			}
		}
	case ProviderGA4, ProviderGTM:
		scripts = append(scripts, "https://www.googletagmanager.com")
		connects = append(connects, "https://www.google-analytics.com", "https://analytics.google.com", "https://*.google-analytics.com")
		images = append(images, "https://www.google-analytics.com", "https://www.googletagmanager.com")
	case ProviderMatomo:
		if origin := originOf(c.MatomoURL); origin != "" {
			add(origin)
		}
	case ProviderCustom:
		// A pasted snippet names the addresses it loads and reports to, so
		// those origins are allowed without reading a policy error first.
		for _, origin := range SnippetOrigins(c.CustomSnippet) {
			add(origin)
		}
	}
	// Only origins reach the header: the setting is validated on the way in,
	// and a value that got in before that check is still not allowed to
	// place a keyword or a second directive into the policy.
	for _, host := range SplitHosts(c.AllowedHosts) {
		if validateAllowedHost(host) == nil {
			add(host)
		}
	}
	return scripts, connects, images
}

// SplitHosts breaks the administrator's allow list on commas, spaces and
// newlines.
func SplitHosts(list string) []string {
	var out []string
	for _, host := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		if host = strings.TrimSpace(host); host != "" {
			out = append(out, host)
		}
	}
	return out
}

// ValidateAllowedHosts refuses an allow list unless every entry is an origin:
// `https?://host[:port]` or the wildcard form `https://*.host`. The entries
// are written verbatim into the page policy, so a quoted keyword such as
// 'unsafe-inline', a bare `*`, a scheme source like data: or a `;` would
// otherwise let the setting rewrite the policy instead of adding to it.
func ValidateAllowedHosts(list string) error {
	for _, host := range SplitHosts(list) {
		if err := validateAllowedHost(host); err != nil {
			return err
		}
	}
	return nil
}

func validateAllowedHost(token string) error {
	bad := func() error {
		return fmt.Errorf("%q: https://host[:port] 또는 https://*.host 형식의 출처만 허용됩니다", token)
	}
	// url.Parse lets these through as part of the host; in a policy they
	// end a source, quote a keyword or start the next directive.
	if strings.ContainsAny(token, ";'\"") || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return bad()
	}
	u, err := url.Parse(token)
	if err != nil {
		return bad()
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Host == "" ||
		u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return bad()
	}
	name := u.Hostname()
	wildcard := strings.HasPrefix(name, "*.")
	host := strings.TrimPrefix(name, "*.")
	if host == "" {
		return bad()
	}
	if net.ParseIP(host) != nil {
		if wildcard {
			return bad()
		}
	} else {
		for _, r := range host {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
				return bad()
			}
		}
	}
	for _, r := range u.Port() {
		if r < '0' || r > '9' {
			return bad()
		}
	}
	return nil
}

// SnippetOrigins lists every http(s) origin written into a tracking snippet:
// the script it loads, the endpoint it posts to, the pixel it requests.
func SnippetOrigins(snippet string) []string {
	origins := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for i := 0; i < len(snippet); {
		start := indexFold(snippet[i:], "http")
		if start < 0 {
			break
		}
		start += i
		end := start
		for end < len(snippet) && !isURLBoundary(snippet[end]) {
			end++
		}
		i = end
		origin := originOf(snippet[start:end])
		if origin == "" || !hasPrefixFold(origin, "http") {
			continue
		}
		if _, dup := seen[origin]; dup {
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	return origins
}

// isURLBoundary reports the characters that cannot appear in a URL written
// inside HTML or JavaScript, which is where each address ends.
func isURLBoundary(b byte) bool {
	switch b {
	case '"', '\'', '`', '<', '>', ' ', '\t', '\n', '\r', ')', ',', ';', '\\', '+':
		return true
	}
	return false
}

func originOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	if scheme != "http" && scheme != "https" {
		return ""
	}
	return scheme + "://" + strings.ToLower(parsed.Host)
}

// AddAllowedHost appends an origin to the allow list, leaving the existing
// entries and their order alone.
func AddAllowedHost(existing, origin string) string {
	origin = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(origin), "/"))
	if origin == "" {
		return existing
	}
	for _, host := range SplitHosts(existing) {
		if strings.EqualFold(host, origin) {
			return existing
		}
	}
	if strings.TrimSpace(existing) == "" {
		return origin
	}
	return strings.TrimSpace(existing) + ", " + origin
}
