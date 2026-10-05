package mysql

import (
	"context"
	"strings"
	"testing"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

type storageQueryer struct {
	logBin      string
	grants      []string
	binlogs     []float64
	tablespaces []map[string]any
	queries     []string
}

func (q *storageQueryer) SystemQuery(_ context.Context, _ string, query string, _ ...any) ([]map[string]any, error) {
	q.queries = append(q.queries, query)
	switch {
	case query == serverProbeSQL:
		return []map[string]any{{"log_bin": q.logBin}}, nil
	case query == "SHOW GRANTS":
		var rows []map[string]any
		for _, g := range q.grants {
			rows = append(rows, map[string]any{"Grants for mon@%": []byte(g)})
		}
		return rows, nil
	case query == schemaBytesSQL:
		return []map[string]any{{"databases_bytes": float64(50 << 30)}}, nil
	case query == tablespacesSQL:
		return q.tablespaces, nil
	case query == "SHOW BINARY LOGS":
		var rows []map[string]any
		for _, size := range q.binlogs {
			rows = append(rows, map[string]any{"Log_name": "binlog", "File_size": size})
		}
		return rows, nil
	}
	panic("unexpected " + query)
}

func (q *storageQueryer) ran(query string) bool {
	for _, x := range q.queries {
		if x == query {
			return true
		}
	}
	return false
}

func scopes(s collector.Snapshot) map[string]float64 {
	out := map[string]float64{}
	for _, c := range s.Capacity {
		out[c.Scope+":"+c.Name] = c.UsedBytes
	}
	return out
}

func collect(q *storageQueryer, engine string, existing ...collector.Capacity) collector.Snapshot {
	s := collector.NewSnapshot("m", engine)
	s.Capacity = append(s.Capacity, existing...)
	collectStorageFootprint(context.Background(), q, dbconn.Profile{ID: "m"}, &s, engine)
	return s
}

func TestMySQLFootprintUsesLiveTablespacesWithProcess(t *testing.T) {
	q := &storageQueryer{logBin: "1", grants: []string{"GRANT SELECT, PROCESS, REPLICATION CLIENT ON *.* TO `mon`@`%`"}, binlogs: []float64{1 << 30, 3 << 30},
		tablespaces: []map[string]any{
			{"name": "app/events", "bytes": float64(100 << 30), "kind": "Single"},
			{"name": "app/ledger#p#p2025", "bytes": float64(10 << 30), "kind": "Single"},
			{"name": "app/ledger#p#p2026", "bytes": float64(5 << 30), "kind": "Single"},
			{"name": "innodb_undo_001", "bytes": float64(2 << 30), "kind": "Undo"},
		}}
	s := collect(q, "mysql", collector.Capacity{Scope: "table", Name: "app.events", UsedBytes: 0}) // a day-old TABLES row
	got := scopes(s)
	if got["cluster:databases"] != float64(117<<30) || got["wal:binlog"] != float64(4<<30) || got["storage:footprint"] != float64(121<<30) {
		t.Fatalf("footprint = InnoDB files + binlogs: %v", got)
	}
	if got["table:app.events"] != float64(100<<30) || got["table:app.ledger"] != float64(15<<30) {
		t.Fatalf("stale table rows must be replaced by live sizes, partitions aggregated: %v", got)
	}
	if q.ran(schemaBytesSQL) || len(s.Limitations) != 0 {
		t.Fatalf("with PROCESS the cached TABLES sizes are not needed: %v %v", q.queries, s.Limitations)
	}
}

func TestMySQLFootprintWithoutGrantsNeverRunsPrivilegedStatements(t *testing.T) {
	for name, grants := range map[string][]string{
		"no grant":            {"GRANT SELECT ON *.* TO `mon`@`%`"},
		"schema-level only":   {"GRANT ALL PRIVILEGES ON `app`.* TO `mon`@`%`"},
		"role (not resolved)": {"GRANT USAGE ON *.* TO `mon`@`%`", "GRANT `monitor_role`@`%` TO `mon`@`%`"},
	} {
		q := &storageQueryer{logBin: "1", grants: grants}
		s := collect(q, "mysql")
		if q.ran("SHOW BINARY LOGS") || q.ran(tablespacesSQL) {
			t.Fatalf("%s: a statement the account may not run would trip the circuit breaker: %v", name, q.queries)
		}
		joined := strings.Join(s.Limitations, " ")
		if !strings.Contains(joined, "BINLOG MONITOR") || !strings.Contains(joined, "GRANT PROCESS") || len(s.Evidence) != 2 {
			t.Fatalf("%s: both gaps must be explained: %v", name, s.Limitations)
		}
		if scopes(s)["storage:footprint"] != float64(50<<30) {
			t.Fatalf("%s: falls back to TABLES sizes: %v", name, scopes(s))
		}
	}
}

func TestMariaDBUsesLiveTablesStatistics(t *testing.T) {
	q := &storageQueryer{logBin: "ON", grants: []string{"GRANT SELECT, PROCESS, BINLOG MONITOR ON *.* TO `mon`@`%`"}, binlogs: []float64{1 << 30}}
	s := collect(q, "mariadb")
	if q.ran(tablespacesSQL) || len(s.Limitations) != 0 || scopes(s)["storage:footprint"] != float64(51<<30) {
		t.Fatalf("MariaDB: TABLES is live, binlogs counted, nothing missing: %v %v %v", q.queries, s.Limitations, scopes(s))
	}
}

func TestMySQLFootprintSkipsBinlogsWhenLoggingIsOff(t *testing.T) {
	q := &storageQueryer{logBin: "0", grants: []string{"GRANT ALL PRIVILEGES ON *.* TO `root`@`%`"}, tablespaces: []map[string]any{{"name": "app/t", "bytes": float64(1 << 30), "kind": "Single"}}}
	s := collect(q, "mysql")
	if q.ran("SHOW BINARY LOGS") || len(s.Limitations) != 0 || scopes(s)["storage:footprint"] != float64(1<<30) {
		t.Fatalf("log_bin=0: no binlog query and nothing missing: %v %v", q.queries, s.Limitations)
	}
}

func TestGrantParsing(t *testing.T) {
	for g, want := range map[string]grants{
		"GRANT ALL PRIVILEGES ON *.* TO `root`@`%` WITH GRANT OPTION": {binlogs: true, process: true},
		"GRANT BINLOG MONITOR ON *.* TO `mon`@`%`":                    {binlogs: true},
		"GRANT PROCESS ON *.* TO `mon`@`%`":                           {process: true},
		"GRANT SUPER ON *.* TO `x`@`h`":                               {binlogs: true},
	} {
		if got := readGrants([]map[string]any{{"g": g}}); got != want {
			t.Fatalf("%s: got %+v want %+v", g, got, want)
		}
	}
}

func TestTablespaceNames(t *testing.T) {
	for in, want := range map[string]string{
		"app/events":              "app.events",
		"app/ledger#p#p2025":      "app.ledger",
		"app/order@002ditems":     "app.order-items",
		"my@002dshop/t#P#p0#SP#s": "my-shop.t",
	} {
		if got := tableName(in); got != want {
			t.Fatalf("tableName(%q) = %q, want %q", in, got, want)
		}
	}
}
