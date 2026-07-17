package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Metadata lineage & impact analysis (Phase 7, FR-META-016/017). Given a table
// or column that is about to change or be retired, this traces every catalog
// asset that depends on it — metrics, relations/joins, golden queries,
// overrides, glossary terms — so an operator can see the blast radius before
// applying a change. It is pure read-only analysis over the loaded catalog.

// ImpactRef is one asset that depends on the analyzed target.
type ImpactRef struct {
	Kind   string `json:"kind"` // metric | relation | preferred_join | forbidden_join | golden_query | override | glossary | downstream_table
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Via    string `json:"via,omitempty"` // which column/edge connects it
}

// AnalyzeImpact returns the dependency footprint of a table (and optionally a
// single column). If column is empty the whole table is analyzed.
func (c *Catalog) AnalyzeImpact(table, column string) map[string]any {
	t, ok := c.ResolveTable(table)
	if !ok {
		return map[string]any{
			"error":      "table not found: " + table,
			"suggestion": "스키마 한정 이름(schema.table)으로 다시 시도하세요.",
		}
	}
	fqn := t.FQN
	col := strings.TrimSpace(column)
	var colDef *Column
	if col != "" {
		colDef = t.ColumnMap[cleanIdent(col)]
		if colDef == nil {
			return map[string]any{"error": "column not found: " + fqn + "." + col}
		}
	}

	// column matcher: whole-table analysis matches any column reference
	matchCol := func(name string) bool {
		return col == "" || strings.EqualFold(strings.TrimSpace(name), col)
	}

	var refs []ImpactRef
	refs = append(refs, c.impactMetrics(fqn, t, col, matchCol)...)
	refs = append(refs, c.impactRelations(t, col, matchCol)...)
	refs = append(refs, c.impactPreferredJoins(t, col, matchCol)...)
	refs = append(refs, c.impactGoldenQueries(t, col)...)
	refs = append(refs, c.impactOverrides(t, col, matchCol)...)
	refs = append(refs, c.impactGlossary(t, colDef, col)...)
	downstream := c.impactDownstream(t)
	refs = append(refs, downstream...)

	byKind := map[string]int{}
	for _, r := range refs {
		byKind[r.Kind]++
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Name < refs[j].Name
	})

	target := fqn
	if col != "" {
		target = fqn + "." + col
	}
	return map[string]any{
		"target":       target,
		"impact_level": impactLevel(byKind),
		"total":        len(refs),
		"count_bykind": byKind,
		"dependents":   refs,
		"note":         "읽기 전용 분석입니다. 변경 전 여기 나열된 자산을 함께 검토/회귀 테스트하세요. 지표·인증조인 의존이 있으면 특히 주의하십시오.",
	}
}

// impactLevel grades the blast radius: metrics or preferred joins depending on
// the target are the most disruptive to break.
func impactLevel(byKind map[string]int) string {
	switch {
	case byKind["metric"] > 0 || byKind["preferred_join"] > 0:
		return "high"
	case byKind["relation"] > 0 || byKind["golden_query"] > 0:
		return "medium"
	case len(byKind) == 0:
		return "none"
	default:
		return "low"
	}
}

func (c *Catalog) impactMetrics(fqn string, t *Table, col string, matchCol func(string) bool) []ImpactRef {
	var refs []ImpactRef
	exprNeedle := strings.ToLower(t.Name)
	if col != "" {
		exprNeedle = strings.ToLower(col)
	}
	for _, m := range c.Metrics {
		hit, via := false, ""
		for _, mt := range m.Tables {
			if strings.EqualFold(mt, fqn) || strings.EqualFold(mt, t.Name) {
				if col == "" {
					hit, via = true, "tables"
				}
			}
		}
		for _, mc := range m.Columns {
			if matchCol(mc) && (col != "" || len(m.Columns) > 0) {
				// only count column match when it belongs to this table's metric
				for _, mt := range m.Tables {
					if strings.EqualFold(mt, fqn) || strings.EqualFold(mt, t.Name) {
						hit, via = true, "columns:"+mc
					}
				}
			}
		}
		if !hit && strings.Contains(strings.ToLower(m.Expression), exprNeedle) {
			// expression textually references the table/column
			if col == "" || strings.Contains(strings.ToLower(m.Expression), strings.ToLower(t.Name)) {
				hit, via = true, "expression"
			}
		}
		if hit {
			refs = append(refs, ImpactRef{Kind: "metric", Name: m.Name, Detail: m.Expression, Via: via})
		}
	}
	return refs
}

func (c *Catalog) impactRelations(t *Table, col string, matchCol func(string) bool) []ImpactRef {
	var refs []ImpactRef
	for _, r := range c.Relations {
		base := r.BaseSchema + "." + r.BaseTable
		ref := r.ReferenceSchema + "." + r.ReferenceTable
		if strings.EqualFold(base, t.FQN) && matchCol(r.BaseColumn) {
			refs = append(refs, ImpactRef{Kind: "relation", Name: base + " → " + ref,
				Detail: r.BaseColumn + " = " + r.ReferenceColumn, Via: r.BaseColumn})
		} else if strings.EqualFold(ref, t.FQN) && matchCol(r.ReferenceColumn) {
			refs = append(refs, ImpactRef{Kind: "relation", Name: base + " → " + ref,
				Detail: r.BaseColumn + " = " + r.ReferenceColumn, Via: r.ReferenceColumn})
		}
	}
	return refs
}

func (c *Catalog) impactPreferredJoins(t *Table, col string, matchCol func(string) bool) []ImpactRef {
	var refs []ImpactRef
	if c.Overrides == nil {
		return refs
	}
	for _, pj := range c.Overrides.PreferredJoins {
		if strings.EqualFold(pj.FromTable, t.FQN) && matchCol(pj.FromColumn) {
			refs = append(refs, ImpactRef{Kind: "preferred_join", Name: pj.FromTable + " → " + pj.ToTable,
				Detail: pj.FromColumn + " = " + pj.ToColumn, Via: pj.FromColumn})
		} else if strings.EqualFold(pj.ToTable, t.FQN) && matchCol(pj.ToColumn) {
			refs = append(refs, ImpactRef{Kind: "preferred_join", Name: pj.FromTable + " → " + pj.ToTable,
				Detail: pj.FromColumn + " = " + pj.ToColumn, Via: pj.ToColumn})
		}
	}
	for _, fj := range c.Overrides.ForbiddenJoins {
		if strings.EqualFold(fj.FromTable, t.FQN) || strings.EqualFold(fj.ToTable, t.FQN) {
			if col == "" {
				refs = append(refs, ImpactRef{Kind: "forbidden_join", Name: fj.FromTable + " ⊗ " + fj.ToTable, Detail: fj.Reason})
			}
		}
	}
	return refs
}

func (c *Catalog) impactGoldenQueries(t *Table, col string) []ImpactRef {
	var refs []ImpactRef
	tblNeedle := strings.ToLower(t.FQN)
	shortNeedle := strings.ToLower(t.Name)
	colNeedle := strings.ToLower(col)
	for _, s := range c.Samples {
		sqlLower := strings.ToLower(s.TargetSQL)
		tableHit := strings.EqualFold(s.TargetTable, t.FQN) || strings.EqualFold(s.TargetTable, t.Name) ||
			strings.Contains(sqlLower, tblNeedle) || containsWord(sqlLower, shortNeedle)
		if !tableHit {
			continue
		}
		if col != "" {
			// require the column to actually appear for column-scoped analysis
			if !strings.EqualFold(s.TargetColumn, col) && !containsWord(sqlLower, colNeedle) {
				continue
			}
		}
		name := fmt.Sprintf("golden#%v", s.ID)
		if s.Question != "" {
			name = truncate(s.Question, 60)
		}
		refs = append(refs, ImpactRef{Kind: "golden_query", Name: name, Detail: s.TargetSQL, Via: s.TargetColumn})
	}
	return refs
}

func (c *Catalog) impactOverrides(t *Table, col string, matchCol func(string) bool) []ImpactRef {
	var refs []ImpactRef
	if c.Overrides == nil {
		return refs
	}
	for _, co := range c.Overrides.Columns {
		if strings.EqualFold(co.Table, t.FQN) || strings.EqualFold(co.Table, t.Name) {
			if matchCol(co.Column) {
				refs = append(refs, ImpactRef{Kind: "override", Name: co.Table + "." + co.Column, Detail: "column override", Via: co.Column})
			}
		}
	}
	if col == "" {
		for _, to := range c.Overrides.Tables {
			if strings.EqualFold(to.Table, t.FQN) || strings.EqualFold(to.Table, t.Name) {
				refs = append(refs, ImpactRef{Kind: "override", Name: to.Table, Detail: "table override"})
			}
		}
	}
	return refs
}

func (c *Catalog) impactGlossary(t *Table, colDef *Column, col string) []ImpactRef {
	var refs []ImpactRef
	if c.Glossary == nil {
		return refs
	}
	// terms whose synonyms name this column (or its logical name)
	targets := map[string]bool{strings.ToLower(t.Name): col == "", strings.ToLower(t.LogicalName): col == ""}
	if colDef != nil {
		targets[strings.ToLower(colDef.Name)] = true
		if colDef.LogicalName != "" {
			targets[strings.ToLower(colDef.LogicalName)] = true
		}
	}
	for _, e := range c.Glossary.Entries {
		hit := false
		for _, syn := range append([]string{e.Term}, e.Synonyms...) {
			if ok := targets[strings.ToLower(strings.TrimSpace(syn))]; ok {
				hit = true
				break
			}
		}
		if hit {
			refs = append(refs, ImpactRef{Kind: "glossary", Name: e.Term, Detail: e.Category})
		}
	}
	return refs
}

// impactDownstream lists tables directly joinable from the target (one hop),
// which may need attention when the target's join keys change.
func (c *Catalog) impactDownstream(t *Table) []ImpactRef {
	var refs []ImpactRef
	seen := map[string]bool{}
	for _, e := range c.Adjacency[t.FQN] {
		if e.To == t.FQN || seen[e.To] {
			continue
		}
		seen[e.To] = true
		refs = append(refs, ImpactRef{Kind: "downstream_table", Name: e.To, Detail: "joinable one hop", Via: e.Relation.BaseColumn})
	}
	return refs
}

// ---- small helpers ----

func containsWord(haystack, word string) bool {
	if word == "" {
		return false
	}
	idx := 0
	for {
		i := strings.Index(haystack[idx:], word)
		if i < 0 {
			return false
		}
		i += idx
		before := i == 0 || !isIdentByte(haystack[i-1])
		afterPos := i + len(word)
		after := afterPos >= len(haystack) || !isIdentByte(haystack[afterPos])
		if before && after {
			return true
		}
		idx = i + 1
	}
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
