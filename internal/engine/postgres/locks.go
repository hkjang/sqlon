package postgres

import (
	"context"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

const locksSQL = `SELECT
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

func (Observability) Locks(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.LockEdge, error) {
	rows, err := q.SystemQuery(ctx, p.ID, locksSQL)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]observability.LockEdge, 0, len(rows))
	for _, row := range rows {
		out = append(out, observability.LockEdge{ProfileID: p.ID, Engine: "postgres",
			BlockerKey: observability.Text(row, "blocker_key"), BlockedKey: observability.Text(row, "blocked_key"),
			BlockerUser: observability.Text(row, "blocker_user"), BlockedUser: observability.Text(row, "blocked_user"),
			LockType: observability.Text(row, "lock_type"), WaitSeconds: observability.Int(row, "wait_seconds"), CollectedAt: now})
	}
	return out, nil
}
