package adapters_test

import (
	"context"
	"strings"
	"testing"

	"sqlon/internal/dbconn"
	"sqlon/internal/engine"
	"sqlon/internal/engine/adapters"
)

// contractQueryer answers each engine's fixed system queries with minimal
// plausible rows and records every query for read-only/license assertions.
type contractQueryer struct {
	queries []string
	rows    []map[string]any
}

func (q *contractQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	q.queries = append(q.queries, query)
	if q.rows != nil {
		return q.rows, nil
	}
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

func assertReadOnlyBaseLicense(t *testing.T, query string) {
	t.Helper()
	upper := strings.ToUpper(strings.TrimSpace(query))
	// SHOW is the only complete replica-status interface on MySQL/MariaDB and
	// is read-only; everything else must be a SELECT.
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "SHOW ") {
		t.Fatalf("non-read-only provider query: %s", query)
	}
	for _, forbidden := range []string{"INSERT ", "UPDATE ", "DELETE ", "ALTER ", "DROP ", "ACTIVE_SESSION_HISTORY", "DBA_HIST", "DBMS_WORKLOAD_REPOSITORY", "DBMS_SQLTUNE"} {
		if strings.Contains(upper, forbidden) {
			t.Fatalf("provider query contains write/licensed feature %q: %s", forbidden, query)
		}
	}
}

// TestEveryDeclaredEngineHasProvidersForItsCapabilities pins wiring to the
// capability declaration: an engine declaring Workload/Sessions/LockTree must
// have a registered implementation, and no implementation may exist for an
// undeclared engine.
func TestEveryDeclaredEngineHasProvidersForItsCapabilities(t *testing.T) {
	engines := engine.NewDefaultRegistry()
	workload := adapters.CollectorProviders()
	observation := adapters.ObservabilityProviders()
	replication := adapters.ReplicationProviders()
	backup := adapters.BackupProviders()
	for _, name := range engines.Names() {
		adapter, _ := engines.Get(name)
		if adapter.Capabilities.Workload {
			if _, ok := workload[name]; !ok {
				t.Errorf("engine %s declares Workload but has no collector provider", name)
			}
		}
		if adapter.Capabilities.Sessions || adapter.Capabilities.LockTree {
			if _, ok := observation[name]; !ok {
				t.Errorf("engine %s declares Sessions/LockTree but has no observability provider", name)
			}
		}
		if adapter.Capabilities.Replication {
			if _, ok := replication[name]; !ok {
				t.Errorf("engine %s declares Replication but has no replication provider", name)
			}
		}
		if adapter.Capabilities.BackupStatus {
			if _, ok := backup[name]; !ok {
				t.Errorf("engine %s declares BackupStatus but has no backup provider", name)
			}
		}
	}
	for name := range backup {
		if _, ok := engines.Get(name); !ok {
			t.Errorf("backup provider %s has no capability declaration", name)
		}
	}
	for name := range workload {
		if _, ok := engines.Get(name); !ok {
			t.Errorf("collector provider %s has no capability declaration", name)
		}
	}
	for name := range observation {
		if _, ok := engines.Get(name); !ok {
			t.Errorf("observability provider %s has no capability declaration", name)
		}
	}
	for name := range replication {
		if _, ok := engines.Get(name); !ok {
			t.Errorf("replication provider %s has no capability declaration", name)
		}
	}
}

// funcQueryer routes fixed system queries to scripted answers and records
// every query for read-only assertions.
type funcQueryer struct {
	fn      func(query string) ([]map[string]any, error)
	queries []string
}

func (f *funcQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	f.queries = append(f.queries, query)
	return f.fn(query)
}

func TestAllReplicationProvidersSatisfyTopologyContract(t *testing.T) {
	answers := map[string]func(string) ([]map[string]any, error){
		"postgres": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.Contains(upper, "PG_IS_IN_RECOVERY"):
				return []map[string]any{{"role": "primary"}}, nil
			case strings.Contains(upper, "PG_STAT_REPLICATION"):
				return []map[string]any{{"name": "standby1", "target": "10.0.0.2", "state": "streaming", "sync_state": "async", "lag_bytes": 1024, "lag_seconds": 1.5}}, nil
			case strings.Contains(upper, "PG_REPLICATION_SLOTS"):
				return []map[string]any{{"name": "slot1", "active": "true", "retained_bytes": 2048}}, nil
			}
			return []map[string]any{}, nil
		},
		"mysql": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.HasPrefix(upper, "SHOW REPLICA STATUS"):
				return []map[string]any{{"Replica_IO_Running": "Yes", "Replica_SQL_Running": "Yes", "Seconds_Behind_Source": 5, "Source_Host": "src", "Source_Port": 3306}}, nil
			case strings.Contains(upper, "PROCESSLIST"):
				return []map[string]any{{"replica_connections": 0}}, nil
			}
			return []map[string]any{}, nil
		},
		"mariadb": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.HasPrefix(upper, "SHOW ALL SLAVES STATUS"):
				return []map[string]any{{"Connection_name": "dc2", "Slave_IO_Running": "Yes", "Slave_SQL_Running": "No", "Seconds_Behind_Master": 42, "Master_Host": "src", "Master_Port": 3306, "Last_SQL_Error": "duplicate key"}}, nil
			case strings.Contains(upper, "PROCESSLIST"):
				return []map[string]any{{"replica_connections": 0}}, nil
			}
			return []map[string]any{}, nil
		},
		"oracle": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.Contains(upper, "V$DATABASE"):
				return []map[string]any{{"database_role": "PHYSICAL STANDBY", "open_mode": "READ ONLY WITH APPLY", "protection_mode": "MAXIMUM PERFORMANCE", "switchover_status": "NOT ALLOWED"}}, nil
			case strings.Contains(upper, "V$DATAGUARD_STATS"):
				return []map[string]any{{"name": "apply lag", "value": "+00 00:05:00", "unit": "day(2) to second(0) interval"}}, nil
			}
			return []map[string]any{}, nil
		},
	}
	expect := map[string]struct {
		role      string
		unhealthy bool
	}{
		"postgres": {role: "primary"},
		"mysql":    {role: "replica"},
		"mariadb":  {role: "replica", unhealthy: true},
		"oracle":   {role: "standby"},
	}
	providers := adapters.ReplicationProviders()
	for engineName, provider := range providers {
		t.Run(engineName, func(t *testing.T) {
			q := &funcQueryer{fn: answers[engineName]}
			data, err := provider.Replication(context.Background(), q, dbconn.Profile{ID: "p", Type: engineName})
			if err != nil {
				t.Fatalf("replication contract: %v", err)
			}
			want := expect[engineName]
			if data.Engine != engineName || data.Role != want.role || len(data.Nodes) == 0 {
				t.Fatalf("replication contract: role=%s nodes=%d data=%+v", data.Role, len(data.Nodes), data)
			}
			sawUnhealthy := false
			for _, node := range data.Nodes {
				if !node.Healthy {
					sawUnhealthy = true
				}
			}
			if sawUnhealthy != want.unhealthy {
				t.Fatalf("health classification: got unhealthy=%v want %v (%+v)", sawUnhealthy, want.unhealthy, data.Nodes)
			}
			for _, query := range q.queries {
				assertReadOnlyBaseLicense(t, query)
			}
		})
	}
}

func TestAllProvidersCollectCommonWorkloadCapacityContract(t *testing.T) {
	providers := adapters.CollectorProviders()
	for _, engineName := range []string{"postgres", "mysql", "mariadb", "oracle"} {
		t.Run(engineName, func(t *testing.T) {
			provider, ok := providers[engineName]
			if !ok {
				t.Fatal("provider missing")
			}
			q := &contractQueryer{}
			snapshot, err := provider.Collect(context.Background(), q, dbconn.Profile{ID: "p", Type: engineName})
			if err != nil || snapshot.Engine != engineName || len(snapshot.Counters) == 0 || len(snapshot.Waits) == 0 || len(snapshot.TopSQL) == 0 || len(snapshot.Capacity) == 0 {
				t.Fatalf("contract failed: snapshot=%+v err=%v", snapshot, err)
			}
			for _, query := range q.queries {
				assertReadOnlyBaseLicense(t, query)
			}
		})
	}
}

func TestAllEngineProvidersSatisfySessionAndLockContract(t *testing.T) {
	tests := []struct {
		engine, sessionKey, sessionView, lockView string
		sessionRow                                map[string]any
	}{
		{"postgres", "42", "PG_STAT_ACTIVITY", "PG_BLOCKING_PIDS", map[string]any{"session_id": "42", "username": "app", "state": "active"}},
		{"mysql", "42", "INFORMATION_SCHEMA.PROCESSLIST", "DATA_LOCK_WAITS", map[string]any{"session_id": "42", "username": "app", "state": "Query"}},
		{"mariadb", "42", "INFORMATION_SCHEMA.PROCESSLIST", "INNODB_LOCK_WAITS", map[string]any{"session_id": "42", "username": "app", "state": "Query"}},
		{"oracle", "1:42:9", "GV$SESSION", "GV$SESSION", map[string]any{"instance_id": "1", "session_id": "42", "serial_no": "9", "username": "APP", "state": "ACTIVE"}},
	}
	providers := adapters.ObservabilityProviders()
	for _, tc := range tests {
		t.Run(tc.engine, func(t *testing.T) {
			provider, ok := providers[tc.engine]
			if !ok {
				t.Fatalf("provider not registered")
			}
			q := &contractQueryer{rows: []map[string]any{tc.sessionRow}}
			sessions, err := provider.Sessions(context.Background(), q, dbconn.Profile{ID: "p", Type: tc.engine})
			if err != nil || len(sessions) != 1 || sessions[0].Engine != tc.engine || sessions[0].SessionKey != tc.sessionKey {
				t.Fatalf("session contract: sessions=%+v err=%v", sessions, err)
			}
			assertFixedSystemQuery(t, q.queries, tc.sessionView)

			q = &contractQueryer{rows: []map[string]any{{"blocker_key": "1", "blocked_key": "2", "lock_type": "TX"}}}
			edges, err := provider.Locks(context.Background(), q, dbconn.Profile{ID: "p", Type: tc.engine})
			if err != nil || len(edges) != 1 || edges[0].Engine != tc.engine || edges[0].BlockerKey != "1" || edges[0].BlockedKey != "2" {
				t.Fatalf("lock contract: edges=%+v err=%v", edges, err)
			}
			assertFixedSystemQuery(t, q.queries, tc.lockView)
		})
	}
}

func TestAllBackupProvidersSatisfyStatusContract(t *testing.T) {
	answers := map[string]func(string) ([]map[string]any, error){
		"postgres": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.Contains(upper, "ARCHIVE_MODE"):
				return []map[string]any{{"setting": "on"}}, nil
			case strings.Contains(upper, "PG_STAT_ARCHIVER"):
				return []map[string]any{{"archived_count": 120, "failed_count": 1, "last_archived_wal": "0001A", "last_archived_at": "2026-07-17T04:00:00Z", "last_failed_wal": "00019", "last_failed_at": "2026-07-16T04:00:00Z"}}, nil
			}
			return []map[string]any{}, nil
		},
		"mysql": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.Contains(upper, "@@LOG_BIN"):
				return []map[string]any{{"log_bin": 1}}, nil
			case strings.HasPrefix(upper, "SHOW BINARY LOG STATUS"):
				return []map[string]any{{"File": "binlog.000042", "Position": 15533}}, nil
			}
			return []map[string]any{}, nil
		},
		"mariadb": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.Contains(upper, "@@LOG_BIN"):
				return []map[string]any{{"log_bin": 1}}, nil
			case strings.HasPrefix(upper, "SHOW MASTER STATUS"):
				return []map[string]any{{"File": "mariadb-bin.000007", "Position": 9812}}, nil
			}
			return []map[string]any{}, nil
		},
		"oracle": func(query string) ([]map[string]any, error) {
			upper := strings.ToUpper(query)
			switch {
			case strings.Contains(upper, "V$DATABASE"):
				return []map[string]any{{"log_mode": "ARCHIVELOG"}}, nil
			case strings.Contains(upper, "V$RMAN_BACKUP_JOB_DETAILS"):
				return []map[string]any{{"status": "FAILED", "input_type": "DB FULL", "started_at": "2026-07-17T01:00:00", "ended_at": "2026-07-17T01:05:00"}}, nil
			case strings.Contains(upper, "V$RECOVERY_FILE_DEST"):
				return []map[string]any{{"name": "+FRA", "space_limit": 1000, "space_used": 950, "space_reclaimable": 10}}, nil
			}
			return []map[string]any{}, nil
		},
	}
	expect := map[string]struct {
		archiving string
		unhealthy bool
	}{
		"postgres": {archiving: "enabled"},
		"mysql":    {archiving: "enabled"},
		"mariadb":  {archiving: "enabled"},
		"oracle":   {archiving: "enabled", unhealthy: true}, // FAILED RMAN job + FRA 94%
	}
	providers := adapters.BackupProviders()
	for engineName, provider := range providers {
		t.Run(engineName, func(t *testing.T) {
			q := &funcQueryer{fn: answers[engineName]}
			data, err := provider.Backup(context.Background(), q, dbconn.Profile{ID: "p", Type: engineName})
			if err != nil {
				t.Fatalf("backup contract: %v", err)
			}
			want := expect[engineName]
			if data.Engine != engineName || data.Archiving != want.archiving || len(data.Items) == 0 {
				t.Fatalf("backup contract: archiving=%s items=%d data=%+v", data.Archiving, len(data.Items), data)
			}
			sawUnhealthy := false
			for _, item := range data.Items {
				if !item.Healthy {
					sawUnhealthy = true
				}
			}
			if sawUnhealthy != want.unhealthy {
				t.Fatalf("health classification: got unhealthy=%v want %v (%+v)", sawUnhealthy, want.unhealthy, data.Items)
			}
			for _, query := range q.queries {
				assertReadOnlyBaseLicense(t, query)
			}
		})
	}
}

func assertFixedSystemQuery(t *testing.T, queries []string, expectedView string) {
	t.Helper()
	if len(queries) != 1 {
		t.Fatalf("expected exactly one fixed system query, got %d", len(queries))
	}
	if !strings.Contains(strings.ToUpper(queries[0]), expectedView) {
		t.Fatalf("unexpected system view, want %s in: %s", expectedView, queries[0])
	}
	assertReadOnlyBaseLicense(t, queries[0])
}
