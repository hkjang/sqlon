package catalog

import (
	"testing"
	"time"
)

func syncApplyCatalog(t *testing.T) *Catalog {
	dir := t.TempDir()
	// existing physical model: one table with a curated description + an
	// about-to-be-dropped column.
	writeJSON(t, dir, "meta_physical_models.json", []any{
		map[string]any{"schema_name": "public", "table_name": "users", "column_order": "1",
			"column_name": "id", "data_type": "TEXT", "is_pk": "Y", "is_fk": "N",
			"description": "사용자 식별자(수기)", "version": 1},
		map[string]any{"schema_name": "public", "table_name": "users", "column_order": "2",
			"column_name": "legacy_col", "data_type": "TEXT", "is_pk": "N", "is_fk": "N",
			"description": "", "version": 1},
	})
	return &Catalog{DataDir: dir, Tables: map[string]*Table{}}
}

func TestApplyPhysicalSnapshotUpsertPreserveRetire(t *testing.T) {
	c := syncApplyCatalog(t)
	cols := []PhysicalColumn{
		// existing id: type changes TEXT→BIGINT; description must be preserved
		{Schema: "public", Table: "users", Column: "id", Ordinal: 1, DataType: "BIGINT", Nullable: false, IsPK: true},
		// new column
		{Schema: "public", Table: "users", Column: "email", Ordinal: 3, DataType: "VARCHAR", Nullable: true, Comment: "이메일"},
		// legacy_col is absent → retire candidate
	}
	res := c.ApplyPhysicalSnapshot(cols, nil, false, "pg", time.Now())
	if res["error"] != nil {
		t.Fatalf("apply failed: %v", res["error"])
	}
	if res["columns_added"].(int) != 1 || res["columns_updated"].(int) != 1 {
		t.Fatalf("expected 1 added + 1 updated, got %+v", res)
	}
	retire := res["retire_candidates"].([]string)
	if len(retire) != 1 || retire[0] != "public.users.legacy_col" {
		t.Fatalf("legacy_col should be a retire candidate: %+v", retire)
	}

	var rows []map[string]any
	readJSONT(t, c.DataDir, "meta_physical_models.json", &rows)
	byCol := map[string]map[string]any{}
	for _, m := range rows {
		byCol[str8(m["column_name"])] = m
	}
	// id: type updated, description preserved
	if byCol["id"]["data_type"] != "BIGINT" {
		t.Fatalf("id type not updated: %v", byCol["id"]["data_type"])
	}
	if byCol["id"]["description"] != "사용자 식별자(수기)" {
		t.Fatalf("curated description must be preserved: %v", byCol["id"]["description"])
	}
	// email added with comment as description
	if byCol["email"] == nil || byCol["email"]["description"] != "이메일" {
		t.Fatalf("email not added correctly: %+v", byCol["email"])
	}
	// legacy_col NOT removed (retire candidate, prune=false)
	if byCol["legacy_col"] == nil {
		t.Fatal("legacy_col must be kept when prune=false")
	}

	// re-apply is idempotent (no changes)
	res2 := c.ApplyPhysicalSnapshot(cols, nil, false, "pg", time.Now())
	if res2["columns_added"].(int) != 0 || res2["columns_updated"].(int) != 0 {
		t.Fatalf("re-apply should be a no-op, got %+v", res2)
	}
}

func TestApplyPhysicalSnapshotPruneRemoves(t *testing.T) {
	c := syncApplyCatalog(t)
	cols := []PhysicalColumn{
		{Schema: "public", Table: "users", Column: "id", Ordinal: 1, DataType: "TEXT", IsPK: true},
	}
	res := c.ApplyPhysicalSnapshot(cols, nil, true, "pg", time.Now())
	if !res["pruned"].(bool) {
		t.Fatalf("expected pruned=true, got %+v", res)
	}
	var rows []map[string]any
	readJSONT(t, c.DataDir, "meta_physical_models.json", &rows)
	for _, m := range rows {
		if str8(m["column_name"]) == "legacy_col" {
			t.Fatal("legacy_col should be pruned")
		}
	}
}

func TestApplyPhysicalSnapshotRelations(t *testing.T) {
	c := syncApplyCatalog(t)
	rels := []RelationUpsert{
		{BaseSchema: "public", BaseTable: "orders", BaseColumn: "user_id",
			RefSchema: "public", RefTable: "users", RefColumn: "id"},
	}
	res := c.ApplyPhysicalSnapshot(nil, rels, false, "pg", time.Now())
	if res["relations_added"].(int) != 1 {
		t.Fatalf("expected 1 relation added, got %+v", res)
	}
	var list []map[string]any
	readJSONT(t, c.DataDir, "topology_relations.json", &list)
	if len(list) != 1 || str8(list[0]["reference_table"]) != "users" {
		t.Fatalf("relation not written: %+v", list)
	}
	// idempotent
	res2 := c.ApplyPhysicalSnapshot(nil, rels, false, "pg", time.Now())
	if res2["relations_added"].(int) != 0 {
		t.Fatalf("re-apply relation should be no-op, got %+v", res2)
	}
}
