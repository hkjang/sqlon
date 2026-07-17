package collector

import (
	"context"
	"strings"
	"testing"

	"sqlon/internal/dbconn"
)

type contractQueryer struct {
	queries []string
	failTop bool
}

func (q *contractQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	q.queries = append(q.queries, query)
	upper := strings.ToUpper(query)
	switch {
	case strings.Contains(upper, "PG_STAT_DATABASE"):
		return []map[string]any{{"transactions": 100, "commits": 90, "rollbacks": 10, "active_connections": 3}}, nil
	case strings.Contains(upper, "GLOBAL_STATUS"):
		return []map[string]any{{"queries": 100, "commits": 90, "rollbacks": 10, "active_connections": 3}}, nil
	case strings.Contains(upper, "V$SYSSTAT"):
		return []map[string]any{{"queries": 100, "commits": 90, "rollbacks": 10}}, nil
	case strings.Contains(upper, "PG_STAT_ACTIVITY") || strings.Contains(upper, "EVENTS_WAITS_SUMMARY") || strings.Contains(upper, "V$SYSTEM_EVENT"):
		return []map[string]any{{"wait_class": "I/O", "wait_event": "read", "wait_count": 2, "time_ms": 5}}, nil
	case strings.Contains(upper, "PG_STAT_STATEMENTS") || strings.Contains(upper, "EVENTS_STATEMENTS_SUMMARY") || strings.Contains(upper, "GV$SQL"):
		return []map[string]any{{"fingerprint": "hash1", "calls": 4, "elapsed_ms": 20, "reads": 8, "rows": 2}}, nil
	case strings.Contains(upper, "GV$SESSION"):
		return []map[string]any{{"active_connections": 3, "running_connections": 1}}, nil
	case strings.Contains(upper, "PG_DATABASE_SIZE") || strings.Contains(upper, "INFORMATION_SCHEMA.TABLES") || strings.Contains(upper, "DBA_TABLESPACE_USAGE_METRICS"):
		return []map[string]any{{"scope": "database", "name": "app", "used_bytes": 1000, "allocated_bytes": 2000, "usage_percent": 50}}, nil
	default:
		return []map[string]any{}, nil
	}
}

func TestAllProvidersCollectCommonWorkloadCapacityContract(t *testing.T) {
	registry := NewRegistry()
	for _, engine := range []string{"postgres", "mysql", "mariadb", "oracle"} {
		t.Run(engine, func(t *testing.T) {
			provider, ok := registry.Get(engine)
			if !ok {
				t.Fatal("provider missing")
			}
			q := &contractQueryer{}
			snapshot, err := provider.Collect(context.Background(), q, dbconn.Profile{ID: "p", Type: engine})
			if err != nil || snapshot.Engine != engine || len(snapshot.Counters) == 0 || len(snapshot.Waits) == 0 || len(snapshot.TopSQL) == 0 || len(snapshot.Capacity) == 0 {
				t.Fatalf("contract failed: snapshot=%+v err=%v", snapshot, err)
			}
			for _, query := range q.queries {
				upper := strings.ToUpper(strings.TrimSpace(query))
				if !strings.HasPrefix(upper, "SELECT") {
					t.Fatalf("non-read-only collector query: %s", query)
				}
				for _, forbidden := range []string{"ACTIVE_SESSION_HISTORY", "DBA_HIST", "DBMS_WORKLOAD_REPOSITORY", "DBMS_SQLTUNE"} {
					if strings.Contains(upper, forbidden) {
						t.Fatalf("licensed Oracle feature in base query: %s", query)
					}
				}
			}
		})
	}
}

func TestMySQLWaitClassNormalization(t *testing.T) {
	for event, want := range map[string]string{"wait/io/file/sql/binlog": "io", "wait/lock/table/sql/handler": "lock", "wait/synch/mutex/x": "sync", "unknown": "other"} {
		if got := mysqlWaitClass(event); got != want {
			t.Fatalf("%s: got %s want %s", event, got, want)
		}
	}
}
