package oracle

import (
	"context"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Observability implements observability.Provider (sessions + locks).
type Observability struct{}

// Session keys are INST_ID:SID:SERIAL# — SERIAL# distinguishes a recycled
// SID, and INST_ID scopes it to a RAC instance.
const sessionsSQL = `SELECT
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

func (Observability) Sessions(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) ([]observability.Session, error) {
	rows, err := q.SystemQuery(ctx, p.ID, sessionsSQL)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]observability.Session, 0, len(rows))
	for _, row := range rows {
		inst := observability.Text(row, "instance_id")
		id := observability.Text(row, "session_id")
		serial := observability.Text(row, "serial_no")
		user := observability.Text(row, "username")
		state := observability.Text(row, "state")
		app := observability.Text(row, "application_name")
		protected, reason := observability.ProtectSession("oracle", user, state, app)
		out = append(out, observability.Session{ProfileID: p.ID, Engine: "oracle", SessionKey: observability.SessionKey(inst, id, serial),
			InstanceID: inst, SessionID: id, Serial: serial, User: user, State: state,
			Service: observability.Text(row, "service_name"), Application: app, Client: observability.Text(row, "client_address"),
			SQLID: observability.Text(row, "sql_id"), WaitClass: observability.Text(row, "wait_class"), WaitEvent: observability.Text(row, "wait_event"),
			TransactionStartedAt: observability.Text(row, "transaction_started_at"), DurationSeconds: observability.Int(row, "duration_seconds"),
			TransactionSeconds: observability.Int(row, "transaction_seconds"), Protected: protected, ProtectionReason: reason, CollectedAt: now})
	}
	observability.SortSessions(out)
	return out, nil
}
