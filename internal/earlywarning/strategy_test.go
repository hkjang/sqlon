package earlywarning

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

func TestRuntimeSettingsOverrideDefaultsAndPersist(t *testing.T) {
	dir := t.TempDir()
	p := pgProfile("100GiB")
	clock := t0
	eng, _, _, _ := newTestEngine(t, dir, p, &clock)
	built := map[string]string{}
	eng.Factory = func(url, console string) Notifier {
		built[url] = console
		return &WebhookNotifier{URL: url, ConsoleURL: console}
	}
	eng.Resolve = func(ref string) (string, error) {
		if !strings.HasPrefix(ref, "plain:") {
			return "", errors.New("environment variable not set")
		}
		return strings.TrimPrefix(ref, "plain:"), nil
	}
	if err := eng.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if v := eng.Settings(); v.Source["min_notify_severity"] != "default" || v.MinNotifySeverity != SevWarning || v.Webhook != "capture" {
		t.Fatalf("defaults: %+v", v)
	}
	if _, err := eng.UpdateSettings(Settings{RenotifyInterval: "30s"}, nil, "admin"); err == nil {
		t.Fatalf("a renotify interval under 1m must be refused")
	}
	if _, err := eng.UpdateSettings(Settings{WebhookRef: "env:NOPE"}, nil, "admin"); err == nil {
		t.Fatalf("an unresolvable webhook must be refused before it replaces a working one")
	}
	v, err := eng.UpdateSettings(Settings{WebhookRef: "plain:https://mm.example/hooks/SECRET", MinNotifySeverity: "critical", DigestAt: "7:30", SchemaEvery: "off", ConsoleURL: "https://sqlon.example/admin/alerts"}, nil, "kim")
	if err != nil {
		t.Fatal(err)
	}
	if v.WebhookRef != "plain:****" || strings.Contains(v.Webhook, "SECRET") || v.MinNotifySeverity != SevCritical || v.DigestAt != "07:30" || v.SchemaEvery != "off" || v.Source["min_notify_severity"] != "runtime" || v.UpdatedBy != "kim" {
		t.Fatalf("settings view: %+v", v)
	}
	if built["https://mm.example/hooks/SECRET"] != "https://sqlon.example/admin/alerts" {
		t.Fatalf("the default channel must be rebuilt with the new webhook and console link: %v", built)
	}
	if cfg := eng.Config(); cfg.MinNotifySeverity != SevCritical || cfg.SchemaEvery >= 0 {
		t.Fatalf("the engine must run with the new values: %+v", cfg)
	}
	// echoing the masked value back keeps the stored webhook
	if v, err = eng.UpdateSettings(Settings{WebhookRef: "plain:****", RenotifyInterval: "2h"}, nil, "kim"); err != nil || v.Webhook != "https://mm.example/…" || v.RenotifyInterval != "2h" {
		t.Fatalf("masked echo: %+v %v", v, err)
	}

	// restart: overrides come back from settings.json
	eng2, _, _, _ := newTestEngine(t, dir, p, &clock)
	eng2.Factory, eng2.Resolve = eng.Factory, eng.Resolve
	if err := eng2.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if v := eng2.Settings(); v.MinNotifySeverity != SevCritical || v.Webhook != "https://mm.example/…" {
		t.Fatalf("runtime settings must survive a restart: %+v", v)
	}
	v, err = eng2.UpdateSettings(Settings{}, []string{"min_notify_severity", "webhook_ref"}, "kim")
	if err != nil || v.MinNotifySeverity != SevWarning || v.Webhook != "capture" || v.Source["webhook_ref"] != "default" {
		t.Fatalf("reset must restore the defaults: %+v %v", v, err)
	}
	if _, err := eng2.UpdateSettings(Settings{}, []string{"nonsense"}, "kim"); err == nil {
		t.Fatalf("unknown reset field must be refused")
	}
}

func firingAlert(id, profile, rule, sev string) Alert {
	return Alert{ID: id, Key: profile + "|" + rule + id, ProfileID: profile, Rule: rule, Severity: sev, State: StateFiring, Title: rule}
}

func TestCorrelationFindsTheRootCause(t *testing.T) {
	forecast := firingAlert("a1", "p", RuleCapacityForecast, SevCritical)
	forecast.Value = 2.5 // days left
	firing := []Alert{
		forecast,
		firingAlert("a2", "p", "maint_wal_archive", SevCritical),
		firingAlert("a3", "p", "maint_wal_retention", SevWarning),
		firingAlert("a4", "p", RuleSchemaChange, SevWarning),         // unrelated
		firingAlert("b1", "q", RuleCapacityUsage, SevCritical),       // other DB
		firingAlert("b2", "p", RuleCollectionDown, SevCritical),      // a forecast does not stop a DB
		firingAlert("a5", "p", "maint_replication_slot", SevWarning), // second root
	}
	incs := correlate(firing)
	if len(incs) != 1 {
		t.Fatalf("one incident expected: %+v", incs)
	}
	inc := incs[0]
	if strings.Join(inc.RootIDs, ",") != "a2,a5" || len(inc.AlertIDs) != 4 || inc.Severity != SevCritical {
		t.Fatalf("roots must be the archiver and the slot, symptoms the WAL and forecast: %+v", inc)
	}
	if inc.Summary != "원인 추정: WAL 아카이브 실패·적체 → 복제 슬롯의 WAL 보존 → pg_wal 과다 → 고갈 예측" {
		t.Fatalf("summary: %s", inc.Summary)
	}
	if c := causeOf(incs); c["a1"] == "" || c["a2"] != "" {
		t.Fatalf("symptoms carry the cause, roots do not: %v", c)
	}
	// a critical full volume does explain the database going down
	full := []Alert{firingAlert("v", "p", RuleCapacityUsage, SevCritical), firingAlert("d", "p", RuleCollectionDown, SevCritical)}
	if incs := correlate(full); len(incs) != 0 {
		t.Fatalf("90%% usage does not stop a database: %+v", incs)
	}
	full[0].Value = 99.6
	if incs := correlate(full); len(incs) != 1 || incs[0].RootIDs[0] != "v" {
		t.Fatalf("a full volume must be the cause of collection_down: %+v", incs)
	}
}

func TestNotificationsNameTheProbableCause(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, notifier, maint, _ := newTestEngine(t, t.TempDir(), p, &clock)
	maint.findings = []observability.MaintenanceFinding{{Category: "wal_archive", Object: "archive_command", Severity: "critical", Detail: "failing"}}
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 92*gib, 3*gib, nil)), EvaluateOptions{})
	var usage *Notification
	for _, call := range notifier.calls {
		for i := range call {
			if call[i].Alert.Rule == RuleCapacityUsage {
				usage = &call[i]
			}
		}
	}
	if usage == nil || !strings.Contains(usage.Cause, "WAL 아카이브 실패") {
		t.Fatalf("the usage alert must say the archiver is the probable cause: %+v", usage)
	}
	if text := FormatText([]Notification{*usage}, nil, ""); !strings.Contains(text, "🔗 원인 추정: WAL 아카이브 실패·적체 → 사용률 임계 초과") {
		t.Fatalf("the message must show the cause:\n%s", text)
	}
	if b := eng.Board([]dbconn.Profile{p}); len(b.Incidents) != 1 || !strings.Contains(b.Headline, "원인 추정") {
		t.Fatalf("board must surface the incident: %+v / %s", b.Incidents, b.Headline)
	}
}

func TestFlappingAlertsStopNotifyingUntilTheySettle(t *testing.T) {
	var b book
	ran := map[string]bool{CheckCapacity: true}
	now := t0
	sent := 0
	for i := 0; i < 4; i++ { // usage hovering around 80%: fire, clear, fire, clear…
		b.reconcile("p", ran, []Condition{cond("usage", CheckCapacity, SevWarning)}, now)
		notes := b.pending(now, fixedPolicy(SevWarning), 6*time.Hour)
		sent += len(notes)
		b.markDelivered(notes, now)
		now = now.Add(5 * time.Minute)
		b.reconcile("p", ran, nil, now)
		notes = b.pending(now, fixedPolicy(SevWarning), 6*time.Hour)
		sent += len(notes)
		b.markDelivered(notes, now)
		now = now.Add(5 * time.Minute)
	}
	// fire+resolve, fire+resolve, then the third fire makes it flapping:
	// its fire and resolve, and the fourth cycle, are held back.
	if sent != 4 {
		t.Fatalf("a flapping alert must stop notifying, sent %d", sent)
	}
	b.reconcile("p", ran, []Condition{cond("usage", CheckCapacity, SevWarning)}, now)
	b.markDelivered(b.pending(now, fixedPolicy(SevWarning), 6*time.Hour), now)
	b.reconcile("p", ran, []Condition{cond("usage", CheckCapacity, SevCritical)}, now)
	notes := b.pending(now, fixedPolicy(SevWarning), 6*time.Hour)
	if len(notes) != 0 {
		// it was never notified in this flapping run, so it has nothing to escalate from
		t.Logf("held: %v", kinds(notes))
	}
	later := now.Add(2 * time.Hour) // settled: still firing, no longer flapping
	b.reconcile("p", ran, []Condition{cond("usage", CheckCapacity, SevCritical)}, later)
	if got := kinds(b.pending(later, fixedPolicy(SevWarning), 6*time.Hour)); len(got) != 1 || got[0] != "firing:usage" {
		t.Fatalf("once it settles the standing alert must be sent: %v", got)
	}
}

func TestCapacityPlanSizesTheVolume(t *testing.T) {
	p := pgProfile("500GiB")
	clock := t0
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	for h := 0; h <= 48; h++ { // +10GiB/day from 300GiB
		clock = t0.Add(time.Duration(h) * time.Hour)
		eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 300*gib+10*gib*float64(h)/24, 0, nil)), EvaluateOptions{})
	}
	plan := eng.PlanCapacity(p, 90)
	if len(plan.Assets) != 1 {
		t.Fatalf("one asset: %+v", plan.Assets)
	}
	a := plan.Assets[0]
	// used 320GiB, +10GiB/day × 90 = 1220GiB needed; at 80% warn → ≥1525GiB
	if a.GrowthBasis != "7d" || a.NeededBytes < 1219*gib || a.NeededBytes > 1221*gib || a.RecommendedBytes != 1525*gib || a.Verdict != "resize" {
		t.Fatalf("plan numbers: %+v", a)
	}
	if a.ShortfallBytes != 1025*gib || a.FullAt == nil || a.WarnCrossAt == nil || len(a.Scenarios) != 3 || *a.Scenarios[1].DaysToFull != 9 {
		t.Fatalf("shortfall/dates/scenarios: %+v", a)
	}
	if !strings.Contains(plan.Summary, "증설이 필요한 자산") {
		t.Fatalf("summary: %s", plan.Summary)
	}
	if ok := eng.PlanCapacity(p, 3); ok.Assets[0].Verdict != "ok" {
		t.Fatalf("3 days fit: %+v", ok.Assets[0])
	}
}

func TestExplainShowsHistorySeriesAndIncident(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, _, maint, _ := newTestEngine(t, t.TempDir(), p, &clock)
	for i := 0; i < 2; i++ { // the usage alert fires, clears, fires again
		eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, gib, nil)), EvaluateOptions{Force: true})
		clock = clock.Add(time.Hour)
		eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 10*gib, gib, nil)), EvaluateOptions{Force: true})
		clock = clock.Add(time.Hour)
	}
	maint.findings = []observability.MaintenanceFinding{{Category: "replication_slot", Object: "dead", Severity: "critical", Detail: "비활성"}}
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, gib, nil)), EvaluateOptions{Force: true})
	var usageID string
	for _, a := range eng.Board([]dbconn.Profile{p}).Firing {
		if a.Rule == RuleCapacityUsage {
			usageID = a.ID
		}
	}
	d, ok := eng.Explain(usageID)
	if !ok || len(d.Past) != 2 || d.Forecast == nil || len(d.Series) == 0 || d.Incident == nil || len(d.Related) == 0 {
		t.Fatalf("explain: past=%d forecast=%v series=%d incident=%v related=%d", len(d.Past), d.Forecast != nil, len(d.Series), d.Incident, len(d.Related))
	}
	if _, ok := eng.Explain("ew-missing"); ok {
		t.Fatalf("unknown id")
	}
}

func TestConfigRiskCausesOnlyThroughItsSetting(t *testing.T) {
	slotCfg := firingAlert("c1", "p", "maint_config_risk", SevInfo)
	slotCfg.Object = "max_slot_wal_keep_size"
	autovac := firingAlert("c2", "p", "maint_config_risk", SevWarning)
	autovac.Object = "autovacuum"
	bloat := firingAlert("b", "p", "maint_bloat", SevWarning)
	if incs := correlate([]Alert{slotCfg, bloat}); len(incs) != 0 {
		t.Fatalf("an unlimited slot setting does not bloat tables: %+v", incs)
	}
	if incs := correlate([]Alert{autovac, bloat}); len(incs) != 1 || incs[0].RootIDs[0] != "c2" {
		t.Fatalf("autovacuum=off is the cause of bloat: %+v", incs)
	}
}

func TestCapacityPlanFlagsAnAssetAlreadyPastWarning(t *testing.T) {
	p := pgProfile("4GiB")
	clock := t0
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 2.6*gib, 1.5*gib, nil)), EvaluateOptions{}) // 102%, no trend yet
	plan := eng.PlanCapacity(p, 90)
	a := plan.Assets[0]
	if a.Verdict != "resize" || a.ShortfallBytes <= 0 || !strings.Contains(a.Advice, "이미 경고 임계") || !strings.Contains(plan.Summary, "증설이 필요한 자산") {
		t.Fatalf("an asset over its limit needs action even without a trend: %+v / %s", a, plan.Summary)
	}
	fresh := pgProfile("100GiB")
	eng2, _, _, _ := newTestEngine(t, t.TempDir(), fresh, &clock)
	eng2.Evaluate(context.Background(), okBatch(footprintSnapshot(fresh, clock, 10*gib, gib, nil)), EvaluateOptions{})
	if plan := eng2.PlanCapacity(fresh, 90); plan.Assets[0].Verdict != "unknown" || !strings.Contains(plan.Summary, "아직 추세를 계산할 수 없는") {
		t.Fatalf("no trend and plenty of room is unknown, not ok: %+v", plan)
	}
}
