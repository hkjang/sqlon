package postgres

import (
	"context"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Observability implements observability.Provider (sessions + locks).
type Observability struct{}

const sessionsSQL = `SELECT
  pid::text AS session_id,
  COALESCE(usename, '') AS username,
  COALESCE(state, '') AS state,
  COALESCE(application_name, '') AS application_name,
  COALESCE(backend_type, '') AS backend_type,
  COALESCE(client_addr::text, '') AS client_address,
  COALESCE(wait_event_type, '') AS wait_class,
  COALESCE(wait_event, '') AS wait_event,
  COALESCE(to_char(query_start AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS query_started_at,
  COALESCE(to_char(xact_start AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS transaction_started_at,
  CASE WHEN state <> 'active' OR query_start IS NULL THEN 0 ELSE EXTRACT(EPOCH FROM (clock_timestamp() - query_start))::bigint END AS duration_seconds,
  CASE WHEN xact_start IS NULL THEN 0 ELSE EXTRACT(EPOCH FROM (clock_timestamp() - xact_start))::bigint END AS transaction_seconds
FROM pg_catalog.pg_stat_activity
WHERE pid <> pg_backend_pid()
ORDER BY query_start NULLS LAST, pid
LIMIT 10000`

func (Observability) Sessions(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.Session, error) {
	rows, err := q.SystemQuery(ctx, p.ID, sessionsSQL)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]observability.Session, 0, len(rows))
	for _, row := range rows {
		id := observability.Text(row, "session_id")
		user := observability.Text(row, "username")
		state := observability.Text(row, "state")
		app := observability.Text(row, "application_name")
		protected, reason := observability.ProtectSession("postgres", user, state, app+" "+observability.Text(row, "backend_type"))
		out = append(out, observability.Session{
			ProfileID: p.ID, Engine: "postgres", SessionKey: id, SessionID: id,
			User: user, State: state, Application: app,
			Client: observability.Text(row, "client_address"), WaitClass: observability.Text(row, "wait_class"),
			WaitEvent: observability.Text(row, "wait_event"), QueryStartedAt: observability.Text(row, "query_started_at"),
			TransactionStartedAt: observability.Text(row, "transaction_started_at"),
			DurationSeconds:      observability.Int(row, "duration_seconds"), TransactionSeconds: observability.Int(row, "transaction_seconds"),
			Protected: protected, ProtectionReason: reason, CollectedAt: now,
		})
	}
	observability.SortSessions(out)
	return out, nil
}
