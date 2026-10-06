package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	// HeartbeatURL is pinged after every healthy cycle (dead man's switch).
	HeartbeatURL string
	// EscalationURL pages on-call for critical alerts unacknowledged for
	// EscalateAfter (0 = default 30m, negative = never, 1ns = at once).
	EscalationURL string
	EscalateAfter time.Duration
	// ChatActions ("mattermost") adds buttons calling back ActionURL.
	ChatActions string
	ActionURL   string
}

// schemaSource adapts the lazily built metasync service.
type schemaSource struct{ s *Server }

func (a schemaSource) Collect(ctx context.Context, req metasync.CollectRequest) (*metasync.RawSnapshot, error) {
	return a.s.metasyncService().Collect(ctx, req)
}

func (a schemaSource) RecentMigrations(ctx context.Context, snap *metasync.RawSnapshot, since time.Time) ([]metasync.Migration, error) {
	return a.s.metasyncService().RecentMigrations(ctx, snap, since)
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
		EscalateAfter:     ew.EscalateAfter,
	})
	eng.Profiles = s.DB
	eng.Maintenance = s.Observability
	if ew.SchemaEvery >= 0 {
		eng.Schema = schemaSource{s: s}
		eng.Migrations = schemaSource{s: s}
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
	// every webhook channel gets chat buttons when they are enabled
	newChannel := func(url, console string) earlywarning.Notifier {
		return &earlywarning.WebhookNotifier{URL: url, ConsoleURL: console, Attach: eng.ChatAttachment}
	}
	if webhook != "" {
		eng.Notifier = newChannel(webhook, ew.ConsoleURL)
	}
	if strings.TrimSpace(ew.EscalationURL) != "" {
		eng.Escalation = newChannel(strings.TrimSpace(ew.EscalationURL), ew.ConsoleURL)
	}
	eng.ConsoleURL, eng.HeartbeatURL = ew.ConsoleURL, strings.TrimSpace(ew.HeartbeatURL)
	eng.ChatActions, eng.ActionURL = strings.ToLower(strings.TrimSpace(ew.ChatActions)), strings.TrimSpace(ew.ActionURL)
	eng.Factory = newChannel
	eng.Resolve = func(ref string) (string, error) {
		return (&dbconn.AlertingConfig{WebhookRef: ref}).ResolveWebhook()
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
			n = newChannel(url, ew.ConsoleURL)
			channels[url] = n
		}
		return n, nil
	}
	eng.RouteEscalation = func(p dbconn.Profile) (earlywarning.Notifier, error) {
		url, err := p.Alerting.ResolveEscalation()
		if err != nil || url == "" {
			return nil, err
		}
		channelsMu.Lock()
		defer channelsMu.Unlock()
		n := channels[url]
		if n == nil {
			n = newChannel(url, ew.ConsoleURL)
			channels[url] = n
		}
		return n, nil
	}
	if err := eng.LoadSettings(); err != nil {
		log.Printf("sqlon: early-warning runtime settings not applied: %v", err)
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
		var prof *dbconn.Profile
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
			prof = &p
		}
		notifier, scope := s.EarlyWarning.DefaultNotifier(), "기본 채널"
		message := "#### 🛡️ SQLON 예방 경보 — 알림 경로 테스트\n이 메시지가 보이면 %s의 예방 경보가 이 채널로 전달됩니다."
		switch channel := r.URL.Query().Get("channel"); channel {
		case "", "team":
			if prof != nil && s.EarlyWarning.Route != nil {
				custom, err := s.EarlyWarning.Route(*prof)
				if err != nil {
					writeAPIError(w, http.StatusBadRequest, err)
					return
				}
				if custom != nil {
					notifier, scope = custom, prof.ID+" 전용 채널"
				}
			}
		case "escalation":
			n, err := s.EarlyWarning.EscalationNotifier(prof)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, err)
				return
			}
			notifier, scope = n, "당직 호출 채널"
			message = "#### 📟 SQLON 당직 호출 — 경로 테스트\n이 메시지가 보이면 확인되지 않은 critical 경보가 %s로 호출됩니다."
			if n == nil {
				writeAPIError(w, http.StatusConflict, errEmpty("no escalation webhook configured (SQLON_ALERT_ESCALATION_WEBHOOK or profile alerting.escalation_ref)"))
				return
			}
		default:
			writeAPIError(w, http.StatusBadRequest, errEmpty("channel must be team or escalation"))
			return
		}
		if notifier == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("no notification webhook configured (SQLON_ALERT_WEBHOOK or profile alerting.webhook_ref)"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		err := notifier.NotifyText(ctx, "test", fmt.Sprintf(message, scope))
		s.earlyWarningAudit(r, "early_warning_test_notification", r.URL.Query().Get("profile"), map[string]any{"delivered": err == nil, "target": notifier.Target(), "channel": r.URL.Query().Get("channel")})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"delivered": false, "target": notifier.Target(), "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"delivered": true, "target": notifier.Target()})
	})
	// Chat servers call this back from message buttons. It carries no SQLON
	// credentials: the signed, expiring token in the context authorizes
	// exactly one action on one alert.
	mux.HandleFunc("POST "+earlywarning.ChatActionPath, func(w http.ResponseWriter, r *http.Request) {
		if s.EarlyWarning == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ephemeral_text": "예방 경보가 꺼져 있습니다."})
			return
		}
		var req struct {
			UserID   string         `json:"user_id"`
			UserName string         `json:"user_name"`
			Context  map[string]any `json:"context"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ephemeral_text": "잘못된 요청입니다."})
			return
		}
		token, _ := req.Context["token"].(string)
		alertID, action, err := s.EarlyWarning.VerifyAction(token)
		if err != nil {
			s.earlyWarningAudit(r, "early_warning_chat_action_rejected", "", map[string]any{"error": err.Error(), "chat_user": firstNonEmpty(req.UserName, req.UserID)})
			writeJSON(w, http.StatusForbidden, map[string]any{"ephemeral_text": "⛔ " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ephemeral_text": s.chatAction(r, alertID, action, "mattermost:"+firstNonEmpty(req.UserName, req.UserID, "unknown"))})
	})
	mux.HandleFunc("GET /api/early-warning/settings", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		writeJSON(w, http.StatusOK, s.EarlyWarning.Settings())
	})
	mux.HandleFunc("PUT /api/early-warning/settings", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		if s.EarlyWarning == nil {
			writeAPIError(w, http.StatusConflict, errEmpty("early warning is disabled"))
			return
		}
		var req struct {
			Settings earlywarning.Settings `json:"settings"`
			Reset    []string              `json:"reset"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		actor := "admin-token"
		if u, err := s.authenticate(r); err == nil && u != nil {
			actor = u.Username
		}
		view, err := s.EarlyWarning.UpdateSettings(req.Settings, req.Reset, actor)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		s.earlyWarningAudit(r, "early_warning_settings", "", map[string]any{"changed": settingsChanged(req.Settings), "reset": req.Reset, "actor": actor})
		writeJSON(w, http.StatusOK, view)
	})
	mux.HandleFunc("GET /api/early-warning/alerts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		raw, _ := json.Marshal(map[string]string{"alert_id": r.PathValue("id")})
		out, err := s.earlyWarningToolScoped(ctx, profiles, "explain_early_warning", raw)
		writeToolResult(w, out, err)
	})
	mux.HandleFunc("GET /api/early-warning/capacity-plan", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		days := 90.0
		if v := r.URL.Query().Get("days"); v != "" {
			fmt.Sscanf(v, "%g", &days)
		}
		raw, _ := json.Marshal(map[string]any{"profile": r.URL.Query().Get("profile"), "target_days": days})
		out, err := s.earlyWarningToolScoped(ctx, profiles, "plan_capacity", raw)
		writeToolResult(w, out, err)
	})
	mux.HandleFunc("POST /api/early-warning/alerts/{id}/fix", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDBA(w, r) {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		var body map[string]any
		if r.ContentLength != 0 {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
		}
		if body == nil {
			body = map[string]any{}
		}
		body["alert_id"] = r.PathValue("id")
		raw, _ := json.Marshal(body)
		out, err := s.earlyWarningToolScoped(ctx, profiles, "propose_early_warning_fix", raw)
		writeToolResult(w, out, err)
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
		"incidents":     board.Incidents,
		"silences":      board.Silences,
		"next_actions":  nextActions(board),
		"delivery":      board.Delivery,
		"channels":      board.Channels,
		"last_cycle_at": board.LastCycleAt,
		"note":          "읽기 전용 예방 경보 현황입니다. next_actions 를 위에서부터 따르세요(원인 경보 우선). 워크플로: MCP 프롬프트 early_warning_triage.",
	}, nil
}

// writeToolResult maps a tool's status onto an HTTP code for REST callers.
func writeToolResult(w http.ResponseWriter, out any, err error) {
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	code := http.StatusOK
	if m, ok := out.(map[string]any); ok {
		switch m["status"] {
		case "not_found":
			code = http.StatusNotFound
		case "forbidden":
			code = http.StatusForbidden
		case "invalid":
			code = http.StatusBadRequest
		case "conflict", "disabled", "not_configured":
			code = http.StatusConflict
		case "error":
			code = http.StatusBadGateway
		}
	}
	writeJSON(w, code, out)
}

// chatAction performs a verified chat-button action and returns the text the
// clicking user sees.
func (s *Server) chatAction(r *http.Request, alertID, action, actor string) string {
	alert, ok := s.EarlyWarning.Alert(alertID)
	if !ok {
		return "이 경보는 더 이상 없습니다."
	}
	audit := func(fields map[string]any) {
		fields["alert"], fields["action"], fields["actor"] = alertID, action, actor
		s.earlyWarningAudit(r, "early_warning_chat_action", alert.ProfileID, fields)
	}
	switch action {
	case earlywarning.ActionAck:
		if alert.State != earlywarning.StateFiring {
			return "이미 해소된 경보입니다: " + alert.Title
		}
		if _, err := s.EarlyWarning.Ack(alertID, actor, "채팅에서 확인"); err != nil {
			return "확인 실패: " + err.Error()
		}
		audit(map[string]any{})
		return "✅ 확인했습니다 — " + alert.Title + " (해소되거나 더 심각해질 때까지 재알림·당직 호출을 멈춥니다)"
	case earlywarning.ActionSilence:
		sil, err := s.EarlyWarning.AddSilence(earlywarning.Silence{ProfileID: alert.ProfileID, Rule: alert.Rule, Reason: "채팅에서 2시간 무음 (" + actor + ")", CreatedBy: actor}, 2*time.Hour)
		if err != nil {
			return "무음 실패: " + err.Error()
		}
		audit(map[string]any{"silence": sil.ID})
		return "🔕 " + alert.ProfileID + " 의 " + alert.Rule + " 알림을 " + sil.EndsAt.In(time.Local).Format("15:04") + "까지 멈췄습니다. 경보는 콘솔에 계속 표시됩니다."
	case earlywarning.ActionFix:
		if alert.State != earlywarning.StateFiring {
			return "이미 해소된 경보입니다: " + alert.Title
		}
		all, err := s.DB.Profiles(r.Context())
		if err != nil {
			return "프로파일 조회 실패: " + err.Error()
		}
		p, found := allowedProfile(all, alert.ProfileID)
		if !found {
			return "프로파일을 찾을 수 없습니다: " + alert.ProfileID
		}
		proposal, err := s.proposeFix(r.Context(), alert, dbconn.ApplyDefaults(p), nil)
		if err != nil {
			return "수정안 생성 실패: " + err.Error()
		}
		if !proposal.Available || proposal.Plan == nil {
			return "자동 수정안이 없습니다: " + proposal.Guidance
		}
		created, reused, err := s.saveFixDraft(alert.ID, *proposal.Plan)
		if err != nil {
			return "수정안 저장 실패: " + err.Error()
		}
		if reused {
			return "🛠 이 경보의 변경계획 " + created.ID + " (" + string(created.State) + ")이 이미 있습니다 — 새로 만들지 않았습니다. 변경 관리에서 이어서 진행하세요."
		}
		audit(map[string]any{"plan": created.ID})
		return "🛠 변경계획 초안 " + created.ID + " (위험도 " + string(created.Risk) + ")을 만들었습니다. 실행하려면 변경 관리에서 제출·승인하세요 — 채팅 버튼으로는 실행되지 않습니다."
	}
	return "알 수 없는 동작입니다."
}
