package mysql

import (
	"context"
	"fmt"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Backup implements observability.BackupProvider. MySQL/MariaDB report the
// binary-log (PITR basis) state; XtraBackup/mysqldump job status lives
// outside the server and is reported as a limitation.
type Backup struct{}

const logBinSQL = `SELECT @@log_bin AS log_bin`

// SHOW BINARY LOG STATUS replaced SHOW MASTER STATUS in MySQL 8.4; older
// servers only accept the legacy form. MariaDB still uses the legacy form.
var mysqlBinlogStatusQueries = []string{"SHOW BINARY LOG STATUS", "SHOW MASTER STATUS"}

func (Backup) Backup(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.BackupData, error) {
	return CollectBackup(ctx, q, p, "mysql", mysqlBinlogStatusQueries)
}

// CollectBackup runs the MySQL-family backup observation labeled with the
// given engine name. statusQueries is the binlog-status query preference
// order.
func CollectBackup(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, engine string, statusQueries []string) (observability.BackupData, error) {
	now := time.Now().UTC()
	data := observability.BackupData{ProfileID: p.ID, Engine: engine, Archiving: "unknown", ArchivingKind: "binlog", Items: []observability.BackupItem{}}
	rows, err := q.SystemQuery(ctx, p.ID, logBinSQL)
	if err != nil {
		return data, fmt.Errorf("collect %s binary log setting: %w", engine, err)
	}
	if len(rows) > 0 {
		if observability.Int(rows[0], "log_bin") > 0 {
			data.Archiving = "enabled"
		} else {
			data.Archiving = "disabled"
		}
	}
	if data.Archiving == "enabled" {
		if statusRows, statusErr := replicaStatusRows(ctx, q, p, statusQueries); statusErr != nil {
			data.Warnings = append(data.Warnings, "바이너리 로그 상태 조회 실패: "+statusErr.Error())
		} else if len(statusRows) > 0 {
			row := statusRows[0]
			data.Items = append(data.Items, observability.BackupItem{
				Name: observability.Text(row, "File"), Kind: "binlog",
				Status:  "position " + observability.Text(row, "Position"),
				Healthy: true, CollectedAt: now,
			})
		}
	}
	data.Limitations = append(data.Limitations, "XtraBackup·mysqldump 등 외부 백업 도구의 잡 상태는 DB 서버 SQL로 확인할 수 없습니다 — 백업 도구 연동이 필요합니다.")
	return data, nil
}
