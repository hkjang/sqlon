package mysql

import (
	"context"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Observability implements observability.Provider (sessions + locks).
type Observability struct{}

const sessionsSQL = `SELECT
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

func (Observability) Sessions(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.Session, error) {
	return CollectSessions(ctx, q, p, "mysql")
}

// CollectSessions runs the MySQL-family session collection labeled with the
// given engine name (mysql or mariadb).
func CollectSessions(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, engine string) ([]observability.Session, error) {
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
		protected, reason := observability.ProtectSession(engine, user, state, "")
		out = append(out, observability.Session{ProfileID: p.ID, Engine: engine, SessionKey: id, SessionID: id,
			User: user, State: state, Service: observability.Text(row, "service_name"), Client: observability.Text(row, "client_address"),
			WaitEvent: observability.Text(row, "wait_event"), TransactionStartedAt: observability.Text(row, "transaction_started_at"),
			DurationSeconds: observability.Int(row, "duration_seconds"), TransactionSeconds: observability.Int(row, "transaction_seconds"),
			Protected: protected, ProtectionReason: reason, CollectedAt: now})
	}
	observability.SortSessions(out)
	return out, nil
}
