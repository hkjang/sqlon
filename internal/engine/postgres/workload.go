// Package postgres holds the PostgreSQL engine adapter: the fixed,
// base-privilege system queries and row normalization behind SQLON's
// role-provider interfaces. Wired by internal/engine/adapters.
package postgres

import (
	"context"
	"fmt"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

// Workload implements collector.Provider.
type Workload struct{}

const workloadSQL = `SELECT
  (xact_commit + xact_rollback)::double precision AS transactions,
  xact_commit::double precision AS commits,
  xact_rollback::double precision AS rollbacks,
  blks_read::double precision AS physical_reads,
  blks_hit::double precision AS buffer_hits,
  tup_returned::double precision AS rows_returned,
  tup_fetched::double precision AS rows_fetched,
  tup_inserted::double precision AS rows_inserted,
  tup_updated::double precision AS rows_updated,
  tup_deleted::double precision AS rows_deleted,
  deadlocks::double precision AS deadlocks,
  temp_bytes::double precision AS temp_bytes,
  numbackends::double precision AS active_connections
FROM pg_catalog.pg_stat_database WHERE datname = current_database()`

const waitsSQL = `SELECT COALESCE(wait_event_type, 'CPU') AS wait_class,
  COALESCE(wait_event, '') AS wait_event, COUNT(*)::double precision AS wait_count
FROM pg_catalog.pg_stat_activity
WHERE state = 'active' AND pid <> pg_backend_pid()
GROUP BY wait_event_type, wait_event ORDER BY wait_count DESC LIMIT 50`

const topSQLSQL = `SELECT queryid::text AS fingerprint, calls::double precision AS calls,
  total_exec_time::double precision AS elapsed_ms,
  mean_exec_time::double precision AS mean_ms,
  (shared_blks_read + local_blks_read)::double precision AS reads,
  rows::double precision AS rows
FROM public.pg_stat_statements
ORDER BY total_exec_time DESC LIMIT 20`

const capacitySQL = `SELECT 'database' AS scope, current_database() AS name,
  pg_database_size(current_database())::double precision AS used_bytes,
  pg_database_size(current_database())::double precision AS allocated_bytes
UNION ALL
SELECT 'table' AS scope, n.nspname || '.' || c.relname AS name,
  pg_total_relation_size(c.oid)::double precision AS used_bytes,
  pg_total_relation_size(c.oid)::double precision AS allocated_bytes
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','m') AND n.nspname NOT IN ('pg_catalog','information_schema')
ORDER BY used_bytes DESC LIMIT 101`

func (Workload) Collect(ctx context.Context, q collector.SystemQueryer, p dbconn.Profile) (collector.Snapshot, error) {
	s := collector.NewSnapshot(p.ID, "postgres")
	rows, err := q.SystemQuery(ctx, p.ID, workloadSQL)
	if err != nil {
		return s, fmt.Errorf("collect PostgreSQL workload counters: %w", err)
	}
	if len(rows) == 0 {
		return s, fmt.Errorf("collect PostgreSQL workload counters: pg_stat_database returned no row")
	}
	r := rows[0]
	for _, spec := range [][2]string{{"transactions", "count"}, {"commits", "count"}, {"rollbacks", "count"}, {"physical_reads", "blocks"}, {"buffer_hits", "blocks"}, {"rows_returned", "rows"}, {"rows_fetched", "rows"}, {"rows_inserted", "rows"}, {"rows_updated", "rows"}, {"rows_deleted", "rows"}, {"deadlocks", "count"}, {"temp_bytes", "bytes"}} {
		s.Counters = append(s.Counters, collector.CumulativeMetric(r, spec[0], spec[1]))
	}
	s.Counters = append(s.Counters, collector.Metric{Name: "active_connections", Value: collector.Number(r, "active_connections"), Unit: "connections"})

	if rows, waitErr := q.SystemQuery(ctx, p.ID, waitsSQL); waitErr != nil {
		s.Warnings = append(s.Warnings, "대기 이벤트 수집 실패: "+waitErr.Error())
	} else {
		for _, row := range rows {
			s.Waits = append(s.Waits, collector.Wait{Class: collector.Text(row, "wait_class"), Event: collector.Text(row, "wait_event"), Count: collector.Number(row, "wait_count")})
		}
	}
	if rows, topErr := q.SystemQuery(ctx, p.ID, topSQLSQL); topErr != nil {
		s.Limitations = append(s.Limitations, "pg_stat_statements가 없거나 조회 권한이 없어 Top SQL을 수집하지 못했습니다.")
	} else {
		for _, row := range rows {
			s.TopSQL = append(s.TopSQL, collector.SQLStat{Fingerprint: collector.Text(row, "fingerprint"), Calls: collector.Number(row, "calls"), ElapsedMS: collector.Number(row, "elapsed_ms"), Reads: collector.Number(row, "reads"), Rows: collector.Number(row, "rows")})
		}
	}
	collector.CollectCapacity(ctx, q, p.ID, capacitySQL, &s)
	return s, nil
}
