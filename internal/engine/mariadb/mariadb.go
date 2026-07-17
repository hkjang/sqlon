// Package mariadb holds the MariaDB engine adapter. It shares the MySQL
// implementation where the engines are compatible and overrides what
// diverges (InnoDB lock views).
package mariadb

import (
	"context"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/engine/mysql"
	"sqlon/internal/observability"
)

// Workload implements collector.Provider.
type Workload struct{}

func (Workload) Collect(ctx context.Context, q collector.SystemQueryer, p dbconn.Profile) (collector.Snapshot, error) {
	return mysql.CollectWorkload(ctx, q, p, "mariadb")
}

// Observability implements observability.Provider (sessions + locks).
type Observability struct{}

// MariaDB still exposes the pre-8.0 INNODB_LOCK_WAITS/INNODB_LOCKS views
// instead of performance_schema.data_lock_waits.
const locksSQL = `SELECT
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

func (Observability) Sessions(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.Session, error) {
	return mysql.CollectSessions(ctx, q, p, "mariadb")
}

func (Observability) Locks(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.LockEdge, error) {
	return mysql.CollectLocks(ctx, q, p, "mariadb", locksSQL)
}

// Replication implements observability.ReplicationProvider. MariaDB reports
// every replication connection through SHOW ALL SLAVES STATUS (multi-source);
// SHOW SLAVE STATUS is the single-source fallback.
type Replication struct{}

var replicaStatusQueries = []string{"SHOW ALL SLAVES STATUS", "SHOW SLAVE STATUS"}

func (Replication) Replication(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.ReplicationData, error) {
	return mysql.CollectReplication(ctx, q, p, "mariadb", replicaStatusQueries)
}

// Backup implements observability.BackupProvider. MariaDB keeps the legacy
// SHOW MASTER STATUS as the primary binlog-status form.
type Backup struct{}

var binlogStatusQueries = []string{"SHOW MASTER STATUS", "SHOW BINARY LOG STATUS"}

func (Backup) Backup(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.BackupData, error) {
	return mysql.CollectBackup(ctx, q, p, "mariadb", binlogStatusQueries)
}
