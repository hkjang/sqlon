package oracle

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Replication implements observability.ReplicationProvider using base-license
// Data Guard views only (V$DATABASE, V$DATAGUARD_STATS, V$ARCHIVE_DEST_STATUS
// are not Diagnostics/Tuning Pack features).
type Replication struct{}

const databaseRoleSQL = `SELECT database_role, open_mode, protection_mode, switchover_status FROM v$database`

const dataguardStatsSQL = `SELECT name, NVL(value, '') AS value, NVL(unit, '') AS unit
FROM v$dataguard_stats WHERE name IN ('transport lag', 'apply lag')`

const archiveDestSQL = `SELECT dest_name AS name, NVL(destination, '') AS target,
  NVL(status, '') AS status, NVL(error, '') AS error
FROM v$archive_dest_status
WHERE status NOT IN ('INACTIVE', 'DEFERRED') AND type <> 'LOCAL'`

func (Replication) Replication(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.ReplicationData, error) {
	now := time.Now().UTC()
	data := observability.ReplicationData{ProfileID: p.ID, Engine: "oracle", Role: "unknown", Nodes: []observability.ReplicationNode{}}
	rows, err := q.SystemQuery(ctx, p.ID, databaseRoleSQL)
	if err != nil {
		return data, fmt.Errorf("collect Oracle database role: %w", err)
	}
	if len(rows) == 0 {
		return data, fmt.Errorf("collect Oracle database role: V$DATABASE returned no row")
	}
	role := observability.Text(rows[0], "database_role")
	data.Details = map[string]string{
		"database_role":     role,
		"open_mode":         observability.Text(rows[0], "open_mode"),
		"protection_mode":   observability.Text(rows[0], "protection_mode"),
		"switchover_status": observability.Text(rows[0], "switchover_status"),
	}
	switch {
	case strings.EqualFold(role, "PRIMARY"):
		data.Role = "primary"
	case strings.Contains(strings.ToUpper(role), "STANDBY"):
		data.Role = "standby"
	default:
		data.Role = strings.ToLower(role)
	}

	// Standby: transport/apply lag from V$DATAGUARD_STATS.
	if data.Role == "standby" {
		if lagRows, lagErr := q.SystemQuery(ctx, p.ID, dataguardStatsSQL); lagErr != nil {
			data.Warnings = append(data.Warnings, "V$DATAGUARD_STATS 조회 실패: "+lagErr.Error())
		} else {
			for _, row := range lagRows {
				name := observability.Text(row, "name")
				value := observability.Text(row, "value")
				seconds, parsed := parseDataGuardInterval(value)
				node := observability.ReplicationNode{
					Name: name, Kind: "dataguard_lag", State: value,
					LagSeconds: observability.LagUnknown, Healthy: true, CollectedAt: now,
				}
				if parsed {
					node.LagSeconds = seconds
				}
				data.Nodes = append(data.Nodes, node)
			}
			if len(lagRows) == 0 {
				data.Limitations = append(data.Limitations, "V$DATAGUARD_STATS가 비어 있습니다 — managed recovery가 실행 중인지 확인하세요.")
			}
		}
	}

	// Both roles: remote archive destinations carry transport health.
	if destRows, destErr := q.SystemQuery(ctx, p.ID, archiveDestSQL); destErr != nil {
		data.Warnings = append(data.Warnings, "V$ARCHIVE_DEST_STATUS 조회 실패: "+destErr.Error())
	} else {
		for _, row := range destRows {
			status := observability.Text(row, "status")
			errorText := observability.Text(row, "error")
			data.Nodes = append(data.Nodes, observability.ReplicationNode{
				Name: observability.Text(row, "name"), Kind: "archive_dest",
				Target: observability.Text(row, "target"), State: status,
				LagSeconds:  observability.LagUnknown,
				Error:       errorText,
				Healthy:     strings.EqualFold(status, "VALID") && errorText == "",
				CollectedAt: now,
			})
		}
		if data.Role == "primary" && len(destRows) == 0 && len(data.Nodes) == 0 {
			data.Role = "standalone"
		}
	}
	return data, nil
}

// parseDataGuardInterval parses V$DATAGUARD_STATS interval values of the form
// "+DD HH:MM:SS" (day-to-second interval) into seconds.
func parseDataGuardInterval(value string) (float64, bool) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "+"))
	if trimmed == "" {
		return 0, false
	}
	parts := strings.Fields(trimmed)
	if len(parts) != 2 {
		return 0, false
	}
	days, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false
	}
	clock := strings.Split(parts[1], ":")
	if len(clock) != 3 {
		return 0, false
	}
	hours, err1 := strconv.Atoi(clock[0])
	minutes, err2 := strconv.Atoi(clock[1])
	seconds, err3 := strconv.ParseFloat(clock[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return float64(days*86400+hours*3600+minutes*60) + seconds, true
}
