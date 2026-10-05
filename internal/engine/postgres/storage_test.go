package postgres

import (
	"context"
	"strings"
	"testing"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

type storageQueryer struct {
	probe   map[string]any
	dirs    map[string]any
	queries []string
}

func (q *storageQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	q.queries = append(q.queries, query)
	if strings.Contains(query, "databases_bytes") {
		return []map[string]any{q.probe}, nil
	}
	return []map[string]any{q.dirs}, nil
}

func capacityByScope(s collector.Snapshot) map[string]float64 {
	out := map[string]float64{}
	for _, c := range s.Capacity {
		out[c.Scope] = c.UsedBytes
	}
	return out
}

func TestStorageFootprintSumsDatabasesWALTempAndLogs(t *testing.T) {
	q := &storageQueryer{
		probe: map[string]any{"databases_bytes": float64(300 << 30), "databases_skipped": int64(0), "listable": "pg_ls_waldir,pg_ls_tmpdir,pg_ls_logdir,pg_ls_archive_statusdir", "logging_collector": "on"},
		dirs:  map[string]any{"wal_bytes": float64(40 << 30), "temp_bytes": float64(2 << 30), "log_bytes": float64(1 << 30)},
	}
	s := collector.NewSnapshot("pg", "postgres")
	collectStorageFootprint(context.Background(), q, dbconn.Profile{ID: "pg"}, &s)
	got := capacityByScope(s)
	if got[collector.ScopeStorage] != float64(343<<30) || got[collector.ScopeWAL] != float64(40<<30) || got[collector.ScopeLog] != float64(1<<30) {
		t.Fatalf("footprint must be databases+wal+temp+log: %+v", s.Capacity)
	}
	if len(s.Limitations) != 0 {
		t.Fatalf("a pg_monitor role has no limitation: %v", s.Limitations)
	}
}

func TestStorageFootprintWithoutPgMonitorNeverCallsDirectoryFunctions(t *testing.T) {
	q := &storageQueryer{probe: map[string]any{"databases_bytes": float64(10 << 30), "databases_skipped": int64(2), "listable": "", "logging_collector": "off"}}
	s := collector.NewSnapshot("pg", "postgres")
	collectStorageFootprint(context.Background(), q, dbconn.Profile{ID: "pg"}, &s)
	if len(q.queries) != 1 {
		t.Fatalf("without EXECUTE on pg_ls_* only the probe may run (a permission error feeds the circuit breaker): %v", q.queries)
	}
	if got := capacityByScope(s); got[collector.ScopeStorage] != float64(10<<30) {
		t.Fatalf("footprint falls back to database sizes: %+v", s.Capacity)
	}
	joined := strings.Join(s.Limitations, " ")
	if !strings.Contains(joined, "pg_monitor") || !strings.Contains(joined, "CONNECT") {
		t.Fatalf("both gaps must be explained: %v", s.Limitations)
	}
	if len(s.Evidence) != 1 || s.Evidence[0].Code != "STORAGE_FOOTPRINT_PARTIAL" {
		t.Fatalf("partial footprint evidence missing: %+v", s.Evidence)
	}
}

func TestStorageFootprintSkipsLogDirWithoutLoggingCollector(t *testing.T) {
	q := &storageQueryer{
		probe: map[string]any{"databases_bytes": float64(1 << 30), "listable": "pg_ls_waldir,pg_ls_logdir", "logging_collector": "off"},
		dirs:  map[string]any{"wal_bytes": float64(1 << 30)},
	}
	s := collector.NewSnapshot("pg", "postgres")
	collectStorageFootprint(context.Background(), q, dbconn.Profile{ID: "pg"}, &s)
	if strings.Contains(strings.Join(q.queries, "\n"), "pg_ls_logdir()") {
		t.Fatalf("pg_ls_logdir errors when the log directory was never created")
	}
}
