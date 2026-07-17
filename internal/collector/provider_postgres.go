package collector

import (
	"context"
	"fmt"
	"time"

	"sqlon/internal/dbconn"
)

type postgresProvider struct{}

const pgWorkloadSQL = `SELECT
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

const pgWaitsSQL = `SELECT COALESCE(wait_event_type, 'CPU') AS wait_class,
  COALESCE(wait_event, '') AS wait_event, COUNT(*)::double precision AS wait_count
FROM pg_catalog.pg_stat_activity
WHERE state = 'active' AND pid <> pg_backend_pid()
GROUP BY wait_event_type, wait_event ORDER BY wait_count DESC LIMIT 50`

const pgTopSQLSQL = `SELECT queryid::text AS fingerprint, calls::double precision AS calls,
  total_exec_time::double precision AS elapsed_ms,
  mean_exec_time::double precision AS mean_ms,
  (shared_blks_read + local_blks_read)::double precision AS reads,
  rows::double precision AS rows
FROM public.pg_stat_statements
ORDER BY total_exec_time DESC LIMIT 20`

const pgCapacitySQL = `SELECT 'database' AS scope, current_database() AS name,
  pg_database_size(current_database())::double precision AS used_bytes,
  pg_database_size(current_database())::double precision AS allocated_bytes
UNION ALL
SELECT 'table' AS scope, n.nspname || '.' || c.relname AS name,
  pg_total_relation_size(c.oid)::double precision AS used_bytes,
  pg_total_relation_size(c.oid)::double precision AS allocated_bytes
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','m') AND n.nspname NOT IN ('pg_catalog','information_schema')
ORDER BY used_bytes DESC LIMIT 101`

func (postgresProvider) Collect(ctx context.Context, q SystemQueryer, p dbconn.Profile) (Snapshot, error) {
	now := time.Now().UTC()
	s := Snapshot{ProfileID: p.ID, Engine: "postgres", Counters: []Metric{}, Rates: map[string]float64{}, Waits: []Wait{}, TopSQL: []SQLStat{}, Capacity: []Capacity{}, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now}
	rows, err := q.SystemQuery(ctx, p.ID, pgWorkloadSQL)
	if err != nil {
		return s, fmt.Errorf("collect PostgreSQL workload counters: %w", err)
	}
	if len(rows) == 0 {
		return s, fmt.Errorf("collect PostgreSQL workload counters: pg_stat_database returned no row")
	}
	r := rows[0]
	for _, spec := range [][2]string{{"transactions", "count"}, {"commits", "count"}, {"rollbacks", "count"}, {"physical_reads", "blocks"}, {"buffer_hits", "blocks"}, {"rows_returned", "rows"}, {"rows_fetched", "rows"}, {"rows_inserted", "rows"}, {"rows_updated", "rows"}, {"rows_deleted", "rows"}, {"deadlocks", "count"}, {"temp_bytes", "bytes"}} {
		s.Counters = append(s.Counters, metric(r, spec[0], spec[1]))
	}
	s.Counters = append(s.Counters, Metric{Name: "active_connections", Value: number(r, "active_connections"), Unit: "connections"})

	if rows, waitErr := q.SystemQuery(ctx, p.ID, pgWaitsSQL); waitErr != nil {
		s.Warnings = append(s.Warnings, "대기 이벤트 수집 실패: "+waitErr.Error())
	} else {
		for _, row := range rows {
			s.Waits = append(s.Waits, Wait{Class: text(row, "wait_class"), Event: text(row, "wait_event"), Count: number(row, "wait_count")})
		}
	}
	if rows, topErr := q.SystemQuery(ctx, p.ID, pgTopSQLSQL); topErr != nil {
		s.Limitations = append(s.Limitations, "pg_stat_statements가 없거나 조회 권한이 없어 Top SQL을 수집하지 못했습니다.")
	} else {
		for _, row := range rows {
			s.TopSQL = append(s.TopSQL, SQLStat{Fingerprint: text(row, "fingerprint"), Calls: number(row, "calls"), ElapsedMS: number(row, "elapsed_ms"), Reads: number(row, "reads"), Rows: number(row, "rows")})
		}
	}
	collectCapacity(ctx, q, p.ID, pgCapacitySQL, &s)
	return s, nil
}

func collectCapacity(ctx context.Context, q SystemQueryer, profileID, query string, snapshot *Snapshot) {
	rows, err := q.SystemQuery(ctx, profileID, query)
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, "용량 수집 실패: "+err.Error())
		return
	}
	for _, row := range rows {
		snapshot.Capacity = append(snapshot.Capacity, Capacity{Scope: text(row, "scope"), Name: text(row, "name"), UsedBytes: number(row, "used_bytes"), AllocatedBytes: number(row, "allocated_bytes"), MaxBytes: number(row, "max_bytes"), UsagePercent: number(row, "usage_percent")})
	}
}
