package mcp

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/earlywarning"
	"sqlon/internal/metasync"
)

// EarlyWarningOptions configures the early-warning engine (예방 경보).
type EarlyWarningOptions struct {
	Disabled bool
	// WebhookURL receives notifications; empty falls back to
	// Options.AlertWebhookURL (the digest webhook).
	WebhookURL string
	// ConsoleURL is linked from notifications (e.g. https://sqlon.example/admin/alerts).
	ConsoleURL       string
	MinSeverity      string
	Renotify         time.Duration
	MaintenanceEvery time.Duration
	// SchemaEvery spaces out schema-change detection; negative disables it.
	SchemaEvery time.Duration
	TimeZone    string
	// DigestAt is the local HH:MM of the daily capacity report ("" = off).
	DigestAt string
}

// schemaSource adapts the lazily built metasync service.
type schemaSource struct{ s *Server }

func (a schemaSource) Collect(ctx context.Context, req metasync.CollectRequest) (*metasync.RawSnapshot, error) {
	return a.s.metasyncService().Collect(ctx, req)
}

func newEarlyWarning(s *Server, dataDir string, coll *collector.Service, opts Options) *earlywarning.Engine {
	ew := opts.EarlyWarning
	if ew.TimeZone != "" {
		if err := earlywarning.SetDisplayLocation(ew.TimeZone); err != nil {
			log.Printf("sqlon: early-warning time zone %q: %v (using Asia/Seoul)", ew.TimeZone, err)
		}
	}
	eng := earlywarning.New(earlywarning.Config{
		Dir:               filepath.Join(dataDir, "operations", "earlywarning"),
		MinNotifySeverity: ew.MinSeverity,
		RenotifyInterval:  ew.Renotify,
		MaintenanceEvery:  ew.MaintenanceEvery,
		SchemaEvery:       ew.SchemaEvery,
		DigestAt:          ew.DigestAt,
	})
	eng.Profiles = s.DB
	eng.Maintenance = s.Observability
	if ew.SchemaEvery >= 0 {
		eng.Schema = schemaSource{s: s}
	}
	if s.Changes != nil {
		eng.Plans = s.Changes
	}
	if scanner, ok := coll.Store.(earlywarning.HistoryScanner); ok {
		eng.History = scanner
	}
	webhook := strings.TrimSpace(ew.WebhookURL)
	if webhook == "" {
		webhook = strings.TrimSpace(opts.AlertWebhookURL)
	}
	if webhook != "" {
		eng.Notifier = &earlywarning.WebhookNotifier{URL: webhook, ConsoleURL: ew.ConsoleURL}
	}
	channels := map[string]earlywarning.Notifier{}
	var channelsMu sync.Mutex
	eng.Route = func(p dbconn.Profile) (earlywarning.Notifier, error) {
		url, err := p.Alerting.ResolveWebhook()
		if err != nil || url == "" || url == webhook {
			return nil, err
		}
		channelsMu.Lock()
		defer channelsMu.Unlock()
		n := channels[url]
		if n == nil {
			n = &earlywarning.WebhookNotifier{URL: url, ConsoleURL: ew.ConsoleURL}
			channels[url] = n
		}
		return n, nil
	}
	return eng
}

// evaluateEarlyWarning runs one engine cycle over a collection batch.
func (s *Server) evaluateEarlyWarning(ctx context.Context, batch collector.BatchResult, force bool) earlywarning.CycleReport {
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return s.EarlyWarning.Evaluate(runCtx, batch, earlywarning.EvaluateOptions{Force: force})
}

func filterProfiles(profiles []dbconn.Profile, id string) []dbconn.Profile {
	id = strings.TrimSpace(id)
	if id == "" {
		return profiles
	}
	for _, p := range profiles {
		if p.ID == id {
			return []dbconn.Profile{p}
		}
	}
	return []dbconn.Profile{}
}

func (s *Server) earlyWarningDisabled() map[string]any {
	return map[string]any{"status": "disabled", "headline": "예방 경보가 꺼져 있습니다 (SQLON_EARLY_WARNING=off).", "firing": []any{}, "profiles": []any{}}
}

func (s *Server) registerEarlyWarningAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/early-warning", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, _, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		if s.EarlyWarning == nil {
			writeJSON(w, http.StatusOK, s.earlyWarningDisabled())
			return
		}
		writeJSON(w, http.StatusOK, s.EarlyWarning.Board(filterProfiles(profiles, r.URL.Query().Get("profile"))))
	})
	mux.HandleFunc("POST /api/early-warning/alerts/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		profiles, _, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		alert, found := s.EarlyWarning.Alert(id)
		if _, allowed := allowedProfile(profiles, alert.ProfileID); !found || !allowed {
			writeAPIError(w, http.StatusNotFound, errEmpty("alert not found or not permitted"))
			return
		}
		var req struct {
			Note string `json:"note"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
				writeAPIError(w, http.StatusBadRequest, err)
				return
			}
		}
		name := "admin-token"
		if actor != nil {
			name = actor.Username
		}
		acked, err := s.EarlyWarning.Ack(id, name, req.Note)
		if err != nil {
			status := http.StatusConflict
			if earlywarning.IsNotFound(err) {
				status = http.StatusNotFound
			}
			writeAPIError(w, status, err)
			return
		}
		s.earlyWarningAudit(r, "early_warning_ack", acked.ProfileID, map[string]any{"alert": id, "rule": acked.Rule, "note": req.Note, "actor": name})
		writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true, "alert": acked})
	})
	mux.HandleFunc("POST /api/early-warning/evaluate", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		batch := s.Collector.CollectAll(ctx, nil, true)
		report := s.evaluateEarlyWarning(ctx, batch, true)
		s.earlyWarningAudit(r, "early_warning_evaluate", "", map[string]any{"firing": report.Firing, "notifications": report.Notifications, "delivery_error": report.DeliveryError})
		profiles, _, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"report": report, "collected": batch.Succeeded, "collection_failed": batch.Failed, "board": s.EarlyWarning.Board(profiles)})
	})
	mux.HandleFunc("POST /api/early-warning/test-notification", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		notifier := s.EarlyWarning.Notifier
		scope := "기본 채널"
		if id := r.URL.Query().Get("profile"); id != "" {
			profiles, _, ok := s.fleetProfilesForRequest(w, r)
			if !ok {
				return
			}
			p, found := allowedProfile(profiles, id)
			if !found {
				writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
				return
			}
			custom, err := s.EarlyWarning.Route(p)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, err)
				return
			}
			if custom != nil {
				notifier, scope = custom, p.ID+" 전용 채널"
			}
		}
		if notifier == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("no notification webhook configured (SQLON_ALERT_WEBHOOK or profile alerting.webhook_ref)"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		err := notifier.NotifyText(ctx, "test", "#### 🛡️ SQLON 예방 경보 — 알림 경로 테스트\n이 메시지가 보이면 "+scope+"의 예방 경보가 이 채널로 전달됩니다.")
		s.earlyWarningAudit(r, "early_warning_test_notification", r.URL.Query().Get("profile"), map[string]any{"delivered": err == nil, "target": notifier.Target()})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"delivered": false, "target": notifier.Target(), "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"delivered": true, "target": notifier.Target()})
	})
	mux.HandleFunc("POST /api/early-warning/disk", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		var req struct {
			Profile  string                    `json:"profile"`
			Profiles []string                  `json:"profiles"`
			Host     string                    `json:"host"`
			Volumes  []earlywarning.DiskVolume `json:"volumes"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		ids := append([]string{}, req.Profiles...)
		if req.Profile != "" {
			ids = append(ids, req.Profile)
		}
		if len(ids) == 0 {
			writeAPIError(w, http.StatusBadRequest, errEmpty("profile is required"))
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		for _, id := range ids {
			if _, found := allowedProfile(profiles, id); !found {
				writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted: "+id))
				return
			}
		}
		report := earlywarning.HostDiskReport{Host: strings.TrimSpace(req.Host), Volumes: req.Volumes}
		for _, id := range ids {
			if err := s.EarlyWarning.ReportDisk(ctx, id, report); err != nil {
				writeAPIError(w, http.StatusBadRequest, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "profiles": ids, "volumes": len(req.Volumes)})
	})
	mux.HandleFunc("POST /api/early-warning/silences", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.requireQueryActor(w, r)
		if !ok {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		var req struct {
			Profile  string `json:"profile"`
			Rule     string `json:"rule"`
			Duration string `json:"duration"`
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		d, err := time.ParseDuration(strings.TrimSpace(req.Duration))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, errEmpty("duration must be like 30m, 2h, 24h"))
			return
		}
		if strings.TrimSpace(req.Profile) == "" {
			// silencing every database is an admin decision
			if !s.requireAdmin(w, r) {
				return
			}
		} else {
			profiles, _, ok := s.fleetProfilesForRequest(w, r)
			if !ok {
				return
			}
			if _, found := allowedProfile(profiles, req.Profile); !found {
				writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
				return
			}
		}
		name := "admin-token"
		if actor != nil {
			name = actor.Username
		}
		silence, err := s.EarlyWarning.AddSilence(earlywarning.Silence{ProfileID: req.Profile, Rule: req.Rule, Reason: req.Reason, CreatedBy: name}, d)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.earlyWarningAudit(r, "early_warning_silence", req.Profile, map[string]any{"silence": silence.ID, "rule": req.Rule, "until": silence.EndsAt, "reason": req.Reason, "actor": name})
		writeJSON(w, http.StatusOK, map[string]any{"silence": silence})
	})
	mux.HandleFunc("DELETE /api/early-warning/silences/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		id := r.PathValue("id")
		current, found := s.EarlyWarning.Silence(id)
		if !found {
			writeAPIError(w, http.StatusNotFound, errEmpty("silence not found or already ended"))
			return
		}
		if current.ProfileID == "" {
			if !s.requireAdmin(w, r) {
				return
			}
		} else {
			profiles, _, ok := s.fleetProfilesForRequest(w, r)
			if !ok {
				return
			}
			if _, allowed := allowedProfile(profiles, current.ProfileID); !allowed {
				writeAPIError(w, http.StatusNotFound, errEmpty("silence not found or already ended"))
				return
			}
		}
		ended, err := s.EarlyWarning.EndSilence(id)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		s.earlyWarningAudit(r, "early_warning_silence_end", ended.ProfileID, map[string]any{"silence": id})
		writeJSON(w, http.StatusOK, map[string]any{"ended": true, "silence": ended})
	})
}

func (s *Server) earlyWarningAudit(r *http.Request, action, profileID string, fields map[string]any) {
	entry := map[string]any{"ts": time.Now().Format(time.RFC3339Nano), "tool": "admin:" + action, "detail": profileID, "remote": r.RemoteAddr}
	for k, v := range fields {
		entry[k] = v
	}
	s.appendAudit(entry)
}

// mcpEarlyWarnings is the get_early_warnings tool.
func (s *Server) mcpEarlyWarnings(ctx context.Context, profileID string) (any, error) {
	if s.EarlyWarning == nil {
		return s.earlyWarningDisabled(), nil
	}
	profiles, err := s.usableProfiles(ctx)
	if err != nil {
		return nil, err
	}
	scoped := filterProfiles(profiles, profileID)
	if profileID != "" && len(scoped) == 0 {
		return map[string]any{"status": "not_found", "warnings": []string{"db profile not found or not permitted"}}, nil
	}
	board := s.EarlyWarning.Board(scoped)
	return map[string]any{
		"status":        "ok",
		"headline":      board.Headline,
		"summary":       board.Summary,
		"firing":        board.Firing,
		"profiles":      board.Profiles,
		"schema_events": board.SchemaEvents,
		"delivery":      board.Delivery,
		"last_cycle_at": board.LastCycleAt,
		"note":          "읽기 전용 예방 경보 현황입니다. 조치는 변경계획으로, 경보 확인(ack)은 /admin/alerts 또는 POST /api/early-warning/alerts/{id}/ack 로 합니다.",
	}, nil
}
