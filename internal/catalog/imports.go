package catalog

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// External metadata import (OpenMetadata et al.). This is the catalog-side,
// source-agnostic pipeline: a caller (e.g. the OpenMetadata connector) hands
// over neutral column/table/glossary metadata, and the catalog resolves it
// against the compiled model, proposes candidates ONLY for gaps jamypg lacks,
// and either previews them (default) or merges them into overrides.json /
// glossary.json with backups. Business meaning still never lands silently:
// preview is the default and apply is an explicit second act; existing
// operator-curated overrides are never overwritten.

// ExternalColumnMeta is one column's curated metadata from an external catalog.
type ExternalColumnMeta struct {
	Table       string // jamypg form "schema.table"
	Column      string
	LogicalName string
	Description string
	PII         bool
}

// ExternalTableMeta is one table's curated metadata.
type ExternalTableMeta struct {
	Table       string
	LogicalName string
	Description string
}

// ExternalGlossaryTerm is one glossary term.
type ExternalGlossaryTerm struct {
	Term        string
	Synonyms    []string
	Description string
	Category    string
}

// ExternalImport bundles what a connector pulled.
type ExternalImport struct {
	Source   string
	Columns  []ExternalColumnMeta
	Tables   []ExternalTableMeta
	Glossary []ExternalGlossaryTerm
}

// importColEntry is one proposed overrides.json column row.
type importColEntry struct {
	Table        string `json:"table"`
	Column       string `json:"column"`
	LogicalName  string `json:"logical_name,omitempty"`
	Description  string `json:"description,omitempty"`
	SemanticType string `json:"semantic_type,omitempty"`
	PII          *bool  `json:"pii,omitempty"`
}

// ImportExternalMetadata resolves and proposes external metadata. When apply is
// false it returns a preview (no writes). When true it merges into the dataset
// files and returns what changed; the caller reloads the catalog.
func (c *Catalog) ImportExternalMetadata(imp ExternalImport, apply bool, now time.Time) map[string]any {
	var (
		colEntries    = []importColEntry{}
		matched       = map[string]*importColEntry{}
		order         []string
		skippedTables = map[string]bool{}
		resolvedTabs  = map[string]bool{}
	)

	entryFor := func(fqn, column string) *importColEntry {
		key := fqn + "|" + strings.ToUpper(column)
		if e := matched[key]; e != nil {
			return e
		}
		e := &importColEntry{Table: fqn, Column: column}
		matched[key] = e
		order = append(order, key)
		return e
	}

	// ---- columns: propose only where jamypg lacks the value ----
	for _, cm := range imp.Columns {
		t, ok := c.ResolveTable(cm.Table)
		if !ok {
			skippedTables[strings.ToUpper(cm.Table)] = true
			continue
		}
		resolvedTabs[t.FQN] = true
		col := t.ColumnMap[cleanIdent(cm.Column)]
		if col == nil {
			continue
		}
		var e *importColEntry
		if cm.LogicalName != "" && col.LogicalName == "" {
			e = entryFor(t.FQN, col.Name)
			e.LogicalName = cm.LogicalName
		}
		if cm.Description != "" && col.Description == "" {
			if e == nil {
				e = entryFor(t.FQN, col.Name)
			}
			e.Description = cm.Description
		}
		if cm.PII && !col.PII {
			if e == nil {
				e = entryFor(t.FQN, col.Name)
			}
			yes := true
			e.PII = &yes
			if col.SemanticType == "" {
				e.SemanticType = "PII"
			}
		}
	}
	for _, k := range order {
		colEntries = append(colEntries, *matched[k])
	}

	// ---- tables: table-level overrides for gaps ----
	tableEntries := []map[string]any{}
	for _, tm := range imp.Tables {
		t, ok := c.ResolveTable(tm.Table)
		if !ok {
			skippedTables[strings.ToUpper(tm.Table)] = true
			continue
		}
		row := map[string]any{"table": t.FQN}
		add := false
		if tm.LogicalName != "" && t.LogicalName == "" {
			row["logical_name"] = tm.LogicalName
			add = true
		}
		if tm.Description != "" && t.Description == "" {
			row["description"] = tm.Description
			add = true
		}
		if add {
			tableEntries = append(tableEntries, row)
		}
	}

	// ---- glossary: only terms not already present (by term, case-insensitive) ----
	existingTerms := map[string]bool{}
	if c.Glossary != nil {
		for _, e := range c.Glossary.Entries {
			existingTerms[strings.ToLower(strings.TrimSpace(e.Term))] = true
		}
	}
	glossaryEntries := []GlossaryEntry{}
	for _, g := range imp.Glossary {
		term := strings.TrimSpace(g.Term)
		if term == "" || existingTerms[strings.ToLower(term)] {
			continue
		}
		existingTerms[strings.ToLower(term)] = true
		cat := g.Category
		if cat == "" {
			cat = "imported"
		}
		glossaryEntries = append(glossaryEntries, GlossaryEntry{
			Term: term, Synonyms: g.Synonyms, Category: cat, Note: g.Description,
		})
	}

	skipped := []string{}
	for t := range skippedTables {
		skipped = append(skipped, t)
	}
	sort.Strings(skipped)

	res := map[string]any{
		"source":            imp.Source,
		"resolved_tables":   len(resolvedTabs),
		"column_candidates": colEntries,
		"table_candidates":  tableEntries,
		"glossary_new":      glossaryEntries,
		"counts": map[string]int{
			"columns": len(colEntries), "tables": len(tableEntries), "glossary": len(glossaryEntries),
		},
		"skipped_tables": skipped,
	}

	if !apply {
		res["applied"] = false
		res["note"] = "미리보기입니다. 반영하려면 apply=true로 다시 호출하세요(기존 운영자 값은 덮어쓰지 않으며 파일은 백업됩니다)."
		return res
	}

	written := map[string]int{}
	var backups []string
	if n, backup, err := mergeImportColumns(c.DataDir, colEntries, tableEntries); err != nil {
		res["error"] = "overrides.json merge failed: " + err.Error()
		return res
	} else if n > 0 {
		written["overrides.json"] = n
		if backup != "" {
			backups = append(backups, backup)
		}
	}
	if n, backup, err := mergeImportGlossary(c.DataDir, glossaryEntries); err != nil {
		res["error"] = "glossary.json merge failed: " + err.Error()
		res["backups"] = backups
		return res
	} else if n > 0 {
		written["glossary.json"] = n
		if backup != "" {
			backups = append(backups, backup)
		}
	}
	res["applied"] = true
	res["written"] = written
	res["backups"] = backups
	res["note"] = "데이터셋 파일에 병합했습니다. 카탈로그 리로드가 필요합니다(서버가 자동 수행)."
	_ = now
	return res
}

// driftItem is one field-level divergence between jamypg and an external
// catalog.
type driftItem struct {
	Table       string `json:"table"`
	Column      string `json:"column,omitempty"`
	Field       string `json:"field"` // logical_name | description | pii
	JamypgValue string `json:"jamypg_value,omitempty"`
	ExtValue    string `json:"ext_value,omitempty"`
}

// DiffExternalMetadata reconciles jamypg against an external catalog without
// writing anything. It classifies each field as:
//   - jamypg_gap: jamypg is empty, the external catalog has a value (import candidate)
//   - conflict:   both have values and they differ (needs a human decision)
//   - ext_gap:    jamypg has a value, the external catalog is empty (export candidate)
func (c *Catalog) DiffExternalMetadata(imp ExternalImport) map[string]any {
	jamypgGaps := []driftItem{}
	conflicts := []driftItem{}
	extGaps := []driftItem{}
	resolved := map[string]bool{}
	skipped := map[string]bool{}

	classify := func(table, column, field, jv, ev string) {
		jv, ev = strings.TrimSpace(jv), strings.TrimSpace(ev)
		switch {
		case jv == "" && ev != "":
			jamypgGaps = append(jamypgGaps, driftItem{Table: table, Column: column, Field: field, ExtValue: ev})
		case jv != "" && ev == "":
			extGaps = append(extGaps, driftItem{Table: table, Column: column, Field: field, JamypgValue: jv})
		case jv != "" && ev != "" && !strings.EqualFold(jv, ev):
			conflicts = append(conflicts, driftItem{Table: table, Column: column, Field: field, JamypgValue: jv, ExtValue: ev})
		}
	}

	for _, cm := range imp.Columns {
		t, ok := c.ResolveTable(cm.Table)
		if !ok {
			skipped[strings.ToUpper(cm.Table)] = true
			continue
		}
		col := t.ColumnMap[cleanIdent(cm.Column)]
		if col == nil {
			continue
		}
		resolved[t.FQN] = true
		classify(t.FQN, col.Name, "logical_name", col.LogicalName, cm.LogicalName)
		classify(t.FQN, col.Name, "description", col.Description, cm.Description)
		// PII is a boolean: represent divergence directionally.
		switch {
		case !col.PII && cm.PII:
			jamypgGaps = append(jamypgGaps, driftItem{Table: t.FQN, Column: col.Name, Field: "pii", ExtValue: "true"})
		case col.PII && !cm.PII:
			extGaps = append(extGaps, driftItem{Table: t.FQN, Column: col.Name, Field: "pii", JamypgValue: "true"})
		}
	}
	for _, tm := range imp.Tables {
		t, ok := c.ResolveTable(tm.Table)
		if !ok {
			skipped[strings.ToUpper(tm.Table)] = true
			continue
		}
		resolved[t.FQN] = true
		classify(t.FQN, "", "logical_name", t.LogicalName, tm.LogicalName)
		classify(t.FQN, "", "description", t.Description, tm.Description)
	}

	skippedList := []string{}
	for k := range skipped {
		skippedList = append(skippedList, k)
	}
	sort.Strings(skippedList)

	return map[string]any{
		"source":          imp.Source,
		"resolved_tables": len(resolved),
		"jamypg_gaps":     jamypgGaps, // import candidates (external → jamypg)
		"conflicts":       conflicts,  // divergent values; reconcile manually
		"ext_gaps":        extGaps,    // export candidates (jamypg → external)
		"counts": map[string]int{
			"jamypg_gaps": len(jamypgGaps), "conflicts": len(conflicts), "ext_gaps": len(extGaps),
		},
		"skipped_tables": skippedList,
		"note":           "읽기 전용 대조입니다. jamypg_gaps는 import, ext_gaps는 export 후보이며 conflicts는 사람이 어느 쪽을 채택할지 결정해야 합니다.",
	}
}

// mergeImportColumns merges proposed column + table overrides into
// overrides.json, protecting existing non-empty fields.
func mergeImportColumns(dataDir string, cols []importColEntry, tables []map[string]any) (int, string, error) {
	if len(cols) == 0 && len(tables) == 0 {
		return 0, "", nil
	}
	path := filepath.Join(dataDir, "overrides.json")
	doc := map[string]any{}
	if err := readJSONFileAs(path, &doc); err != nil {
		return 0, "", err
	}

	existing, _ := doc["columns"].([]any)
	findCol := func(table, column string) map[string]any {
		for _, e := range existing {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if strings.EqualFold(str8(m["table"]), table) && strings.EqualFold(str8(m["column"]), column) {
				return m
			}
		}
		return nil
	}
	applied := 0
	for _, c := range cols {
		m := findCol(c.Table, c.Column)
		if m == nil {
			m = map[string]any{"table": c.Table, "column": c.Column}
			existing = append(existing, m)
		}
		if c.LogicalName != "" && str8(m["logical_name"]) == "" {
			m["logical_name"] = c.LogicalName
			applied++
		}
		if c.Description != "" && str8(m["description"]) == "" {
			m["description"] = c.Description
			applied++
		}
		if c.SemanticType != "" && str8(m["semantic_type"]) == "" {
			m["semantic_type"] = c.SemanticType
			applied++
		}
		if c.PII != nil {
			if _, has := m["pii"]; !has {
				m["pii"] = *c.PII
				applied++
			}
		}
	}
	doc["columns"] = existing

	if len(tables) > 0 {
		exTabs, _ := doc["tables"].([]any)
		findTab := func(table string) map[string]any {
			for _, e := range exTabs {
				if m, ok := e.(map[string]any); ok && strings.EqualFold(str8(m["table"]), table) {
					return m
				}
			}
			return nil
		}
		for _, t := range tables {
			table := str8(t["table"])
			m := findTab(table)
			if m == nil {
				m = map[string]any{"table": table}
				exTabs = append(exTabs, m)
			}
			for _, f := range []string{"logical_name", "description"} {
				if v := str8(t[f]); v != "" && str8(m[f]) == "" {
					m[f] = v
					applied++
				}
			}
		}
		doc["tables"] = exTabs
	}

	if applied == 0 {
		return 0, "", nil
	}
	backup, err := writeJSONFile(dataDir, "overrides.json", doc)
	return applied, backup, err
}

// mergeImportGlossary appends new glossary terms to glossary.json.
func mergeImportGlossary(dataDir string, terms []GlossaryEntry) (int, string, error) {
	if len(terms) == 0 {
		return 0, "", nil
	}
	path := filepath.Join(dataDir, "glossary.json")
	doc := Glossary{}
	if err := readJSONFileAs(path, &doc); err != nil {
		return 0, "", err
	}
	have := map[string]bool{}
	for _, e := range doc.Entries {
		have[strings.ToLower(strings.TrimSpace(e.Term))] = true
	}
	applied := 0
	for _, t := range terms {
		if have[strings.ToLower(strings.TrimSpace(t.Term))] {
			continue
		}
		doc.Entries = append(doc.Entries, t)
		have[strings.ToLower(strings.TrimSpace(t.Term))] = true
		applied++
	}
	if applied == 0 {
		return 0, "", nil
	}
	backup, err := writeJSONFile(dataDir, "glossary.json", doc)
	return applied, backup, err
}
