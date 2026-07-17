package catalog

import (
	"sort"
	"strings"
)

// Health reports catalog compilation quality: load issues, coverage gaps, and
// cross-reference errors, so broken metadata is visible at startup and via
// the get_catalog_health tool instead of silently degrading SQL quality.
func (c *Catalog) Health() map[string]any {
	errCount, warnCount := 0, 0
	for _, i := range c.Issues {
		if i.Level == "error" {
			errCount++
		} else {
			warnCount++
		}
	}
	noLogical, noDesc, noSemantic := []string{}, []string{}, 0
	dateCols, piiCols := 0, []string{}
	for _, t := range c.Tables {
		if t.LogicalName == "" {
			noLogical = append(noLogical, t.FQN)
		}
		if t.Description == "" {
			noDesc = append(noDesc, t.FQN)
		}
		for _, col := range t.Columns {
			if col.SemanticType != "" {
				dateCols++
			} else if c.isDateLikeName(col.Name) {
				noSemantic++
			}
			if col.PII {
				piiCols = append(piiCols, t.FQN+"."+col.Name)
			}
		}
	}
	sort.Strings(noLogical)
	sort.Strings(noDesc)
	sort.Strings(piiCols)
	status := "ok"
	if warnCount > 0 {
		status = "degraded"
	}
	if errCount > 0 {
		status = "error"
	}
	return map[string]any{
		"status":                       status,
		"summary":                      c.Summary(),
		"dialect":                      c.Dialect,
		"error_count":                  errCount,
		"warning_count":                warnCount,
		"issues":                       c.Issues,
		"tables_without_logical_name":  capList(noLogical, 30),
		"tables_without_description":   capList(noDesc, 30),
		"date_columns_typed":           dateCols,
		"date_like_columns_untyped":    noSemantic,
		"pii_columns":                  piiCols,
		"metric_definitions":           len(c.Metrics),
		"glossary_entries":             glossarySize(c.Glossary),
		"forbidden_joins":              len(c.ForbiddenJoins),
		"feedback_success_table_count": len(c.FeedbackUsage),
		"learned_rules":                len(c.LearnedRules),
	}
}

// validateRelations cross-checks the join graph against compiled tables and
// columns; broken edges become load issues instead of silent no-ops.
func (c *Catalog) validateRelations() {
	for _, r := range c.Relations {
		from, okF := c.ResolveTable(r.BaseSchema + "." + r.BaseTable)
		if !okF {
			c.Issues = append(c.Issues, LoadIssue{Level: "error", Source: "topology_relations.json", Message: "relation base table not in catalog", Table: r.BaseSchema + "." + r.BaseTable})
			continue
		}
		to, okT := c.ResolveTable(r.ReferenceSchema + "." + r.ReferenceTable)
		if !okT {
			c.Issues = append(c.Issues, LoadIssue{Level: "error", Source: "topology_relations.json", Message: "relation reference table not in catalog", Table: r.ReferenceSchema + "." + r.ReferenceTable})
			continue
		}
		for _, col := range splitColumns(r.BaseColumn) {
			if from.ColumnMap[col] == nil {
				c.Issues = append(c.Issues, LoadIssue{Level: "error", Source: "topology_relations.json", Message: "relation base column not in table", Table: from.FQN, Column: col})
			}
		}
		for _, col := range splitColumns(r.ReferenceColumn) {
			if to.ColumnMap[col] == nil {
				c.Issues = append(c.Issues, LoadIssue{Level: "error", Source: "topology_relations.json", Message: "relation reference column not in table", Table: to.FQN, Column: col})
			}
		}
	}
}

// isDateLikeName recognizes generic date-column naming suffixes plus any
// operator-configured well-known date columns (e.g. legacy abbreviations
// that don't follow the generic suffix convention).
func (c *Catalog) isDateLikeName(name string) bool {
	for _, suf := range []string{"_DT", "_MON", "_DT_TM"} {
		if len(name) > len(suf) && name[len(name)-len(suf):] == suf {
			return true
		}
	}
	if c.Overrides == nil {
		return false
	}
	for _, n := range c.Overrides.WellKnownDateColumns {
		if n == name {
			return true
		}
	}
	return false
}

func glossarySize(g *Glossary) int {
	if g == nil {
		return 0
	}
	return len(g.Entries)
}

func capList(list []string, n int) []string {
	if len(list) > n {
		return list[:n]
	}
	return list
}

// PIIColumnNames returns the set of column names (upper-cased) flagged PII
// anywhere in the catalog. Used by the execution layer to mask values as a
// defense-in-depth behind validate_sql's PII output block.
func (c *Catalog) PIIColumnNames() map[string]bool {
	out := map[string]bool{}
	for _, t := range c.Tables {
		for _, col := range t.Columns {
			if col.PII {
				out[strings.ToUpper(col.Name)] = true
			}
		}
	}
	return out
}
