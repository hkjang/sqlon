package catalog

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Metadata-sync → catalog apply (auto-reflect physical model). A collected
// snapshot's PHYSICAL facts (columns, types, nullability, PK/FK, FK relations)
// are merged into meta_physical_models.json / topology_relations.json and the
// catalog is reloaded. This is consistent with the standing principle —
// physical structure is auto-collected — while BUSINESS meaning is preserved:
// existing column/table descriptions are never overwritten. Deletions are
// retire candidates and are NOT removed unless prune=true (an explicit,
// admin-only opt-in).

// PhysicalColumn is one collected physical column, source-agnostic.
type PhysicalColumn struct {
	Schema          string
	Table           string
	Column          string
	Ordinal         int
	DataType        string
	LengthPrecision string
	Nullable        bool
	IsPK            bool
	IsFK            bool
	Comment         string
}

// RelationUpsert is one collected FK relation.
type RelationUpsert struct {
	BaseSchema, BaseTable, BaseColumn string
	RefSchema, RefTable, RefColumn    string
}

// ApplyPhysicalSnapshot merges collected physical columns + FK relations into
// the dataset files (with backups) and reports what changed. The caller reloads
// the catalog. prune=true additionally removes physical rows for tables/columns
// that were in the catalog (within the collected schemas) but absent from the
// snapshot; prune=false leaves them and reports them as retire candidates.
func (c *Catalog) ApplyPhysicalSnapshot(cols []PhysicalColumn, rels []RelationUpsert, prune bool, source string, now time.Time) map[string]any {
	// collected scope: only schemas present in the snapshot are touched, so
	// unrelated datasets/rows are never disturbed.
	collectedSchemas := map[string]bool{}
	collectedCols := map[string]bool{} // SCHEMA.TABLE.COLUMN (upper)
	for _, pc := range cols {
		collectedSchemas[strings.ToUpper(pc.Schema)] = true
		collectedCols[strings.ToUpper(pc.Schema+"."+pc.Table+"."+pc.Column)] = true
	}

	path := filepath.Join(c.DataDir, "meta_physical_models.json")
	var rows []map[string]any
	if err := readJSONFileAs(path, &rows); err != nil {
		return map[string]any{"error": "read meta_physical_models.json: " + err.Error()}
	}

	rowKey := func(m map[string]any) string {
		return strings.ToUpper(str8(m["schema_name"]) + "." + str8(m["table_name"]) + "." + str8(m["column_name"]))
	}
	index := map[string]map[string]any{}
	for _, m := range rows {
		index[rowKey(m)] = m
	}

	added, updated := 0, 0
	for _, pc := range cols {
		key := strings.ToUpper(pc.Schema + "." + pc.Table + "." + pc.Column)
		nullc := "N"
		if pc.Nullable {
			nullc = "Y"
		}
		if m := index[key]; m != nil {
			// update PHYSICAL facts only; never touch description (business meaning)
			changed := false
			changed = setIfDiff(m, "data_type", strings.ToUpper(pc.DataType)) || changed
			changed = setIfDiff(m, "length_precision", pc.LengthPrecision) || changed
			changed = setIfDiff(m, "null_constraint", nullc) || changed
			changed = setIfDiff(m, "is_pk", ynStr(pc.IsPK)) || changed
			changed = setIfDiff(m, "is_fk", ynStr(pc.IsFK)) || changed
			changed = setIfDiff(m, "column_order", strconv.Itoa(pc.Ordinal)) || changed
			if changed {
				updated++
			}
		} else {
			row := map[string]any{
				"id":               physicalRowID(pc.Schema, pc.Table, pc.Column),
				"schema_name":      pc.Schema,
				"table_name":       pc.Table,
				"column_order":     strconv.Itoa(pc.Ordinal),
				"column_name":      pc.Column,
				"data_type":        strings.ToUpper(pc.DataType),
				"length_precision": pc.LengthPrecision,
				"null_constraint":  nullc,
				"is_pk":            ynStr(pc.IsPK),
				"is_fk":            ynStr(pc.IsFK),
				"description":      pc.Comment, // DB comment is a physical artifact; seeds new rows
				"version":          1,
			}
			rows = append(rows, row)
			index[key] = row
			added++
		}
	}

	// deletions within the collected scope
	retireCols := []string{}
	kept := rows[:0]
	for _, m := range rows {
		schemaUp := strings.ToUpper(str8(m["schema_name"]))
		key := rowKey(m)
		// a column is a retire candidate when its schema was collected but the
		// exact column is absent from the snapshot (covers dropped columns and
		// dropped tables within collected schemas)
		colAbsent := collectedSchemas[schemaUp] && !collectedCols[key]
		if colAbsent {
			retireCols = append(retireCols, str8(m["schema_name"])+"."+str8(m["table_name"])+"."+str8(m["column_name"]))
			if prune {
				continue // drop it
			}
		}
		kept = append(kept, m)
	}
	rows = kept

	var backups []string
	written := map[string]int{}
	if added > 0 || updated > 0 || (prune && len(retireCols) > 0) {
		backup, err := writeJSONFile(c.DataDir, "meta_physical_models.json", rows)
		if err != nil {
			return map[string]any{"error": "write meta_physical_models.json: " + err.Error()}
		}
		written["meta_physical_models.json"] = added + updated
		if backup != "" {
			backups = append(backups, backup)
		}
	}

	relAdded, relBackup, relErr := c.upsertRelations(rels)
	if relErr != nil {
		return map[string]any{"error": "write topology_relations.json: " + relErr.Error(), "backups": backups}
	}
	if relAdded > 0 {
		written["topology_relations.json"] = relAdded
		if relBackup != "" {
			backups = append(backups, relBackup)
		}
	}

	return map[string]any{
		"source":            source,
		"columns_added":     added,
		"columns_updated":   updated,
		"relations_added":   relAdded,
		"retire_candidates": retireCols,
		"pruned":            prune && len(retireCols) > 0,
		"written":           written,
		"backups":           backups,
		"note": "물리 구조를 카탈로그에 반영했습니다. 기존 설명(업무 의미)은 보존했고, 삭제분은 " +
			ternary(prune, "prune로 제거했습니다.", "retire 후보로만 표시(제거 안 함)했습니다. 카탈로그 리로드가 필요합니다."),
	}
}

// upsertRelations appends FK relations to topology_relations.json, skipping
// duplicates and preserving existing rows.
func (c *Catalog) upsertRelations(rels []RelationUpsert) (int, string, error) {
	if len(rels) == 0 {
		return 0, "", nil
	}
	path := filepath.Join(c.DataDir, "topology_relations.json")
	var list []map[string]any
	if err := readJSONFileAs(path, &list); err != nil {
		return 0, "", err
	}
	seen := map[string]bool{}
	key := func(bs, bt, bc, rs, rt, rc string) string {
		return strings.ToUpper(bs + "." + bt + "." + bc + ">" + rs + "." + rt + "." + rc)
	}
	for _, m := range list {
		seen[key(str8(m["base_schema"]), str8(m["base_table"]), str8(m["base_column"]),
			str8(m["reference_schema"]), str8(m["reference_table"]), str8(m["reference_column"]))] = true
	}
	added := 0
	for _, r := range rels {
		if r.BaseTable == "" || r.RefTable == "" || r.BaseColumn == "" || r.RefColumn == "" {
			continue
		}
		k := key(r.BaseSchema, r.BaseTable, r.BaseColumn, r.RefSchema, r.RefTable, r.RefColumn)
		if seen[k] {
			continue
		}
		seen[k] = true
		list = append(list, map[string]any{
			"base_schema": r.BaseSchema, "base_table": r.BaseTable, "base_column": r.BaseColumn,
			"reference_schema": r.RefSchema, "reference_table": r.RefTable, "reference_column": r.RefColumn,
			"cardinality": "N:1", "join_type": "INNER", "provision_type": "FK",
		})
		added++
	}
	if added == 0 {
		return 0, "", nil
	}
	backup, err := writeJSONFile(c.DataDir, "topology_relations.json", list)
	return added, backup, err
}

// ---- helpers ----

// setIfDiff sets m[key]=val when different; reports whether it changed.
func setIfDiff(m map[string]any, key, val string) bool {
	if str8(m[key]) == val {
		return false
	}
	m[key] = val
	return true
}

func ynStr(b bool) string {
	if b {
		return "Y"
	}
	return "N"
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// physicalRowID derives a stable UUID-shaped id for a new physical row from its
// identity, matching the deterministic id style already in the dataset.
func physicalRowID(schema, table, column string) string {
	h := sha1.Sum([]byte("physical:" + strings.ToLower(schema+"."+table+"."+column)))
	hexs := hex.EncodeToString(h[:])
	return hexs[0:8] + "-" + hexs[8:12] + "-" + hexs[12:16] + "-" + hexs[16:20] + "-" + hexs[20:32]
}
