// Package mysql holds the MySQL engine adapter. MariaDB shares most of this
// implementation through the exported Collect* functions; behavior that
// diverges (e.g. lock views) lives in the mariadb package.
package mysql

import (
	"context"
	"fmt"
	"strings"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

// Workload implements collector.Provider.
type Workload struct{}

const workloadSQL = `SELECT
  MAX(CASE WHEN VARIABLE_NAME='Questions' THEN VARIABLE_VALUE+0 END) AS queries,
  MAX(CASE WHEN VARIABLE_NAME='Com_commit' THEN VARIABLE_VALUE+0 END) AS commits,
  MAX(CASE WHEN VARIABLE_NAME='Com_rollback' THEN VARIABLE_VALUE+0 END) AS rollbacks,
  MAX(CASE WHEN VARIABLE_NAME='Connections' THEN VARIABLE_VALUE+0 END) AS connections_total,
  MAX(CASE WHEN VARIABLE_NAME='Threads_connected' THEN VARIABLE_VALUE+0 END) AS active_connections,
  MAX(CASE WHEN VARIABLE_NAME='Threads_running' THEN VARIABLE_VALUE+0 END) AS running_connections,
  MAX(CASE WHEN VARIABLE_NAME='Created_tmp_disk_tables' THEN VARIABLE_VALUE+0 END) AS temp_disk_tables,
  MAX(CASE WHEN VARIABLE_NAME='Innodb_buffer_pool_reads' THEN VARIABLE_VALUE+0 END) AS physical_reads,
  MAX(CASE WHEN VARIABLE_NAME='Innodb_buffer_pool_read_requests' THEN VARIABLE_VALUE+0 END) AS buffer_read_requests
FROM performance_schema.global_status`

const waitsSQL = `SELECT EVENT_NAME AS wait_event, COUNT_STAR+0 AS wait_count,
  SUM_TIMER_WAIT/1000000000 AS time_ms
FROM performance_schema.events_waits_summary_global_by_event_name
WHERE COUNT_STAR > 0 ORDER BY SUM_TIMER_WAIT DESC LIMIT 50`

const topSQLSQL = `SELECT DIGEST AS fingerprint, COUNT_STAR+0 AS calls,
  SUM_TIMER_WAIT/1000000000 AS elapsed_ms,
  SUM_ROWS_EXAMINED+0 AS reads, SUM_ROWS_SENT+0 AS rows
FROM performance_schema.events_statements_summary_by_digest
WHERE DIGEST IS NOT NULL ORDER BY SUM_TIMER_WAIT DESC LIMIT 20`

const capacitySQL = `SELECT 'database' AS scope, DATABASE() AS name,
  SUM(COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)) AS used_bytes,
  SUM(COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)+COALESCE(DATA_FREE,0)) AS allocated_bytes
FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()
UNION ALL
SELECT 'table' AS scope, CONCAT(TABLE_SCHEMA,'.',TABLE_NAME) AS name,
  COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0) AS used_bytes,
  COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)+COALESCE(DATA_FREE,0) AS allocated_bytes
FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_TYPE='BASE TABLE'
ORDER BY used_bytes DESC LIMIT 101`

func (Workload) Collect(ctx context.Context, q collector.SystemQueryer, p dbconn.Profile) (collector.Snapshot, error) {
	return CollectWorkload(ctx, q, p, "mysql")
}

// CollectWorkload runs the MySQL-family workload collection labeled with the
// given engine name (mysql or mariadb).
func CollectWorkload(ctx context.Context, q collector.SystemQueryer, p dbconn.Profile, engine string) (collector.Snapshot, error) {
	s := collector.NewSnapshot(p.ID, engine)
	rows, err := q.SystemQuery(ctx, p.ID, workloadSQL)
	if err != nil {
		return s, fmt.Errorf("collect %s workload counters: %w", engine, err)
	}
	if len(rows) == 0 {
		return s, fmt.Errorf("collect %s workload counters: performance_schema.global_status returned no row", engine)
	}
	r := rows[0]
	for _, spec := range [][2]string{{"queries", "count"}, {"commits", "count"}, {"rollbacks", "count"}, {"connections_total", "count"}, {"temp_disk_tables", "count"}, {"physical_reads", "pages"}, {"buffer_read_requests", "pages"}} {
		s.Counters = append(s.Counters, collector.CumulativeMetric(r, spec[0], spec[1]))
	}
	for _, name := range []string{"active_connections", "running_connections"} {
		s.Counters = append(s.Counters, collector.Metric{Name: name, Value: collector.Number(r, name), Unit: "connections"})
	}
	if rows, waitErr := q.SystemQuery(ctx, p.ID, waitsSQL); waitErr != nil {
		s.Warnings = append(s.Warnings, "Performance Schema 대기 수집 실패: "+waitErr.Error())
	} else {
		for _, row := range rows {
			s.Waits = append(s.Waits, collector.Wait{Class: waitClass(collector.Text(row, "wait_event")), Event: collector.Text(row, "wait_event"), Count: collector.Number(row, "wait_count"), TimeMS: collector.Number(row, "time_ms")})
		}
	}
	if rows, topErr := q.SystemQuery(ctx, p.ID, topSQLSQL); topErr != nil {
		s.Limitations = append(s.Limitations, "Performance Schema statement digest가 비활성 또는 권한 부족으로 Top SQL을 수집하지 못했습니다.")
	} else {
		for _, row := range rows {
			s.TopSQL = append(s.TopSQL, collector.SQLStat{Fingerprint: collector.Text(row, "fingerprint"), Calls: collector.Number(row, "calls"), ElapsedMS: collector.Number(row, "elapsed_ms"), Reads: collector.Number(row, "reads"), Rows: collector.Number(row, "rows")})
		}
	}
	collector.CollectCapacity(ctx, q, p.ID, capacitySQL, &s)
	return s, nil
}

// waitClass maps a Performance Schema event name onto SQLON's coarse wait
// classes.
func waitClass(event string) string {
	lower := strings.ToLower(event)
	for _, class := range []string{"io", "lock", "network", "sync", "cpu"} {
		if strings.Contains(lower, class) {
			return class
		}
	}
	return "other"
}
