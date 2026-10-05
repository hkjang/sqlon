package postgres

import (
	"context"
	"strings"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

// storageProbeSQL measures what PostgreSQL itself puts on the data volume
// that the per-database capacity query cannot see — every other database in
// the instance — and reports which directory-listing functions this role may
// execute. Probing privileges first keeps the follow-up query error-free:
// a permission error would count toward the profile's circuit breaker, and
// three in a row lock every caller out of the profile for 30 seconds.
//
// pg_database_size is guarded by CASE (not FILTER) so it is never called for
// a database this role cannot CONNECT to.
const storageProbeSQL = `SELECT
  COALESCE(SUM(CASE WHEN has_database_privilege(d.oid, 'CONNECT') OR pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER')
                    THEN pg_database_size(d.oid) ELSE 0 END), 0)::double precision AS databases_bytes,
  COUNT(*) FILTER (WHERE NOT (has_database_privilege(d.oid, 'CONNECT') OR pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER')))::bigint AS databases_skipped,
  COALESCE((SELECT string_agg(p.proname, ',')
     FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
     WHERE n.nspname = 'pg_catalog' AND p.pronargs = 0
       AND p.proname IN ('pg_ls_waldir', 'pg_ls_tmpdir', 'pg_ls_logdir', 'pg_ls_archive_statusdir')
       AND has_function_privilege(p.oid, 'EXECUTE')), '') AS listable,
  COALESCE((SELECT setting FROM pg_catalog.pg_settings WHERE name = 'logging_collector'), '') AS logging_collector
FROM pg_catalog.pg_database d`

// Directory measurements, appended only when the probe says the function
// exists and is executable. pg_ls_tmpdir tolerates a missing pgsql_tmp; the
// log directory is only listed when logging_collector created it, because
// pg_ls_logdir errors on a missing directory.
const (
	walDirPart = `(SELECT COALESCE(SUM(size), 0) FROM pg_catalog.pg_ls_waldir())::double precision AS wal_bytes`
	tmpDirPart = `(SELECT COALESCE(SUM(size), 0) FROM pg_catalog.pg_ls_tmpdir())::double precision AS temp_bytes`
	logDirPart = `(SELECT COALESCE(SUM(size), 0) FROM pg_catalog.pg_ls_logdir())::double precision AS log_bytes`
)

// MonitorPrivilegeHint is the limitation reported when the monitoring role
// cannot list the WAL directory. Exported for tests and documentation.
const MonitorPrivilegeHint = "WAL·임시파일·로그 디렉터리 크기를 보려면 모니터링 계정에 pg_monitor 역할을 부여하세요 (GRANT pg_monitor TO <계정>). 지금은 데이터베이스 크기만으로 저장공간을 추정합니다."

// collectStorageFootprint appends the instance-wide storage rows and their
// sum as the storage/footprint asset — the number that has to stay below the
// volume size. It is best-effort: failures become warnings, never errors.
func collectStorageFootprint(ctx context.Context, q collector.SystemQueryer, p dbconn.Profile, s *collector.Snapshot) {
	rows, err := q.SystemQuery(ctx, p.ID, storageProbeSQL)
	if err != nil {
		s.Warnings = append(s.Warnings, "저장공간 점유량 수집 실패: "+err.Error())
		return
	}
	if len(rows) == 0 {
		return
	}
	probe := rows[0]
	total := collector.Number(probe, "databases_bytes")
	s.Capacity = append(s.Capacity, collector.Capacity{Scope: collector.ScopeCluster, Name: "databases", UsedBytes: total, AllocatedBytes: total})
	if skipped := collector.Number(probe, "databases_skipped"); skipped > 0 {
		s.Limitations = append(s.Limitations, "CONNECT 권한이 없는 데이터베이스는 저장공간 합계에서 빠졌습니다 (pg_read_all_stats 또는 pg_monitor 역할로 해결).")
	}

	listable := map[string]bool{}
	for _, name := range strings.Split(collector.Text(probe, "listable"), ",") {
		listable[strings.TrimSpace(name)] = true
	}
	var parts []string
	if listable["pg_ls_waldir"] {
		parts = append(parts, walDirPart)
	}
	if listable["pg_ls_tmpdir"] {
		parts = append(parts, tmpDirPart)
	}
	if listable["pg_ls_logdir"] && strings.EqualFold(collector.Text(probe, "logging_collector"), "on") {
		parts = append(parts, logDirPart)
	}
	measured := []string{"databases"}
	if len(parts) > 0 {
		dirRows, dirErr := q.SystemQuery(ctx, p.ID, "SELECT "+strings.Join(parts, ",\n  "))
		if dirErr != nil {
			s.Warnings = append(s.Warnings, "WAL·임시파일 디렉터리 크기 수집 실패: "+dirErr.Error())
		} else if len(dirRows) > 0 {
			for _, spec := range []struct{ column, scope, name string }{
				{"wal_bytes", collector.ScopeWAL, "pg_wal"},
				{"temp_bytes", collector.ScopeTemp, "pgsql_tmp"},
				{"log_bytes", collector.ScopeLog, "log"},
			} {
				if _, ok := columnPresent(dirRows[0], spec.column); !ok {
					continue
				}
				v := collector.Number(dirRows[0], spec.column)
				s.Capacity = append(s.Capacity, collector.Capacity{Scope: spec.scope, Name: spec.name, UsedBytes: v, AllocatedBytes: v})
				total += v
				measured = append(measured, spec.scope)
			}
		}
	}
	if !listable["pg_ls_waldir"] {
		s.Limitations = append(s.Limitations, MonitorPrivilegeHint)
		s.Evidence = append(s.Evidence, collector.Evidence{Code: "STORAGE_FOOTPRINT_PARTIAL", Severity: "info", Summary: MonitorPrivilegeHint, Attributes: map[string]any{"measured": measured}, CollectedAt: s.CollectedAt})
	}
	s.Capacity = append(s.Capacity, collector.Capacity{Scope: collector.ScopeStorage, Name: collector.FootprintName, UsedBytes: total, AllocatedBytes: total})
}

func columnPresent(row map[string]any, name string) (any, bool) {
	for key, v := range row {
		if strings.EqualFold(key, name) {
			return v, true
		}
	}
	return nil, false
}
