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
	return observability.New(q, adapters.ObservabilityProviders())
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
