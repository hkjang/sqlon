package collector

import (
	"context"
	"fmt"
	"time"

	"sqlon/internal/dbconn"
)

type oracleProvider struct{}

// Base-license dynamic performance views only. No AWR/ASH/ADDM/DBA_HIST or
// advisor packages are used; dbconn.SystemQuery enforces license policy again.
const oracleWorkloadSQL = `SELECT
  SUM(CASE WHEN name='user commits' THEN value ELSE 0 END) AS commits,
  SUM(CASE WHEN name='user rollbacks' THEN value ELSE 0 END) AS rollbacks,
  SUM(CASE WHEN name='execute count' THEN value ELSE 0 END) AS queries,
  SUM(CASE WHEN name='parse count (total)' THEN value ELSE 0 END) AS parses,
  SUM(CASE WHEN name='physical reads' THEN value ELSE 0 END) AS physical_reads,
  SUM(CASE WHEN name='physical writes' THEN value ELSE 0 END) AS physical_writes,
  SUM(CASE WHEN name='redo size' THEN value ELSE 0 END) AS redo_bytes,
  SUM(CASE WHEN name='sorts (disk)' THEN value ELSE 0 END) AS disk_sorts
FROM v$sysstat`

const oracleConnectionsSQL = `SELECT COUNT(*) AS active_connections,
  SUM(CASE WHEN status='ACTIVE' AND type='USER' THEN 1 ELSE 0 END) AS running_connections
FROM gv$session WHERE type='USER'`

const oracleWaitsSQL = `SELECT wait_class, event AS wait_event,
  total_waits AS wait_count, time_waited_micro/1000 AS time_ms
FROM v$system_event WHERE wait_class <> 'Idle' AND total_waits > 0
ORDER BY time_waited_micro DESC FETCH FIRST 50 ROWS ONLY`

const oracleTopSQLSQL = `SELECT sql_id AS fingerprint, TO_CHAR(plan_hash_value) AS plan_hash,
  SUM(executions) AS calls, SUM(elapsed_time)/1000 AS elapsed_ms,
  SUM(cpu_time)/1000 AS cpu_ms, SUM(disk_reads) AS reads,
  SUM(rows_processed) AS rows
FROM gv$sql WHERE sql_id IS NOT NULL
GROUP BY sql_id, plan_hash_value ORDER BY SUM(elapsed_time) DESC
FETCH FIRST 20 ROWS ONLY`

const oracleCapacitySQL = `SELECT 'tablespace' AS scope, m.tablespace_name AS name,
  m.used_space * t.block_size AS used_bytes,
  m.tablespace_size * t.block_size AS allocated_bytes,
  m.tablespace_size * t.block_size AS max_bytes,
  m.used_percent AS usage_percent
FROM dba_tablespace_usage_metrics m
JOIN dba_tablespaces t ON t.tablespace_name=m.tablespace_name
ORDER BY m.used_percent DESC FETCH FIRST 100 ROWS ONLY`

func (oracleProvider) Collect(ctx context.Context, q SystemQueryer, p dbconn.Profile) (Snapshot, error) {
	now := time.Now().UTC()
	s := Snapshot{ProfileID: p.ID, Engine: "oracle", Counters: []Metric{}, Rates: map[string]float64{}, Waits: []Wait{}, TopSQL: []SQLStat{}, Capacity: []Capacity{}, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now}
	rows, err := q.SystemQuery(ctx, p.ID, oracleWorkloadSQL)
	if err != nil {
		return s, fmt.Errorf("collect Oracle workload counters: %w", err)
	}
	if len(rows) == 0 {
		return s, fmt.Errorf("collect Oracle workload counters: V$SYSSTAT returned no row")
	}
	r := rows[0]
	for _, spec := range [][2]string{{"queries", "count"}, {"commits", "count"}, {"rollbacks", "count"}, {"parses", "count"}, {"physical_reads", "blocks"}, {"physical_writes", "blocks"}, {"redo_bytes", "bytes"}, {"disk_sorts", "count"}} {
		s.Counters = append(s.Counters, metric(r, spec[0], spec[1]))
	}
	if rows, connectionErr := q.SystemQuery(ctx, p.ID, oracleConnectionsSQL); connectionErr != nil {
		s.Warnings = append(s.Warnings, "Oracle 세션 카운터 수집 실패: "+connectionErr.Error())
	} else if len(rows) > 0 {
		s.Counters = append(s.Counters,
			Metric{Name: "active_connections", Value: number(rows[0], "active_connections"), Unit: "connections"},
			Metric{Name: "running_connections", Value: number(rows[0], "running_connections"), Unit: "connections"})
	}
	if rows, waitErr := q.SystemQuery(ctx, p.ID, oracleWaitsSQL); waitErr != nil {
		s.Warnings = append(s.Warnings, "Oracle 대기 이벤트 수집 실패: "+waitErr.Error())
	} else {
		for _, row := range rows {
			s.Waits = append(s.Waits, Wait{Class: text(row, "wait_class"), Event: text(row, "wait_event"), Count: number(row, "wait_count"), TimeMS: number(row, "time_ms")})
		}
	}
	if rows, topErr := q.SystemQuery(ctx, p.ID, oracleTopSQLSQL); topErr != nil {
		s.Limitations = append(s.Limitations, "GV$SQL 조회 권한이 없어 Top SQL을 수집하지 못했습니다.")
	} else {
		for _, row := range rows {
			s.TopSQL = append(s.TopSQL, SQLStat{Fingerprint: text(row, "fingerprint"), PlanHash: text(row, "plan_hash"), Calls: number(row, "calls"), ElapsedMS: number(row, "elapsed_ms"), CPUMS: number(row, "cpu_ms"), Reads: number(row, "reads"), Rows: number(row, "rows")})
		}
	}
	collectCapacity(ctx, q, p.ID, oracleCapacitySQL, &s)
	return s, nil
}
