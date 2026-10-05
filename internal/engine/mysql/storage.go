package mysql

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

// The MySQL-family storage footprint: the data files plus the binary logs,
// the usual reason a MySQL volume fills (a long binlog_expire_logs_seconds
// under a write burst).
//
// Sizes: MySQL 8 caches information_schema.TABLES statistics for
// information_schema_stats_expiry (24h by default), so a table that grew
// 100GB this morning can still read 0 there. With PROCESS the real
// allocated file sizes come from INNODB_TABLESPACES instead, for the
// footprint and for per-table rows. MariaDB's TABLES is live.
//
// Statements that raise on a missing privilege (SHOW BINARY LOGS,
// INNODB_TABLESPACES) feed the profile's circuit breaker, so the grants are
// read first with SHOW GRANTS, which never fails for the current user.

const (
	serverProbeSQL      = `SELECT @@log_bin AS log_bin`
	schemaBytesSQL      = `SELECT COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) AS databases_bytes FROM information_schema.TABLES WHERE TABLE_SCHEMA NOT IN ('mysql', 'sys', 'performance_schema', 'information_schema')`
	tablespacesSQL      = `SELECT NAME AS name, ALLOCATED_SIZE AS bytes, SPACE_TYPE AS kind FROM information_schema.INNODB_TABLESPACES`
	maxTableRows        = 100
	binlogHint          = "binlog 크기를 보려면 모니터링 계정에 binlog 조회 권한을 주세요 (MySQL: GRANT REPLICATION CLIENT ON *.* TO <계정>; MariaDB 10.5+: GRANT BINLOG MONITOR ON *.* TO <계정>). 지금은 데이터 크기만으로 저장공간을 추정합니다."
	staleStatsHint      = "MySQL 8 은 information_schema.TABLES 의 크기를 최대 24시간(information_schema_stats_expiry) 캐시하므로 증가가 늦게 보일 수 있습니다. 실시간 InnoDB 파일 크기를 보려면 GRANT PROCESS ON *.* TO <계정>; 을 주세요."
	privilegeAllOrSuper = "ALL PRIVILEGES"
)

type grants struct{ binlogs, process bool }

// readGrants inspects global grants (ON *.*). Privileges reached only
// through a role are not detected; that errs on the side of not running a
// statement that would fail.
func readGrants(rows []map[string]any) grants {
	var g grants
	for _, row := range rows {
		for _, v := range row {
			line := strings.ToUpper(strings.TrimSpace(textOf(v)))
			if !strings.Contains(line, " ON *.* ") {
				continue
			}
			has := func(privs ...string) bool {
				for _, p := range privs {
					if strings.Contains(line, p) {
						return true
					}
				}
				return false
			}
			g.binlogs = g.binlogs || has(privilegeAllOrSuper, "REPLICATION CLIENT", "BINLOG MONITOR", "SUPER")
			g.process = g.process || has(privilegeAllOrSuper, "PROCESS")
		}
	}
	return g
}

// canListBinlogs is kept for callers that only need the binlog decision.
func canListBinlogs(rows []map[string]any) bool { return readGrants(rows).binlogs }

func textOf(v any) string {
	if b, ok := v.([]byte); ok { // raw driver value; SystemQuery normally converts
		return string(b)
	}
	return collector.Text(map[string]any{"v": v}, "v")
}

// tableName turns an InnoDB tablespace name ("app/order@002ditems#p#p0")
// into the schema.table form the TABLES-based rows use.
func tableName(space string) string {
	if i := strings.Index(space, "#"); i >= 0 { // partitions aggregate into their table
		space = space[:i]
	}
	space = strings.Replace(space, "/", ".", 1)
	var b strings.Builder
	for i := 0; i < len(space); i++ {
		if space[i] == '@' && i+4 < len(space) {
			if r, err := strconv.ParseUint(space[i+1:i+5], 16, 32); err == nil {
				b.WriteRune(rune(r))
				i += 4
				continue
			}
		}
		b.WriteByte(space[i])
	}
	return b.String()
}

func collectStorageFootprint(ctx context.Context, q collector.SystemQueryer, p dbconn.Profile, s *collector.Snapshot, engine string) {
	probe, err := q.SystemQuery(ctx, p.ID, serverProbeSQL)
	if err != nil {
		s.Warnings = append(s.Warnings, "저장공간 점유량 수집 실패: "+err.Error())
		return
	}
	logBin := len(probe) > 0 && (collector.Text(probe[0], "log_bin") == "1" || strings.EqualFold(collector.Text(probe[0], "log_bin"), "ON"))
	grantRows, _ := q.SystemQuery(ctx, p.ID, "SHOW GRANTS")
	g := readGrants(grantRows)
	var missing []string

	total := -1.0
	if engine == "mysql" && g.process {
		if rows, err := q.SystemQuery(ctx, p.ID, tablespacesSQL); err == nil {
			total = 0
			tables := map[string]float64{}
			for _, row := range rows {
				bytes := collector.Number(row, "bytes")
				total += bytes
				if strings.EqualFold(collector.Text(row, "kind"), "Single") {
					tables[tableName(collector.Text(row, "name"))] += bytes
				}
			}
			replaceTableRows(s, tables)
		} else {
			s.Warnings = append(s.Warnings, "InnoDB 테이블스페이스 크기 수집 실패: "+err.Error())
		}
	}
	if total < 0 {
		rows, err := q.SystemQuery(ctx, p.ID, schemaBytesSQL)
		if err != nil {
			s.Warnings = append(s.Warnings, "저장공간 점유량 수집 실패: "+err.Error())
			return
		}
		if len(rows) > 0 {
			total = collector.Number(rows[0], "databases_bytes")
		}
		if engine == "mysql" {
			missing = append(missing, staleStatsHint)
		}
	}
	s.Capacity = append(s.Capacity, collector.Capacity{Scope: collector.ScopeCluster, Name: "databases", UsedBytes: total, AllocatedBytes: total})

	if logBin {
		if !g.binlogs {
			missing = append(missing, binlogHint)
		} else if logs, err := q.SystemQuery(ctx, p.ID, "SHOW BINARY LOGS"); err != nil {
			s.Warnings = append(s.Warnings, "binlog 크기 수집 실패: "+err.Error())
		} else {
			binlogs := 0.0
			for _, row := range logs {
				binlogs += collector.Number(row, "File_size")
			}
			s.Capacity = append(s.Capacity, collector.Capacity{Scope: collector.ScopeWAL, Name: "binlog", UsedBytes: binlogs, AllocatedBytes: binlogs})
			total += binlogs
		}
	}
	for _, hint := range missing {
		s.Limitations = append(s.Limitations, hint)
		s.Evidence = append(s.Evidence, collector.Evidence{Code: "STORAGE_FOOTPRINT_PARTIAL", Severity: "info", Summary: hint, CollectedAt: s.CollectedAt})
	}
	s.Capacity = append(s.Capacity, collector.Capacity{Scope: collector.ScopeStorage, Name: collector.FootprintName, UsedBytes: total, AllocatedBytes: total})
}

// replaceTableRows swaps the (possibly day-old) TABLES-based table rows for
// live tablespace sizes, keeping the largest tables like capacitySQL does.
func replaceTableRows(s *collector.Snapshot, tables map[string]float64) {
	kept := s.Capacity[:0]
	for _, c := range s.Capacity {
		if c.Scope != "table" {
			kept = append(kept, c)
		}
	}
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return tables[names[i]] > tables[names[j]] })
	if len(names) > maxTableRows {
		names = names[:maxTableRows]
	}
	for _, name := range names {
		kept = append(kept, collector.Capacity{Scope: "table", Name: name, UsedBytes: tables[name], AllocatedBytes: tables[name]})
	}
	s.Capacity = kept
}
