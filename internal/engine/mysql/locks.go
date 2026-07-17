package mysql

import (
	"context"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

const locksSQL = `SELECT
  CAST(bt.PROCESSLIST_ID AS CHAR) AS blocker_key,
  CAST(rt.PROCESSLIST_ID AS CHAR) AS blocked_key,
  COALESCE(bp.USER, '') AS blocker_user,
  COALESCE(rp.USER, '') AS blocked_user,
  COALESCE(rdl.OBJECT_NAME, rdl.LOCK_TYPE, 'InnoDB') AS lock_type,
  COALESCE(rp.TIME, 0) AS wait_seconds
FROM performance_schema.data_lock_waits AS w
JOIN performance_schema.threads AS rt ON rt.THREAD_ID = w.REQUESTING_THREAD_ID
JOIN performance_schema.threads AS bt ON bt.THREAD_ID = w.BLOCKING_THREAD_ID
LEFT JOIN performance_schema.data_locks AS rdl ON rdl.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
LEFT JOIN information_schema.PROCESSLIST AS rp ON rp.ID = rt.PROCESSLIST_ID
LEFT JOIN information_schema.PROCESSLIST AS bp ON bp.ID = bt.PROCESSLIST_ID
WHERE rt.PROCESSLIST_ID IS NOT NULL AND bt.PROCESSLIST_ID IS NOT NULL
ORDER BY wait_seconds DESC, blocked_key
LIMIT 10000`

func (Observability) Locks(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.LockEdge, error) {
	return CollectLocks(ctx, q, p, "mysql", locksSQL)
}

// CollectLocks turns a MySQL-family lock-wait query result into lock edges.
// MariaDB passes its own query because its InnoDB lock views differ.
func CollectLocks(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, engine, query string) ([]observability.LockEdge, error) {
	rows, err := q.SystemQuery(ctx, p.ID, query)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]observability.LockEdge, 0, len(rows))
	for _, row := range rows {
		out = append(out, observability.LockEdge{ProfileID: p.ID, Engine: engine,
			BlockerKey: observability.Text(row, "blocker_key"), BlockedKey: observability.Text(row, "blocked_key"),
			BlockerUser: observability.Text(row, "blocker_user"), BlockedUser: observability.Text(row, "blocked_user"),
			LockType: observability.Text(row, "lock_type"), WaitSeconds: observability.Int(row, "wait_seconds"), CollectedAt: now})
	}
	return out, nil
}
