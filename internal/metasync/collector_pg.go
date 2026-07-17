package metasync

import (
	"context"
	"strings"
)

// PostgreSQL system-catalog collection. Uses information_schema for portable
// column/constraint data and pg_catalog for comments, view SQL, index detail,
// and row-count estimates. All queries are read-only.
func (c *Collector) collectPostgres(ctx context.Context, req CollectRequest) ([]TableAsset, error) {
	schemaFilter, params := pgSchemaFilter(req.Schemas)

	// tables + views + comments + row estimates
	tblQ := `
SELECT n.nspname AS schema, c.relname AS name,
       CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table'
            WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view' ELSE 'table' END AS kind,
       COALESCE(obj_description(c.oid), '') AS comment,
       COALESCE(c.reltuples, 0)::bigint AS est_rows,
       COALESCE(pg_get_viewdef(c.oid), '') AS view_sql
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','v','m') AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')` +
		schemaFilter + ` ORDER BY n.nspname, c.relname`
	tblRows, err := c.q.SystemQuery(ctx, req.SourceID, tblQ, params...)
	if err != nil {
		return nil, err
	}
	tables := map[string]*TableAsset{}
	var order []string
	for _, r := range tblRows {
		kind := asString(r["kind"])
		if !req.IncludeViews && (kind == "view" || kind == "materialized_view") {
			continue
		}
		t := &TableAsset{
			Schema: asString(r["schema"]), Name: asString(r["name"]), Kind: kind,
			Comment: asString(r["comment"]), EstRowCount: asInt64(r["est_rows"]),
		}
		if kind == "view" || kind == "materialized_view" {
			t.ViewSQL = strings.TrimSpace(asString(r["view_sql"]))
		}
		key := t.FQN()
		tables[key] = t
		order = append(order, key)
	}

	// columns (+ comments via pg_catalog)
	colQ := `
SELECT c.table_schema AS schema, c.table_name AS name, c.column_name AS col,
       c.ordinal_position AS ord, c.data_type AS data_type,
       CASE WHEN c.character_maximum_length IS NOT NULL
              THEN c.data_type || '(' || c.character_maximum_length || ')'
            WHEN c.numeric_precision IS NOT NULL AND c.numeric_scale IS NOT NULL AND c.data_type IN ('numeric','decimal')
              THEN c.data_type || '(' || c.numeric_precision || ',' || c.numeric_scale || ')'
            ELSE c.data_type END AS full_type,
       c.is_nullable AS nullable, COALESCE(c.column_default, '') AS col_default,
       COALESCE(c.generation_expression, '') AS gen_expr,
       COALESCE(col_description(fc.oid, c.ordinal_position::int), '') AS comment
FROM information_schema.columns c
JOIN pg_namespace n ON n.nspname = c.table_schema
JOIN pg_class fc ON fc.relname = c.table_name AND fc.relnamespace = n.oid
WHERE c.table_schema NOT IN ('pg_catalog','information_schema','pg_toast')` +
		pgSchemaFilterCol(req.Schemas) + ` ORDER BY c.table_schema, c.table_name, c.ordinal_position`
	colRows, err := c.q.SystemQuery(ctx, req.SourceID, colQ, params...)
	if err != nil {
		return nil, err
	}
	for _, r := range colRows {
		key := asString(r["schema"]) + "." + asString(r["name"])
		t := tables[key]
		if t == nil {
			continue
		}
		t.Columns = append(t.Columns, ColumnAsset{
			Name: asString(r["col"]), Ordinal: int(asInt64(r["ord"])),
			DataType: asString(r["data_type"]), FullType: asString(r["full_type"]),
			Nullable: asBool(r["nullable"], "yes"), Default: asString(r["col_default"]),
			Generated: asString(r["gen_expr"]), Comment: asString(r["comment"]),
		})
	}

	// constraints (PK/FK/UNIQUE/CHECK) — read from pg_catalog, NOT
	// information_schema.table_constraints, because the latter is invisible to
	// a SELECT-only account (it requires INSERT/UPDATE/REFERENCES/... privilege
	// to list a table's constraints). pg_constraint is readable by any role,
	// which is exactly what a read-only metadata-collection account has.
	conQ := `
SELECT n.nspname AS schema, rel.relname AS name, con.conname AS cname,
       CASE con.contype WHEN 'p' THEN 'PRIMARY KEY' WHEN 'f' THEN 'FOREIGN KEY'
            WHEN 'u' THEN 'UNIQUE' WHEN 'c' THEN 'CHECK' ELSE con.contype::text END AS ctype,
       COALESCE(a.attname, '') AS col, COALESCE(k.ord, 0) AS col_ord,
       COALESCE(fn.nspname, '') AS ref_schema, COALESCE(frel.relname, '') AS ref_table,
       COALESCE(fa.attname, '') AS ref_col,
       CASE WHEN con.contype = 'c' THEN COALESCE(pg_get_constraintdef(con.oid), '') ELSE '' END AS check_clause
FROM pg_constraint con
JOIN pg_class rel ON rel.oid = con.conrelid
JOIN pg_namespace n ON n.oid = rel.relnamespace
LEFT JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
LEFT JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
LEFT JOIN pg_class frel ON frel.oid = con.confrelid
LEFT JOIN pg_namespace fn ON fn.oid = frel.relnamespace
LEFT JOIN pg_attribute fa ON fa.attrelid = con.confrelid
     AND fa.attnum = con.confkey[k.ord]
WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')` +
		pgSchemaFilterN(req.Schemas) + ` ORDER BY n.nspname, rel.relname, con.conname, k.ord`
	conRows, err := c.q.SystemQuery(ctx, req.SourceID, conQ, params...)
	if err == nil {
		accumConstraints(tables, conRows)
	}

	// indexes
	{
		idxQ := `
SELECT n.nspname AS schema, t.relname AS name, i.relname AS iname,
       ix.indisunique AS is_unique, ix.indisprimary AS is_primary,
       a.attname AS col, array_position(ix.indkey, a.attnum) AS col_pos
FROM pg_index ix
JOIN pg_class i ON i.oid = ix.indexrelid
JOIN pg_class t ON t.oid = ix.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(ix.indkey)
WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')` +
			pgSchemaFilterN(req.Schemas) + ` ORDER BY n.nspname, t.relname, i.relname, col_pos`
		if idxRows, err := c.q.SystemQuery(ctx, req.SourceID, idxQ, params...); err == nil {
			accumIndexes(tables, idxRows)
		}
	}

	out := make([]TableAsset, 0, len(order))
	for _, k := range order {
		out = append(out, *tables[k])
	}
	return out, nil
}

// pgSchemaFilter builds a "AND n.nspname = ANY($1)" clause and params.
func pgSchemaFilter(schemas []string) (string, []any) {
	list := cleanSchemas(schemas)
	if len(list) == 0 {
		return "", nil
	}
	return " AND n.nspname = ANY($1)", []any{pgArray(list)}
}
func pgSchemaFilterCol(schemas []string) string {
	if len(cleanSchemas(schemas)) == 0 {
		return ""
	}
	return " AND c.table_schema = ANY($1)"
}
func pgSchemaFilterN(schemas []string) string {
	if len(cleanSchemas(schemas)) == 0 {
		return ""
	}
	return " AND n.nspname = ANY($1)"
}

// pgArray renders a Go slice as a Postgres text array literal for = ANY().
func pgArray(list []string) string {
	esc := make([]string, len(list))
	for i, s := range list {
		esc[i] = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "{" + strings.Join(esc, ",") + "}"
}
