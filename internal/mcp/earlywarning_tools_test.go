package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/earlywarning"
	"sqlon/internal/observability"
)

type fakeMaintenance struct {
	findings []observability.MaintenanceFinding
}

func (f fakeMaintenance) Maintenance(context.Context, dbconn.Profile) observability.Response[observability.MaintenanceData] {
	return observability.Response[observability.MaintenanceData]{Status: "ok", Data: observability.MaintenanceData{Findings: f.findings}}
}

const gib = float64(1 << 30)

func footprintBatch(p dbconn.Profile, at time.Time, used float64) collector.BatchResult {
	snap := collector.Snapshot{ProfileID: p.ID, Engine: "postgres", CollectedAt: at, Capacity: []collector.Capacity{
		{Scope: collector.ScopeCluster, Name: "databases", UsedBytes: used},
		{Scope: collector.ScopeStorage, Name: collector.FootprintName, UsedBytes: used},
	}}
	collector.ApplyDeclaredLimit(&snap, p)
	return collector.BatchResult{Results: []collector.ProfileResult{{Status: "ok", Snapshot: snap, CollectedAt: at}}}
}

// earlyWarningFixture: a production DB at 95% with a failing archiver (the
// cause), a bloated table (fixable) and an unlimited slot retention setting.
func earlyWarningFixture(t *testing.T) (*Server, dbconn.Profile) {
	t.Helper()
	s, _ := newAdminMux(t, "tok")
	p := dbconn.Profile{ID: "orders-prod", Name: "주문 DB", Type: "postgres", Environment: "production",
		ConnectString: "127.0.0.1:1/none", Username: "sqlon_mon", PasswordRef: "env:NOPE",
		Capacity: &dbconn.CapacityConfig{StorageLimit: "100GiB"}}
	if err := dbconn.SaveProfiles(s.opDir(), []dbconn.Profile{p}); err != nil {
		t.Fatal(err)
	}
	s.EarlyWarning.Schema = nil
	s.EarlyWarning.Maintenance = fakeMaintenance{findings: []observability.MaintenanceFinding{
		{Category: "wal_archive", Object: "archive_command", Severity: "critical", Detail: "failing"},
		{Category: "bloat", Object: "public.orders", Severity: "warning", Detail: "dead tuples"},
		{Category: "config_risk", Object: "max_slot_wal_keep_size", Severity: "warning", Detail: "unlimited"},
	}}
	s.EarlyWarning.Evaluate(context.Background(), footprintBatch(dbconn.ApplyDefaults(p), time.Now().UTC(), 95*gib), earlywarning.EvaluateOptions{Force: true})
	return s, dbconn.ApplyDefaults(p)
}

func adminCtx() context.Context {
	return context.WithValue(context.Background(), ctxKeyHTTPAdmin{}, true)
}

func callEW(t *testing.T, s *Server, ctx context.Context, name string, args map[string]any) map[string]any {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	out, err := s.callTool(ctx, params)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	b, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func firingByRule(t *testing.T, s *Server, rule string) string {
	t.Helper()
	for _, a := range s.EarlyWarning.Board(nil).Firing {
		if a.Rule == rule {
			return a.ID
		}
	}
	board := s.EarlyWarning.Board([]dbconn.Profile{{ID: "orders-prod", Type: "postgres"}})
	for _, a := range board.Firing {
		if a.Rule == rule {
			return a.ID
		}
	}
	t.Fatalf("no firing %s", rule)
	return ""
}

func TestEarlyWarningToolsAreRegisteredAndGated(t *testing.T) {
	defs := map[string]map[string]any{}
	for _, d := range (&Server{}).tools() {
		defs[d["name"].(string)] = d
	}
	for name := range earlyWarningToolNames {
		d, ok := defs[name]
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		ann := d["annotations"].(map[string]any)
		readOnly := name == "get_early_warnings" || name == "explain_early_warning" || name == "plan_capacity"
		if ann["readOnlyHint"] != readOnly {
			t.Fatalf("%s readOnlyHint=%v", name, ann["readOnlyHint"])
		}
	}
	s, _ := earlyWarningFixture(t)
	notAdmin := context.WithValue(context.Background(), ctxKeyHTTPAdmin{}, false)
	for name := range earlyWarningToolNames {
		if got := callEW(t, s, notAdmin, name, map[string]any{"alert_id": "x", "profile": "orders-prod", "action": "list"}); got["status"] != "forbidden" {
			t.Fatalf("%s over HTTP without the token must be forbidden, got %v", name, got["status"])
		}
	}
}

func TestEarlyWarningToolsStrategicLoop(t *testing.T) {
	s, p := earlyWarningFixture(t)
	ctx := adminCtx()

	board := callEW(t, s, ctx, "get_early_warnings", nil)
	incidents, _ := board["incidents"].([]any)
	if len(incidents) != 1 || !strings.Contains(incidents[0].(map[string]any)["summary"].(string), "WAL 아카이브") {
		t.Fatalf("the archiver must be named as the probable cause: %v", board["incidents"])
	}
	actions, _ := board["next_actions"].([]any)
	var tools []string
	for _, a := range actions {
		tools = append(tools, a.(map[string]any)["tool"].(string))
	}
	if !strings.Contains(strings.Join(tools, ","), "propose_early_warning_fix") {
		t.Fatalf("a fixable alert must yield a propose action: %v", tools)
	}

	usage := firingByRule(t, s, earlywarning.RuleCapacityUsage)
	ex := callEW(t, s, ctx, "explain_early_warning", map[string]any{"alert_id": usage})
	detail := ex["detail"].(map[string]any)
	if detail["incident"] == nil || detail["forecast"] == nil {
		t.Fatalf("explain must carry the incident and forecast: %v", detail)
	}

	plan := callEW(t, s, ctx, "plan_capacity", map[string]any{"profile": p.ID, "target_days": 30})
	if plan["status"] != "ok" || plan["plan"].(map[string]any)["profile_id"] != p.ID {
		t.Fatalf("plan_capacity: %v", plan)
	}

	// fix proposals: a draft plan for the bloat, guidance for the archiver
	bloat := firingByRule(t, s, "maint_bloat")
	fix := callEW(t, s, ctx, "propose_early_warning_fix", map[string]any{"alert_id": bloat})
	prop := fix["proposal"].(map[string]any)
	pl := prop["plan"].(map[string]any)
	if prop["available"] != true || pl["state"] != string(change.Draft) || !strings.HasPrefix(pl["steps"].([]any)[0].(map[string]any)["command"].(string), "VACUUM (ANALYZE) public.orders") {
		t.Fatalf("bloat fix must be a draft VACUUM plan: %v", prop)
	}
	if _, ok := s.Changes.Get(pl["id"].(string)); !ok {
		t.Fatalf("the draft must be stored in the change workflow")
	}
	slotCfg := firingByRule(t, s, "maint_config_risk")
	fix = callEW(t, s, ctx, "propose_early_warning_fix", map[string]any{"alert_id": slotCfg})
	steps := fix["proposal"].(map[string]any)["plan"].(map[string]any)["steps"].([]any)
	if len(steps) != 2 || steps[0].(map[string]any)["command"] != "ALTER SYSTEM SET max_slot_wal_keep_size = '20480MB'" {
		t.Fatalf("slot budget defaults to 20%% of the 100GiB limit: %v", steps)
	}
	archive := firingByRule(t, s, "maint_wal_archive")
	fix = callEW(t, s, ctx, "propose_early_warning_fix", map[string]any{"alert_id": archive})
	if fix["proposal"].(map[string]any)["available"] != false || !strings.Contains(fix["proposal"].(map[string]any)["guidance"].(string), "archive_command") {
		t.Fatalf("no SQL fix for a broken archive target, only guidance: %v", fix)
	}

	// act: silence, ack, disk report, settings, profile config
	sil := callEW(t, s, ctx, "manage_early_warning_silences", map[string]any{"action": "create", "profile": p.ID, "rule": "capacity_*", "duration": "2h", "reason": "증설 작업"})
	silID := sil["silence"].(map[string]any)["id"].(string)
	if list := callEW(t, s, ctx, "manage_early_warning_silences", map[string]any{"action": "list"}); len(list["silences"].([]any)) != 1 {
		t.Fatalf("list: %v", list)
	}
	if end := callEW(t, s, ctx, "manage_early_warning_silences", map[string]any{"action": "end", "silence_id": silID}); end["status"] != "ok" {
		t.Fatalf("end: %v", end)
	}
	if ack := callEW(t, s, ctx, "acknowledge_early_warning", map[string]any{"alert_id": archive, "note": "백업 스토리지 복구 중"}); ack["status"] != "ok" {
		t.Fatalf("ack: %v", ack)
	}
	disk := callEW(t, s, ctx, "report_host_disk", map[string]any{"profile": p.ID, "host": "db01", "volumes": []map[string]any{{"mount": "/pg", "total_bytes": 1000, "used_bytes": 500, "avail_bytes": 500}}})
	if disk["status"] != "ok" {
		t.Fatalf("report_host_disk: %v", disk)
	}
	set := callEW(t, s, ctx, "configure_early_warning", map[string]any{"action": "set", "settings": map[string]any{"webhook_ref": "plain:https://mm.example/hooks/SECRET", "min_notify_severity": "critical"}})
	if b, _ := json.Marshal(set); set["status"] != "ok" || strings.Contains(string(b), "SECRET") {
		t.Fatalf("configure set must succeed without echoing the secret: %s", b)
	}
	if got := callEW(t, s, ctx, "configure_early_warning", map[string]any{"action": "reset", "fields": []string{"all"}}); got["settings"].(map[string]any)["min_notify_severity"] != "warning" {
		t.Fatalf("reset: %v", got)
	}
	cfg := callEW(t, s, ctx, "configure_profile_alerting", map[string]any{"profile": p.ID, "capacity": map[string]any{"storage_limit": "2TiB", "critical_days": 5}, "alerting": map[string]any{"webhook_ref": "plain:https://team.example/hooks/T", "min_severity": "warning"}})
	if cfg["status"] != "ok" {
		t.Fatalf("configure_profile_alerting: %v", cfg)
	}
	stored, _ := dbconn.GetProfile(s.opDir(), p.ID)
	if stored.ConnectString != "127.0.0.1:1/none" || stored.Capacity.StorageLimit != "2TiB" || stored.Alerting.WebhookRef != "plain:https://team.example/hooks/T" {
		t.Fatalf("only capacity/alerting may change: %+v", stored)
	}
	if got := callEW(t, s, ctx, "configure_profile_alerting", map[string]any{"profile": p.ID, "capacity": map[string]any{"storage_limit": "lots"}}); got["status"] != "invalid" {
		t.Fatalf("an invalid limit must be refused: %v", got)
	}
	callEW(t, s, ctx, "configure_profile_alerting", map[string]any{"profile": p.ID, "clear": []string{"alerting"}})
	if stored, _ = dbconn.GetProfile(s.opDir(), p.ID); stored.Alerting != nil {
		t.Fatalf("clear alerting")
	}
	if got := callEW(t, s, ctx, "test_alert_channel", nil); got["status"] != "not_configured" {
		t.Fatalf("no channel: %v", got)
	}
	if got := callEW(t, s, ctx, "test_alert_channel", map[string]any{"channel": "escalation"}); got["status"] != "not_configured" || !strings.Contains(fmt.Sprint(got), "escalation_ref") {
		t.Fatalf("no on-call channel: %v", got)
	}
	var paged atomic.Value
	oncall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		paged.Store(string(b))
	}))
	defer oncall.Close()
	if got := callEW(t, s, ctx, "configure_early_warning", map[string]any{"action": "set", "settings": map[string]any{"escalation_ref": "plain:" + oncall.URL, "escalate_after": "15m"}}); got["status"] != "ok" {
		t.Fatalf("set escalation: %v", got)
	}
	if got := callEW(t, s, ctx, "test_alert_channel", map[string]any{"channel": "escalation"}); got["delivered"] != true {
		t.Fatalf("escalation test: %v", got)
	}
	if body, _ := paged.Load().(string); !strings.Contains(body, "당직 호출") {
		t.Fatalf("the on-call channel got the test page: %q", body)
	}
}

func TestEarlyWarningToolsRespectProfilePermissionsInMetaMode(t *testing.T) {
	s, mux, _, alice := newAuthServer(t)
	hdr := withCookie(alice)
	hdr["Content-Type"] = "application/json"
	if rec := doReq(t, mux, "POST", "/api/db-profiles", `{"id":"alice-db","type":"postgres","connect_string":"h:1/x","username":"u","password_ref":"env:X"}`, hdr); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	u, _ := s.Meta.Store.GetUserByUsername(context.Background(), "alice")
	ctx := withUser(context.Background(), u)
	if got := callEW(t, s, ctx, "manage_early_warning_silences", map[string]any{"action": "create", "duration": "1h", "reason": "x"}); got["status"] != "forbidden" {
		t.Fatalf("a non-admin may not silence every database: %v", got)
	}
	if got := callEW(t, s, ctx, "manage_early_warning_silences", map[string]any{"action": "create", "profile": "alice-db", "duration": "1h", "reason": "점검"}); got["status"] != "ok" {
		t.Fatalf("a user may silence their own database: %v", got)
	}
	if got := callEW(t, s, ctx, "plan_capacity", map[string]any{"profile": "someone-elses"}); got["status"] != "not_found" {
		t.Fatalf("profiles outside the user's grants are invisible: %v", got)
	}
	for _, name := range []string{"configure_early_warning", "configure_profile_alerting", "run_early_warning_check", "test_alert_channel", "propose_early_warning_fix"} {
		if got := callEW(t, s, ctx, name, map[string]any{"action": "get", "profile": "alice-db", "alert_id": "x"}); got["status"] != "forbidden" {
			t.Fatalf("%s must require admin/dba for a regular user: %v", name, got)
		}
	}
}

func TestEarlyWarningTriagePromptAndREST(t *testing.T) {
	s, p := earlyWarningFixture(t)
	found := false
	for _, pr := range s.prompts() {
		found = found || pr["name"] == "early_warning_triage"
	}
	res, err := s.getPrompt(json.RawMessage(`{"name":"early_warning_triage"}`))
	if !found || err != nil || !strings.Contains(res["messages"].([]map[string]any)[0]["content"].(map[string]any)["text"].(string), "propose_early_warning_fix") {
		t.Fatalf("prompt: found=%v err=%v", found, err)
	}

	mux := newMuxFor(s)
	auth := map[string]string{"X-Admin-Token": "tok", "Content-Type": "application/json"}
	if rec := doReq(t, mux, "PUT", "/api/early-warning/settings", `{"settings":{"digest_at":"08:30"}}`, auth); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"digest_at":"08:30"`) {
		t.Fatalf("PUT settings: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "GET", "/api/early-warning/settings", "", nil); rec.Code != 401 {
		t.Fatalf("settings need the token: %d", rec.Code)
	}
	usage := firingByRule(t, s, earlywarning.RuleCapacityUsage)
	if rec := doReq(t, mux, "GET", "/api/early-warning/alerts/"+usage, "", auth); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"incident"`) {
		t.Fatalf("alert detail: %d", rec.Code)
	}
	if rec := doReq(t, mux, "GET", "/api/early-warning/alerts/ew-none", "", auth); rec.Code != 404 {
		t.Fatalf("unknown alert: %d", rec.Code)
	}
	if rec := doReq(t, mux, "GET", "/api/early-warning/capacity-plan?profile="+p.ID+"&days=30", "", auth); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"target_days":30`) {
		t.Fatalf("capacity plan: %d %s", rec.Code, rec.Body.String())
	}
	bloat := firingByRule(t, s, "maint_bloat")
	if rec := doReq(t, mux, "POST", "/api/early-warning/alerts/"+bloat+"/fix", "", auth); rec.Code != 200 || !strings.Contains(rec.Body.String(), "VACUUM") {
		t.Fatalf("fix: %d %s", rec.Code, rec.Body.String())
	}
}

func newMuxFor(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func TestEarlyWarningAlertsOfOtherUsersAreInvisible(t *testing.T) {
	s, mux, admin, alice := newAuthServer(t)
	hdr := withCookie(admin)
	hdr["Content-Type"] = "application/json"
	if rec := doReq(t, mux, "POST", "/api/db-profiles", `{"id":"admin-db","type":"postgres","environment":"production","connect_string":"h:1/x","username":"u","password_ref":"env:X"}`, hdr); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	failed := collector.BatchResult{Results: []collector.ProfileResult{{Status: "error", ErrorCode: "X", Snapshot: collector.Snapshot{ProfileID: "admin-db"}}}}
	for i := 0; i < 3; i++ {
		s.EarlyWarning.Evaluate(context.Background(), failed, earlywarning.EvaluateOptions{})
	}
	adminUser, _ := s.Meta.Store.GetUserByUsername(context.Background(), "admin")
	board := s.EarlyWarning.Board([]dbconn.Profile{{ID: "admin-db"}})
	if len(board.Firing) != 1 {
		t.Fatalf("fixture: %+v", board.Firing)
	}
	id := board.Firing[0].ID
	u, _ := s.Meta.Store.GetUserByUsername(context.Background(), "alice")
	ctx := withUser(context.Background(), u)
	for _, name := range []string{"explain_early_warning", "acknowledge_early_warning"} {
		if got := callEW(t, s, ctx, name, map[string]any{"alert_id": id}); got["status"] != "not_found" {
			t.Fatalf("%s on another user's alert must look like it does not exist: %v", name, got)
		}
	}
	if got := callEW(t, s, withUser(context.Background(), adminUser), "explain_early_warning", map[string]any{"alert_id": id}); got["status"] != "ok" {
		t.Fatalf("the owner sees it: %v", got)
	}
	_ = alice
}

// Agents iterate these lists; an empty result must be [] rather than null.
func TestEarlyWarningToolListsAreNeverNull(t *testing.T) {
	s, _ := newAdminMux(t, "tok")
	s.EarlyWarning.Notifier = &earlywarning.WebhookNotifier{URL: "http://127.0.0.1:1/hook"} // nothing left to suggest
	_ = dbconn.SaveProfiles(s.opDir(), nil)
	ctx := adminCtx()
	for name, keys := range map[string][]string{
		"get_early_warnings":      {"firing", "incidents", "next_actions", "silences", "schema_events"},
		"run_early_warning_check": {"opened", "resolved_ids", "next_actions"},
	} {
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": map[string]any{}})
		out, err := s.callTool(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(out)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		for _, k := range keys {
			if string(m[k]) == "null" || m[k] == nil {
				t.Fatalf("%s.%s must be an array, got %s", name, k, m[k])
			}
		}
	}
}

// Mattermost calls the action URL with {user_id, user_name, context}; it
// carries no SQLON credentials, only the signed token from the button.
func TestChatActionCallbackActsOnlyWithAValidToken(t *testing.T) {
	s, _ := earlyWarningFixture(t)
	mux := newMuxFor(s)
	s.EarlyWarning.ChatActions, s.EarlyWarning.ConsoleURL = "mattermost", "https://sqlon.example/admin/alerts"
	_ = s.EarlyWarning.LoadSettings()
	tokenFor := func(rule, action string) (string, string) {
		board := s.EarlyWarning.Board([]dbconn.Profile{{ID: "orders-prod"}})
		for _, a := range board.Firing {
			if a.Rule == rule {
				att := s.EarlyWarning.ChatAttachment(earlywarning.Notification{Kind: earlywarning.KindFiring, Alert: a})
				for _, b := range att["actions"].([]map[string]any) {
					if strings.HasPrefix(b["id"].(string), action) {
						return a.ID, b["integration"].(map[string]any)["context"].(map[string]any)["token"].(string)
					}
				}
			}
		}
		t.Fatalf("no %s button for %s", action, rule)
		return "", ""
	}
	post := func(token string) (int, string) {
		body, _ := json.Marshal(map[string]any{"user_id": "u1", "user_name": "kim.dba", "context": map[string]any{"token": token}})
		rec := doReq(t, mux, "POST", earlywarning.ChatActionPath, string(body), map[string]string{"Content-Type": "application/json"})
		var out map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out["ephemeral_text"]
	}
	if code, text := post("forged.token"); code != 403 || !strings.Contains(text, "invalid") {
		t.Fatalf("a forged token must be refused: %d %s", code, text)
	}
	usageID, ack := tokenFor(earlywarning.RuleCapacityUsage, "ack")
	if code, text := post(ack); code != 200 || !strings.Contains(text, "확인했습니다") {
		t.Fatalf("ack: %d %s", code, text)
	}
	if a, _ := s.EarlyWarning.Alert(usageID); a.AckedBy != "mattermost:kim.dba" {
		t.Fatalf("the ack must record the chat user: %+v", a)
	}
	_, silence := tokenFor("maint_wal_archive", "silence")
	if code, text := post(silence); code != 200 || !strings.Contains(text, "멈췄습니다") || len(s.EarlyWarning.Board([]dbconn.Profile{{ID: "orders-prod"}}).Silences) != 1 {
		t.Fatalf("silence: %d %s", code, text)
	}
	_, fix := tokenFor("maint_bloat", "fix")
	code, text := post(fix)
	if code != 200 || !strings.Contains(text, "변경계획 초안") || !strings.Contains(text, "실행되지 않습니다") {
		t.Fatalf("fix: %d %s", code, text)
	}
	if code, again := post(fix); code != 200 || again == "" { // a double click
		t.Fatalf("second click: %d %s", code, again)
	}
	drafts := 0
	for _, p := range s.Changes.List() {
		if p.State == change.Draft && strings.Contains(p.Reason, "블로트") {
			drafts++
		}
	}
	if drafts != 1 {
		t.Fatalf("the fix button creates exactly one draft, even when clicked twice, and never executes: %d", drafts)
	}
}

// docker stop, systemd and Kubernetes send SIGTERM: the server must stop
// and write the early-warning series it keeps in memory between saves.
func TestServeServerContextFlushesOnShutdown(t *testing.T) {
	s, p := earlyWarningFixture(t)
	series := filepath.Join(s.opDir(), "operations", "earlywarning", "series", "orders-prod.json")
	_ = os.Remove(series)
	// a second cycle inside the ten-minute save throttle: held in memory only
	s.EarlyWarning.Evaluate(context.Background(), footprintBatch(p, time.Now().UTC().Add(time.Minute), 96*gib), earlywarning.EvaluateOptions{})
	if _, err := os.Stat(series); !os.IsNotExist(err) {
		t.Fatalf("precondition: the periodic save is throttled: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeServerContext(ctx, "127.0.0.1:0", s) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("server did not stop")
	}
	if _, err := os.Stat(series); err != nil {
		t.Fatalf("series must be flushed on shutdown: %v", err)
	}
}
