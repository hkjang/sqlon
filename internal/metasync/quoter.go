package metasync

import "strings"

// quoter builds dialect-correct identifiers and a bounded sample expression
// for the profiler. Sampling is a plain LIMIT wrap (arbitrary rows, which is
// fine for statistical profiling) so it works identically on all three
// engines and never scans more than the cap.
type quoter struct {
	dialect string
}

func newQuoter(dialect string) quoter { return quoter{dialect: dialect} }

// ident quotes one identifier part.
func (q quoter) ident(name string) string {
	if q.dialect == "postgres" {
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// qualified quotes schema.table.
func (q quoter) qualified(schema, table string) string {
	return q.ident(schema) + "." + q.ident(table)
}

// sample returns "<schema.table>" or "(SELECT * FROM <schema.table> LIMIT n) sub"
// when a positive cap is given.
func (q quoter) sample(schema, table string, limit int) string {
	full := q.qualified(schema, table)
	if limit <= 0 {
		return full
	}
	return "(SELECT * FROM " + full + " LIMIT " + itoa(limit) + ") " + q.ident("jamypg_sample")
}
