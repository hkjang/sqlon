package metasync

import (
	"context"
	"fmt"
	"strings"
)

// collectOracle uses ALL_* dictionary views only. These are base-license
// metadata sources and respect the connected account's object privileges.
func (c *Collector) collectOracle(ctx context.Context, req CollectRequest) ([]TableAsset, error) {
	filter, args := oracleOwnerFilter(req.Schemas, "t.owner")
	tableSQL := `SELECT t.owner AS "schema", t.table_name AS "name", 'table' AS "kind",
 NVL(tc.comments,'') AS "comment", NVL(t.num_rows,0) AS "est_rows", '' AS "view_sql"
 FROM all_tables t LEFT JOIN all_tab_comments tc ON tc.owner=t.owner AND tc.table_name=t.table_name
 WHERE t.owner NOT IN ('SYS','SYSTEM','XDB','OUTLN','DBSNMP')` + filter
	rows, err := c.q.SystemQuery(ctx, req.SourceID, tableSQL, args...)
	if err != nil {
		return nil, err
	}
	if req.IncludeViews {
		vf, va := oracleOwnerFilter(req.Schemas, "v.owner")
		viewSQL := `SELECT v.owner AS "schema", v.view_name AS "name", 'view' AS "kind",
 NVL(tc.comments,'') AS "comment", 0 AS "est_rows", v.text AS "view_sql"
 FROM all_views v LEFT JOIN all_tab_comments tc ON tc.owner=v.owner AND tc.table_name=v.view_name
 WHERE v.owner NOT IN ('SYS','SYSTEM','XDB','OUTLN','DBSNMP')` + vf
		vr, e := c.q.SystemQuery(ctx, req.SourceID, viewSQL, va...)
		if e == nil {
			rows = append(rows, vr...)
		}
	}
	tables := map[string]*TableAsset{}
	order := []string{}
	for _, r := range rows {
		t := &TableAsset{Schema: asString(r["schema"]), Name: asString(r["name"]), Kind: asString(r["kind"]), Comment: asString(r["comment"]), EstRowCount: asInt64(r["est_rows"]), ViewSQL: strings.TrimSpace(asString(r["view_sql"]))}
		tables[t.FQN()] = t
		order = append(order, t.FQN())
	}
	cf, ca := oracleOwnerFilter(req.Schemas, "c.owner")
	colSQL := `SELECT c.owner AS "schema", c.table_name AS "name", c.column_name AS "col", c.column_id AS "ord",
 c.data_type AS "data_type", c.data_type || CASE WHEN c.data_type IN ('VARCHAR2','CHAR','NVARCHAR2','NCHAR') THEN '('||c.char_length||')' WHEN c.data_precision IS NOT NULL THEN '('||c.data_precision||NVL2(c.data_scale,','||c.data_scale,'')||')' ELSE '' END AS "full_type",
 c.nullable AS "nullable", c.data_default AS "col_default", c.virtual_column AS "gen_expr", NVL(cc.comments,'') AS "comment"
 FROM all_tab_columns c LEFT JOIN all_col_comments cc ON cc.owner=c.owner AND cc.table_name=c.table_name AND cc.column_name=c.column_name
 WHERE c.owner NOT IN ('SYS','SYSTEM','XDB','OUTLN','DBSNMP')` + cf + ` ORDER BY c.owner,c.table_name,c.column_id`
	cr, err := c.q.SystemQuery(ctx, req.SourceID, colSQL, ca...)
	if err != nil {
		return nil, err
	}
	for _, r := range cr {
		if t := tables[asString(r["schema"])+"."+asString(r["name"])]; t != nil {
			t.Columns = append(t.Columns, ColumnAsset{Name: asString(r["col"]), Ordinal: int(asInt64(r["ord"])), DataType: asString(r["data_type"]), FullType: asString(r["full_type"]), Nullable: asBool(r["nullable"], "y"), Default: asString(r["col_default"]), Generated: asString(r["gen_expr"]), Comment: asString(r["comment"])})
		}
	}
	qf, qa := oracleOwnerFilter(req.Schemas, "c.owner")
	conSQL := `SELECT c.owner AS "schema", c.table_name AS "name", c.constraint_name AS "cname", DECODE(c.constraint_type,'P','PRIMARY KEY','R','FOREIGN KEY','U','UNIQUE','C','CHECK',c.constraint_type) AS "ctype", cc.column_name AS "col", cc.position AS "col_ord", NVL(rc.owner,'') AS "ref_schema", NVL(rc.table_name,'') AS "ref_table", NVL(rcc.column_name,'') AS "ref_col", NVL(c.search_condition_vc,'') AS "check_clause" FROM all_constraints c LEFT JOIN all_cons_columns cc ON cc.owner=c.owner AND cc.constraint_name=c.constraint_name LEFT JOIN all_constraints rc ON rc.owner=c.r_owner AND rc.constraint_name=c.r_constraint_name LEFT JOIN all_cons_columns rcc ON rcc.owner=rc.owner AND rcc.constraint_name=rc.constraint_name AND rcc.position=cc.position WHERE c.constraint_type IN ('P','R','U','C')` + qf
	if rr, e := c.q.SystemQuery(ctx, req.SourceID, conSQL, qa...); e == nil {
		accumConstraints(tables, rr)
	}
	if ir, e := c.q.SystemQuery(ctx, req.SourceID, `SELECT i.table_owner AS "schema", i.table_name AS "name", i.index_name AS "iname", DECODE(i.uniqueness,'UNIQUE',1,0) AS "is_unique", DECODE(c.constraint_type,'P',1,0) AS "is_primary", ic.column_name AS "col", ic.column_position AS "col_pos" FROM all_indexes i JOIN all_ind_columns ic ON ic.index_owner=i.owner AND ic.index_name=i.index_name LEFT JOIN all_constraints c ON c.owner=i.table_owner AND c.index_name=i.index_name WHERE i.table_owner NOT IN ('SYS','SYSTEM','XDB','OUTLN','DBSNMP')`+strings.ReplaceAll(qf, "c.owner", "i.table_owner"), qa...); e == nil {
		accumIndexes(tables, ir)
	}
	out := make([]TableAsset, 0, len(order))
	for _, k := range order {
		out = append(out, *tables[k])
	}
	return out, nil
}

func oracleOwnerFilter(schemas []string, column string) (string, []any) {
	list := cleanSchemas(schemas)
	if len(list) == 0 {
		return "", nil
	}
	ph := make([]string, len(list))
	args := make([]any, len(list))
	for i, s := range list {
		ph[i] = fmt.Sprintf(":%d", i+1)
		args[i] = strings.ToUpper(s)
	}
	return " AND " + column + " IN (" + strings.Join(ph, ",") + ")", args
}
