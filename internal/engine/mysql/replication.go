package mysql

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

// SHOW REPLICA/SLAVE STATUS is read-only (the pool session is read-only via
// DSN) and is the only complete replica-status interface on MySQL/MariaDB —
// performance_schema lacks lag on older versions and MariaDB entirely.
// Queries are tried in order: first the modern keyword, then the legacy one
// for servers that predate it.
var mysqlReplicaStatusQueries = []string{"SHOW REPLICA STATUS", "SHOW SLAVE STATUS"}

const binlogDumpSQL = `SELECT COUNT(*) AS replica_connections
FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%'`

func (Replication) Replication(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.ReplicationData, error) {
	return CollectReplication(ctx, q, p, "mysql", mysqlReplicaStatusQueries)
}

// CollectReplication runs the MySQL-family replication collection labeled
// with the given engine name. statusQueries is the replica-status query
// preference order (MariaDB passes SHOW ALL SLAVES STATUS first).
func CollectReplication(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, engine string, statusQueries []string) (observability.ReplicationData, error) {
	now := time.Now().UTC()
	data := observability.ReplicationData{ProfileID: p.ID, Engine: engine, Role: "unknown", Nodes: []observability.ReplicationNode{}}

	rows, err := replicaStatusRows(ctx, q, p, statusQueries)
	if err != nil {
		return data, fmt.Errorf("collect %s replica status: %w", engine, err)
	}
	for _, row := range rows {
		channel := observability.Text(row, "Channel_Name", "Connection_name")
		if channel == "" {
			channel = "default"
		}
		io := observability.Text(row, "Replica_IO_Running", "Slave_IO_Running")
		sqlThread := observability.Text(row, "Replica_SQL_Running", "Slave_SQL_Running")
		node := observability.ReplicationNode{
			Name: channel, Kind: "channel",
			Target:      observability.SessionKey(observability.Text(row, "Source_Host", "Master_Host"), observability.Text(row, "Source_Port", "Master_Port")),
			State:       "IO:" + io + " SQL:" + sqlThread,
			LagSeconds:  observability.LagUnknown,
			Healthy:     strings.EqualFold(io, "Yes") && strings.EqualFold(sqlThread, "Yes"),
			CollectedAt: now,
		}
		// NULL Seconds_Behind means unmeasurable (e.g. IO thread stopped) —
		// keep LagUnknown instead of reporting a false zero.
		if lag := rowValueText(row, "Seconds_Behind_Source", "Seconds_Behind_Master"); lag != "" && !strings.EqualFold(lag, "null") {
			node.LagSeconds = observability.Number(row, "Seconds_Behind_Source", "Seconds_Behind_Master")
		}
		for _, errorColumn := range []string{"Last_IO_Error", "Last_SQL_Error", "Last_Error"} {
			if message := observability.Text(row, errorColumn); message != "" {
				node.Error = message
				break
			}
		}
		data.Nodes = append(data.Nodes, node)
	}

	replicaConnections := int64(0)
	if countRows, countErr := q.SystemQuery(ctx, p.ID, binlogDumpSQL); countErr != nil {
		data.Warnings = append(data.Warnings, "복제 연결 스레드 조회 실패: "+countErr.Error())
	} else if len(countRows) > 0 {
		replicaConnections = observability.Int(countRows[0], "replica_connections")
		if replicaConnections > 0 {
			data.Nodes = append(data.Nodes, observability.ReplicationNode{
				Name: "binlog_dump", Kind: "primary_feed",
				State:      fmt.Sprintf("%d replica connection(s)", replicaConnections),
				LagSeconds: observability.LagUnknown, Healthy: true, CollectedAt: now,
			})
		}
	}

	switch {
	case len(rows) > 0:
		data.Role = "replica"
	case replicaConnections > 0:
		data.Role = "primary"
	default:
		data.Role = "standalone"
	}
	return data, nil
}

// replicaStatusRows tries each status query in order. An empty result set is
// a valid answer (not a replica); only when every candidate errors is the
// last error returned.
func replicaStatusRows(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, queries []string) ([]map[string]any, error) {
	var lastErr error
	for _, query := range queries {
		rows, err := q.SystemQuery(ctx, p.ID, query)
		if err == nil {
			return rows, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// rowValueText reads the raw column as text without trimming semantics —
// used to distinguish NULL from 0 before a numeric conversion.
func rowValueText(row map[string]any, names ...string) string {
	for _, name := range names {
		for key, value := range row {
			if strings.EqualFold(key, name) {
				if value == nil {
					return ""
				}
				return strings.TrimSpace(fmt.Sprint(value))
			}
		}
	}
	return ""
}
