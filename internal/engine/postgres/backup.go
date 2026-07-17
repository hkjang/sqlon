package postgres

import (
	"context"
	"fmt"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Backup implements observability.BackupProvider. PostgreSQL itself only
// exposes WAL-archiving health; external backup tools (pgBackRest, Barman)
// are reported as a limitation, never guessed.
type Backup struct{}

const archiveModeSQL = `SELECT setting FROM pg_catalog.pg_settings WHERE name = 'archive_mode'`

const archiverSQL = `SELECT
  archived_count, failed_count,
  COALESCE(last_archived_wal, '') AS last_archived_wal,
  COALESCE(to_char(last_archived_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS last_archived_at,
  COALESCE(last_failed_wal, '') AS last_failed_wal,
  COALESCE(to_char(last_failed_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS last_failed_at
FROM pg_catalog.pg_stat_archiver`

func (Backup) Backup(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.BackupData, error) {
	now := time.Now().UTC()
	data := observability.BackupData{ProfileID: p.ID, Engine: "postgres", Archiving: "unknown", ArchivingKind: "wal_archive", Items: []observability.BackupItem{}}
	rows, err := q.SystemQuery(ctx, p.ID, archiveModeSQL)
	if err != nil {
		return data, fmt.Errorf("collect PostgreSQL archive_mode: %w", err)
	}
	mode := ""
	if len(rows) > 0 {
		mode = observability.Text(rows[0], "setting")
	}
	if mode == "on" || mode == "always" {
		data.Archiving = "enabled"
	} else if mode != "" {
		data.Archiving = "disabled"
	}

	if archRows, archErr := q.SystemQuery(ctx, p.ID, archiverSQL); archErr != nil {
		data.Warnings = append(data.Warnings, "pg_stat_archiver 조회 실패: "+archErr.Error())
	} else if len(archRows) > 0 && data.Archiving == "enabled" {
		row := archRows[0]
		lastOK := observability.Text(row, "last_archived_at")
		lastFail := observability.Text(row, "last_failed_at")
		data.LastSuccessAt, data.LastFailureAt = lastOK, lastFail
		// String comparison is valid: both timestamps are fixed-width ISO-8601.
		healthy := lastFail == "" || (lastOK != "" && lastOK >= lastFail)
		item := observability.BackupItem{
			Name: "wal_archiver", Kind: "wal_archiver",
			Status:     fmt.Sprintf("archived=%d failed=%d", observability.Int(row, "archived_count"), observability.Int(row, "failed_count")),
			Detail:     observability.Text(row, "last_archived_wal"),
			OccurredAt: lastOK, Healthy: healthy, CollectedAt: now,
		}
		if !healthy {
			item.Error = "마지막 WAL 아카이브 시도가 실패했습니다 (" + observability.Text(row, "last_failed_wal") + ")"
		}
		data.Items = append(data.Items, item)
	}
	data.Limitations = append(data.Limitations, "pgBackRest·Barman 등 외부 백업 도구의 잡 상태는 DB 서버 SQL로 확인할 수 없습니다 — 백업 도구 연동이 필요합니다.")
	return data, nil
}
