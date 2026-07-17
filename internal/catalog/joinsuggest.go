package catalog

import (
	"sort"
	"strings"
)

// JoinSuggestion is a candidate relation-graph edge discovered from key
// metadata. Suggestions are NEVER auto-applied: the operator reviews them and
// pastes suggested_override into overrides.json preferred_joins.
type JoinSuggestion struct {
	FromTable         string        `json:"from_table"`
	FromColumn        string        `json:"from_column"`
	ToTable           string        `json:"to_table"`
	ToColumn          string        `json:"to_column"`
	Cardinality       string        `json:"cardinality"`
	Evidence          []string      `json:"evidence"`
	Score             float64       `json:"score"`
	Confidence        float64       `json:"confidence"`
	SuggestedOverride PreferredJoin `json:"suggested_override"`
}

// builtinAuditColumns covers near-universal ORM/audit-column conventions;
// operator-specific ones (legacy abbreviations, etc.) are added via
// Overrides.AuditColumnNames rather than hardcoded here.
var builtinAuditColumns = map[string]bool{
	"CREATED_AT": true, "UPDATED_AT": true, "VERSION": true,
}

func (c *Catalog) isAuditColumn(name string) bool {
	if builtinAuditColumns[name] {
		return true
	}
	if c.Overrides == nil {
		return false
	}
	for _, n := range c.Overrides.AuditColumnNames {
		if n == name {
			return true
		}
	}
	return false
}

// SuggestJoins discovers edges missing from the relation graph: for every
// table M whose primary key is a single column C, any other table carrying C
// is a candidate N:1 edge T.C -> M.C. Evidence (FK flag, index, matching
// types, golden-SQL co-occurrence) ranks candidates; ubiquitous columns are
// penalized so CUST_NO does not fan out into 150 noise suggestions.
func (c *Catalog) SuggestJoins(scopeTables []string, topK int) map[string]any {
	if topK <= 0 {
		topK = 20
	}
	scope := map[string]bool{}
	for _, tn := range scopeTables {
		if t, ok := c.ResolveTable(tn); ok {
			scope[t.FQN] = true
		}
	}
	inScope := func(a, b string) bool {
		if len(scope) == 0 {
			return true
		}
		return scope[a] || scope[b]
	}
	hasDirectEdge := func(a, b string) bool {
		for _, e := range c.Adjacency[a] {
			if e.To == b {
				return true
			}
		}
		return false
	}
	// column ubiquity for the noise penalty
	tablesWithColumn := map[string]int{}
	for _, t := range c.Tables {
		for _, col := range t.Columns {
			tablesWithColumn[col.Name]++
		}
	}
	// masters: single-column primary keys
	type master struct {
		table *Table
		col   *Column
	}
	var masters []master
	for _, t := range c.Tables {
		if len(t.PrimaryKeys) == 1 {
			if col := t.ColumnMap[t.PrimaryKeys[0]]; col != nil && !c.isAuditColumn(col.Name) {
				masters = append(masters, master{t, col})
			}
		}
	}

	var out []JoinSuggestion
	for _, m := range masters {
		for _, t := range c.Tables {
			if t.FQN == m.table.FQN || !inScope(t.FQN, m.table.FQN) {
				continue
			}
			col := t.ColumnMap[m.col.Name]
			if col == nil || hasDirectEdge(t.FQN, m.table.FQN) {
				continue
			}
			if _, forbidden := c.IsForbiddenJoin(t.FQN, m.table.FQN); forbidden {
				continue
			}
			score := 1.0
			evidence := []string{m.col.Name + " is the single-column PK of " + m.table.FQN}
			if col.IsFK {
				score += 2
				evidence = append(evidence, "column is flagged FK on "+t.FQN)
			}
			if col.Indexed {
				score += 1
				evidence = append(evidence, "column is indexed on "+t.FQN)
			}
			if col.DataType != "" && col.DataType == m.col.DataType {
				score += 1
				evidence = append(evidence, "data types match ("+col.DataType+")")
			}
			if n := c.tableCoOccurrence(t, m.table); n > 0 {
				boost := float64(n)
				if boost > 3 {
					boost = 3
				}
				score += boost
				evidence = append(evidence, "tables co-occur in golden SQL examples")
			}
			if n := tablesWithColumn[m.col.Name]; n > 50 {
				score -= 1.5
				evidence = append(evidence, "penalty: column appears in many tables; verify business meaning")
			}
			if score <= 1 {
				continue
			}
			conf := 0.5 + 0.05*score
			if conf > 0.85 {
				conf = 0.85 // suggestions never outrank operator-curated joins
			}
			out = append(out, JoinSuggestion{
				FromTable: t.FQN, FromColumn: m.col.Name,
				ToTable: m.table.FQN, ToColumn: m.col.Name,
				Cardinality: "N:1",
				Evidence:    evidence,
				Score:       round(score),
				Confidence:  round(conf),
				SuggestedOverride: PreferredJoin{
					FromTable: t.FQN, FromColumn: m.col.Name,
					ToTable: m.table.FQN, ToColumn: m.col.Name,
					Cardinality: "N:1", JoinType: "INNER",
					Description: "auto-suggested: " + m.col.Name + " references " + m.table.FQN + " (" + strings.TrimSpace(m.table.LogicalName) + ")",
					Confidence:  round(conf),
				},
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].FromTable+out[i].ToTable < out[j].FromTable+out[j].ToTable
		}
		return out[i].Score > out[j].Score
	})
	total := len(out)
	if len(out) > topK {
		out = out[:topK]
	}
	return map[string]any{
		"suggestions":     out,
		"total_found":     total,
		"returned":        len(out),
		"scope_tables":    scopeTables,
		"how_to_apply":    "검토 후 suggested_override 항목을 overrides.json의 preferred_joins 배열에 추가하고 서버를 재기동하세요. 자동 적용되지 않습니다.",
		"existing_edges":  len(c.Relations),
		"master_pk_count": len(masters),
	}
}

// tableCoOccurrence counts golden examples whose target tables include both.
func (c *Catalog) tableCoOccurrence(a, b *Table) int {
	n := 0
	for _, s := range c.Samples {
		tt := strings.ToUpper(s.TargetTable)
		if strings.Contains(tt, a.Name) && strings.Contains(tt, b.Name) {
			n++
		}
	}
	return n
}
