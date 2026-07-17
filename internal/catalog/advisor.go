package catalog

import (
	"regexp"
	"sort"
	"strings"
)

// Index advisor support (DBA co-pilot). PredicateColumns extracts the columns a
// query filters/joins/orders on and reports whether each is already covered by
// an index — the raw signal an index advisor aggregates across slow queries.
// Heuristic (regex over masked SQL), so results are review candidates, never
// auto-applied.

// IndexCandidateCol is one predicate column with its index-coverage status.
type IndexCandidateCol struct {
	Table   string `json:"table"`
	Column  string `json:"column"`
	Indexed bool   `json:"indexed"`
}

// predOpRE captures an identifier used against a comparison/predicate operator
// (left side: `col =`, `t.col LIKE`, `col IN (`, `col BETWEEN`, `col IS`).
var predOpRE = regexp.MustCompile(`(?i)([a-zA-Z_][\w$#]*(?:\s*\.\s*[a-zA-Z_][\w$#]*)?)\s*(?:=|<|>|<=|>=|<>|!=|\bLIKE\b|\bIN\b|\bBETWEEN\b|\bIS\b)`)

// predRhsRE captures an identifier on the right of `=` (join keys: `a.x = b.y`).
var predRhsRE = regexp.MustCompile(`=\s*([a-zA-Z_][\w$#]*(?:\s*\.\s*[a-zA-Z_][\w$#]*)?)`)

// orderByRE captures the ORDER BY column list.
var orderByRE = regexp.MustCompile(`(?i)\border\s+by\s+([^)]+?)(?:\blimit\b|\boffset\b|$)`)

// PredicateColumns returns the catalog columns referenced in WHERE/JOIN/ORDER BY
// predicates of the SQL, each tagged with whether an index already covers it as
// a leading column.
func (c *Catalog) PredicateColumns(sql string) []IndexCandidateCol {
	masked := maskSQL(sql)
	tables := c.sqlTables(sql)
	if len(tables) == 0 {
		return nil
	}

	// collect candidate column tokens (last dotted segment → column name)
	tokens := map[string]bool{}
	addTok := func(raw string) {
		raw = strings.TrimSpace(raw)
		if i := strings.LastIndex(raw, "."); i >= 0 {
			raw = raw[i+1:]
		}
		if id := cleanIdent(raw); id != "" {
			tokens[id] = true
		}
	}
	for _, m := range predOpRE.FindAllStringSubmatch(masked, -1) {
		addTok(m[1])
	}
	for _, m := range predRhsRE.FindAllStringSubmatch(masked, -1) {
		addTok(m[1])
	}
	if m := orderByRE.FindStringSubmatch(masked); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			f := strings.Fields(strings.TrimSpace(part)) // drop ASC/DESC
			if len(f) > 0 {
				addTok(f[0])
			}
		}
	}
	if len(tokens) == 0 {
		return nil
	}

	seen := map[string]bool{}
	var out []IndexCandidateCol
	for _, fqn := range tables {
		t, ok := c.ResolveTable(fqn)
		if !ok {
			continue
		}
		for _, col := range t.Columns {
			if !tokens[strings.ToUpper(col.Name)] {
				continue
			}
			key := t.FQN + "." + col.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, IndexCandidateCol{Table: t.FQN, Column: col.Name, Indexed: t.columnHasLeadingIndex(col.Name)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table+out[i].Column < out[j].Table+out[j].Column })
	return out
}

// columnHasLeadingIndex reports whether a column is a PK, marked indexed, or the
// leading column of some index (a trailing index column does not help
// single-column predicates).
func (t *Table) columnHasLeadingIndex(colName string) bool {
	if col := t.ColumnMap[strings.ToUpper(colName)]; col != nil {
		if col.IsPK || col.Indexed {
			return true
		}
	}
	// leading = lowest Seq per index name
	type idxLead struct {
		col string
		seq int
	}
	lead := map[string]idxLead{}
	for _, ix := range t.Indexes {
		cur, ok := lead[ix.IndexName]
		if !ok || ix.Seq < cur.seq {
			lead[ix.IndexName] = idxLead{col: ix.ColumnName, seq: ix.Seq}
		}
	}
	for _, l := range lead {
		if strings.EqualFold(l.col, colName) {
			return true
		}
	}
	return false
}
