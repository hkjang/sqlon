package earlywarning

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/metasync"
	"sqlon/internal/observability"
	"sqlon/internal/storage"
)

type staticProfiles []dbconn.Profile

func (s staticProfiles) Profiles(context.Context) ([]dbconn.Profile, error) { return s, nil }

type captureNotifier struct {
	mu    sync.Mutex
	calls [][]Notification
	texts []string
	fail  error
	id    string
}

func (c *captureNotifier) NotifyText(_ context.Context, kind, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, kind+":"+text)
	return c.fail
}

func (c *captureNotifier) ID() string {
	if c.id == "" {
		return "capture"
	}
	return c.id
}

func (c *captureNotifier) Notify(_ context.Context, notes []Notification, _ map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, notes)
	return c.fail
}
func (c *captureNotifier) Target() string { return "capture" }

func (c *captureNotifier) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, call := range c.calls {
		out = append(out, kinds(call)...)
	}
	return out
}

type fakeMaintenance struct {
	findings []observability.MaintenanceFinding
	status   string
	calls    atomic.Int64 // profiles run concurrently
}

func (f *fakeMaintenance) Maintenance(context.Context, dbconn.Profile) observability.Response[observability.MaintenanceData] {
	f.calls.Add(1)
	status := f.status
	if status == "" {
		status = "ok"
	}
	return observability.Response[observability.MaintenanceData]{Status: status, Data: observability.MaintenanceData{Findings: f.findings}}
}

type fakeSchema struct{ snap *metasync.RawSnapshot }

func (f *fakeSchema) Collect(context.Context, metasync.CollectRequest) (*metasync.RawSnapshot, error) {
	if f.snap == nil {
		return nil, errors.New("unreachable")
	}
	cp := *f.snap
	return &cp, nil
}

func okBatch(snaps ...collector.Snapshot) collector.BatchResult {
	b := collector.BatchResult{}
	for _, s := range snaps {
		b.Results = append(b.Results, collector.ProfileResult{Status: "ok", Snapshot: s, CollectedAt: s.CollectedAt})
	}
	return b
}

func newTestEngine(t *testing.T, dir string, p dbconn.Profile, clock *time.Time) (*Engine, *captureNotifier, *fakeMaintenance, *fakeSchema) {
	t.Helper()
	eng := New(Config{Dir: dir, MaintenanceEvery: 5 * time.Minute, SchemaEvery: 15 * time.Minute})
	notifier := &captureNotifier{}
	maint := &fakeMaintenance{}
	schema := &fakeSchema{}
	eng.Profiles, eng.Notifier, eng.Maintenance, eng.Schema = staticProfiles{p}, notifier, maint, schema
	eng.Now = func() time.Time { return *clock }
	eng.Logf = t.Logf
	return eng, notifier, maint, schema
}

// TestEngineWarnsBeforeTheDiskFills drives a volume that fills at a steady
// rate through hourly cycles and checks the whole path: forecast → alert →
// one notification, escalation as the projection shortens, persistence
// across a restart without re-notifying, and resolution after cleanup.
func TestEngineWarnsBeforeTheDiskFills(t *testing.T) {
	dir := t.TempDir()
	p := pgProfile("1TiB")
	clock := t0
	eng, notifier, _, _ := newTestEngine(t, dir, p, &clock)
	eng.cfg.RenotifyInterval = 48 * time.Hour // reminders are covered by the lifecycle tests

	perDay := 50 * gib
	used := func(h int) float64 { return 500*gib + perDay*float64(h)/24 }
	for h := 0; h <= 36; h++ {
		clock = t0.Add(time.Duration(h) * time.Hour)
		eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, used(h), 4*gib, nil)), EvaluateOptions{})
	}
	// ~579GiB used at h=36, 445GiB free at 50GiB/day → ~8.9 days: warning.
	got := notifier.all()
	if len(got) != 1 || got[0] != "firing:capacity_forecast" {
		t.Fatalf("expected exactly one forecast notification, got %v", got)
	}
	board := eng.Board([]dbconn.Profile{p})
	if board.Summary.Warning != 1 || board.Profiles[0].Forecasts[0].DaysToFull == nil {
		t.Fatalf("board must show the warning and the projection: %+v", board.Summary)
	}

	// Restart: state reloads and nothing is re-sent.
	if err := eng.Flush(); err != nil {
		t.Fatal(err)
	}
	eng2, notifier2, _, _ := newTestEngine(t, dir, p, &clock)
	eng2.cfg.RenotifyInterval = 48 * time.Hour
	clock = clock.Add(time.Hour)
	eng2.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, used(37), 4*gib, nil)), EvaluateOptions{})
	if got := notifier2.all(); len(got) != 0 {
		t.Fatalf("a restart must not re-send standing alerts: %v", got)
	}
	if f := eng2.Board([]dbconn.Profile{p}).Profiles[0].Forecasts[0]; f.GrowthLong == nil || !f.GrowthLong.Valid {
		t.Fatalf("series must survive the restart: %+v", f.GrowthLong)
	}

	// Growth jumps to 400GiB/day → under 3 days left: escalation.
	base := used(37)
	for h := 1; h <= 6; h++ {
		clock = clock.Add(time.Hour)
		eng2.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, base+400*gib*float64(h)/24, 4*gib, nil)), EvaluateOptions{})
	}
	if got := notifier2.all(); len(got) == 0 || got[0] != "escalated:capacity_forecast" {
		t.Fatalf("expected escalation to critical, got %v", got)
	}

	// Cleanup: usage drops and growth stops — the alert resolves and says so.
	for h := 1; h <= 30; h++ {
		clock = clock.Add(time.Hour)
		eng2.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 300*gib, 4*gib, nil)), EvaluateOptions{})
	}
	all := strings.Join(notifier2.all(), ",")
	// The 8x jump in growth is also a surge in its own right.
	for _, want := range []string{"firing:capacity_growth_surge", "resolved:capacity_forecast", "resolved:capacity_growth_surge"} {
		if !strings.Contains(all, want) {
			t.Fatalf("expected %s, got %v", want, all)
		}
	}
	if n := len(eng2.Board([]dbconn.Profile{p}).Firing); n != 0 {
		t.Fatalf("after cleanup nothing may still fire, %d left", n)
	}
}

func TestEngineCollectionDownAndRecovery(t *testing.T) {
	p := pgProfile("1TiB")
	clock := t0
	eng, notifier, maint, _ := newTestEngine(t, t.TempDir(), p, &clock)
	maint.findings = []observability.MaintenanceFinding{{Category: "wal_archive", Object: "archive_command", Severity: "critical", Detail: "failing"}}
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 100*gib, gib, nil)), EvaluateOptions{})
	if maint.calls.Load() != 1 {
		t.Fatalf("maintenance must run on the first cycle, ran %d", maint.calls.Load())
	}
	failed := collector.BatchResult{Results: []collector.ProfileResult{{Status: "error", ErrorCode: "COLLECTION_FAILED", Error: "no space left on device", Snapshot: collector.Snapshot{ProfileID: p.ID}}}}
	for i := 1; i <= 3; i++ {
		clock = clock.Add(time.Minute)
		eng.Evaluate(context.Background(), failed, EvaluateOptions{})
	}
	if maint.calls.Load() != 1 {
		t.Fatalf("maintenance must not run against an unreachable DB")
	}
	board := eng.Board([]dbconn.Profile{p})
	rules := map[string]string{}
	for _, a := range board.Firing {
		rules[a.Rule] = a.Severity
	}
	if rules[RuleCollectionDown] != SevCritical || rules["maint_wal_archive"] != SevCritical {
		t.Fatalf("collection_down must fire and the archive alert must survive the blind period: %v", rules)
	}
	clock = clock.Add(time.Minute)
	maint.findings = nil
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 100*gib, gib, nil)), EvaluateOptions{Force: true})
	if n := len(eng.Board([]dbconn.Profile{p}).Firing); n != 0 {
		t.Fatalf("recovery + a clean maintenance run must clear everything, %d left", n)
	}
	got := strings.Join(notifier.all(), ",")
	for _, want := range []string{"firing:maint_wal_archive", "firing:collection_down", "resolved:collection_down", "resolved:maint_wal_archive"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestEngineRetriesFailedDeliveryWithBackoff(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	notifier.fail = errors.New("503")
	snap := func() collector.Snapshot { return footprintSnapshot(p, clock, 95*gib, gib, nil) } // 96%
	eng.Evaluate(context.Background(), okBatch(snap()), EvaluateOptions{})
	clock = clock.Add(30 * time.Second)
	eng.Evaluate(context.Background(), okBatch(snap()), EvaluateOptions{})
	if n := len(notifier.calls); n != 1 {
		t.Fatalf("a failed delivery backs off before retrying, got %d calls", n)
	}
	notifier.fail = nil
	clock = clock.Add(2 * time.Minute)
	eng.Evaluate(context.Background(), okBatch(snap()), EvaluateOptions{})
	if n := len(notifier.calls); n != 2 || notifier.calls[1][0].Kind != KindFiring {
		t.Fatalf("the undelivered alert must be retried as a first notification: %d calls", n)
	}
	board := eng.Board([]dbconn.Profile{p})
	if board.Delivery.Failed != 1 || board.Delivery.ConsecutiveFailures != 0 || board.Delivery.LastSuccessAt == nil {
		t.Fatalf("delivery status must record the failure and the recovery: %+v", board.Delivery)
	}
}

func TestEngineSchemaWatchBaselineThenChange(t *testing.T) {
	dir := t.TempDir()
	p := pgProfile("")
	clock := t0
	eng, notifier, _, schema := newTestEngine(t, dir, p, &clock)
	schema.snap = schemaSnap("h1", t0, metasync.TableAsset{Schema: "public", Name: "orders", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint")}})
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	if got := notifier.all(); len(got) != 0 {
		t.Fatalf("the first schema snapshot is a baseline, not a change: %v", got)
	}
	schema.snap = schemaSnap("h2", t0)
	clock = clock.Add(5 * time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	if got := notifier.all(); len(got) != 0 {
		t.Fatalf("schema is only checked every 15 minutes: %v", got)
	}
	clock = clock.Add(15 * time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	if got := notifier.all(); len(got) != 1 || got[0] != "firing:schema_change" {
		t.Fatalf("a dropped production table must notify once: %v", got)
	}
	// A partial snapshot must never be diffed (it would look like drops).
	partial := schemaSnap("h3", t0)
	partial.Status = "partial"
	schema.snap = partial
	clock = clock.Add(20 * time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	if got := notifier.all(); len(got) != 1 {
		t.Fatalf("partial schema snapshots must be skipped: %v", got)
	}
	if st := eng.Board([]dbconn.Profile{p}).Profiles[0].SchemaStatus; !strings.Contains(st, "부분 수집") {
		t.Fatalf("the skip must be visible: %q", st)
	}
}

type memHistory []storage.Record

func (m *memHistory) Scan(_ context.Context, q storage.Query, fn func(storage.Record) error) error {
	for _, r := range *m {
		if r.ProfileID == q.ProfileID && !r.CollectedAt.Before(q.Since) {
			if err := fn(r); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestEngineSeedsSeriesFromStoredSnapshots(t *testing.T) {
	p := pgProfile("1TiB")
	var history memHistory
	for h := 0; h < 48; h++ {
		at := t0.Add(time.Duration(h) * time.Hour)
		data, _ := json.Marshal(footprintSnapshot(p, at, 100*gib+gib*float64(h), gib, nil))
		history = append(history, storage.Record{Kind: collector.SnapshotKind, ProfileID: p.ID, CollectedAt: at, Data: data})
	}
	clock := t0.Add(48 * time.Hour)
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.History = &history
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 148*gib, gib, nil)), EvaluateOptions{})
	f := eng.Board([]dbconn.Profile{p}).Profiles[0].Forecasts[0]
	if f.GrowthLong == nil || !f.GrowthLong.Valid || f.GrowthLong.BytesPerDay < 23*gib || f.GrowthLong.BytesPerDay > 25*gib {
		t.Fatalf("seeded history must give a 7-day trend of ~24GiB/day on the first cycle: %+v", f.GrowthLong)
	}
}

func TestEngineForgetsRemovedProfiles(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, gib, nil)), EvaluateOptions{})
	eng.Profiles = staticProfiles{}
	clock = clock.Add(time.Minute)
	eng.Evaluate(context.Background(), collector.BatchResult{}, EvaluateOptions{})
	if n := len(eng.Board(nil).Firing); n != 0 {
		t.Fatalf("alerts of a deleted profile must close")
	}
	for _, k := range notifier.all() {
		if strings.HasPrefix(k, "resolved:") {
			t.Fatalf("closing a deleted profile's alerts must be silent: %v", notifier.all())
		}
	}
}

func TestWebhookPayloadIsChatCompatible(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n := &WebhookNotifier{URL: srv.URL + "/hooks/secret-token", ConsoleURL: "https://sqlon.example/admin/alerts"}
	if strings.Contains(n.Target(), "secret-token") {
		t.Fatalf("the webhook secret must not be shown: %s", n.Target())
	}
	err := n.Notify(context.Background(), []Notification{{Kind: KindFiring, Alert: Alert{ProfileID: "prod-pg", Severity: SevCritical, Title: "저장공간 전체 점유량 고갈 예측: 약 2.5일 후 가득 참", Detail: "남은 공간 100 GiB", Recommendation: "볼륨 증설", FirstSeen: t0}}}, map[string]string{"prod-pg": "주문 DB"})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := body["text"].(string)
	for _, want := range []string{"🚨 SQLON 예방 경보", "🔴 **CRITICAL** · 주문 DB (prod-pg)", "> 남은 공간 100 GiB", "> 조치: 볼륨 증설", "https://sqlon.example/admin/alerts"} {
		if !strings.Contains(text, want) {
			t.Fatalf("message missing %q:\n%s", want, text)
		}
	}
	if notes, _ := body["notifications"].([]any); len(notes) != 1 {
		t.Fatalf("structured notifications missing: %v", body)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid webhook", http.StatusBadRequest)
	}))
	defer failing.Close()
	err = (&WebhookNotifier{URL: failing.URL}).Notify(context.Background(), []Notification{{Kind: KindFiring}}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid webhook") {
		t.Fatalf("a non-2xx answer must surface as an error with the body: %v", err)
	}
}

func TestEngineRebuildsSeriesAfterRestartBeforeFirstSave(t *testing.T) {
	p := pgProfile("1TiB")
	var history memHistory
	clock := t0
	dir := t.TempDir()
	eng, _, _, _ := newTestEngine(t, dir, p, &clock)
	eng.History = &history
	for m := 0; m < 5; m++ { // five minutes: no periodic series save yet
		clock = t0.Add(time.Duration(m) * time.Minute)
		snap := footprintSnapshot(p, clock, 100*gib, gib, nil)
		data, _ := json.Marshal(snap)
		history = append(history, storage.Record{Kind: collector.SnapshotKind, ProfileID: p.ID, CollectedAt: clock, Data: data})
		eng.Evaluate(context.Background(), okBatch(snap), EvaluateOptions{})
	}
	eng2, _, _, _ := newTestEngine(t, dir, p, &clock)
	eng2.History = &history
	eng2.ensureSeries(context.Background(), p.ID, clock)
	if pts := len(eng2.series[p.ID].Assets["storage:footprint"].Points); pts == 0 {
		t.Fatalf("a restart before the first save must rebuild the series from stored snapshots")
	}
}
