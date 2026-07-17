package observability

import (
	"context"

	"sqlon/internal/dbconn"
)

type mariadbProvider struct{}

const mariadbLocksSQL = `SELECT
  CAST(b.trx_mysql_thread_id AS CHAR) AS blocker_key,
  CAST(r.trx_mysql_thread_id AS CHAR) AS blocked_key,
  COALESCE(bp.USER, '') AS blocker_user,
  COALESCE(rp.USER, '') AS blocked_user,
  COALESCE(rl.lock_table, rl.lock_type, 'InnoDB') AS lock_type,
  CASE WHEN r.trx_wait_started IS NULL THEN 0 ELSE TIMESTAMPDIFF(SECOND, r.trx_wait_started, UTC_TIMESTAMP()) END AS wait_seconds
FROM information_schema.INNODB_LOCK_WAITS AS w
JOIN information_schema.INNODB_TRX AS r ON r.trx_id = w.requesting_trx_id
JOIN information_schema.INNODB_TRX AS b ON b.trx_id = w.blocking_trx_id
LEFT JOIN information_schema.INNODB_LOCKS AS rl ON rl.lock_id = w.requested_lock_id
LEFT JOIN information_schema.PROCESSLIST AS rp ON rp.ID = r.trx_mysql_thread_id
LEFT JOIN information_schema.PROCESSLIST AS bp ON bp.ID = b.trx_mysql_thread_id
ORDER BY wait_seconds DESC, blocked_key
LIMIT 10000`

func (mariadbProvider) Sessions(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]Session, error) {
	items, err := (mysqlProvider{}).Sessions(ctx, q, p)
	for i := range items {
		items[i].Engine = "mariadb"
	}
	return items, err
}

func (mariadbProvider) Locks(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]LockEdge, error) {
	return collectMySQLLocks(ctx, q, p, "mariadb", mariadbLocksSQL)
}
