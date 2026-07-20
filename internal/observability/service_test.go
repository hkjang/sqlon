package observability_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/engine/adapters"
	"sqlon/internal/observability"
)

type fakeQueryer struct {
	rows []map[string]any
	err  error
	seen string
}

func (f *fakeQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	f.seen = query
	return f.rows, f.err
}

func newTestService(q observability.SystemQueryer) *observability.Service {
	return observability.New(q, adapters.ObservabilityProviders(), adapters.ReplicationProviders(), adapters.BackupProviders(), adapters.SecurityProviders(), adapters.ConfigProviders(), adapters.MaintenanceProviders())
}

func TestOracleSessionsUseRACSafeKeyAndProtectSystemSession(t *testing.T) {
	q := &fakeQueryer{rows: []map[string]any{{
		"INSTANCE_ID": "2", "SESSION_ID": "41", "SERIAL_NO": "99", "USERNAME": "SYS",
		"STATE": "ACTIVE", "SQL_ID": "abc", "DURATION_SECONDS": int64(601), "WAIT_EVENT": "enq: TX",
	}}}
	svc := newTestService(q)
	res := svc.Sessions(context.Background(), dbconn.Profile{ID: "ora", Type: "oracle"})
	if res.Status != "warning" || res.Data.LongRunning != 1 || res.Data.Waiting != 1 {
		t.Fatalf("unexpected summary: %+v", res)
	}
	got := res.Data.Sessions[0]
	if got.SessionKey != "2:41:99" || !got.Protected || got.SQLID != "abc" {
		t.Fatalf("Oracle session identity/protection lost: %+v", got)
	}
	if strings.Contains(strings.ToUpper(q.seen), "ACTIVE_SESSION_HISTORY") || strings.Contains(strings.ToUpper(q.seen), "DBA_HIST") {
		t.Fatalf("base provider used licensed view: %s", q.seen)
	}
}

func TestLockTreeFindsTransitiveRootAndAffectedSessions(t *testing.T) {
	q := &fakeQueryer{rows: []map[string]any{
		{"blocker_key": "10", "blocked_key": "20", "blocker_user": "root", "wait_seconds": 8},
		{"blocker_key": "20", "blocked_key": "30", "blocker_user": "mid", "wait_seconds": 4},
	}}
	svc := newTestService(q)
	res := svc.Locks(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	if res.Status != "critical" || res.Data.BlockedSessions != 2 || len(res.Data.Roots) != 1 {
		t.Fatalf("unexpected lock response: %+v", res)
	}
	if root := res.Data.Roots[0]; root.SessionKey != "10" || root.AffectedSessions != 2 {
		t.Fatalf("wrong root: %+v", root)
	}
}

func TestCollectionDistinguishesPermissionFailure(t *testing.T) {
	q := &fakeQueryer{err: errors.New("system query failed: ORA-00942: table or view does not exist")}
	res := newTestService(q).Sessions(context.Background(), dbconn.Profile{ID: "ora", Type: "oracle"})
	if res.Status != "permission_denied" || len(res.Data.Sessions) != 0 || res.Evidence[0].Code != "COLLECTION_PERMISSION_DENIED" {
		t.Fatalf("permission failure hidden or misclassified: %+v", res)
	}
}

func TestSessionAndTransactionDurationAreSeparated(t *testing.T) {
	q := &fakeQueryer{rows: []map[string]any{{"session_id": "7", "state": "active", "duration_seconds": int64(12), "transaction_seconds": int64(900)}}}
	res := newTestService(q).Sessions(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	got := res.Data.Sessions[0]
	if got.DurationSeconds != 12 || got.TransactionSeconds != 900 {
		t.Fatalf("durations conflated: %+v", got)
	}
	if res.CollectedAt.IsZero() || res.TraceID == "" {
		t.Fatalf("freshness envelope missing: %+v", res)
	}
}

func TestNoContentionIsEvidenceNotMissingData(t *testing.T) {
	svc := newTestService(&fakeQueryer{rows: []map[string]any{}})
	svc.Now = func() time.Time { return time.Unix(100, 0) }
	res := svc.Locks(context.Background(), dbconn.Profile{ID: "my", Type: "mysql"})
	if res.Status != "ok" || res.Evidence[0].Code != "NO_LOCK_CONTENTION" || res.Data.Edges == nil {
		t.Fatalf("empty successful snapshot ambiguous: %+v", res)
	}
}

func TestReplicationBrokenNodeEscalatesToCritical(t *testing.T) {
	// A mariadb replica whose SQL thread stopped: the service must classify
	// the response as critical with REPLICATION_BROKEN evidence.
	q := &fakeQueryer{rows: []map[string]any{{"Slave_IO_Running": "Yes", "Slave_SQL_Running": "No", "Seconds_Behind_Master": nil, "Last_SQL_Error": "duplicate key"}}}
	res := newTestService(q).Replication(context.Background(), dbconn.Profile{ID: "maria", Type: "mariadb"})
	if res.Status != "critical" || res.Data.Role != "replica" {
		t.Fatalf("broken replication not critical: %+v", res)
	}
	found := false
	for _, evidence := range res.Evidence {
		if evidence.Code == "REPLICATION_BROKEN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("REPLICATION_BROKEN evidence missing: %+v", res.Evidence)
	}
	if len(res.Limitations) == 0 {
		t.Fatalf("unmeasurable lag must be reported as a limitation: %+v", res)
	}
}

func TestReplicationUnsupportedEngineIsExplicit(t *testing.T) {
	svc := observability.New(&fakeQueryer{}, adapters.ObservabilityProviders(), nil, nil, nil, nil, nil)
	res := svc.Replication(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	if res.Status != "unsupported" || len(res.Limitations) == 0 {
		t.Fatalf("missing provider must be explicit, got %+v", res)
	}
	backup := svc.Backup(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	if backup.Status != "unsupported" || len(backup.Limitations) == 0 {
		t.Fatalf("missing backup provider must be explicit, got %+v", backup)
	}
}

func TestMaintenanceUnknownEngineIsExplicit(t *testing.T) {
	// An unregistered engine must yield an explicit "unsupported", never a
	// false all-clear.
	svc := newTestService(&fakeQueryer{})
	res := svc.Maintenance(context.Background(), dbconn.Profile{ID: "x", Type: "sqlite"})
	if res.Status != "unsupported" || len(res.Limitations) == 0 {
		t.Fatalf("unknown engine must be explicit, got %+v", res)
	}
}

func TestMaintenanceMySQLProviderRuns(t *testing.T) {
	// MySQL now ships maintenance checks: with an empty result set the service
	// reports a healthy "ok" envelope (checks ran, no findings).
	svc := newTestService(&fakeQueryer{rows: []map[string]any{}})
	res := svc.Maintenance(context.Background(), dbconn.Profile{ID: "my", Type: "mysql"})
	if res.Status != "ok" || res.Data.Checks == 0 {
		t.Fatalf("mysql maintenance should run checks and be ok on empty data, got %+v", res)
	}
}

func TestMaintenanceCriticalEscalatesEnvelope(t *testing.T) {
	// PostgreSQL database past its freeze_max_age → the provider emits a
	// critical wraparound finding and the service envelope must be "critical".
	q := &fakeQueryer{rows: []map[string]any{
		{"kind": "database", "object": "app", "xid_age": int64(2_000_000_000), "freeze_max_age": int64(200_000_000)},
	}}
	res := newTestService(q).Maintenance(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	if res.Status != "critical" {
		t.Fatalf("critical wraparound must set critical envelope: %+v", res)
	}
	found := false
	for _, e := range res.Evidence {
		if e.Code == "MAINTENANCE_RISK_CRITICAL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MAINTENANCE_RISK_CRITICAL evidence missing: %+v", res.Evidence)
	}
}

func TestBackupPITRDisabledIsWarningWithEvidence(t *testing.T) {
	// PostgreSQL with archive_mode=off: every query returns the same row, so
	// archive_mode resolves to "off" → PITR disabled warning.
	q := &fakeQueryer{rows: []map[string]any{{"setting": "off"}}}
	res := newTestService(q).Backup(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	if res.Status != "warning" || res.Data.Archiving != "disabled" {
		t.Fatalf("disabled archiving not surfaced: %+v", res)
	}
	found := false
	for _, evidence := range res.Evidence {
		if evidence.Code == "BACKUP_PITR_DISABLED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("BACKUP_PITR_DISABLED evidence missing: %+v", res.Evidence)
	}
	if len(res.Limitations) == 0 {
		t.Fatalf("external-tool limitation must always be reported: %+v", res)
	}
}

func TestSecurityCriticalFindingEscalatesResponse(t *testing.T) {
	// A login-capable non-default superuser: every query returns this row, so
	// the postgres provider flags it critical.
	q := &fakeQueryer{rows: []map[string]any{{"name": "app_admin", "is_superuser": "true", "can_login": "true"}}}
	res := newTestService(q).Security(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	if res.Status != "critical" || len(res.Data.Findings) == 0 {
		t.Fatalf("critical privilege finding not escalated: %+v", res)
	}
	found := false
	for _, evidence := range res.Evidence {
		if evidence.Code == "SECURITY_EXCESS_PRIVILEGE" && evidence.Severity == "critical" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SECURITY_EXCESS_PRIVILEGE evidence missing: %+v", res.Evidence)
	}
}

func TestConfigDriftDetectsDivergenceAndBooleanEquivalence(t *testing.T) {
	// pg_settings-shaped rows; provider returns them, service compares to baseline.
	q := &fakeQueryer{rows: []map[string]any{
		{"name": "max_connections", "setting": "200", "pending_restart": "true"},
		{"name": "ssl", "setting": "on", "pending_restart": "false"},
		{"name": "work_mem", "setting": "4096", "pending_restart": "false"},
	}}
	p := dbconn.Profile{ID: "pg", Type: "postgres", ConfigBaseline: map[string]string{
		"max_connections": "100", // drift (200≠100), pending_restart
		"ssl":             "true", // match via on↔true
		"shared_buffers":  "1GB",  // unknown (not in live values)
	}}
	res := newTestService(q).ConfigDrift(context.Background(), p)
	if res.Status != "warning" || res.Data.Drifted != 1 || res.Data.Checked != 3 {
		t.Fatalf("unexpected drift summary: status=%s drifted=%d checked=%d", res.Status, res.Data.Drifted, res.Data.Checked)
	}
	byParam := map[string]observability.ConfigDriftItem{}
	for _, it := range res.Data.Items {
		byParam[it.Parameter] = it
	}
	if byParam["max_connections"].Status != "drift" || !byParam["max_connections"].PendingRestart {
		t.Fatalf("max_connections should be drift+pending: %+v", byParam["max_connections"])
	}
	if byParam["ssl"].Status != "match" {
		t.Fatalf("ssl on↔true should match: %+v", byParam["ssl"])
	}
	if byParam["shared_buffers"].Status != "unknown" {
		t.Fatalf("absent live value should be unknown: %+v", byParam["shared_buffers"])
	}
	found := false
	for _, e := range res.Evidence {
		if e.Code == "CONFIG_DRIFT_DETECTED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("CONFIG_DRIFT_DETECTED evidence missing: %+v", res.Evidence)
	}
}

func TestConfigDriftAbsentBaselineIsNotConfigured(t *testing.T) {
	res := newTestService(&fakeQueryer{}).ConfigDrift(context.Background(), dbconn.Profile{ID: "pg", Type: "postgres"})
	if res.Status != "not_configured" || len(res.Data.Items) != 0 {
		t.Fatalf("absent baseline must be explicit not_configured, got %+v", res)
	}
}

func TestProtectedSessionPolicyCoversReplicationAndBackgroundWorkers(t *testing.T) {
	for _, tc := range []struct{ engine, user, state, app string }{
		{"postgres", "postgres", "", "walreceiver"},
		{"postgres", "", "background", "checkpointer"},
		{"mysql", "system user", "Binlog Dump", ""},
		{"oracle", "SYSTEM", "ACTIVE", "sqlplus"},
	} {
		if protected, reason := observability.ProtectSession(tc.engine, tc.user, tc.state, tc.app); !protected || reason == "" {
			t.Fatalf("system session not protected: %+v", tc)
		}
	}
}
