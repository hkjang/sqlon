package observability

import (
	"context"

	"sqlon/internal/dbconn"
)

type postgresProvider struct{}

const postgresSessionsSQL = `SELECT
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

const postgresLocksSQL = `SELECT
  blocker_pid::text AS blocker_key,
  blocked.pid::text AS blocked_key,
  COALESCE(blocker.usename, '') AS blocker_user,
  COALESCE(blocked.usename, '') AS blocked_user,
  COALESCE(blocked.wait_event_type, 'Lock') AS lock_type,
  CASE WHEN blocked.query_start IS NULL THEN 0 ELSE EXTRACT(EPOCH FROM (clock_timestamp() - blocked.query_start))::bigint END AS wait_seconds
FROM pg_catalog.pg_stat_activity AS blocked
CROSS JOIN LATERAL unnest(pg_catalog.pg_blocking_pids(blocked.pid)) AS blocker_pid
LEFT JOIN pg_catalog.pg_stat_activity AS blocker ON blocker.pid = blocker_pid
ORDER BY wait_seconds DESC, blocked.pid
LIMIT 10000`

func (postgresProvider) Sessions(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]Session, error) {
	rows, err := q.SystemQuery(ctx, p.ID, postgresSessionsSQL)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]Session, 0, len(rows))
	for _, row := range rows {
		id := textValue(row, "session_id")
		user := textValue(row, "username")
		state := textValue(row, "state")
		app := textValue(row, "application_name")
		protected, reason := protectSession("postgres", user, state, app+" "+textValue(row, "backend_type"))
		out = append(out, Session{
			ProfileID: p.ID, Engine: "postgres", SessionKey: id, SessionID: id,
			User: user, State: state, Application: app,
			Client: textValue(row, "client_address"), WaitClass: textValue(row, "wait_class"),
			WaitEvent: textValue(row, "wait_event"), QueryStartedAt: textValue(row, "query_started_at"),
			TransactionStartedAt: textValue(row, "transaction_started_at"),
			DurationSeconds:      intValue(row, "duration_seconds"), TransactionSeconds: intValue(row, "transaction_seconds"),
			Protected: protected, ProtectionReason: reason, CollectedAt: now,
		})
	}
	sortSessions(out)
	return out, nil
}

func (postgresProvider) Locks(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]LockEdge, error) {
	rows, err := q.SystemQuery(ctx, p.ID, postgresLocksSQL)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]LockEdge, 0, len(rows))
	for _, row := range rows {
		out = append(out, LockEdge{ProfileID: p.ID, Engine: "postgres",
			BlockerKey: textValue(row, "blocker_key"), BlockedKey: textValue(row, "blocked_key"),
			BlockerUser: textValue(row, "blocker_user"), BlockedUser: textValue(row, "blocked_user"),
			LockType: textValue(row, "lock_type"), WaitSeconds: intValue(row, "wait_seconds"), CollectedAt: now})
	}
	return out, nil
}
