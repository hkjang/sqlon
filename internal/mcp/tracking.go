package mcp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"sqlon/internal/tracking"
)

// Visitor tracking: the administrator picks a provider on /admin/settings and
// every served HTML page carries the snippet with a per-request nonce plus the
// Content-Security-Policy that names what the snippet needs. Blocked origins
// reported by browsers are kept in memory and shown next to the settings so a
// silent policy error becomes a one-click allow. See internal/tracking.

// trackingConfig returns the effective configuration; off until an
// administrator turns it on, and always off without a meta DB.
func (s *Server) trackingConfig() tracking.Config {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.tracking
}

// trackPage injects the snippet and sets the page policy when tracking is
// active for the path. An inactive configuration leaves the page and its
// headers untouched, which is the state of every fresh install.
func (s *Server) trackPage(w http.ResponseWriter, path string, page []byte) []byte {
	cfg := s.trackingConfig()
	if !cfg.Active(path) {
		return page
	}
	nonce := newNonce()
	policy := tracking.PolicyFor(cfg, path, nonce)
	w.Header().Set("Content-Security-Policy", policy.Enforced)
	w.Header().Set("Content-Security-Policy-Report-Only", policy.ReportOnly)
	return tracking.Inject(page, cfg.Snippet(nonce), cfg.Placement)
}

func newNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *Server) registerTracking(mux *http.ServeMux) {
	// Same-origin proxy for the Momento collector: the tracker and its beacons
	// travel through this server, so no external origin enters the policy.
	mux.HandleFunc(tracking.ProxyPath+"/", s.handleMomentoProxy)

	// Browsers post here what the page policy refused. No authentication: the
	// report comes from the browser without any of our credentials, and it is
	// only accepted while tracking is on.
	mux.HandleFunc("POST "+tracking.ReportPath, func(w http.ResponseWriter, r *http.Request) {
		cfg := s.trackingConfig()
		if !cfg.Enabled || cfg.Provider == tracking.ProviderNone {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var body struct {
			Report struct {
				BlockedURI         string `json:"blocked-uri"`
				EffectiveDirective string `json:"effective-directive"`
				ViolatedDirective  string `json:"violated-directive"`
				DocumentURI        string `json:"document-uri"`
				Disposition        string `json:"disposition"`
			} `json:"csp-report"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		rep := body.Report
		directive := rep.EffectiveDirective
		if directive == "" {
			directive = rep.ViolatedDirective
		}
		page := rep.DocumentURI
		if u, err := url.Parse(rep.DocumentURI); err == nil && u.Path != "" {
			page = u.Path
		}
		s.trackingViolations.Record(rep.BlockedURI, directive, rep.Disposition, page)
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- admin: blocked origins next to the settings form ----
	mux.HandleFunc("GET /api/tracking/violations", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		cfg := s.trackingConfig()
		out := map[string]any{
			"enabled":    cfg.Enabled && cfg.Provider != tracking.ProviderNone,
			"provider":   cfg.Provider,
			"proxy":      cfg.UsesProxy(),
			"violations": s.trackingViolations.List(cfg),
		}
		if err := cfg.Validate(); err != nil {
			out["validation_error"] = err.Error()
		}
		if p := tracking.PolicyFor(cfg, "/", "…"); p.Enforced != "" {
			out["policy"] = p.Enforced
			out["policy_report_only"] = p.ReportOnly
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /api/tracking/violations", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		s.trackingViolations.Forget()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	// One click from "this origin was blocked" to "allowed": append it to the
	// allow-list setting and re-apply.
	mux.HandleFunc("POST /api/tracking/allow", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMeta(w) || !s.requireAdmin(w, r) {
			return
		}
		actor, _ := s.authenticate(r)
		var req struct {
			Origin string `json:"origin"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		origin := strings.TrimSpace(req.Origin)
		if u, err := url.Parse(origin); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			writeAPIError(w, http.StatusBadRequest, errEmpty("origin must be an http(s) origin such as https://collector.example.com"))
			return
		}
		stored, err := s.Meta.Store.GetSettings(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		updated := tracking.AddAllowedHost(stored[tracking.SetAllowedHosts], origin)
		if err := s.Meta.ApplySetting(r.Context(), tracking.SetAllowedHosts, updated, actorName(actor)); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.ApplySettings(r.Context()); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		s.adminAudit(r, "tracking_allow_origin", origin+" by "+actorName(actor), nil)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "allowed_hosts": updated})
	})
}

// handleMomentoProxy forwards /momento/* to the configured collector. The
// collector receives visitor beacons only, so the app's own credentials are
// stripped before the request leaves.
func (s *Server) handleMomentoProxy(w http.ResponseWriter, r *http.Request) {
	cfg := s.trackingConfig()
	if !cfg.UsesProxy() {
		http.NotFound(w, r)
		return
	}
	target, err := url.Parse(strings.TrimRight(cfg.MomentoURL, "/"))
	if err != nil || target.Host == "" {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, tracking.ProxyPath)
			pr.Out.URL.RawPath = ""
			pr.SetURL(target)
			for _, h := range []string{"Cookie", "Authorization", "X-Admin-Token", "X-MCP-Key"} {
				pr.Out.Header.Del(h)
			}
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
