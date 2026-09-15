package tracking

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxViolations bounds the recorder. Blocked requests repeat on every page
// view, so the interesting information is which origins are blocked, not how
// many times — a small buffer of distinct origins is enough to fix a snippet.
const MaxViolations = 100

// Violation is one origin the content security policy refused, kept with the
// directive that refused it so the console can say what to allow.
type Violation struct {
	Origin    string `json:"origin"`
	Directive string `json:"directive"`
	// Disposition is "enforce" when the browser actually blocked the request
	// and "report" when a report-only policy merely flagged it.
	Disposition string    `json:"disposition"`
	Page        string    `json:"page"`
	Count       int       `json:"count"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Allowed     bool      `json:"allowed"`
}

// Recorder collects policy violations reported by browsers. It is deliberately
// in memory: the reports are a live troubleshooting aid for the person pasting
// a snippet, not an audit record.
type Recorder struct {
	mu         sync.Mutex
	violations map[string]*Violation
	now        func() time.Time
}

func NewRecorder() *Recorder {
	return &Recorder{violations: map[string]*Violation{}, now: time.Now}
}

// Record notes one blocked request. Anything that is not an http origin, such
// as "inline", a browser extension or a data: URL, is ignored because allowing
// it is neither possible nor useful.
func (r *Recorder) Record(blockedURI, directive, disposition, page string) {
	origin := originOf(blockedURI)
	if origin == "" {
		return
	}
	directive = strings.TrimSpace(strings.ToLower(directive))
	if i := strings.IndexByte(directive, ' '); i > 0 {
		directive = directive[:i]
	}
	if directive == "" {
		directive = "connect-src"
	}
	if disposition != "report" {
		disposition = "enforce"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := directive + " " + origin
	if existing, ok := r.violations[key]; ok {
		existing.Count++
		existing.LastSeen = r.now()
		existing.Page = page
		existing.Disposition = disposition
		return
	}
	if len(r.violations) >= MaxViolations {
		r.evictOldest()
	}
	now := r.now()
	r.violations[key] = &Violation{Origin: origin, Directive: directive, Disposition: disposition, Page: page, Count: 1, FirstSeen: now, LastSeen: now}
}

func (r *Recorder) evictOldest() {
	var oldestKey string
	var oldest time.Time
	for key, v := range r.violations {
		if oldestKey == "" || v.LastSeen.Before(oldest) {
			oldestKey, oldest = key, v.LastSeen
		}
	}
	delete(r.violations, oldestKey)
}

// List returns the blocked origins, most recent first, marking the ones the
// configuration already allows so a fixed snippet stops nagging.
func (r *Recorder) List(c Config) []Violation {
	allowed := map[string]struct{}{}
	scripts, connects, images := c.PolicySources()
	for _, group := range [][]string{scripts, connects, images} {
		for _, origin := range group {
			allowed[strings.ToLower(strings.TrimSuffix(origin, "/"))] = struct{}{}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]Violation, 0, len(r.violations))
	for _, v := range r.violations {
		copied := *v
		_, known := allowed[strings.ToLower(copied.Origin)]
		copied.Allowed = known || matchesWildcard(copied.Origin, allowed)
		items = append(items, copied)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].LastSeen.Equal(items[j].LastSeen) {
			return items[i].Origin < items[j].Origin
		}
		return items[i].LastSeen.After(items[j].LastSeen)
	})
	return items
}

// Forget drops the recorded violations, which is what an administrator does
// after fixing a snippet to check whether anything is still blocked.
func (r *Recorder) Forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.violations = map[string]*Violation{}
}

// matchesWildcard covers policy entries such as https://*.google-analytics.com.
func matchesWildcard(origin string, allowed map[string]struct{}) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	lower := strings.ToLower(origin)
	host := strings.ToLower(parsed.Host)
	for pattern := range allowed {
		star := strings.Index(pattern, "*.")
		if star < 0 {
			continue
		}
		if strings.HasPrefix(lower, pattern[:star]) && strings.HasSuffix(host, pattern[star+1:]) {
			return true
		}
	}
	return false
}
