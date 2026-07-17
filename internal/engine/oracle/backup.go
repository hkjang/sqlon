package oracle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Backup implements observability.BackupProvider using base-license views:
// V$DATABASE (log mode), V$RMAN_BACKUP_JOB_DETAILS (controlfile RMAN
// history), V$RECOVERY_FILE_DEST (FRA usage). No Diagnostics/Tuning Pack
// features are involved.
type Backup struct{}

const logModeSQL = `SELECT log_mode FROM v$database`

const rmanJobsSQL = `SELECT status, input_type,
  TO_CHAR(start_time, 'YYYY-MM-DD"T"HH24:MI:SS') AS started_at,
  TO_CHAR(end_time, 'YYYY-MM-DD"T"HH24:MI:SS') AS ended_at
FROM v$rman_backup_job_details
ORDER BY end_time DESC FETCH FIRST 5 ROWS ONLY`

const fraSQL = `SELECT NVL(name, '') AS name, space_limit, space_used, space_reclaimable
FROM v$recovery_file_dest`

func (Backup) Backup(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.BackupData, error) {
	now := time.Now().UTC()
	data := observability.BackupData{ProfileID: p.ID, Engine: "oracle", Archiving: "unknown", ArchivingKind: "archivelog", Items: []observability.BackupItem{}}
	rows, err := q.SystemQuery(ctx, p.ID, logModeSQL)
	if err != nil {
		return data, fmt.Errorf("collect Oracle log mode: %w", err)
	}
	if len(rows) > 0 {
		if strings.EqualFold(observability.Text(rows[0], "log_mode"), "ARCHIVELOG") {
			data.Archiving = "enabled"
		} else {
			data.Archiving = "disabled"
		}
	}

	if jobRows, jobErr := q.SystemQuery(ctx, p.ID, rmanJobsSQL); jobErr != nil {
		data.Warnings = append(data.Warnings, "V$RMAN_BACKUP_JOB_DETAILS 조회 실패: "+jobErr.Error())
	} else {
		for _, row := range jobRows {
			status := observability.Text(row, "status")
			healthy := strings.HasPrefix(strings.ToUpper(status), "COMPLETED")
			item := observability.BackupItem{
				Name: "rman:" + observability.Text(row, "input_type"), Kind: "rman_job",
				Status: status, OccurredAt: observability.Text(row, "ended_at"),
				Detail:  "started " + observability.Text(row, "started_at"),
				Healthy: healthy, CollectedAt: now,
			}
			if !healthy {
				item.Error = "RMAN 백업 작업이 " + status + " 상태로 종료되었습니다"
			}
			if healthy && data.LastSuccessAt == "" {
				data.LastSuccessAt = item.OccurredAt
			}
			if !healthy && data.LastFailureAt == "" {
				data.LastFailureAt = item.OccurredAt
			}
			data.Items = append(data.Items, item)
		}
		if len(jobRows) == 0 {
			data.Limitations = append(data.Limitations, "controlfile에 RMAN 백업 이력이 없습니다 — RMAN 외 도구를 사용 중이면 별도 연동이 필요합니다.")
		}
	}

	if fraRows, fraErr := q.SystemQuery(ctx, p.ID, fraSQL); fraErr != nil {
		data.Warnings = append(data.Warnings, "V$RECOVERY_FILE_DEST 조회 실패: "+fraErr.Error())
	} else if len(fraRows) > 0 && observability.Number(fraRows[0], "space_limit") > 0 {
		row := fraRows[0]
		limit := observability.Number(row, "space_limit")
		used := observability.Number(row, "space_used") - observability.Number(row, "space_reclaimable")
		percent := used / limit * 100
		item := observability.BackupItem{
			Name: observability.Text(row, "name"), Kind: "fra",
			Status:  fmt.Sprintf("%.1f%% 사용 (회수 가능 제외)", percent),
			Healthy: percent < 90, CollectedAt: now,
		}
		if !item.Healthy {
			item.Error = "Fast Recovery Area 사용률이 90%를 초과했습니다 — 아카이브 중단 위험"
		}
		data.Items = append(data.Items, item)
	}
	return data, nil
}
