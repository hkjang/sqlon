package observability

import (
	"context"

	"sqlon/internal/dbconn"
)

type oracleProvider struct{}

// These base-license queries intentionally avoid ASH, AWR, ADDM and advisor
// views. dbconn.SystemQuery additionally enforces the profile license policy.
const oracleSessionsSQL = `SELECT
  TO_CHAR(s.inst_id) AS instance_id,
  TO_CHAR(s.sid) AS session_id,
  TO_CHAR(s.serial#) AS serial_no,
  NVL(s.username, '') AS username,
  NVL(s.status, '') AS state,
  NVL(s.service_name, '') AS service_name,
  NVL(s.module, s.program) AS application_name,
  NVL(s.machine, '') AS client_address,
  NVL(s.sql_id, '') AS sql_id,
  NVL(s.wait_class, '') AS wait_class,
  NVL(s.event, '') AS wait_event,
  NVL(TO_CHAR(t.start_date, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS transaction_started_at,
  CASE WHEN s.status = 'ACTIVE' THEN s.last_call_et ELSE 0 END AS duration_seconds,
  CASE WHEN t.start_date IS NULL THEN 0 ELSE TRUNC((SYSDATE - t.start_date) * 86400) END AS transaction_seconds
FROM gv$session s
LEFT JOIN gv$transaction t ON t.inst_id = s.inst_id AND t.addr = s.taddr
WHERE s.sid <> SYS_CONTEXT('USERENV', 'SID')
ORDER BY duration_seconds DESC, s.inst_id, s.sid
FETCH FIRST 10000 ROWS ONLY`

const oracleLocksSQL = `SELECT
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

func (oracleProvider) Sessions(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]Session, error) {
	rows, err := q.SystemQuery(ctx, p.ID, oracleSessionsSQL)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]Session, 0, len(rows))
	for _, row := range rows {
		inst := textValue(row, "instance_id")
		id := textValue(row, "session_id")
		serial := textValue(row, "serial_no")
		user := textValue(row, "username")
		state := textValue(row, "state")
		app := textValue(row, "application_name")
		protected, reason := protectSession("oracle", user, state, app)
		out = append(out, Session{ProfileID: p.ID, Engine: "oracle", SessionKey: sessionKey(inst, id, serial),
			InstanceID: inst, SessionID: id, Serial: serial, User: user, State: state,
			Service: textValue(row, "service_name"), Application: app, Client: textValue(row, "client_address"),
			SQLID: textValue(row, "sql_id"), WaitClass: textValue(row, "wait_class"), WaitEvent: textValue(row, "wait_event"),
			TransactionStartedAt: textValue(row, "transaction_started_at"), DurationSeconds: intValue(row, "duration_seconds"),
			TransactionSeconds: intValue(row, "transaction_seconds"), Protected: protected, ProtectionReason: reason, CollectedAt: now})
	}
	sortSessions(out)
	return out, nil
}

func (oracleProvider) Locks(ctx context.Context, q SystemQueryer, p dbconn.Profile) ([]LockEdge, error) {
	rows, err := q.SystemQuery(ctx, p.ID, oracleLocksSQL)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]LockEdge, 0, len(rows))
	for _, row := range rows {
		out = append(out, LockEdge{ProfileID: p.ID, Engine: "oracle",
			BlockerKey: textValue(row, "blocker_key"), BlockedKey: textValue(row, "blocked_key"),
			BlockerUser: textValue(row, "blocker_user"), BlockedUser: textValue(row, "blocked_user"),
			LockType: textValue(row, "lock_type"), WaitSeconds: intValue(row, "wait_seconds"),
			BlockedSQLID: textValue(row, "blocked_sql_id"), CollectedAt: now})
	}
	return out, nil
}
