package metasync

import (
	"context"
	"testing"
	"time"
)

// fakeQuerier serves canned system-catalog rows keyed by a substring of the
// query, so collector logic is unit-testable without a live DB.
type fakeQuerier struct {
	dialect string
	rows    map[string][]map[string]any
}

func (f *fakeQuerier) ProfileDialect(_ context.Context, _ string) (string, error) {
	return f.dialect, nil
}
func (f *fakeQuerier) SystemQuery(_ context.Context, _, query string, _ ...any) ([]map[string]any, error) {
	for k, v := range f.rows {
		if containsStr(query, k) {
			return v, nil
		}
	}
	return nil, nil
}
func containsStr(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func tbl(schema, name string, cols ...ColumnAsset) TableAsset {
	t := TableAsset{Schema: schema, Name: name, Kind: "table", Columns: cols}
	t.StructHash = structHash(t)
	return t
}
func col(name, fullType string, nullable, pk bool) ColumnAsset {
	return ColumnAsset{Name: name, FullType: fullType, DataType: baseType(fullType), Nullable: nullable, IsPrimaryKey: pk, Ordinal: 1}
}
func baseType(ft string) string {
	if i := indexOf(ft, "("); i >= 0 {
		return ft[:i]
	}
	return ft
}

func snap(id string, tables ...TableAsset) *RawSnapshot {
	s := &RawSnapshot{SnapshotID: id, SourceID: "src", CollectedAt: time.Unix(0, 0), Tables: tables}
	s.SchemaHash = schemaHash(tables)
	return s
}

func TestStructHashIgnoresCommentAndRowCount(t *testing.T) {
	a := tbl("s", "t", col("id", "int", false, true))
	b := a
	b.Comment = "a business comment"
	b.EstRowCount = 999999
	b.StructHash = structHash(b)
	if a.StructHash != b.StructHash {
		t.Fatal("comment/row-count changes must not alter the structural hash")
	}
	// a real structural change must alter it
	c := tbl("s", "t", col("id", "bigint", false, true))
	if a.StructHash == c.StructHash {
		t.Fatal("type change must alter the structural hash")
	}
}

func TestOracleMetadataCollectorNormalizesBaseDictionaryViews(t *testing.T) {
	f := &fakeQuerier{dialect: "oracle", rows: map[string][]map[string]any{
		"FROM all_tables":      {{"schema": "APP", "name": "ORDERS", "kind": "table", "comment": "주문", "est_rows": int64(12)}},
		"FROM all_tab_columns": {{"schema": "APP", "name": "ORDERS", "col": "ID", "ord": int64(1), "data_type": "NUMBER", "full_type": "NUMBER(10,0)", "nullable": "N"}},
		"FROM all_constraints": {{"schema": "APP", "name": "ORDERS", "cname": "PK_ORDERS", "ctype": "PRIMARY KEY", "col": "ID", "col_ord": int64(1)}},
		"FROM all_indexes":     {{"schema": "APP", "name": "ORDERS", "iname": "PK_ORDERS", "is_unique": int64(1), "is_primary": int64(1), "col": "ID", "col_pos": int64(1)}},
	}}
	snap, err := NewCollector(f).Collect(context.Background(), CollectRequest{SourceID: "ora", Schemas: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Dialect != "oracle" || len(snap.Tables) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	table := snap.Tables[0]
	if table.FQN() != "APP.ORDERS" || len(table.Columns) != 1 || !table.Columns[0].IsPrimaryKey || len(table.Indexes) != 1 {
		t.Fatalf("oracle table = %+v", table)
	}
}

func TestDiffDetectsChanges(t *testing.T) {
	base := snap("s1",
		tbl("s", "keep", col("id", "int", false, true)),
		tbl("s", "gone", col("id", "int", false, true)),
		tbl("s", "evolve", col("id", "int", false, true), col("amt", "int", true, false)),
	)
	cur := snap("s2",
		tbl("s", "keep", col("id", "int", false, true)), // unchanged
		tbl("s", "added", col("id", "int", false, true)),
		tbl("s", "evolve",
			col("id", "bigint", false, true),        // type change
			col("name", "varchar(64)", true, false), // added column; amt removed
		),
	)
	cs := Diff(base, cur)

	got := map[ChangeKind]int{}
	for _, ch := range cs.Changes {
		got[ch.Kind]++
	}
	want := map[ChangeKind]int{
		TableAdded: 1, TableRemoved: 1, TypeChanged: 1, ColumnAdded: 1, ColumnRemoved: 1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: got %d, want %d (changes: %+v)", k, got[k], n, cs.Changes)
		}
	}
	// "keep" must NOT appear in changed tables (structural hash matched)
	for _, tname := range cs.ChangedTables {
		if tname == "s.keep" {
			t.Fatal("unchanged table must be skipped")
		}
	}
	// deletions are retire candidates, not immediate removals (AC-02)
	for _, ch := range cs.Changes {
		if ch.Kind == TableRemoved && ch.Disposition != "retire_candidate" {
			t.Fatalf("removed table must be a retire candidate, got %q", ch.Disposition)
		}
		if ch.Kind == ColumnRemoved && ch.Disposition != "retire_candidate" {
			t.Fatalf("removed column must be a retire candidate, got %q", ch.Disposition)
		}
	}
}

func TestDiffNoBaseline(t *testing.T) {
	cur := snap("s1", tbl("s", "t", col("id", "int", false, true)))
	cs := Diff(nil, cur)
	if cs.Summary[TableAdded] != 1 {
		t.Fatalf("first snapshot should report all tables as added, got %+v", cs.Summary)
	}
}

func TestCollectPostgresFromFake(t *testing.T) {
	f := &fakeQuerier{
		dialect: "postgres",
		rows: map[string][]map[string]any{
			"FROM pg_class c": {
				{"schema": "s", "name": "users", "kind": "table", "comment": "user master", "est_rows": int64(10), "view_sql": ""},
			},
			"FROM information_schema.columns c": {
				{"schema": "s", "name": "users", "col": "id", "ord": int64(1), "data_type": "integer", "full_type": "integer", "nullable": "NO", "col_default": "", "gen_expr": "", "comment": "pk"},
				{"schema": "s", "name": "users", "col": "email", "ord": int64(2), "data_type": "character varying", "full_type": "character varying(256)", "nullable": "YES", "col_default": "", "gen_expr": "", "comment": ""},
			},
			"FROM pg_constraint con": {
				{"schema": "s", "name": "users", "cname": "users_pk", "ctype": "PRIMARY KEY", "col": "id", "col_ord": int64(1), "ref_schema": "", "ref_table": "", "ref_col": "", "check_clause": ""},
			},
		},
	}
	c := NewCollector(f)
	snap, err := c.Collect(context.Background(), CollectRequest{SourceID: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tables) != 1 || snap.Tables[0].FQN() != "s.users" {
		t.Fatalf("tables: %+v", snap.Tables)
	}
	tbl := snap.Tables[0]
	if len(tbl.Columns) != 2 {
		t.Fatalf("columns: %+v", tbl.Columns)
	}
	if !tbl.Columns[0].IsPrimaryKey {
		t.Fatal("id should be flagged PK from the constraint")
	}
	if snap.ObjectCount.Columns != 2 || snap.ObjectCount.Tables != 1 {
		t.Fatalf("object count: %+v", snap.ObjectCount)
	}
	if snap.SchemaHash == "" {
		t.Fatal("schema hash must be set")
	}
}
