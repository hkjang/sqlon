package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// One-click apply for approved candidates (Phase 9 follow-up, FR-META-021).
// ApplyApproved merges every approved-but-not-yet-applied decision into the
// dataset files — overrides.json columns[], metrics.json,
// topology_relations.json, meta_code_dict.json — after backing each file up.
// The caller (server layer) then recompiles the catalog. This keeps the
// original principle intact: nothing reaches the files without an explicit
// approval, and apply is a second explicit act.

// ApplyApproved writes approved decisions into the dataset files under
// dataDir. It is idempotent: applied records are stamped applied_at and
// skipped next time, and every merge also dedupes against file content.
func (c *Catalog) ApplyApproved(dataDir string, now time.Time) map[string]any {
	stored, err := c.loadReviewRecords()
	if err != nil {
		return map[string]any{"error": "failed to load decisions: " + err.Error()}
	}

	var pending []ReviewRecord
	for _, r := range stored {
		if r.Status == "approved" && r.AppliedAt == "" {
			pending = append(pending, r)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if len(pending) == 0 {
		return map[string]any{
			"applied": 0,
			"note":    "적용할 승인 후보가 없습니다 (모두 반영되었거나 승인된 항목이 없음).",
		}
	}

	written := map[string]int{}
	var backups []string
	var applyErr error

	// group by destination and merge file by file
	track := func(file string, n int, backup string, err error) bool {
		if err != nil {
			applyErr = fmt.Errorf("%s: %w", file, err)
			return false
		}
		if n > 0 {
			written[file] += n
			if backup != "" {
				backups = append(backups, backup)
			}
		}
		return true
	}

	var colRecs, metricRecs, relRecs, codeRecs []ReviewRecord
	for _, r := range pending {
		switch r.Kind {
		case "logical_name", "semantic_type", "description":
			colRecs = append(colRecs, r)
		case "metric":
			metricRecs = append(metricRecs, r)
		case "relation":
			relRecs = append(relRecs, r)
		case "code_dict":
			codeRecs = append(codeRecs, r)
		}
	}

	if len(colRecs) > 0 {
		n, backup, err := applyColumnOverrides(dataDir, colRecs)
		if !track("overrides.json", n, backup, err) {
			return applyFailure(applyErr, backups)
		}
	}
	if len(metricRecs) > 0 {
		n, backup, err := applyMetrics(dataDir, metricRecs)
		if !track("metrics.json", n, backup, err) {
			return applyFailure(applyErr, backups)
		}
	}
	if len(relRecs) > 0 {
		n, backup, err := applyRelations(dataDir, relRecs)
		if !track("topology_relations.json", n, backup, err) {
			return applyFailure(applyErr, backups)
		}
	}
	if len(codeRecs) > 0 {
		n, backup, err := applyCodeDicts(dataDir, codeRecs)
		if !track("meta_code_dict.json", n, backup, err) {
			return applyFailure(applyErr, backups)
		}
	}

	// stamp records applied so re-apply is a no-op
	ts := now.UTC().Format(time.RFC3339)
	for _, r := range pending {
		rec := stored[r.ID]
		rec.AppliedAt = ts
		stored[r.ID] = rec
	}
	if err := c.saveReviewRecords(stored); err != nil {
		return map[string]any{"error": "files written but failed to stamp decisions: " + err.Error(), "written": written}
	}

	return map[string]any{
		"applied": len(pending),
		"written": written,
		"backups": backups,
		"note":    "데이터셋 파일에 병합했습니다. 카탈로그 리로드가 필요합니다(서버가 자동 수행). code_dict의 빈 label은 코드값으로 채워졌으니 업무 의미로 다듬으세요.",
	}
}

func applyFailure(err error, backups []string) map[string]any {
	return map[string]any{
		"error":   "apply failed: " + err.Error(),
		"backups": backups,
		"note":    "일부 파일이 이미 수정되었을 수 있습니다. backups 경로로 복원할 수 있습니다.",
	}
}

// ---- per-file mergers ----

func readJSONFileAs(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // treat as empty
		}
		return err
	}
	return json.Unmarshal(b, dst)
}

func writeJSONFile(dataDir, file string, v any) (string, error) {
	backup, err := backupDatasetFile(dataDir, file)
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return backup, err
	}
	path := filepath.Join(dataDir, file)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return backup, err
	}
	return backup, os.Rename(tmp, path)
}

// applyColumnOverrides merges logical_name/semantic_type/description records
// into overrides.json columns[], updating an existing table+column entry or
// appending a new one. Existing non-empty fields are NOT overwritten — an
// operator's curation wins over a generated candidate.
func applyColumnOverrides(dataDir string, recs []ReviewRecord) (int, string, error) {
	path := filepath.Join(dataDir, "overrides.json")
	doc := map[string]any{}
	if err := readJSONFileAs(path, &doc); err != nil {
		return 0, "", err
	}
	cols, _ := doc["columns"].([]any)

	find := func(table, column string) map[string]any {
		for _, e := range cols {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			t, _ := m["table"].(string)
			col, _ := m["column"].(string)
			if strings.EqualFold(t, table) && strings.EqualFold(col, column) {
				return m
			}
		}
		return nil
	}

	fieldOf := map[string]string{"logical_name": "logical_name", "semantic_type": "semantic_type", "description": "description"}
	applied := 0
	for _, r := range recs {
		field := fieldOf[r.Kind]
		val, ok := r.Suggested.(string)
		if !ok || strings.TrimSpace(val) == "" {
			continue
		}
		entry := find(r.Table, r.Column)
		if entry == nil {
			entry = map[string]any{"table": r.Table, "column": r.Column}
			cols = append(cols, entry)
		}
		if existing, _ := entry[field].(string); strings.TrimSpace(existing) != "" {
			continue // operator curation wins
		}
		entry[field] = val
		applied++
	}
	if applied == 0 {
		return 0, "", nil
	}
	doc["columns"] = cols
	backup, err := writeJSONFile(dataDir, "overrides.json", doc)
	return applied, backup, err
}

// applyMetrics appends approved metric definitions to metrics.json, skipping
// any whose name already exists.
func applyMetrics(dataDir string, recs []ReviewRecord) (int, string, error) {
	path := filepath.Join(dataDir, "metrics.json")
	var list []map[string]any
	if err := readJSONFileAs(path, &list); err != nil {
		return 0, "", err
	}
	names := map[string]bool{}
	for _, m := range list {
		if n, _ := m["name"].(string); n != "" {
			names[strings.ToLower(n)] = true
		}
	}
	applied := 0
	for _, r := range recs {
		m := asMap(r.Suggested)
		if m == nil {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" || names[strings.ToLower(name)] {
			continue
		}
		names[strings.ToLower(name)] = true
		list = append(list, m)
		applied++
	}
	if applied == 0 {
		return 0, "", nil
	}
	backup, err := writeJSONFile(dataDir, "metrics.json", list)
	return applied, backup, err
}

// applyRelations appends approved relations to topology_relations.json using
// the file's base_schema/base_table split form, skipping duplicates.
func applyRelations(dataDir string, recs []ReviewRecord) (int, string, error) {
	path := filepath.Join(dataDir, "topology_relations.json")
	var list []map[string]any
	if err := readJSONFileAs(path, &list); err != nil {
		return 0, "", err
	}
	key := func(bs, bt, bc, rs, rt, rc string) string {
		return strings.ToUpper(bs + "." + bt + "." + bc + ">" + rs + "." + rt + "." + rc)
	}
	seen := map[string]bool{}
	for _, m := range list {
		seen[key(str8(m["base_schema"]), str8(m["base_table"]), str8(m["base_column"]),
			str8(m["reference_schema"]), str8(m["reference_table"]), str8(m["reference_column"]))] = true
	}
	applied := 0
	for _, r := range recs {
		m := asMap(r.Suggested)
		if m == nil {
			continue
		}
		bs, bt := splitFQN(str8(m["base_table"]))
		rs, rt := splitFQN(str8(m["reference_table"]))
		bc, rc := str8(m["base_column"]), str8(m["reference_column"])
		if bt == "" || rt == "" || bc == "" || rc == "" {
			continue
		}
		k := key(bs, bt, bc, rs, rt, rc)
		if seen[k] {
			continue
		}
		seen[k] = true
		row := map[string]any{
			"base_schema": bs, "base_table": bt, "base_column": bc,
			"reference_schema": rs, "reference_table": rt, "reference_column": rc,
			"provision_type": "approved_candidate",
		}
		if v := str8(m["cardinality"]); v != "" {
			row["cardinality"] = v
		}
		if v := str8(m["join_type"]); v != "" {
			row["join_type"] = v
		}
		list = append(list, row)
		applied++
	}
	if applied == 0 {
		return 0, "", nil
	}
	backup, err := writeJSONFile(dataDir, "topology_relations.json", list)
	return applied, backup, err
}

// applyCodeDicts appends approved code dictionaries to meta_code_dict.json in
// its row form. Empty labels fall back to the code itself so the file stays
// loadable; operators refine the text afterwards.
func applyCodeDicts(dataDir string, recs []ReviewRecord) (int, string, error) {
	path := filepath.Join(dataDir, "meta_code_dict.json")
	var list []map[string]any
	if err := readJSONFileAs(path, &list); err != nil {
		return 0, "", err
	}
	seen := map[string]bool{}
	for _, m := range list {
		seen[strings.ToUpper(str8(m["schema_name"])+"."+str8(m["table_name"])+"."+str8(m["column_name"]))] = true
	}
	applied := 0
	for _, r := range recs {
		m := asMap(r.Suggested)
		if m == nil {
			continue
		}
		schema, table := splitFQN(r.Table)
		k := strings.ToUpper(schema + "." + table + "." + r.Column)
		if seen[k] {
			continue
		}
		entries, _ := m["entries"].([]any)
		var parts []string
		for _, e := range entries {
			em := asMap(e)
			if em == nil {
				continue
			}
			code := str8(em["code"])
			if code == "" {
				continue
			}
			label := str8(em["label"])
			if label == "" {
				label = code
			}
			parts = append(parts, code+":"+label)
		}
		if len(parts) == 0 {
			continue
		}
		seen[k] = true
		list = append(list, map[string]any{
			"schema_name": schema, "table_name": table, "column_name": r.Column,
			"common_division_code": strings.ToUpper(str8(m["code_dict"])),
			"code_dict_txt":        strings.Join(parts, ", "),
		})
		applied++
	}
	if applied == 0 {
		return 0, "", nil
	}
	backup, err := writeJSONFile(dataDir, "meta_code_dict.json", list)
	return applied, backup, err
}

// ---- helpers ----

// asMap normalizes a Suggested payload (map[string]any live, or generic after
// a JSON round-trip through decisions.json) into map[string]any.
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func str8(v any) string {
	s, _ := v.(string)
	return s
}

func splitFQN(fqn string) (schema, table string) {
	if i := strings.LastIndex(fqn, "."); i >= 0 {
		return fqn[:i], fqn[i+1:]
	}
	return "", fqn
}
