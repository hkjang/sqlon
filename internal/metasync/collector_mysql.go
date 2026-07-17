package metasync

import (
	"context"
	"sort"
	"strings"
)

// shared constraint/index accumulation from information_schema-shaped rows.
// Row keys: schema,name,cname,ctype,col,col_ord,ref_schema,ref_table,ref_col,check_clause.
func accumConstraints(tables map[string]*TableAsset, rows []map[string]any) {
	type key struct{ tbl, cname string }
	acc := map[key]*ConstraintAsset{}
	var order []key
	for _, r := range rows {
		tbl := asString(r["schema"]) + "." + asString(r["name"])
		if tables[tbl] == nil {
			continue
		}
		k := key{tbl, asString(r["cname"])}
		ca := acc[k]
		if ca == nil {
			ca = &ConstraintAsset{
				Name: asString(r["cname"]), Type: strings.ToUpper(asString(r["ctype"])),
				RefSchema: asString(r["ref_schema"]), RefTable: asString(r["ref_table"]),
				Definition: strings.TrimSpace(asString(r["check_clause"])),
			}
			acc[k] = ca
			order = append(order, k)
		}
		if col := asString(r["col"]); col != "" && !contains(ca.Columns, col) {
			ca.Columns = append(ca.Columns, col)
		}
		if rc := asString(r["ref_col"]); rc != "" && !contains(ca.RefColumns, rc) {
			ca.RefColumns = append(ca.RefColumns, rc)
		}
	}
	// attach + set column PK/FK flags
	for _, k := range order {
		ca := acc[k]
		t := tables[k.tbl]
		t.Constraints = append(t.Constraints, *ca)
		for i := range t.Columns {
			if contains(ca.Columns, t.Columns[i].Name) {
				switch ca.Type {
				case "PRIMARY KEY":
					t.Columns[i].IsPrimaryKey = true
				case "FOREIGN KEY":
					t.Columns[i].IsForeignKey = true
				}
			}
		}
	}
}

// row keys: schema,name,iname,is_unique,is_primary,col,col_pos
func accumIndexes(tables map[string]*TableAsset, rows []map[string]any) {
	type key struct{ tbl, iname string }
	acc := map[key]*IndexAsset{}
	var order []key
	for _, r := range rows {
		tbl := asString(r["schema"]) + "." + asString(r["name"])
		if tables[tbl] == nil {
			continue
		}
		k := key{tbl, asString(r["iname"])}
		ia := acc[k]
		if ia == nil {
			ia = &IndexAsset{
				Name:    asString(r["iname"]),
				Unique:  asBool(r["is_unique"], "1", "t", "true", "yes"),
				Primary: asBool(r["is_primary"], "1", "t", "true", "yes"),
			}
			acc[k] = ia
			order = append(order, k)
		}
		if col := asString(r["col"]); col != "" {
			ia.Columns = append(ia.Columns, col)
		}
	}
	for _, k := range order {
		tables[k.tbl].Indexes = append(tables[k.tbl].Indexes, *acc[k])
	}
}

// MySQL / MariaDB system-catalog collection. information_schema carries
// tables, columns (with COLUMN_TYPE, comments), key_column_usage (PK/FK),
// statistics (indexes), and views.
func (c *Collector) collectMySQL(ctx context.Context, req CollectRequest, dialect string) ([]TableAsset, error) {
	schemas := cleanSchemas(req.Schemas)
	inClause, params := mysqlSchemaIn(schemas)

	tblQ := `
SELECT table_schema AS ` + "`schema`" + `, table_name AS name,
       CASE WHEN table_type = 'VIEW' THEN 'view' ELSE 'table' END AS kind,
       COALESCE(table_comment, '') AS comment,
       COALESCE(table_rows, 0) AS est_rows
FROM information_schema.tables
WHERE table_schema NOT IN ('mysql','information_schema','performance_schema','sys')` +
		inClause + ` ORDER BY table_schema, table_name`
	tblRows, err := c.q.SystemQuery(ctx, req.SourceID, tblQ, params...)
	if err != nil {
		return nil, err
	}
	tables := map[string]*TableAsset{}
	var order []string
	for _, r := range tblRows {
		kind := asString(r["kind"])
		if !req.IncludeViews && kind == "view" {
			continue
		}
		t := &TableAsset{
			Schema: asString(r["schema"]), Name: asString(r["name"]), Kind: kind,
			Comment: asString(r["comment"]), EstRowCount: asInt64(r["est_rows"]),
		}
		tables[t.FQN()] = t
		order = append(order, t.FQN())
	}

	colQ := `
SELECT table_schema AS ` + "`schema`" + `, table_name AS name, column_name AS col,
       ordinal_position AS ord, data_type AS data_type, column_type AS full_type,
       is_nullable AS nullable, COALESCE(column_default, '') AS col_default,
       COALESCE(generation_expression, '') AS gen_expr, COALESCE(column_comment, '') AS comment
FROM information_schema.columns
WHERE table_schema NOT IN ('mysql','information_schema','performance_schema','sys')` +
		inClause + ` ORDER BY table_schema, table_name, ordinal_position`
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

	// PK / UNIQUE constraints — derived from information_schema.statistics
	// (index metadata), which a SELECT-only account CAN see. MySQL/MariaDB
	// read-only users cannot list information_schema.table_constraints, so we
	// synthesize PK/UNIQUE from the PRIMARY index and unique (non_unique=0)
	// indexes instead. This is the read-only-safe equivalent.
	pkQ := `
SELECT table_schema AS ` + "`schema`" + `, table_name AS name, index_name AS cname,
       CASE WHEN index_name = 'PRIMARY' THEN 'PRIMARY KEY' ELSE 'UNIQUE' END AS ctype,
       column_name AS col, seq_in_index AS col_ord,
       '' AS ref_schema, '' AS ref_table, '' AS ref_col, '' AS check_clause
FROM information_schema.statistics
WHERE non_unique = 0 AND table_schema NOT IN ('mysql','information_schema','performance_schema','sys')` +
		inClause + ` ORDER BY table_schema, table_name, index_name, seq_in_index`
	if conRows, err := c.q.SystemQuery(ctx, req.SourceID, pkQ, params...); err == nil {
		accumConstraints(tables, conRows)
	}
	// FK constraints — best effort via key_column_usage. Visible to MySQL
	// read-only users; on MariaDB a SELECT-only account may not see referential
	// rows, in which case FKs are simply not captured (a documented privilege
	// limitation, not a failure).
	fkQ := `
SELECT kcu.table_schema AS ` + "`schema`" + `, kcu.table_name AS name, kcu.constraint_name AS cname,
       'FOREIGN KEY' AS ctype, kcu.column_name AS col, kcu.ordinal_position AS col_ord,
       COALESCE(kcu.referenced_table_schema,'') AS ref_schema,
       COALESCE(kcu.referenced_table_name,'') AS ref_table,
       COALESCE(kcu.referenced_column_name,'') AS ref_col, '' AS check_clause
FROM information_schema.key_column_usage kcu
WHERE kcu.referenced_table_name IS NOT NULL
  AND kcu.table_schema NOT IN ('mysql','information_schema','performance_schema','sys')` +
		mysqlKCUFilter(schemas) + ` ORDER BY kcu.table_schema, kcu.table_name, kcu.constraint_name, kcu.ordinal_position`
	if fkRows, err := c.q.SystemQuery(ctx, req.SourceID, fkQ, params...); err == nil {
		accumConstraints(tables, fkRows)
	}

	{
		idxQ := `
SELECT table_schema AS ` + "`schema`" + `, table_name AS name, index_name AS iname,
       CASE WHEN non_unique = 0 THEN 1 ELSE 0 END AS is_unique,
       CASE WHEN index_name = 'PRIMARY' THEN 1 ELSE 0 END AS is_primary,
       column_name AS col, seq_in_index AS col_pos
FROM information_schema.statistics
WHERE table_schema NOT IN ('mysql','information_schema','performance_schema','sys')` +
			inClause + ` ORDER BY table_schema, table_name, index_name, seq_in_index`
		if idxRows, err := c.q.SystemQuery(ctx, req.SourceID, idxQ, params...); err == nil {
			accumIndexes(tables, idxRows)
		}
	}

	// view SQL
	if req.IncludeViews {
		viewQ := `
SELECT table_schema AS ` + "`schema`" + `, table_name AS name, view_definition AS view_sql
FROM information_schema.views
WHERE table_schema NOT IN ('mysql','information_schema','performance_schema','sys')` + inClause
		if vRows, err := c.q.SystemQuery(ctx, req.SourceID, viewQ, params...); err == nil {
			for _, r := range vRows {
				if t := tables[asString(r["schema"])+"."+asString(r["name"])]; t != nil {
					t.ViewSQL = strings.TrimSpace(asString(r["view_sql"]))
				}
			}
		}
	}

	out := make([]TableAsset, 0, len(order))
	for _, k := range order {
		out = append(out, *tables[k])
	}
	return out, nil
}

func mysqlSchemaIn(schemas []string) (string, []any) {
	if len(schemas) == 0 {
		return "", nil
	}
	ph := make([]string, len(schemas))
	params := make([]any, len(schemas))
	for i, s := range schemas {
		ph[i] = "?"
		params[i] = s
	}
	return " AND table_schema IN (" + strings.Join(ph, ",") + ")", params
}

func mysqlKCUFilter(schemas []string) string {
	if len(schemas) == 0 {
		return ""
	}
	ph := make([]string, len(schemas))
	for i := range schemas {
		ph[i] = "?"
	}
	return " AND kcu.table_schema IN (" + strings.Join(ph, ",") + ")"
}

func cleanSchemas(schemas []string) []string {
	var out []string
	for _, s := range schemas {
		s = strings.TrimSpace(s)
		if s != "" && !systemSchemas[strings.ToLower(s)] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
