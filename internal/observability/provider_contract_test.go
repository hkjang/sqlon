package observability

import (
	"context"
	"strings"
	"testing"

	"sqlon/internal/dbconn"
)

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
	registry := NewRegistry()
	for _, tc := range tests {
		t.Run(tc.engine, func(t *testing.T) {
			provider, ok := registry.Get(tc.engine)
			if !ok {
				t.Fatalf("provider not registered")
			}
			q := &fakeQueryer{rows: []map[string]any{tc.sessionRow}}
			sessions, err := provider.Sessions(context.Background(), q, dbconn.Profile{ID: "p", Type: tc.engine})
			if err != nil || len(sessions) != 1 || sessions[0].Engine != tc.engine || sessions[0].SessionKey != tc.sessionKey {
				t.Fatalf("session contract: sessions=%+v err=%v", sessions, err)
			}
			assertFixedReadOnlySystemQuery(t, q.seen, tc.sessionView)

			q = &fakeQueryer{rows: []map[string]any{{"blocker_key": "1", "blocked_key": "2", "lock_type": "TX"}}}
			edges, err := provider.Locks(context.Background(), q, dbconn.Profile{ID: "p", Type: tc.engine})
			if err != nil || len(edges) != 1 || edges[0].Engine != tc.engine || edges[0].BlockerKey != "1" || edges[0].BlockedKey != "2" {
				t.Fatalf("lock contract: edges=%+v err=%v", edges, err)
			}
			assertFixedReadOnlySystemQuery(t, q.seen, tc.lockView)
		})
	}
}

func assertFixedReadOnlySystemQuery(t *testing.T, query, expectedView string) {
	t.Helper()
	upper := strings.ToUpper(strings.TrimSpace(query))
	if !strings.HasPrefix(upper, "SELECT") || !strings.Contains(upper, expectedView) {
		t.Fatalf("unexpected system query: %s", query)
	}
	for _, forbidden := range []string{"INSERT ", "UPDATE ", "DELETE ", "ALTER ", "DROP ", "ACTIVE_SESSION_HISTORY", "DBA_HIST", "DBMS_WORKLOAD_REPOSITORY", "DBMS_SQLTUNE"} {
		if strings.Contains(upper, forbidden) {
			t.Fatalf("provider query contains write/licensed feature %q: %s", forbidden, query)
		}
	}
}

func TestProtectedSessionPolicyCoversReplicationAndBackgroundWorkers(t *testing.T) {
	for _, tc := range []struct{ engine, user, state, app string }{
		{"postgres", "postgres", "", "walreceiver"},
		{"postgres", "", "background", "checkpointer"},
		{"mysql", "system user", "Binlog Dump", ""},
		{"oracle", "SYSTEM", "ACTIVE", "sqlplus"},
	} {
		if protected, reason := protectSession(tc.engine, tc.user, tc.state, tc.app); !protected || reason == "" {
			t.Fatalf("system session not protected: %+v", tc)
		}
	}
}
