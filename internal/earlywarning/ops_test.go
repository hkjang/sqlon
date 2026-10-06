package earlywarning

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/metasync"
)

// ---- heartbeat ----

func TestHeartbeatPingsOnlyWhenSQLONCanStillWarn(t *testing.T) {
	var pings int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { atomic.AddInt32(&pings, 1) }))
	defer srv.Close()
	p := pgProfile("100GiB")
	clock := t0
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.HeartbeatURL = srv.URL + "/ping/abc"
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 10*gib, 0, nil)), EvaluateOptions{})
	if atomic.LoadInt32(&pings) != 1 {
		t.Fatalf("a healthy cycle must ping, got %d", pings)
	}
	// the default channel starts failing: keep evaluating, stop vouching
	notifier.fail = errors.New("503")
	clock = clock.Add(time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, 0, nil)), EvaluateOptions{})
	clock = clock.Add(time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, 0, nil)), EvaluateOptions{})
	b := eng.Board([]dbconn.Profile{p})
	if atomic.LoadInt32(&pings) != 1 || !strings.Contains(b.Heartbeat.LastSkipped, "전달 실패") {
		t.Fatalf("no ping while alerts cannot be delivered: pings=%d status=%+v", pings, b.Heartbeat)
	}
	notifier.fail = nil
	clock = clock.Add(5 * time.Minute) // past the retry backoff
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, 0, nil)), EvaluateOptions{})
	if b := eng.Board([]dbconn.Profile{p}); atomic.LoadInt32(&pings) != 2 || b.Heartbeat.LastSkipped != "" || b.Heartbeat.Pings != 2 {
		t.Fatalf("pings resume once delivery recovers: pings=%d %+v", pings, b.Heartbeat)
	}
}

// ---- escalation ----

func TestEscalationPagesUnacknowledgedCriticalAlerts(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	oncall := &captureNotifier{id: "oncall"}
	eng.Escalation = oncall
	eng.cfg.EscalateAfter = 30 * time.Minute
	tick := func(d time.Duration, used float64) {
		clock = clock.Add(d)
		eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, used, 0, nil)), EvaluateOptions{})
	}
	tick(0, 85*gib) // warning only
	tick(time.Hour, 85*gib)
	if len(oncall.calls) != 0 {
		t.Fatalf("warnings never page")
	}
	tick(time.Minute, 95*gib) // becomes critical now
	tick(29*time.Minute, 95*gib)
	if len(oncall.calls) != 0 {
		t.Fatalf("paging waits 30 minutes from the moment the alert became critical")
	}
	tick(2*time.Minute, 95*gib)
	if got := oncall.all(); len(got) != 1 || got[0] != "page:capacity_usage" {
		t.Fatalf("expected one page: %v", got)
	}
	tick(time.Hour, 95*gib)
	if len(oncall.calls) != 1 {
		t.Fatalf("a page is sent once")
	}
	tick(time.Minute, 10*gib) // cleaned up
	if got := oncall.all(); got[len(got)-1] != "page_resolved:capacity_usage" {
		t.Fatalf("on-call must hear the resolution: %v", got)
	}
}

func TestEscalationRespectsAckSilenceAndPerProfileChannel(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	oncall := &captureNotifier{id: "oncall"}
	team := &captureNotifier{id: "team-oncall"}
	eng.Escalation, eng.cfg.EscalateAfter = oncall, 30*time.Minute
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, 0, nil)), EvaluateOptions{})
	id := eng.Board([]dbconn.Profile{p}).Firing[0].ID
	if _, err := eng.Ack(id, "dba", "보고 있음"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Hour)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, 0, nil)), EvaluateOptions{})
	if len(oncall.calls) != 0 {
		t.Fatalf("an acknowledged alert must not page")
	}

	q := dbconn.ApplyDefaults(dbconn.Profile{ID: "q", Type: "postgres", Capacity: &dbconn.CapacityConfig{StorageLimit: "100GiB"}})
	eng.Profiles = staticProfiles{p, q}
	eng.RouteEscalation = func(pr dbconn.Profile) (Notifier, error) {
		if pr.ID == "q" {
			return team, nil
		}
		return nil, nil
	}
	sil, err := eng.AddSilence(Silence{ProfileID: "q", Rule: "capacity_usage", Reason: "볼륨 증설 작업"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eng.cfg.EscalateAfter = time.Nanosecond // page at once
	clock = clock.Add(time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, 0, nil), footprintSnapshot(q, clock, 99*gib, 0, nil)), EvaluateOptions{})
	if len(team.calls) != 0 || len(oncall.calls) != 0 {
		t.Fatalf("a silenced critical alert must not page: team=%v server=%v", team.all(), oncall.all())
	}
	if _, err := eng.EndSilence(sil.ID); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, 0, nil), footprintSnapshot(q, clock, 99*gib, 0, nil)), EvaluateOptions{})
	if got := strings.Join(team.all(), ","); got != "page:capacity_usage" || len(oncall.calls) != 0 {
		t.Fatalf("after the silence q's own on-call is paged, not the server's: team=%v server=%v", team.all(), oncall.all())
	}
}

// ---- chat actions ----

func TestChatActionTokensAndAttachments(t *testing.T) {
	dir := t.TempDir()
	p := pgProfile("100GiB")
	clock := t0
	eng, _, _, _ := newTestEngine(t, dir, p, &clock)
	if err := eng.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	note := Notification{Kind: KindFiring, Alert: Alert{ID: "ew-1", ProfileID: p.ID, Rule: "maint_bloat", Severity: SevWarning, Title: "블로트"}}
	if eng.ChatAttachment(note) != nil {
		t.Fatalf("buttons are off by default")
	}
	eng.ChatActions, eng.ConsoleURL = "mattermost", "https://sqlon.example/admin/alerts"
	if err := eng.LoadSettings(); err != nil { // as at startup with -alert-chat-actions
		t.Fatal(err)
	}
	att := eng.ChatAttachment(note)
	actions, _ := att["actions"].([]map[string]any)
	if len(actions) != 3 {
		t.Fatalf("ack, silence and fix (bloat is fixable): %+v", att)
	}
	integ := actions[0]["integration"].(map[string]any)
	if integ["url"] != "https://sqlon.example"+ChatActionPath {
		t.Fatalf("callback goes to the console origin: %v", integ["url"])
	}
	token := integ["context"].(map[string]any)["token"].(string)
	if id, action, err := eng.VerifyAction(token); err != nil || id != "ew-1" || action != ActionAck {
		t.Fatalf("verify: %s %s %v", id, action, err)
	}
	if _, _, err := eng.VerifyAction(strings.Replace(token, token[2:6], "AAAA", 1)); err == nil {
		t.Fatalf("a tampered token must be rejected")
	}
	other := New(Config{Dir: t.TempDir()}) // another server's key
	other.ChatActions = "mattermost"
	if err := other.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	other.mu.Lock()
	forged := other.signActionLocked("ew-1", ActionFix, clock)
	other.mu.Unlock()
	if _, _, err := eng.VerifyAction(forged); err == nil {
		t.Fatalf("well-formed claims signed with another key must be rejected")
	}
	if nonFix := eng.ChatAttachment(Notification{Kind: KindFiring, Alert: Alert{ID: "ew-2", Rule: RuleCapacityUsage, Severity: SevCritical}}); len(nonFix["actions"].([]map[string]any)) != 2 {
		t.Fatalf("no fix button where no fix exists")
	}
	if eng.ChatAttachment(Notification{Kind: KindResolved, Alert: note.Alert}) != nil {
		t.Fatalf("nothing to act on for a resolution")
	}

	// the key survives a restart: yesterday's buttons still work
	eng2, _, _, _ := newTestEngine(t, dir, p, &clock)
	eng2.ChatActions = "mattermost"
	if err := eng2.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng2.VerifyAction(token); err != nil {
		t.Fatalf("token from before the restart: %v", err)
	}
	clock = clock.Add(8 * 24 * time.Hour)
	if _, _, err := eng2.VerifyAction(token); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("a week-old button must expire: %v", err)
	}
}

func TestWebhookCarriesAttachments(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}))
	defer srv.Close()
	n := &WebhookNotifier{URL: srv.URL, Attach: func(n Notification) map[string]any {
		if n.Kind != KindFiring {
			return nil
		}
		return map[string]any{"actions": []any{map[string]any{"id": "ack1"}}}
	}}
	if err := n.Notify(context.Background(), []Notification{{Kind: KindFiring, Alert: Alert{ID: "a"}}, {Kind: KindResolved, Alert: Alert{ID: "b"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if atts, _ := body["attachments"].([]any); len(atts) != 1 {
		t.Fatalf("one attachment for the firing alert only: %v", body["attachments"])
	}
}

// ---- growth attribution ----

func TestGrowthAttributionNamesWhatGrew(t *testing.T) {
	p := pgProfile("500GiB")
	clock := t0
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	ctx := context.Background()
	for h := 0; h <= 72; h++ {
		clock = t0.Add(time.Duration(h) * time.Hour)
		days := float64(h) / 24
		tables := map[string]float64{"public.events": 100*gib + 10*gib*days, "public.logs": 20*gib + 2*gib*days, "public.static": 50 * gib}
		if h >= 48 {
			tables["public.orders_bak_20261003"] = 12 * gib // CREATE TABLE AS copy on day 2
		}
		data := 300*gib + 12*gib*days
		if h >= 48 {
			data += 12 * gib
		}
		_ = eng.ReportDisk(ctx, p.ID, HostDiskReport{Host: "db01", Volumes: []DiskVolume{{Mount: "/pg", TotalBytes: 1000 * gib, UsedBytes: data + 50*gib + 5*gib*days, AvailBytes: 1000*gib - data - 50*gib - 5*gib*days}}})
		eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, data, gib, tables)), EvaluateOptions{})
	}
	b := eng.Board([]dbconn.Profile{p})
	var fp, vol *Forecast
	for i := range b.Profiles[0].Forecasts {
		f := &b.Profiles[0].Forecasts[i]
		switch f.Scope {
		case collector.ScopeStorage:
			fp = f
		case collector.ScopeVolume:
			vol = f
		}
	}
	if fp == nil || fp.Attribution == nil || fp.Attribution.Contributors[0].Name != "public.events" {
		t.Fatalf("events grew most: %+v", fp)
	}
	names := map[string]float64{}
	for _, c := range fp.Attribution.Contributors {
		names[c.Name] = c.Bytes
	}
	if names["public.orders_bak_20261003"] != 12*gib || names["public.static"] != 0 {
		t.Fatalf("a table created this week counts in full; a static one not at all: %v", names)
	}
	if vol == nil || vol.Attribution == nil || vol.Attribution.Contributors[1].Kind != "outside_db" || vol.Attribution.Contributors[1].Bytes < 14*gib || vol.Attribution.Contributors[1].Bytes > 16*gib {
		t.Fatalf("the volume must split DB growth from outside files (+15GiB over 3 days): %+v", vol.Attribution)
	}
	if !strings.Contains(fp.Attribution.text(), "public.events +30.0 GiB") {
		t.Fatalf("text: %s", fp.Attribution.text())
	}
}

// ---- migration awareness ----

func TestSchemaChangesFromAMigrationToolAreDeploys(t *testing.T) {
	p := pgProfile("")
	base := schemaSnap("h1", t0, metasync.TableAsset{Schema: "public", Name: "orders", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint"), col("memo", "text")}})
	cur := schemaSnap("h2", t0.Add(15*time.Minute), metasync.TableAsset{Schema: "public", Name: "orders", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint")}})
	migs := []metasync.Migration{{Tool: "flyway", Version: "12", Description: "drop memo", AppliedBy: "app", AppliedAt: t0.Add(10 * time.Minute)}}
	c := schemaConditions(p, base, cur, nil, nil, migs)[0]
	if c.Severity != SevWarning || c.Attributes["unplanned"] != 0 || !strings.Contains(c.Detail, "[배포 마이그레이션] 컬럼 삭제") || !strings.Contains(c.Detail, "flyway 12 drop memo by app") || !strings.Contains(c.Title, "배포 마이그레이션 1건") {
		t.Fatalf("a column dropped by a Flyway migration is a deploy, but data is gone — a warning, not an incident: %+v\n%s", c, c.Detail)
	}
	if c := schemaConditions(p, base, cur, nil, nil, nil)[0]; c.Severity != SevCritical {
		t.Fatalf("the same drop by hand stays critical: %+v", c)
	}
	added := schemaSnap("h3", t0.Add(15*time.Minute), metasync.TableAsset{Schema: "public", Name: "orders", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint"), col("memo", "text"), col("grade", "text")}})
	if c := schemaConditions(p, base, added, nil, nil, migs)[0]; c.Severity != SevInfo || strings.Contains(c.Recommendation, "오남용") {
		t.Fatalf("an additive deploy is information only, without the unplanned-DDL advice: %+v", c)
	}
	if !strings.Contains(c.Recommendation, "삭제") {
		t.Fatalf("a destructive deploy says what to check: %s", c.Recommendation)
	}
	if c := schemaConditions(p, base, added, nil, nil, nil)[0]; c.Severity != SevWarning {
		t.Fatalf("the same column added by hand in production is a warning: %+v", c)
	}
}

type migrationQueryer struct{ queries []string }

func (m *migrationQueryer) ProfileDialect(context.Context, string) (string, error) {
	return "postgres", nil
}
func (m *migrationQueryer) SystemQuery(_ context.Context, _ string, q string, args ...any) ([]map[string]any, error) {
	m.queries = append(m.queries, q)
	switch {
	case strings.Contains(q, "flyway_schema_history"):
		return []map[string]any{
			{"version": "12", "description": "add grade", "applied_by": "app", "applied_at": "2026-10-06T01:02:03Z", "ok": true},
			{"version": "13", "description": "failed", "applied_by": "app", "applied_at": "2026-10-06T01:05:00Z", "ok": false},
		}, nil
	case strings.Contains(q, "django_migrations"):
		return nil, errors.New("permission denied")
	}
	return nil, nil
}

func TestRecentMigrationsReadsHistoryTablesFound(t *testing.T) {
	q := &migrationQueryer{}
	svc := metasync.NewService(q, t.TempDir())
	snap := &metasync.RawSnapshot{SourceID: "p", Dialect: "postgres", Tables: []metasync.TableAsset{
		{Schema: "app", Name: "flyway_schema_history"}, {Schema: "public", Name: "django_migrations"}, {Schema: "public", Name: "orders"},
	}}
	migs, err := svc.RecentMigrations(context.Background(), snap, t0)
	if len(migs) != 1 || migs[0].Version != "12" || migs[0].AppliedAt.IsZero() {
		t.Fatalf("one successful flyway migration: %+v", migs)
	}
	if err == nil || !strings.Contains(err.Error(), "django") {
		t.Fatalf("an unreadable history table is reported, not fatal: %v", err)
	}
	if len(q.queries) != 2 || !strings.Contains(q.queries[0], `"app"."flyway_schema_history"`) || !strings.Contains(q.queries[0], "$1") {
		t.Fatalf("quoted identifiers and a bound parameter: %v", q.queries)
	}
}

// ---- settings for the new channels ----

func TestEscalationAndChatSettings(t *testing.T) {
	p := pgProfile("")
	clock := t0
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.Factory = func(url, console string) Notifier { return &WebhookNotifier{URL: url, ConsoleURL: console} }
	eng.Resolve = func(ref string) (string, error) { return strings.TrimPrefix(ref, "plain:"), nil }
	if err := eng.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.UpdateSettings(Settings{ChatActions: "slack"}, nil, "a"); err == nil {
		t.Fatalf("only mattermost buttons are supported")
	}
	if _, err := eng.UpdateSettings(Settings{EscalateAfter: "10s"}, nil, "a"); err == nil {
		t.Fatalf("sub-minute escalation other than 0 must be refused")
	}
	v, err := eng.UpdateSettings(Settings{EscalationRef: "plain:https://oncall.example/hooks/X", EscalateAfter: "0", HeartbeatRef: "plain:https://hc.example/ping/uuid", ChatActions: "Mattermost", ActionURL: "https://sqlon.internal"}, nil, "a")
	if err != nil || v.Escalation != "https://oncall.example/…" || v.EscalateAfter != "0m" || v.Heartbeat != "https://hc.example/…" || v.ChatActions != "mattermost" || v.ActionURL != "https://sqlon.internal" || v.EscalationRef != "plain:****" {
		t.Fatalf("settings: %+v %v", v, err)
	}
	if v, _ = eng.UpdateSettings(Settings{}, []string{"escalation_ref", "escalate_after", "heartbeat_ref", "chat_actions"}, "a"); v.Escalation != "" || v.EscalateAfter != "30m" || v.Heartbeat != "" || v.ChatActions != "off" {
		t.Fatalf("reset: %+v", v)
	}
}

func TestChatActionKeyOnlyWhenButtonsOnAndOffDisarms(t *testing.T) {
	dir := t.TempDir()
	e := New(Config{Dir: dir})
	if err := e.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "action.key")); !os.IsNotExist(err) {
		t.Fatalf("no signing key may be written while chat actions are off: %v", err)
	}
	e.ChatActions = "mattermost"
	if err := e.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "action.key")); err != nil || fi.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Fatalf("the key is created private once buttons are on: %v %v", fi, err)
	}
	e.mu.Lock()
	tok := e.signActionLocked("a1", ActionAck, e.now())
	e.mu.Unlock()
	if id, act, err := e.VerifyAction(tok); err != nil || id != "a1" || act != ActionAck {
		t.Fatalf("valid token: %s %s %v", id, act, err)
	}
	if _, err := e.UpdateSettings(Settings{ChatActions: "off"}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.VerifyAction(tok); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("turning buttons off must disarm posted ones: %v", err)
	}
}

type fakeMigrations struct{ applied []metasync.Migration }

func (f *fakeMigrations) RecentMigrations(_ context.Context, _ *metasync.RawSnapshot, since time.Time) ([]metasync.Migration, error) {
	var out []metasync.Migration
	for _, m := range f.applied {
		if !m.AppliedAt.Before(since) {
			out = append(out, m)
		}
	}
	return out, nil
}

// The sequence found in live testing: a Flyway deploy, then a manual ALTER a
// few minutes later. The deploy is still inside the lookup window, but it
// was already seen by the previous check, so it must not excuse the ALTER.
func TestOnlyNewMigrationsExplainASchemaChange(t *testing.T) {
	p := pgProfile("")
	clock := t0
	eng, _, _, schema := newTestEngine(t, t.TempDir(), p, &clock)
	migs := &fakeMigrations{}
	eng.Migrations = migs
	orders := func(hash string, cols ...string) *metasync.RawSnapshot {
		var cs []metasync.ColumnAsset
		for _, c := range cols {
			cs = append(cs, col(c, "text"))
		}
		return schemaSnap(hash, clock, metasync.TableAsset{Schema: "public", Name: "orders", Kind: "table", Columns: cs})
	}
	check := func(snap *metasync.RawSnapshot) *Alert {
		schema.snap = snap
		eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 10*gib, 0, nil)), EvaluateOptions{Force: true})
		events := eng.Board([]dbconn.Profile{p}).SchemaEvents
		if len(events) == 0 {
			return nil
		}
		return &events[0]
	}
	check(orders("h1", "id")) // baseline

	clock = clock.Add(time.Minute)
	migs.applied = append(migs.applied, metasync.Migration{Tool: "flyway", Version: "2", Description: "add grade", AppliedAt: clock.Add(-10 * time.Second)})
	if a := check(orders("h2", "id", "grade")); a == nil || a.Severity != SevInfo || !strings.Contains(a.Title, "배포 마이그레이션 1건") {
		t.Fatalf("the deploy: %+v", a)
	}
	clock = clock.Add(2 * time.Minute) // V2 is still within the 10-minute slack
	if a := check(orders("h3", "id", "grade", "memo")); a == nil || a.Severity != SevWarning || strings.Contains(a.Title, "배포") || !strings.Contains(a.Detail, "[계획 외]") {
		t.Fatalf("a manual ALTER after a deploy is unplanned: %+v", a)
	}
	clock = clock.Add(2 * time.Minute)
	migs.applied = append(migs.applied, metasync.Migration{Tool: "flyway", Version: "3", Description: "drop grade", AppliedAt: clock.Add(-5 * time.Second)})
	a := check(orders("h4", "id", "memo"))
	if a == nil || !strings.Contains(a.Title, "배포 마이그레이션 1건") || !strings.Contains(a.Detail, "flyway 3 drop grade") || strings.Contains(a.Detail, "flyway 2") || a.Severity != SevWarning {
		t.Fatalf("only V3 explains the drop, which still warns: %+v", a)
	}
}

// Webhook and ping URLs carry their secret in the path; a transport error
// must not put it on the board, in heartbeat status or in logs.
func TestTransportErrorsDoNotLeakSecretURLs(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused from here on
	secret := "/hooks/s3cr3t-hook-id"
	w := &WebhookNotifier{URL: deadURL + secret}
	err := w.NotifyText(context.Background(), "test", "x")
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "/…") {
		t.Fatalf("webhook error must mask the URL: %v", err)
	}
	if err := ping(context.Background(), deadURL+"/ping/s3cr3t-uuid"); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("heartbeat error must mask the URL: %v", err)
	}
	if err := (&WebhookNotifier{URL: "http://bad host/hooks/s3cr3t"}).NotifyText(context.Background(), "test", "x"); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("an invalid URL must not be echoed: %v", err)
	}
}

func TestSettingDurationsReadLikeTheyAreTyped(t *testing.T) {
	for d, want := range map[time.Duration]string{6 * time.Hour: "6h", 2 * time.Minute: "2m", 90 * time.Minute: "1h30m", 45 * time.Second: "45s", time.Hour + 30*time.Second: "1h0m30s"} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
		if back, err := time.ParseDuration(shortDuration(d)); err != nil || back != d {
			t.Errorf("%v does not round-trip: %v %v", d, back, err)
		}
	}
}
