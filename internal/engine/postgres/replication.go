package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Replication implements observability.ReplicationProvider.
type Replication struct{}

const roleSQL = `SELECT CASE WHEN pg_is_in_recovery() THEN 'replica' ELSE 'primary' END AS role`

const standbysSQL = `SELECT
  COALESCE(application_name, '') AS name,
  COALESCE(client_addr::text, '') AS target,
  COALESCE(state, '') AS state,
  COALESCE(sync_state, '') AS sync_state,
  COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn), 0)::double precision AS lag_bytes,
  COALESCE(EXTRACT(EPOCH FROM replay_lag), 0)::double precision AS lag_seconds
FROM pg_catalog.pg_stat_replication`

const slotsSQL = `SELECT slot_name AS name, active,
  COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn), 0)::double precision AS retained_bytes
FROM pg_catalog.pg_replication_slots`

const receiverSQL = `SELECT COALESCE(status, '') AS state,
  COALESCE(sender_host, '') AS sender_host, COALESCE(sender_port, 0) AS sender_port
FROM pg_catalog.pg_stat_wal_receiver`

const replicaLagSQL = `SELECT
  COALESCE(EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())), 0)::double precision AS lag_seconds,
  pg_is_wal_replay_paused() AS paused`

func (Replication) Replication(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.ReplicationData, error) {
	now := time.Now().UTC()
	data := observability.ReplicationData{ProfileID: p.ID, Engine: "postgres", Role: "unknown", Nodes: []observability.ReplicationNode{}}
	rows, err := q.SystemQuery(ctx, p.ID, roleSQL)
	if err != nil {
		return data, fmt.Errorf("collect PostgreSQL replication role: %w", err)
	}
	if len(rows) == 0 {
		return data, fmt.Errorf("collect PostgreSQL replication role: no row returned")
	}
	data.Role = observability.Text(rows[0], "role")

	if data.Role == "primary" {
		observed := false
		if rows, standbyErr := q.SystemQuery(ctx, p.ID, standbysSQL); standbyErr != nil {
			data.Warnings = append(data.Warnings, "pg_stat_replication 조회 실패: "+standbyErr.Error())
			observed = true // collection failed — do not misreport as standalone
		} else {
			for _, row := range rows {
				state := observability.Text(row, "state")
				data.Nodes = append(data.Nodes, observability.ReplicationNode{
					Name: observability.Text(row, "name"), Kind: "standby",
					Target: observability.Text(row, "target"), State: state,
					SyncState:  observability.Text(row, "sync_state"),
					LagSeconds: observability.Number(row, "lag_seconds"), LagBytes: observability.Number(row, "lag_bytes"),
					Healthy: state == "streaming", CollectedAt: now,
				})
			}
			observed = observed || len(rows) > 0
		}
		if rows, slotErr := q.SystemQuery(ctx, p.ID, slotsSQL); slotErr != nil {
			data.Warnings = append(data.Warnings, "pg_replication_slots 조회 실패: "+slotErr.Error())
			observed = true
		} else {
			for _, row := range rows {
				active := strings.EqualFold(observability.Text(row, "active"), "true")
				node := observability.ReplicationNode{
					Name: observability.Text(row, "name"), Kind: "slot",
					State: "inactive",
					// An inactive slot silently retains WAL until the disk
					// fills — that is a health problem, not an idle detail.
					LagSeconds: observability.LagUnknown,
					LagBytes:   observability.Number(row, "retained_bytes"),
					Healthy:    active, CollectedAt: now,
				}
				if active {
					node.State = "active"
				} else {
					node.Error = "비활성 복제 슬롯이 WAL을 보존하고 있습니다"
				}
				data.Nodes = append(data.Nodes, node)
			}
			observed = observed || len(rows) > 0
		}
		if !observed {
			data.Role = "standalone"
		}
		return data, nil
	}

	// replica: WAL receiver + applied-transaction lag
	if rows, receiverErr := q.SystemQuery(ctx, p.ID, receiverSQL); receiverErr != nil {
		data.Warnings = append(data.Warnings, "pg_stat_wal_receiver 조회 실패: "+receiverErr.Error())
	} else if len(rows) == 0 {
		data.Limitations = append(data.Limitations, "WAL receiver가 없습니다 — 아카이브 복구 또는 수신 중단 상태일 수 있습니다.")
		data.Nodes = append(data.Nodes, observability.ReplicationNode{
			Name: "wal_receiver", Kind: "receiver", State: "missing",
			LagSeconds: observability.LagUnknown, Error: "스트리밍 수신 프로세스가 없습니다", Healthy: false, CollectedAt: now,
		})
	} else {
		row := rows[0]
		state := observability.Text(row, "state")
		node := observability.ReplicationNode{
			Name: "wal_receiver", Kind: "receiver", State: state,
			Target:     observability.SessionKey(observability.Text(row, "sender_host"), observability.Text(row, "sender_port")),
			LagSeconds: observability.LagUnknown,
			Healthy:    state == "streaming", CollectedAt: now,
		}
		if lagRows, lagErr := q.SystemQuery(ctx, p.ID, replicaLagSQL); lagErr != nil {
			data.Warnings = append(data.Warnings, "복제 적용 지연 조회 실패: "+lagErr.Error())
		} else if len(lagRows) > 0 {
			node.LagSeconds = observability.Number(lagRows[0], "lag_seconds")
			if strings.EqualFold(observability.Text(lagRows[0], "paused"), "true") {
				node.Healthy = false
				node.Error = "WAL 재생이 일시 중지된 상태입니다"
			}
		}
		data.Nodes = append(data.Nodes, node)
	}
	return data, nil
}
