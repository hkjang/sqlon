package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sqlon/internal/dbconn"
	"sqlon/internal/engine/postgres"
	"sqlon/internal/observability"
)

// routedQueryer answers each of the three maintenance queries independently so
// a single test can exercise wraparound, bloat, and slot checks together.
type routedQueryer struct {
	wraparound []map[string]any
	bloat      []map[string]any
	slots      []map[string]any
	settings   []map[string]any // server probe; nil → probe fails (old fixtures)
	archiver   []map[string]any
	walDir     []map[string]any
	xacts      []map[string]any
	wrapErr    error
	bloatErr   error
	slotErr    error
	seen       []string
}

func (q *routedQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	q.seen = append(q.seen, query)
	upper := strings.ToUpper(query)
	switch {
	case strings.Contains(upper, "DATFROZENXID") || strings.Contains(upper, "RELFROZENXID"):
		return q.wraparound, q.wrapErr
	case strings.Contains(upper, "PG_STAT_USER_TABLES"):
		return q.bloat, q.bloatErr
	case strings.Contains(upper, "PG_REPLICATION_SLOTS"):
		return q.slots, q.slotErr
	case strings.Contains(upper, "__LISTABLE"):
		if q.settings == nil {
			return nil, errors.New("settings probe not stubbed")
		}
		return q.settings, nil
	case strings.Contains(upper, "PG_STAT_ARCHIVER"):
		return q.archiver, nil
	case strings.Contains(upper, "PG_LS_WALDIR") || strings.Contains(upper, "PG_LS_ARCHIVE_STATUSDIR"):
		return q.walDir, nil
	case strings.Contains(upper, "PG_PREPARED_XACTS"):
		return q.xacts, nil
	}
	return nil, errors.New("unexpected query: " + query)
}

func findingsByCategory(data observability.MaintenanceData) map[string][]observability.MaintenanceFinding {
	out := map[string][]observability.MaintenanceFinding{}
	for _, f := range data.Findings {
		out[f.Category] = append(out[f.Category], f)
	}
	return out
}

func TestMaintenanceWraparoundSeverity(t *testing.T) {
	q := &routedQueryer{
		// past freeze_max_age (200M) → critical; a healthy DB well under → skipped.
		wraparound: []map[string]any{
			{"kind": "database", "object": "app", "xid_age": int64(230_000_000), "freeze_max_age": int64(200_000_000)},
			{"kind": "table", "object": "public.t_warn", "xid_age": int64(185_000_000), "freeze_max_age": int64(200_000_000)},
			{"kind": "table", "object": "public.t_ok", "xid_age": int64(1_000_000), "freeze_max_age": int64(200_000_000)},
		},
		bloat: []map[string]any{},
		slots: []map[string]any{},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byCat := findingsByCategory(data)
	wrap := byCat["wraparound"]
	if len(wrap) != 2 {
		t.Fatalf("expected 2 wraparound findings (critical+warning), got %d: %+v", len(wrap), wrap)
	}
	var sawCritical, sawWarning, sawOK bool
	for _, f := range wrap {
		switch {
		case strings.Contains(f.Object, "app"):
			sawCritical = f.Severity == "critical"
		case strings.Contains(f.Object, "t_warn"):
			sawWarning = f.Severity == "warning"
		case strings.Contains(f.Object, "t_ok"):
			sawOK = true
		}
	}
	if !sawCritical {
		t.Fatalf("database past freeze_max_age must be critical: %+v", wrap)
	}
	if !sawWarning {
		t.Fatalf("table at 92%% of freeze_max_age must be warning: %+v", wrap)
	}
	if sawOK {
		t.Fatalf("healthy table must not produce a finding: %+v", wrap)
	}
}

func TestMaintenanceWraparoundHardCeilingIsCritical(t *testing.T) {
	// No freeze setting available (0), but age is 85% of 2^31 → critical anyway.
	q := &routedQueryer{
		wraparound: []map[string]any{
			{"kind": "database", "object": "legacy", "xid_age": int64(1_850_000_000), "freeze_max_age": int64(0)},
		},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wrap := findingsByCategory(data)["wraparound"]
	if len(wrap) != 1 || wrap[0].Severity != "critical" {
		t.Fatalf("85%% of 2^31 must be critical regardless of freeze setting: %+v", wrap)
	}
}

func TestMaintenanceBloatIsCappedAtWarning(t *testing.T) {
	q := &routedQueryer{
		wraparound: []map[string]any{},
		bloat: []map[string]any{
			{"object": "public.big", "live_tuples": int64(300_000), "dead_tuples": int64(300_000), "dead_ratio_pct": 50.0, "last_vacuum": "2026-01-01"},
			{"object": "public.small", "live_tuples": int64(100), "dead_tuples": int64(5_000), "dead_ratio_pct": 98.0}, // below min dead-tuple floor → skipped
		},
		slots: []map[string]any{},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bloat := findingsByCategory(data)["bloat"]
	if len(bloat) != 1 {
		t.Fatalf("only the large bloated table should surface: %+v", bloat)
	}
	if bloat[0].Severity != "warning" {
		t.Fatalf("bloat must be capped at warning so it cannot mask wraparound: %+v", bloat[0])
	}
}

func TestMaintenanceInactiveSlotSeverity(t *testing.T) {
	q := &routedQueryer{
		wraparound: []map[string]any{},
		bloat:      []map[string]any{},
		slots: []map[string]any{
			{"slot_name": "dead_slot", "slot_type": "physical", "active": false, "retained_bytes": int64(9 << 30)},  // >8GiB critical
			{"slot_name": "warn_slot", "slot_type": "logical", "active": false, "retained_bytes": int64(2 << 30)},   // >1GiB warning
			{"slot_name": "live_slot", "slot_type": "physical", "active": true, "retained_bytes": int64(100 << 30)}, // connected but 100GiB behind → warning
			{"slot_name": "near_slot", "slot_type": "physical", "active": true, "retained_bytes": int64(2 << 30)},   // connected, modest lag → ignored
			{"slot_name": "tiny_slot", "slot_type": "physical", "active": false, "retained_bytes": int64(10 << 20)}, // small → ignored
		},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	slots := findingsByCategory(data)["replication_slot"]
	if len(slots) != 3 {
		t.Fatalf("two large inactive slots and the far-behind connected slot should surface: %+v", slots)
	}
	sev := map[string]string{}
	for _, f := range slots {
		sev[f.Object] = f.Severity
	}
	// A connected consumer 100GiB behind pins exactly as much WAL as an
	// abandoned slot; it is a warning (the consumer may still catch up).
	if sev["dead_slot"] != "critical" || sev["warn_slot"] != "warning" || sev["live_slot"] != "warning" {
		t.Fatalf("slot severities wrong: %+v", sev)
	}
}

func TestMaintenanceWraparoundErrorAborts(t *testing.T) {
	// Wraparound is fail-closed: a hard error must abort, not silently skip.
	q := &routedQueryer{wrapErr: errors.New("permission denied for pg_database")}
	_, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err == nil {
		t.Fatalf("wraparound collection error must propagate")
	}
}

func TestMaintenanceBloatErrorIsSoftLimitation(t *testing.T) {
	// A bloat/slot permission error is downgraded to a limitation so the
	// (successful) wraparound check still returns.
	q := &routedQueryer{
		wraparound: []map[string]any{},
		bloatErr:   errors.New("permission denied for pg_stat_user_tables"),
		slots:      []map[string]any{},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatalf("bloat error must not abort the whole check: %v", err)
	}
	if len(data.Limitations) == 0 {
		t.Fatalf("skipped bloat check must be reported as a limitation: %+v", data)
	}
}

// pg16Settings is a server probe result for a PostgreSQL 16 primary whose
// monitoring role holds pg_monitor.
func pg16Settings(extra ...map[string]any) []map[string]any {
	rows := []map[string]any{
		{"name": "server_version_num", "setting": "160004", "unit": ""},
		{"name": "archive_mode", "setting": "on", "unit": ""},
		{"name": "archive_command", "setting": "cp %p /archive/%f", "unit": ""},
		{"name": "max_wal_size", "setting": "1024", "unit": "MB"},
		{"name": "wal_keep_size", "setting": "0", "unit": "MB"},
		{"name": "wal_segment_size", "setting": "16777216", "unit": "B"},
		{"name": "max_slot_wal_keep_size", "setting": "-1", "unit": "MB"},
		{"name": "autovacuum", "setting": "on", "unit": ""},
		{"name": "__listable", "setting": "pg_ls_waldir,pg_ls_archive_statusdir", "unit": ""},
		{"name": "__in_recovery", "setting": "off", "unit": ""},
	}
	return append(rows, extra...)
}

func TestMaintenanceUsesWALStatusOn13Plus(t *testing.T) {
	q := &routedQueryer{
		settings: pg16Settings(),
		slots: []map[string]any{
			{"slot_name": "gone", "slot_type": "logical", "active": false, "wal_status": "lost", "retained_bytes": int64(0)},
			{"slot_name": "edge", "slot_type": "physical", "active": true, "wal_status": "unreserved", "retained_bytes": int64(512 << 20)},
		},
		archiver: []map[string]any{{"failing": false}},
		walDir:   []map[string]any{{"wal_bytes": float64(1 << 30), "ready_files": float64(2)}},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	usedWALStatus := false
	for _, query := range q.seen {
		if strings.Contains(query, "wal_status") {
			usedWALStatus = true
		}
	}
	if !usedWALStatus {
		t.Fatalf("PostgreSQL 16 must read wal_status: %v", q.seen)
	}
	byCat := findingsByCategory(data)
	sev := map[string]string{}
	for _, f := range byCat["replication_slot"] {
		sev[f.Object] = f.Severity
	}
	if sev["gone"] != "warning" || sev["edge"] != "warning" {
		t.Fatalf("lost/unreserved slots must warn: %+v", byCat["replication_slot"])
	}
	risks := byCat["config_risk"]
	if len(risks) != 1 || risks[0].Object != "max_slot_wal_keep_size" || risks[0].Severity != "info" {
		t.Fatalf("unlimited slot retention with slots present must be an info risk: %+v", risks)
	}
}

func TestMaintenanceFailingArchiverIsCritical(t *testing.T) {
	q := &routedQueryer{
		settings: pg16Settings(),
		archiver: []map[string]any{{"failing": true, "failed_count": int64(42), "last_failed_wal": "00000001000000000000002A", "since_archived_s": 3600.0}},
		walDir:   []map[string]any{{"wal_bytes": float64(12 << 30), "ready_files": float64(600)}}, // 600×16MiB = 9.4GiB backlog
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	byCat := findingsByCategory(data)
	objects := map[string]string{}
	for _, f := range byCat["wal_archive"] {
		objects[f.Object] = f.Severity
	}
	if objects["archive_command"] != "critical" {
		t.Fatalf("an archiver failing for an hour must be critical: %+v", byCat["wal_archive"])
	}
	if objects["archive_status"] != "critical" {
		t.Fatalf("a 9.4GiB archive backlog must be critical: %+v", byCat["wal_archive"])
	}
	wal := byCat["wal_retention"]
	// 12GiB against a 1GiB budget: 12x and 11GiB excess.
	if len(wal) != 1 || wal[0].Severity != "critical" {
		t.Fatalf("pg_wal at 12x its budget must be critical: %+v", wal)
	}
}

func TestMaintenanceBriefArchiveFailureIsWarning(t *testing.T) {
	q := &routedQueryer{
		settings: pg16Settings(),
		archiver: []map[string]any{{"failing": true, "failed_count": int64(1), "since_archived_s": 90.0}},
		walDir:   []map[string]any{{"wal_bytes": float64(1 << 30), "ready_files": float64(3)}},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	archive := findingsByCategory(data)["wal_archive"]
	if len(archive) != 1 || archive[0].Severity != "warning" {
		t.Fatalf("an archiver that last succeeded 90s ago is a warning, not critical: %+v", archive)
	}
}

func TestMaintenanceEmptyArchiveCommandWarns(t *testing.T) {
	settings := pg16Settings()
	for _, row := range settings {
		if row["name"] == "archive_command" {
			row["setting"] = ""
		}
	}
	q := &routedQueryer{settings: settings, archiver: []map[string]any{{"failing": false}}, walDir: []map[string]any{{"wal_bytes": float64(1 << 30), "ready_files": float64(0)}}}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	archive := findingsByCategory(data)["wal_archive"]
	if len(archive) != 1 || archive[0].Severity != "warning" {
		t.Fatalf("archive_mode=on with no archive command accumulates WAL: %+v", archive)
	}
}

func TestMaintenanceWithoutPgMonitorSkipsDirectorySizes(t *testing.T) {
	settings := pg16Settings()
	for _, row := range settings {
		if row["name"] == "__listable" {
			row["setting"] = ""
		}
	}
	q := &routedQueryer{settings: settings, archiver: []map[string]any{{"failing": false}}}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range q.seen {
		if strings.Contains(query, "pg_ls_waldir()") || strings.Contains(query, "pg_ls_archive_statusdir()") {
			t.Fatalf("a role without EXECUTE on pg_ls_* must never call them (circuit breaker): %s", query)
		}
	}
	found := false
	for _, l := range data.Limitations {
		if strings.Contains(l, "pg_monitor") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the skipped WAL check must say why: %+v", data.Limitations)
	}
}

func TestMaintenanceVacuumBlockers(t *testing.T) {
	q := &routedQueryer{
		settings: pg16Settings(),
		archiver: []map[string]any{{"failing": false}},
		walDir:   []map[string]any{{"wal_bytes": float64(1 << 30), "ready_files": float64(0)}},
		xacts: []map[string]any{
			{"source": "session", "object": "4242", "owner": "app", "state": "idle in transaction", "open_seconds": 8 * 3600.0},
			{"source": "prepared", "object": "tx-17", "owner": "app", "state": "prepared", "open_seconds": 2400.0},
			{"source": "session", "object": "5151", "owner": "etl", "state": "active", "open_seconds": 600.0},
		},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	sev := map[string]string{}
	for _, f := range findingsByCategory(data)["vacuum_blocker"] {
		sev[f.Object] = f.Severity
	}
	if sev["pid 4242"] != "critical" || sev["prepared tx-17"] != "warning" || sev["pid 5151"] != "" {
		t.Fatalf("vacuum blocker severities wrong: %+v", sev)
	}
}

func TestMaintenanceSkipsVacuumBlockersOnStandby(t *testing.T) {
	settings := pg16Settings()
	for _, row := range settings {
		if row["name"] == "__in_recovery" {
			row["setting"] = "on"
		}
	}
	q := &routedQueryer{settings: settings, xacts: []map[string]any{{"source": "session", "object": "1", "open_seconds": 99999.0}}}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsByCategory(data)["vacuum_blocker"]; len(got) != 0 {
		t.Fatalf("a standby's long queries do not hold back the primary: %+v", got)
	}
}
