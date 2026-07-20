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
			{"slot_name": "dead_slot", "slot_type": "physical", "active": false, "retained_bytes": int64(9 << 30)},   // >8GiB critical
			{"slot_name": "warn_slot", "slot_type": "logical", "active": false, "retained_bytes": int64(2 << 30)},    // >1GiB warning
			{"slot_name": "live_slot", "slot_type": "physical", "active": true, "retained_bytes": int64(100 << 30)},  // active → ignored
			{"slot_name": "tiny_slot", "slot_type": "physical", "active": false, "retained_bytes": int64(10 << 20)},  // small → ignored
		},
	}
	data, err := postgres.Maintenance{}.Maintenance(context.Background(), q, dbconn.Profile{ID: "pg", Type: "postgres"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	slots := findingsByCategory(data)["replication_slot"]
	if len(slots) != 2 {
		t.Fatalf("only the two large inactive slots should surface: %+v", slots)
	}
	sev := map[string]string{}
	for _, f := range slots {
		sev[f.Object] = f.Severity
	}
	if sev["dead_slot"] != "critical" || sev["warn_slot"] != "warning" {
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
