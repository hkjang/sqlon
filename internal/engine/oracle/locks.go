package oracle

import (
	"context"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

const locksSQL = `SELECT
  TO_CHAR(NVL(s.blocking_instance, s.inst_id)) || ':' || TO_CHAR(s.blocking_session) || ':' || TO_CHAR(NVL(bs.serial#, 0)) AS blocker_key,
  TO_CHAR(s.inst_id) || ':' || TO_CHAR(s.sid) || ':' || TO_CHAR(s.serial#) AS blocked_key,
  NVL(bs.username, '') AS blocker_user,
  NVL(s.username, '') AS blocked_user,
  NVL(s.event, 'Oracle lock') AS lock_type,
  s.seconds_in_wait AS wait_seconds,
  NVL(s.sql_id, '') AS blocked_sql_id
FROM gv$session s
LEFT JOIN gv$session bs ON bs.inst_id = s.blocking_instance AND bs.sid = s.blocking_session
WHERE s.blocking_session IS NOT NULL
ORDER BY s.seconds_in_wait DESC, s.inst_id, s.sid
FETCH FIRST 10000 ROWS ONLY`

func (Observability) Locks(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.LockEdge, error) {
	rows, err := q.SystemQuery(ctx, p.ID, locksSQL)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]observability.LockEdge, 0, len(rows))
	for _, row := range rows {
		out = append(out, observability.LockEdge{ProfileID: p.ID, Engine: "oracle",
			BlockerKey: observability.Text(row, "blocker_key"), BlockedKey: observability.Text(row, "blocked_key"),
			BlockerUser: observability.Text(row, "blocker_user"), BlockedUser: observability.Text(row, "blocked_user"),
			LockType: observability.Text(row, "lock_type"), WaitSeconds: observability.Int(row, "wait_seconds"),
			BlockedSQLID: observability.Text(row, "blocked_sql_id"), CollectedAt: now})
	}
	return out, nil
}
