package mcp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sqlon/internal/meta"
)

// Document handoff — sender side of the company-wide HANDOFF-STANDARD.
//
// sqlon hands its DBA digest (a per-profile operational summary) to the
// services that can read markdown (muni, ptium, weekly). Services never hold
// each other's credentials: the logged-in user mints a single-use, five-minute
// claim bound to one document, the browser opens the receiver's /handoff with
// {source, claim}, and the receiver fetches the body from us with the claim
// alone. Used or expired claims answer 404 without saying which.
//
// Endpoint names, request shape, and status codes are fixed by the standard —
// six services interlock on them. Do not rename.
//
//	POST /api/v1/handoff/claims          {"resource":"dba-digest:<profile>","format":"markdown"} → 201
//	GET  /api/v1/handoff/claims/{claim}  → 200 text/markdown (once) | 404
//	GET  /api/v1/handoff/targets         → the admin-configured destinations that accept markdown
//
// The target list is a stored setting (handoff_targets); it defaults to
// empty, in which case the "send to" button never appears.

const (
	handoffClaimTTL       = 5 * time.Minute
	handoffFormatMarkdown = "markdown"
	handoffResourceDigest = "dba-digest" // "dba-digest" (all profiles) or "dba-digest:<profile>"
)

// handoffAccepts is the standard's format table: what each service receives.
// sqlon only sends markdown, so a destination is offered only when its row
// lists markdown.
var handoffAccepts = map[string][]string{
	"muni":   {"markdown"},
	"kanpic": {"csv", "xlsx"},
	"ptium":  {"markdown", "docx", "csv", "xlsx", "txt"},
	"weekly": {"markdown", "docx", "pptx"},
}

// handoffTarget is one allow-listed destination.
type handoffTarget struct {
	Service string `json:"service"`
	Origin  string `json:"origin"`
}

// parseHandoffTargets reads the handoff_targets setting: comma/newline
// separated "service=https://origin" entries. Entries that are malformed,
// name an unknown service, or carry a path/query are skipped — the allow
// list must never widen by accident. Output is sorted by service and
// de-duplicated on service (last one wins).
func parseHandoffTargets(raw string) []handoffTarget {
	byService := map[string]string{}
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		svc, origin, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		svc = strings.ToLower(strings.TrimSpace(svc))
		if _, known := handoffAccepts[svc]; !known {
			continue
		}
		origin, ok = normalizeHandoffOrigin(origin)
		if !ok {
			continue
		}
		byService[svc] = origin
	}
	out := make([]handoffTarget, 0, len(byService))
	for svc, origin := range byService {
		out = append(out, handoffTarget{Service: svc, Origin: origin})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// normalizeHandoffOrigin accepts "http(s)://host[:port]" (an optional single
// trailing slash is tolerated) and returns the bare origin.
func normalizeHandoffOrigin(s string) (string, bool) {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// handoffTargetsFor filters the allow list to services that accept format.
func handoffTargetsFor(targets []handoffTarget, format string) []handoffTarget {
	out := []handoffTarget{}
	for _, t := range targets {
		for _, f := range handoffAccepts[t.Service] {
			if f == format {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// ---- claim store ----

type handoffClaim struct {
	Resource    string
	Filename    string
	ContentType string
	Body        []byte
	ExpiresAt   time.Time
}

// handoffStore keeps live claims in memory. Claims are short and single-use,
// so nothing survives a restart on purpose; a restart merely invalidates the
// handful of claims minted in the last five minutes.
type handoffStore struct {
	mu     sync.Mutex
	claims map[string]*handoffClaim
	now    func() time.Time
}

func newHandoffStore() *handoffStore {
	return &handoffStore{claims: map[string]*handoffClaim{}, now: time.Now}
}

// issue stores the claim under a fresh ≥128-bit random token.
func (h *handoffStore) issue(c *handoffClaim) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	now := h.now()
	c.ExpiresAt = now.Add(handoffClaimTTL)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked(now)
	h.claims[token] = c
	return token, nil
}

// take consumes the claim: it is returned at most once and only before expiry.
func (h *handoffStore) take(token string) (*handoffClaim, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.sweepLocked(now)
	c, ok := h.claims[token]
	if !ok {
		return nil, false
	}
	delete(h.claims, token)
	if !now.Before(c.ExpiresAt) {
		return nil, false
	}
	return c, true
}

func (h *handoffStore) sweepLocked(now time.Time) {
	for k, c := range h.claims {
		if !now.Before(c.ExpiresAt) {
			delete(h.claims, k)
		}
	}
}

// ---- HTTP surface ----

func (s *Server) registerHandoff(mux *http.ServeMux) {
	if s.handoff == nil {
		s.handoff = newHandoffStore()
	}
	mux.HandleFunc("GET /api/v1/handoff/targets", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"targets": handoffTargetsFor(s.currentHandoffTargets(), handoffFormatMarkdown),
			"format":  handoffFormatMarkdown,
			"source":  s.handoffSource(r),
		})
	})

	mux.HandleFunc("POST /api/v1/handoff/claims", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		var req struct {
			Resource string `json:"resource"`
			Format   string `json:"format"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		if req.Format != handoffFormatMarkdown {
			writeAPIError(w, http.StatusBadRequest, errEmpty("unsupported format: this service sends markdown only"))
			return
		}
		profile, ok := parseHandoffResource(req.Resource)
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("document not found or not permitted"))
			return
		}
		// bound to one document the caller can read: the digest of a profile
		// the actor may use (the all-profiles digest follows /api/db/digest).
		if profile != "" {
			if _, err := s.profileByID(r, profile); err != nil {
				writeAPIError(w, http.StatusNotFound, errEmpty("document not found or not permitted"))
				return
			}
			if err := s.canUseProfileID(r.Context(), actor, profile); err != nil {
				writeAPIError(w, http.StatusNotFound, errEmpty("document not found or not permitted"))
				return
			}
		}
		now := time.Now()
		body := renderDBADigestMarkdown(s.mcpDBADigest(profile, 7, 0), s.handoffSource(r), now)
		claim := &handoffClaim{
			Resource:    req.Resource,
			Filename:    handoffDigestFilename(profile, now),
			ContentType: "text/markdown; charset=utf-8",
			Body:        body,
		}
		token, err := s.handoff.issue(claim)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		// audit the document, never the claim
		s.adminAudit(r, "handoff_claim_create", req.Resource+" by "+actorName(actor), nil)
		writeJSON(w, http.StatusCreated, map[string]any{
			"claim":        token,
			"source":       s.handoffSource(r),
			"filename":     claim.Filename,
			"content_type": claim.ContentType,
			"bytes":        len(claim.Body),
			"expires_at":   claim.ExpiresAt.Format(time.RFC3339),
		})
	})

	// The claim is the credential: no login, one use, 404 for anything else.
	mux.HandleFunc("GET /api/v1/handoff/claims/{claim}", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.handoff.take(r.PathValue("claim"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.adminAudit(r, "handoff_claim_serve", c.Resource, nil)
		w.Header().Set("Content-Type", c.ContentType)
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(c.Filename))
		w.Header().Set("Content-Length", strconv.Itoa(len(c.Body)))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(c.Body)
	})
}

// parseHandoffResource maps a resource id to a digest profile ("" = all).
func parseHandoffResource(res string) (profile string, ok bool) {
	res = strings.TrimSpace(res)
	if res == handoffResourceDigest {
		return "", true
	}
	if p, found := strings.CutPrefix(res, handoffResourceDigest+":"); found && strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p), true
	}
	return "", false
}

// currentHandoffTargets reads the live allow list (set by ApplySettings).
func (s *Server) currentHandoffTargets() []handoffTarget {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.handoffTargets
}

// handoffSource is this service's origin as the receiver must reach it: the
// handoff_public_url setting when set, else the request's own scheme+host
// (honoring a reverse proxy's X-Forwarded-Proto / X-Forwarded-Host).
func (s *Server) handoffSource(r *http.Request) string {
	s.settingsMu.RLock()
	pub := s.handoffPublicURL
	s.settingsMu.RUnlock()
	if pub != "" {
		return pub
	}
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// applyHandoffSettings parses the stored allow list; called under settingsMu.
func (s *Server) applyHandoffSettings(eff map[string]string) {
	s.handoffTargets = parseHandoffTargets(eff[meta.SetHandoffTargets])
	s.handoffPublicURL = ""
	if o, ok := normalizeHandoffOrigin(eff[meta.SetHandoffPublicURL]); ok {
		s.handoffPublicURL = o
	}
}

// ---- markdown rendering ----

func handoffDigestFilename(profile string, now time.Time) string {
	scope := "전체"
	if profile != "" {
		scope = profile
	}
	return fmt.Sprintf("DBA 다이제스트 %s %s.md", scope, now.Format("2006-01-02"))
}

// renderDBADigestMarkdown turns the digest map (mcpDBADigest) into the
// document that is handed over. The footer names the origin so the receiver
// can record where the document came from.
func renderDBADigestMarkdown(d map[string]any, source string, now time.Time) []byte {
	var b strings.Builder
	profile, _ := d["profile"].(string)
	scope := "전체 프로파일"
	if profile != "" {
		scope = "`" + profile + "`"
	}
	days, _ := d["window_days"].(int)
	slowMs, _ := d["slow_ms"].(int)
	total, _ := d["total_queries"].(int)
	errRate, _ := d["error_rate"].(float64)
	slow, _ := d["slow_queries"].(int)
	p95, _ := d["latency_p95_ms"].(int64)
	maxMs, _ := d["latency_max_ms"].(int64)
	idxCount, _ := d["index_candidate_count"].(int)
	headline, _ := d["headline"].(string)

	fmt.Fprintf(&b, "# DBA 다이제스트 — %s (최근 %d일)\n\n", scope, days)
	if headline != "" {
		fmt.Fprintf(&b, "> %s\n\n", headline)
	}
	fmt.Fprintf(&b, "- 기간: 최근 %d일 (느린 쿼리 기준 %dms)\n", days, slowMs)
	fmt.Fprintf(&b, "- 총 쿼리: %d건\n", total)
	fmt.Fprintf(&b, "- 오류율: %.1f%%\n", errRate*100)
	fmt.Fprintf(&b, "- 느린 쿼리: %d건\n", slow)
	fmt.Fprintf(&b, "- 지연 p95 / 최대: %dms / %dms\n", p95, maxMs)
	if h, ok := d["peak_hour"].(int); ok && h >= 0 {
		fmt.Fprintf(&b, "- 피크 시간대: %02d시\n", h)
	}
	fmt.Fprintf(&b, "- 인덱스 후보: %d개\n", idxCount)

	if tables, ok := d["top_tables"].([]countItem); ok && len(tables) > 0 {
		b.WriteString("\n## 많이 조회된 테이블\n\n| 테이블 | 조회 수 |\n|---|---|\n")
		for _, t := range tables {
			fmt.Fprintf(&b, "| %s | %d |\n", mdCell(t.Key), t.Count)
		}
	}
	if cands, ok := d["top_index_candidates"].([]map[string]any); ok && len(cands) > 0 {
		b.WriteString("\n## 인덱스 후보 (상위)\n\n| 테이블 | 컬럼 | 느린 쿼리 수 | 평균 ms | 제안 DDL |\n|---|---|---|---|---|\n")
		for _, c := range cands {
			tbl, _ := c["table"].(string)
			col, _ := c["column"].(string)
			occ, _ := c["occurrences"].(int)
			avg, _ := c["avg_ms"].(int64)
			ddl, _ := c["ddl"].(string)
			fmt.Fprintf(&b, "| %s | %s | %d | %d | `%s` |\n", mdCell(tbl), mdCell(col), occ, avg, mdCell(ddl))
		}
		b.WriteString("\n인덱스 후보 DDL은 검토 후 변경계획으로 수행하세요.\n")
	}
	fmt.Fprintf(&b, "\n---\n출처: sqlon %s · 생성 %s\n", source, now.Format(time.RFC3339))
	return []byte(b.String())
}

// mdCell keeps a value inside one markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}
