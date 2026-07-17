package collector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/dbconn"
)

type mysqlProvider struct{}

const mysqlWorkloadSQL = `SELECT
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

const mysqlWaitsSQL = `SELECT EVENT_NAME AS wait_event, COUNT_STAR+0 AS wait_count,
  SUM_TIMER_WAIT/1000000000 AS time_ms
FROM performance_schema.events_waits_summary_global_by_event_name
WHERE COUNT_STAR > 0 ORDER BY SUM_TIMER_WAIT DESC LIMIT 50`

const mysqlTopSQLSQL = `SELECT DIGEST AS fingerprint, COUNT_STAR+0 AS calls,
  SUM_TIMER_WAIT/1000000000 AS elapsed_ms,
  SUM_ROWS_EXAMINED+0 AS reads, SUM_ROWS_SENT+0 AS rows
FROM performance_schema.events_statements_summary_by_digest
WHERE DIGEST IS NOT NULL ORDER BY SUM_TIMER_WAIT DESC LIMIT 20`

const mysqlCapacitySQL = `SELECT 'database' AS scope, DATABASE() AS name,
  SUM(COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)) AS used_bytes,
  SUM(COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)+COALESCE(DATA_FREE,0)) AS allocated_bytes
FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()
UNION ALL
SELECT 'table' AS scope, CONCAT(TABLE_SCHEMA,'.',TABLE_NAME) AS name,
  COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0) AS used_bytes,
  COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)+COALESCE(DATA_FREE,0) AS allocated_bytes
FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_TYPE='BASE TABLE'
ORDER BY used_bytes DESC LIMIT 101`

func (mysqlProvider) Collect(ctx context.Context, q SystemQueryer, p dbconn.Profile) (Snapshot, error) {
	return collectMySQL(ctx, q, p, "mysql")
}

func collectMySQL(ctx context.Context, q SystemQueryer, p dbconn.Profile, engine string) (Snapshot, error) {
	now := time.Now().UTC()
	s := Snapshot{ProfileID: p.ID, Engine: engine, Counters: []Metric{}, Rates: map[string]float64{}, Waits: []Wait{}, TopSQL: []SQLStat{}, Capacity: []Capacity{}, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now}
	rows, err := q.SystemQuery(ctx, p.ID, mysqlWorkloadSQL)
	if err != nil {
		return s, fmt.Errorf("collect %s workload counters: %w", engine, err)
	}
	if len(rows) == 0 {
		return s, fmt.Errorf("collect %s workload counters: performance_schema.global_status returned no row", engine)
	}
	r := rows[0]
	for _, spec := range [][2]string{{"queries", "count"}, {"commits", "count"}, {"rollbacks", "count"}, {"connections_total", "count"}, {"temp_disk_tables", "count"}, {"physical_reads", "pages"}, {"buffer_read_requests", "pages"}} {
		s.Counters = append(s.Counters, metric(r, spec[0], spec[1]))
	}
	for _, name := range []string{"active_connections", "running_connections"} {
		s.Counters = append(s.Counters, Metric{Name: name, Value: number(r, name), Unit: "connections"})
	}
	if rows, waitErr := q.SystemQuery(ctx, p.ID, mysqlWaitsSQL); waitErr != nil {
		s.Warnings = append(s.Warnings, "Performance Schema 대기 수집 실패: "+waitErr.Error())
	} else {
		for _, row := range rows {
			s.Waits = append(s.Waits, Wait{Class: mysqlWaitClass(text(row, "wait_event")), Event: text(row, "wait_event"), Count: number(row, "wait_count"), TimeMS: number(row, "time_ms")})
		}
	}
	if rows, topErr := q.SystemQuery(ctx, p.ID, mysqlTopSQLSQL); topErr != nil {
		s.Limitations = append(s.Limitations, "Performance Schema statement digest가 비활성 또는 권한 부족으로 Top SQL을 수집하지 못했습니다.")
	} else {
		for _, row := range rows {
			s.TopSQL = append(s.TopSQL, SQLStat{Fingerprint: text(row, "fingerprint"), Calls: number(row, "calls"), ElapsedMS: number(row, "elapsed_ms"), Reads: number(row, "reads"), Rows: number(row, "rows")})
		}
	}
	collectCapacity(ctx, q, p.ID, mysqlCapacitySQL, &s)
	return s, nil
}

func mysqlWaitClass(event string) string {
	for _, class := range []string{"io", "lock", "network", "sync", "cpu"} {
		if containsFold(event, class) {
			return class
		}
	}
	return "other"
}

func containsFold(value, part string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(part))
}
