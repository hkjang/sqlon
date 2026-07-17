package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

type scriptedQueryer struct {
	answer func(query string) ([]map[string]any, error)
}

func (s scriptedQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	return s.answer(query)
}

func TestReplicationNullLagStaysUnknownNotZero(t *testing.T) {
	q := scriptedQueryer{answer: func(query string) ([]map[string]any, error) {
		if strings.HasPrefix(strings.ToUpper(query), "SHOW REPLICA STATUS") {
			return []map[string]any{{"Replica_IO_Running": "No", "Replica_SQL_Running": "Yes", "Seconds_Behind_Source": nil, "Last_IO_Error": "connection refused"}}, nil
		}
		return []map[string]any{{"replica_connections": 0}}, nil
	}}
	data, err := (Replication{}).Replication(context.Background(), q, dbconn.Profile{ID: "p", Type: "mysql"})
	if err != nil {
		t.Fatal(err)
	}
	node := data.Nodes[0]
	if node.LagSeconds != observability.LagUnknown {
		t.Fatalf("NULL Seconds_Behind reported as %v — must stay LagUnknown", node.LagSeconds)
	}
	if node.Healthy || node.Error != "connection refused" {
		t.Fatalf("broken IO thread not surfaced: %+v", node)
	}
}

func TestReplicationFallsBackToLegacyStatusQuery(t *testing.T) {
	var seen []string
	q := scriptedQueryer{answer: func(query string) ([]map[string]any, error) {
		seen = append(seen, query)
		upper := strings.ToUpper(query)
		switch {
		case strings.HasPrefix(upper, "SHOW REPLICA STATUS"):
			return nil, errors.New("syntax error near 'REPLICA'")
		case strings.HasPrefix(upper, "SHOW SLAVE STATUS"):
			return []map[string]any{{"Slave_IO_Running": "Yes", "Slave_SQL_Running": "Yes", "Seconds_Behind_Master": 3, "Master_Host": "src"}}, nil
		}
		return []map[string]any{{"replica_connections": 0}}, nil
	}}
	data, err := (Replication{}).Replication(context.Background(), q, dbconn.Profile{ID: "p", Type: "mysql"})
	if err != nil || data.Role != "replica" || data.Nodes[0].LagSeconds != 3 {
		t.Fatalf("legacy fallback failed: data=%+v err=%v (queries=%v)", data, err, seen)
	}
}

func TestReplicationStandaloneWhenNoChannelsOrDumpThreads(t *testing.T) {
	q := scriptedQueryer{answer: func(query string) ([]map[string]any, error) {
		if strings.Contains(strings.ToUpper(query), "PROCESSLIST") {
			return []map[string]any{{"replica_connections": 0}}, nil
		}
		return []map[string]any{}, nil
	}}
	data, err := (Replication{}).Replication(context.Background(), q, dbconn.Profile{ID: "p", Type: "mysql"})
	if err != nil || data.Role != "standalone" || len(data.Nodes) != 0 {
		t.Fatalf("standalone detection failed: %+v err=%v", data, err)
	}
}
