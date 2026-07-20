package dbconn

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RedundantIndex is one index made redundant by another on the same table —
// either an exact duplicate (identical column list) or a leading-prefix subset
// (its columns are a strict prefix of a wider index, which can already serve
// the same lookups). Redundant indexes add write and storage overhead for no
// read benefit. Read-only diagnosis — dropping goes through a change plan.
type RedundantIndex struct {
	TableName  string `json:"table_name"`
	IndexName  string `json:"index_name"`
	Columns    string `json:"columns"`
	Kind       string `json:"kind"`       // duplicate | prefix
	CoveredBy  string `json:"covered_by"` // the index that makes this one redundant
	SizeBytes  int64  `json:"size_bytes"`
	DropScript string `json:"drop_script"`
}

// indexMeta is the engine-agnostic shape the detector reasons over.
type indexMeta struct {
	Table   string
	Name    string
	Columns []string
	Unique  bool
	Size    int64
	Drop    string
}

// detectRedundantIndexes finds duplicate and prefix-redundant indexes. It is a
// pure function of the index metadata so it can be unit-tested without a DB.
//
// Rules (conservative — never flag something whose removal changes semantics):
//   - A UNIQUE index is never reported as redundant: dropping it would drop a
//     uniqueness guarantee, which a covering index does not provide.
//   - Duplicate: two indexes with identical ordered column lists. The one kept
//     is a UNIQUE peer if present, else the lexicographically-first name; the
//     rest are reported as duplicates covered by the keeper.
//   - Prefix: a non-unique index whose column list is a strict prefix of
//     another index on the same table (the wider index serves the same leading
//     lookups). Not reported if already reported as a duplicate.
func detectRedundantIndexes(indexes []indexMeta) []RedundantIndex {
	byTable := map[string][]indexMeta{}
	for _, idx := range indexes {
		if len(idx.Columns) == 0 {
			continue
		}
		byTable[idx.Table] = append(byTable[idx.Table], idx)
	}
	tables := make([]string, 0, len(byTable))
	for t := range byTable {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	var out []RedundantIndex
	for _, table := range tables {
		list := byTable[table]
		// Stable order: name — makes "keeper" selection and output deterministic.
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		reported := map[string]bool{}

		// ---- duplicates: group by exact ordered column list ----
		groups := map[string][]int{}
		for i, idx := range list {
			key := strings.Join(idx.Columns, "\x00")
			groups[key] = append(groups[key], i)
		}
		for _, members := range groups {
			if len(members) < 2 {
				continue
			}
			keeper := members[0]
			for _, m := range members {
				if list[m].Unique { // prefer keeping a unique peer
					keeper = m
					break
				}
			}
			for _, m := range members {
				if m == keeper || list[m].Unique {
					continue
				}
				out = append(out, redundant(list[m], "duplicate", list[keeper].Name))
				reported[list[m].Name] = true
			}
		}

		// ---- prefix redundancy: A.cols is a strict prefix of B.cols ----
		for i := range list {
			a := list[i]
			if a.Unique || reported[a.Name] {
				continue
			}
			for j := range list {
				if i == j {
					continue
				}
				b := list[j]
				if len(a.Columns) < len(b.Columns) && isPrefix(a.Columns, b.Columns) {
					out = append(out, redundant(a, "prefix", b.Name))
					reported[a.Name] = true
					break
				}
			}
		}
	}
	return out
}

func isPrefix(short, long []string) bool {
	if len(short) >= len(long) {
		return false
	}
	for i := range short {
		if short[i] != long[i] {
			return false
		}
	}
	return true
}

func redundant(idx indexMeta, kind, coveredBy string) RedundantIndex {
	return RedundantIndex{
		TableName:  idx.Table,
		IndexName:  idx.Name,
		Columns:    strings.Join(idx.Columns, ","),
		Kind:       kind,
		CoveredBy:  coveredBy,
		SizeBytes:  idx.Size,
		DropScript: idx.Drop,
	}
}

// ListRedundantIndexes returns duplicate and prefix-redundant indexes for the
// profile's engine (PostgreSQL, MySQL, MariaDB). Partial and expression
// indexes are excluded because their equivalence cannot be decided from column
// lists alone.
func (m *Manager) ListRedundantIndexes(ctx context.Context, profileID string) ([]RedundantIndex, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	db, err := m.db(p)
	if err != nil {
		return nil, err
	}

	var indexes []indexMeta
	switch d.Name() {
	case "postgres":
		const q = `SELECT n.nspname || '.' || t.relname AS table_name,
			ic.relname AS index_name,
			idx.indisunique AS is_unique,
			pg_relation_size(idx.indexrelid)::bigint AS size_bytes,
			(SELECT string_agg(a.attname, ',' ORDER BY k.ord)
			 FROM unnest(idx.indkey) WITH ORDINALITY AS k(attnum, ord)
			 JOIN pg_catalog.pg_attribute a ON a.attrelid = idx.indrelid AND a.attnum = k.attnum
			) AS columns
		FROM pg_catalog.pg_index idx
		JOIN pg_catalog.pg_class ic ON ic.oid = idx.indexrelid
		JOIN pg_catalog.pg_class t ON t.oid = idx.indrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema')
		  AND idx.indpred IS NULL
		  AND idx.indexprs IS NULL
		ORDER BY table_name, index_name
		LIMIT 5000`
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("query indexes failed: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var table, name, cols string
			var unique bool
			var size int64
			if err := rows.Scan(&table, &name, &unique, &size, &cols); err != nil {
				return nil, err
			}
			indexes = append(indexes, indexMeta{
				Table: table, Name: name, Unique: unique, Size: size,
				Columns: splitCols(cols),
				Drop:    fmt.Sprintf("DROP INDEX %s;", name),
			})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	case "mysql", "mariadb":
		const q = `SELECT TABLE_SCHEMA, TABLE_NAME, INDEX_NAME,
			MAX(NON_UNIQUE) AS non_unique,
			GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) AS columns
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA NOT IN ('mysql','information_schema','performance_schema','sys')
		GROUP BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME
		ORDER BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME
		LIMIT 5000`
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			// information_schema is always present, but be defensive.
			return []RedundantIndex{}, nil
		}
		defer rows.Close()
		for rows.Next() {
			var schema, table, name, cols string
			var nonUnique int
			if err := rows.Scan(&schema, &table, &name, &nonUnique, &cols); err != nil {
				return nil, err
			}
			indexes = append(indexes, indexMeta{
				Table: schema + "." + table, Name: name, Unique: nonUnique == 0,
				Columns: splitCols(cols),
				Drop:    fmt.Sprintf("ALTER TABLE %s.%s DROP INDEX %s;", schema, table, name),
			})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	default:
		return []RedundantIndex{}, nil
	}

	result := detectRedundantIndexes(indexes)
	if result == nil {
		result = []RedundantIndex{}
	}
	return result, nil
}

func splitCols(csv string) []string {
	var out []string
	for _, c := range strings.Split(csv, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}
