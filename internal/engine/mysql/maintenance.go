package mysql

import (
	"context"
	"fmt"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Maintenance implements observability.MaintenanceProvider for MySQL.
type Maintenance struct{}

func (Maintenance) Maintenance(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.MaintenanceData, error) {
	return CollectMaintenance(ctx, q, p, "mysql")
}

// InnoDB history list length: a large value means the purge thread is behind,
// usually because a long-running transaction is pinning old row versions —
// undo bloat that eventually degrades everything.
const historyListSQL = `SELECT COUNT AS value
FROM information_schema.INNODB_METRICS
WHERE NAME = 'trx_rseg_history_len'`

// Tables without a primary key: unsafe for row-based replication and cause
// full-table scans on InnoDB secondary operations.
const tablesWithoutPKSQL = `SELECT t.TABLE_SCHEMA AS table_schema, t.TABLE_NAME AS table_name
FROM information_schema.TABLES t
WHERE t.TABLE_TYPE = 'BASE TABLE'
  AND t.TABLE_SCHEMA NOT IN ('mysql','information_schema','performance_schema','sys')
  AND NOT EXISTS (
    SELECT 1 FROM information_schema.TABLE_CONSTRAINTS c
    WHERE c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
      AND c.CONSTRAINT_TYPE = 'PRIMARY KEY')
ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME
LIMIT 50`

const (
	historyWarn     = 1_000_000
	historyCritical = 10_000_000
)

// CollectMaintenance runs the MySQL-family proactive-maintenance checks labeled
// for the given engine (mysql|mariadb). Each check soft-fails to a limitation
// so one unavailable view never blanks the others.
func CollectMaintenance(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, engine string) (observability.MaintenanceData, error) {
	now := time.Now().UTC()
	data := observability.MaintenanceData{ProfileID: p.ID, Engine: engine, Findings: []observability.MaintenanceFinding{}}

	// 1. InnoDB history list length (undo/purge lag)
	if rows, err := q.SystemQuery(ctx, p.ID, historyListSQL); err != nil {
		data.Limitations = append(data.Limitations, "InnoDB history list length 수집을 건너뜀: "+err.Error())
	} else {
		data.Checks++
		for _, row := range rows {
			v := observability.Number(row, "value")
			severity := ""
			switch {
			case v >= historyCritical:
				severity = "critical"
			case v >= historyWarn:
				severity = "warning"
			}
			if severity == "" {
				continue
			}
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category:       "undo_purge_lag",
				Object:         "InnoDB history list",
				Detail:         fmt.Sprintf("history list length %.0f — 퍼지 스레드가 밀려 있습니다(장기 미커밋 트랜잭션 의심)", v),
				Metric:         "history_list_length",
				Value:          v,
				Threshold:      historyWarn,
				Recommendation: "장기 실행/미커밋 트랜잭션을 찾아 정리하세요(세션 화면). 방치 시 언두 테이블스페이스가 계속 증가합니다.",
				Severity:       severity,
				CollectedAt:    now,
			})
		}
	}

	// 2. Tables without a primary key
	if rows, err := q.SystemQuery(ctx, p.ID, tablesWithoutPKSQL); err != nil {
		data.Limitations = append(data.Limitations, "PK 없는 테이블 수집을 건너뜀: "+err.Error())
	} else {
		data.Checks++
		for _, row := range rows {
			schema := observability.Text(row, "table_schema")
			table := observability.Text(row, "table_name")
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category:       "no_primary_key",
				Object:         schema + "." + table,
				Detail:         "기본키(PRIMARY KEY)가 없습니다 — row 기반 복제에서 성능 저하·정합성 위험이 있습니다",
				Metric:         "",
				Recommendation: "적절한 PRIMARY KEY를 변경계획으로 추가하세요. 자연키가 없으면 대리키(AUTO_INCREMENT)를 검토하세요.",
				Severity:       "warning",
				CollectedAt:    now,
			})
		}
	}

	return data, nil
}
