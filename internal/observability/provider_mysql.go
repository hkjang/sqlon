package observability

import (
	"context"

	"sqlon/internal/dbconn"
)

type mysqlProvider struct{}

const mysqlSessionsSQL = `SELECT
  CAST(p.ID AS CHAR) AS session_id,
  COALESCE(p.USER, '') AS username,
  COALESCE(p.COMMAND, '') AS state,
  COALESCE(p.DB, '') AS service_name,
  COALESCE(p.HOST, '') AS client_address,
  COALESCE(p.STATE, '') AS wait_event,
  COALESCE(DATE_FORMAT(trx.trx_started, '%Y-%m-%dT%H:%i:%sZ'), '') AS transaction_started_at,
  CASE WHEN p.COMMAND = 'Query' THEN p.TIME ELSE 0 END AS duration_seconds,
  CASE WHEN trx.trx_started IS NULL THEN 0 ELSE TIMESTAMPDIFF(SECOND, trx.trx_started, UTC_TIMESTAMP()) END AS transaction_seconds
FROM information_schema.PROCESSLIST AS p
LEFT JOIN information_schema.INNODB_TRX AS trx ON trx.trx_mysql_thread_id = p.ID
WHERE p.ID <> CONNECTION_ID()
ORDER BY duration_seconds DESC, p.ID
LIMIT 10000`

const mysqlLocksSQL = `SELECT
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

func (mysqlProvider) Sessions(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]Session, error) {
	rows, err := q.SystemQuery(ctx, p.ID, mysqlSessionsSQL)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]Session, 0, len(rows))
	for _, row := range rows {
		id := textValue(row, "session_id")
		user := textValue(row, "username")
		state := textValue(row, "state")
		protected, reason := protectSession("mysql", user, state, "")
		out = append(out, Session{ProfileID: p.ID, Engine: "mysql", SessionKey: id, SessionID: id,
			User: user, State: state, Service: textValue(row, "service_name"), Client: textValue(row, "client_address"),
			WaitEvent: textValue(row, "wait_event"), TransactionStartedAt: textValue(row, "transaction_started_at"),
			DurationSeconds: intValue(row, "duration_seconds"), TransactionSeconds: intValue(row, "transaction_seconds"),
			Protected: protected, ProtectionReason: reason, CollectedAt: now})
	}
	sortSessions(out)
	return out, nil
}

func (mysqlProvider) Locks(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]LockEdge, error) {
	return collectMySQLLocks(ctx, q, p, "mysql", mysqlLocksSQL)
}

func collectMySQLLocks(ctx context.Context, q SystemQueryer, p dbconn.Profile, engine, query string) ([]LockEdge, error) {
	rows, err := q.SystemQuery(ctx, p.ID, query)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]LockEdge, 0, len(rows))
	for _, row := range rows {
		out = append(out, LockEdge{ProfileID: p.ID, Engine: engine,
			BlockerKey: textValue(row, "blocker_key"), BlockedKey: textValue(row, "blocked_key"),
			BlockerUser: textValue(row, "blocker_user"), BlockedUser: textValue(row, "blocked_user"),
			LockType: textValue(row, "lock_type"), WaitSeconds: intValue(row, "wait_seconds"), CollectedAt: now})
	}
	return out, nil
}
