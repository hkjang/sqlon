package earlywarning

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

func diskReport(host string, usedGiB, totalGiB float64) HostDiskReport {
	return HostDiskReport{Host: host, Volumes: []DiskVolume{{Mount: "/var/lib/postgresql", TotalBytes: totalGiB * gib, UsedBytes: usedGiB * gib, AvailBytes: (totalGiB - usedGiB) * gib}}}
}

func firingRules(e *Engine, p dbconn.Profile) map[string]Alert {
	out := map[string]Alert{}
	for _, a := range e.Board([]dbconn.Profile{p}).Firing {
		out[a.Rule+"|"+a.Object] = a
	}
	return out
}

// The disk fills with files PostgreSQL never sees (a dump in the data
// volume): only the host report can tell, and it must keep working while
// the database itself is unreachable.
func TestHostDiskReportSeesWhatSQLCannot(t *testing.T) {
	p := pgProfile("") // no declared limit at all
	clock := t0
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	ctx := context.Background()
	for h := 0; h <= 30; h++ {
		clock = t0.Add(time.Duration(h) * time.Hour)
		// The database stays at 41GiB; a backup job fills the volume 9GiB/h
		// until it is 92.5% full (370 of 400GiB).
		if err := eng.ReportDisk(ctx, p.ID, diskReport("db01", 100+9*float64(h), 400)); err != nil {
			t.Fatal(err)
		}
		eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, 40*gib, gib, nil)), EvaluateOptions{})
	}
	rules := firingRules(eng, p)
	usage, ok := rules[RuleCapacityUsage+"|volume:/var/lib/postgresql"]
	if !ok || usage.Severity != SevCritical || usage.Check != CheckHostDisk {
		t.Fatalf("a 92.5%% volume filled by non-database files must alert from the host report: %v", rules)
	}
	if _, ok := rules[RuleLimitUndeclared+"|storage:footprint"]; ok {
		t.Fatalf("with a host report the limit is known; 'no limit declared' must not show")
	}
	if !strings.Contains(usage.Detail, "DB 밖 파일 약 329.0 GiB") {
		t.Fatalf("the detail must say how much of the volume is outside the database: %s", usage.Detail)
	}
	if !strings.Contains(usage.Detail, "디스크 실측(df) 한도") || !strings.Contains(usage.Detail, "db01") {
		t.Fatalf("the detail must say the limit comes from df and name the host: %s", usage.Detail)
	}
	if fc, ok := rules[RuleCapacityForecast+"|volume:/var/lib/postgresql"]; !ok || fc.Severity != SevCritical {
		t.Fatalf("a volume growing 144GiB/day with ~120GiB left must forecast critical: %v", rules)
	}
	if b := eng.Board([]dbconn.Profile{p}); b.Summary.WithoutLimit != 0 || b.Profiles[0].DiskHost != "db01" {
		t.Fatalf("a host-reported volume is a known limit: %+v", b.Summary)
	}

	// The database stops (its volume is full): collection fails, the
	// volume alert must stay and collection_down must join it.
	failed := collector.BatchResult{Results: []collector.ProfileResult{{Status: "error", ErrorCode: "COLLECTION_FAILED", Error: "no space left on device", Snapshot: collector.Snapshot{ProfileID: p.ID}}}}
	for i := 0; i < 3; i++ {
		clock = clock.Add(time.Minute)
		_ = eng.ReportDisk(ctx, p.ID, diskReport("db01", 400, 400))
		eng.Evaluate(ctx, failed, EvaluateOptions{})
	}
	rules = firingRules(eng, p)
	if _, ok := rules[RuleCapacityUsage+"|volume:/var/lib/postgresql"]; !ok {
		t.Fatalf("the volume alert must survive the database going down: %v", rules)
	}
	if _, ok := rules[RuleCollectionDown+"|collector"]; !ok {
		t.Fatalf("collection_down must fire too: %v", rules)
	}
	_ = notifier
}

func TestHostDiskStaleReportWarnsAndResolvesWhenReportingResumes(t *testing.T) {
	p := pgProfile("")
	clock := t0
	eng, _, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	ctx := context.Background()
	_ = eng.ReportDisk(ctx, p.ID, diskReport("db01", 10, 400))
	clock = clock.Add(20 * time.Minute)
	eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	if _, ok := firingRules(eng, p)[RuleHostDiskStale+"|db01"]; !ok {
		t.Fatalf("a report older than 15 minutes means the agent stopped")
	}
	_ = eng.ReportDisk(ctx, p.ID, diskReport("db01", 10, 400))
	clock = clock.Add(time.Minute)
	eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	if _, ok := firingRules(eng, p)[RuleHostDiskStale+"|db01"]; ok {
		t.Fatalf("a fresh report must resolve the stale warning")
	}
}

func TestHostDiskReportValidation(t *testing.T) {
	bad := []HostDiskReport{
		{},
		{Volumes: []DiskVolume{{Mount: "", TotalBytes: 1}}},
		{Volumes: []DiskVolume{{Mount: "/a", TotalBytes: 0}}},
		{Volumes: []DiskVolume{{Mount: "/a", TotalBytes: 10, UsedBytes: 50}}},
		{Volumes: []DiskVolume{{Mount: "/a", TotalBytes: 10}, {Mount: "/a", TotalBytes: 10}}},
	}
	for i, r := range bad {
		if r.Validate() == nil {
			t.Fatalf("report %d must be rejected: %+v", i, r)
		}
	}
	// ext4 reserves blocks for root: PostgreSQL can only fill used+avail.
	v := DiskVolume{Mount: "/d", TotalBytes: 100, UsedBytes: 60, AvailBytes: 35}
	if v.fillableBytes() != 95 {
		t.Fatalf("fillable = used+avail, got %v", v.fillableBytes())
	}
}

func TestSilenceHoldsNotificationsAndReleasesThemAfter(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	ctx := context.Background()
	s, err := eng.AddSilence(Silence{ProfileID: p.ID, Rule: "capacity_*", Reason: "볼륨 증설 작업"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, 95*gib, gib, nil)), EvaluateOptions{})
	if got := notifier.all(); len(got) != 0 {
		t.Fatalf("a silenced alert must not be sent: %v", got)
	}
	board := eng.Board([]dbconn.Profile{p})
	if len(board.Firing) == 0 || board.Firing[0].SilencedUntil == nil || len(board.Silences) != 1 {
		t.Fatalf("the console must still show the alert and its silence: %+v", board.Firing)
	}
	if _, err := eng.EndSilence(s.ID); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, 95*gib, gib, nil)), EvaluateOptions{})
	if got := notifier.all(); len(got) == 0 || got[0] != "firing:capacity_usage" {
		t.Fatalf("what is still true when the silence ends must be sent: %v", got)
	}
	if _, err := eng.AddSilence(Silence{Reason: "x"}, 30*24*time.Hour); err == nil {
		t.Fatalf("silences longer than 7 days must be refused")
	}
	if _, err := eng.AddSilence(Silence{}, time.Hour); err == nil {
		t.Fatalf("a silence needs a reason")
	}
}

func TestSilenceRuleMatching(t *testing.T) {
	a := &Alert{ProfileID: "p", Rule: "maint_wal_archive"}
	now := t0
	cases := []struct {
		s    Silence
		want bool
	}{
		{Silence{EndsAt: now.Add(time.Hour)}, true},
		{Silence{ProfileID: "q", EndsAt: now.Add(time.Hour)}, false},
		{Silence{Rule: "maint_*", EndsAt: now.Add(time.Hour)}, true},
		{Silence{Rule: "maint_bloat", EndsAt: now.Add(time.Hour)}, false},
		{Silence{EndsAt: now}, false},
	}
	for i, c := range cases {
		if got := c.s.matches(a, now); got != c.want {
			t.Fatalf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestPerProfileChannelAndMinimumSeverity(t *testing.T) {
	team := dbconn.ApplyDefaults(dbconn.Profile{ID: "team-db", Type: "postgres", Environment: "production",
		Capacity: &dbconn.CapacityConfig{StorageLimit: "100GiB"}, Alerting: &dbconn.AlertingConfig{WebhookRef: "plain:https://team.example/hooks/x"}})
	dev := dbconn.ApplyDefaults(dbconn.Profile{ID: "dev-db", Type: "postgres", Environment: "development",
		Capacity: &dbconn.CapacityConfig{StorageLimit: "100GiB"}, Alerting: &dbconn.AlertingConfig{MinSeverity: "critical"}})
	clock := t0
	eng, def, _, _ := newTestEngine(t, t.TempDir(), team, &clock)
	eng.Profiles = staticProfiles{team, dev}
	teamChannel := &captureNotifier{id: "team"}
	eng.Route = func(p dbconn.Profile) (Notifier, error) {
		if p.ID == team.ID {
			return teamChannel, nil
		}
		return nil, nil
	}
	eng.Evaluate(context.Background(), okBatch(
		footprintSnapshot(team, clock, 85*gib, gib, nil), // 86% → warning
		footprintSnapshot(dev, clock, 85*gib, gib, nil),  // warning, but dev only wants critical
	), EvaluateOptions{})
	if got := teamChannel.all(); len(got) != 1 || teamChannel.calls[0][0].Alert.ProfileID != team.ID {
		t.Fatalf("the team DB's alert must go to its own channel: %v", got)
	}
	if got := def.all(); len(got) != 0 {
		t.Fatalf("nothing for the default channel: the team alert is routed, dev is below its minimum: %v", got)
	}
	clock = clock.Add(time.Minute)
	eng.Evaluate(context.Background(), okBatch(
		footprintSnapshot(team, clock, 85*gib, gib, nil),
		footprintSnapshot(dev, clock, 95*gib, gib, nil), // now critical
	), EvaluateOptions{})
	if got := def.all(); len(got) != 1 || def.calls[0][0].Alert.ProfileID != dev.ID {
		t.Fatalf("dev's critical alert goes to the default channel: %v", got)
	}

	// A broken team channel backs off on its own; the default keeps working.
	teamChannel.fail = errors.New("410 gone")
	clock = clock.Add(7 * time.Hour) // reminders due everywhere
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(team, clock, 85*gib, gib, nil), footprintSnapshot(dev, clock, 95*gib, gib, nil)), EvaluateOptions{})
	board := eng.Board([]dbconn.Profile{team, dev})
	if len(board.Channels) != 1 || board.Channels[0].ConsecutiveFailures != 1 || board.Delivery.ConsecutiveFailures != 0 {
		t.Fatalf("channel health must be tracked per destination: channels=%+v default=%+v", board.Channels, board.Delivery)
	}
}

func TestBrokenChannelFallsBackToDefault(t *testing.T) {
	p := pgProfile("100GiB")
	clock := t0
	eng, def, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.Route = func(dbconn.Profile) (Notifier, error) {
		return nil, errors.New("environment variable TEAM_HOOK is not set")
	}
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 95*gib, gib, nil)), EvaluateOptions{})
	if len(def.all()) != 1 {
		t.Fatalf("an unresolvable channel must not lose the alert")
	}
	b := eng.Board([]dbconn.Profile{p})
	if len(b.Channels) != 1 || !strings.Contains(b.Channels[0].LastError, "TEAM_HOOK") {
		t.Fatalf("the routing error must be visible: %+v", b.Channels)
	}
}

func TestDailyReportOncePerDayInsideItsWindow(t *testing.T) {
	if err := SetDisplayLocation("Asia/Seoul"); err != nil {
		t.Fatal(err)
	}
	p := pgProfile("500GiB")
	clock := time.Date(2026, 10, 6, 8, 50, 0, 0, displayLocation()) // 08:50 KST
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.cfg.DigestAt = "09:00"
	tick := func() {
		eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 200*gib, 10*gib, nil)), EvaluateOptions{})
	}
	tick()
	if len(notifier.texts) != 0 {
		t.Fatalf("no report before 09:00")
	}
	clock = clock.Add(15 * time.Minute) // 09:05
	tick()
	clock = clock.Add(10 * time.Minute)
	tick()
	if len(notifier.texts) != 1 || !strings.HasPrefix(notifier.texts[0], "digest:") {
		t.Fatalf("exactly one report per day: %v", notifier.texts)
	}
	text := notifier.texts[0]
	for _, want := range []string{"일일 용량 리포트 — 2026-10-06 (화)", "42.0% (210.0 GiB / 500.0 GiB)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
	clock = time.Date(2026, 10, 7, 12, 0, 0, 0, displayLocation()) // next day, outside the window
	tick()
	if len(notifier.texts) != 1 {
		t.Fatalf("a report missed by more than two hours is skipped, not sent at noon")
	}
	if _, err := ParseDigestTime("25:00"); err == nil {
		t.Fatalf("invalid time accepted")
	}
	if v, _ := ParseDigestTime("7:5"); v != "07:05" {
		t.Fatalf("normalization: %q", v)
	}
}

// A dead agent is a loss of visibility, not good news: the last volume
// alert must stay firing next to the "reports stopped" warning.
func TestStaleDiskReportDoesNotResolveVolumeAlerts(t *testing.T) {
	p := pgProfile("")
	clock := t0
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	ctx := context.Background()
	_ = eng.ReportDisk(ctx, p.ID, diskReport("db01", 380, 400))
	eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	clock = clock.Add(20 * time.Minute) // the agent died
	eng.Evaluate(ctx, okBatch(footprintSnapshot(p, clock, gib, 0, nil)), EvaluateOptions{})
	rules := firingRules(eng, p)
	if _, ok := rules[RuleCapacityUsage+"|volume:/var/lib/postgresql"]; !ok {
		t.Fatalf("the 95%% volume alert must survive the agent going silent: %v", rules)
	}
	if _, ok := rules[RuleHostDiskStale+"|db01"]; !ok {
		t.Fatalf("the stale warning must fire: %v", rules)
	}
	for _, k := range notifier.all() {
		if strings.HasPrefix(k, "resolved:") {
			t.Fatalf("nothing may be announced as resolved: %v", notifier.all())
		}
	}
}

func TestDailyReportWaitsForSomethingToReport(t *testing.T) {
	if err := SetDisplayLocation("Asia/Seoul"); err != nil {
		t.Fatal(err)
	}
	p := pgProfile("500GiB")
	clock := time.Date(2026, 10, 6, 9, 1, 0, 0, displayLocation())
	eng, notifier, _, _ := newTestEngine(t, t.TempDir(), p, &clock)
	eng.cfg.DigestAt = "09:00"
	eng.Profiles = staticProfiles{}
	eng.Evaluate(context.Background(), collector.BatchResult{}, EvaluateOptions{})
	if len(notifier.texts) != 0 {
		t.Fatalf("an empty fleet must not produce a report: %v", notifier.texts)
	}
	eng.Profiles = staticProfiles{p}
	clock = clock.Add(time.Minute)
	eng.Evaluate(context.Background(), okBatch(footprintSnapshot(p, clock, 100*gib, gib, nil)), EvaluateOptions{})
	if len(notifier.texts) != 1 {
		t.Fatalf("once there is a database, the day's report goes out: %v", notifier.texts)
	}
}
