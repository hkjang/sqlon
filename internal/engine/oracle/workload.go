// Package oracle holds the Oracle engine adapter. Every query here uses
// base-license dynamic performance views only — no AWR/ASH/ADDM/DBA_HIST or
// advisor packages; dbconn.SystemQuery enforces the profile license policy
// again at execution time.
package oracle

import (
	"context"
	"fmt"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

// Workload implements collector.Provider.
type Workload struct{}

const workloadSQL = `SELECT
  SUM(CASE WHEN name='user commits' THEN value ELSE 0 END) AS commits,
  SUM(CASE WHEN name='user rollbacks' THEN value ELSE 0 END) AS rollbacks,
  SUM(CASE WHEN name='execute count' THEN value ELSE 0 END) AS queries,
  SUM(CASE WHEN name='parse count (total)' THEN value ELSE 0 END) AS parses,
  SUM(CASE WHEN name='physical reads' THEN value ELSE 0 END) AS physical_reads,
  SUM(CASE WHEN name='physical writes' THEN value ELSE 0 END) AS physical_writes,
  SUM(CASE WHEN name='redo size' THEN value ELSE 0 END) AS redo_bytes,
  SUM(CASE WHEN name='sorts (disk)' THEN value ELSE 0 END) AS disk_sorts
FROM v$sysstat`

const connectionsSQL = `SELECT COUNT(*) AS active_connections,
  SUM(CASE WHEN status='ACTIVE' AND type='USER' THEN 1 ELSE 0 END) AS running_connections
FROM gv$session WHERE type='USER'`

const waitsSQL = `SELECT wait_class, event AS wait_event,
  total_waits AS wait_count, time_waited_micro/1000 AS time_ms
FROM v$system_event WHERE wait_class <> 'Idle' AND total_waits > 0
ORDER BY time_waited_micro DESC FETCH FIRST 50 ROWS ONLY`

const topSQLSQL = `SELECT sql_id AS fingerprint, TO_CHAR(plan_hash_value) AS plan_hash,
  SUM(executions) AS calls, SUM(elapsed_time)/1000 AS elapsed_ms,
  SUM(cpu_time)/1000 AS cpu_ms, SUM(disk_reads) AS reads,
  SUM(rows_processed) AS rows
FROM gv$sql WHERE sql_id IS NOT NULL
GROUP BY sql_id, plan_hash_value ORDER BY SUM(elapsed_time) DESC
FETCH FIRST 20 ROWS ONLY`

const capacitySQL = `SELECT 'tablespace' AS scope, m.tablespace_name AS name,
  m.used_space * t.block_size AS used_bytes,
  m.tablespace_size * t.block_size AS allocated_bytes,
  m.tablespace_size * t.block_size AS max_bytes,
  m.used_percent AS usage_percent
FROM dba_tablespace_usage_metrics m
JOIN dba_tablespaces t ON t.tablespace_name=m.tablespace_name
ORDER BY m.used_percent DESC FETCH FIRST 100 ROWS ONLY`

func (Workload) Collect(ctx context.Context, q collector.SystemQueryer, p dbconn.Profile) (collector.Snapshot, error) {
	s := collector.NewSnapshot(p.ID, "oracle")
	rows, err := q.SystemQuery(ctx, p.ID, workloadSQL)
	if err != nil {
		return s, fmt.Errorf("collect Oracle workload counters: %w", err)
	}
	if len(rows) == 0 {
		return s, fmt.Errorf("collect Oracle workload counters: V$SYSSTAT returned no row")
	}
	r := rows[0]
	for _, spec := range [][2]string{{"queries", "count"}, {"commits", "count"}, {"rollbacks", "count"}, {"parses", "count"}, {"physical_reads", "blocks"}, {"physical_writes", "blocks"}, {"redo_bytes", "bytes"}, {"disk_sorts", "count"}} {
		s.Counters = append(s.Counters, collector.CumulativeMetric(r, spec[0], spec[1]))
	}
	if rows, connectionErr := q.SystemQuery(ctx, p.ID, connectionsSQL); connectionErr != nil {
		s.Warnings = append(s.Warnings, "Oracle 세션 카운터 수집 실패: "+connectionErr.Error())
	} else if len(rows) > 0 {
		s.Counters = append(s.Counters,
			collector.Metric{Name: "active_connections", Value: collector.Number(rows[0], "active_connections"), Unit: "connections"},
			collector.Metric{Name: "running_connections", Value: collector.Number(rows[0], "running_connections"), Unit: "connections"})
	}
	if rows, waitErr := q.SystemQuery(ctx, p.ID, waitsSQL); waitErr != nil {
		s.Warnings = append(s.Warnings, "Oracle 대기 이벤트 수집 실패: "+waitErr.Error())
	} else {
		for _, row := range rows {
			s.Waits = append(s.Waits, collector.Wait{Class: collector.Text(row, "wait_class"), Event: collector.Text(row, "wait_event"), Count: collector.Number(row, "wait_count"), TimeMS: collector.Number(row, "time_ms")})
		}
	}
	if rows, topErr := q.SystemQuery(ctx, p.ID, topSQLSQL); topErr != nil {
		s.Limitations = append(s.Limitations, "GV$SQL 조회 권한이 없어 Top SQL을 수집하지 못했습니다.")
	} else {
		for _, row := range rows {
			s.TopSQL = append(s.TopSQL, collector.SQLStat{Fingerprint: collector.Text(row, "fingerprint"), PlanHash: collector.Text(row, "plan_hash"), Calls: collector.Number(row, "calls"), ElapsedMS: collector.Number(row, "elapsed_ms"), CPUMS: collector.Number(row, "cpu_ms"), Reads: collector.Number(row, "reads"), Rows: collector.Number(row, "rows")})
		}
	}
	collector.CollectCapacity(ctx, q, p.ID, capacitySQL, &s)
	return s, nil
}
