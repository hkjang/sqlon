package dbconn

import (
	"fmt"
	"sort"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func parsePostgresPolicyCondition(condition string, allowed map[string]struct{}) (*PolicyCondition, error) {
	tree, err := pgquery.Parse("SELECT 1 WHERE (" + condition + ")")
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL predicate condition: %v", sanitizeParseError(err))
	}
	sel, err := postgresSingleSelect(tree)
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL predicate condition: %w", err)
	}
	if sel.WhereClause == nil {
		return nil, fmt.Errorf("invalid PostgreSQL predicate condition: WHERE expression is missing")
	}

	columnSet := map[string]struct{}{}
	var atoms []string
	for _, atom := range flattenPostgresAnd(sel.WhereClause) {
		key, cols, err := canonicalPostgresConditionAtom(atom, allowed)
		if err != nil {
			return nil, err
		}
		atoms = append(atoms, key)
		for _, col := range cols {
			columnSet[col] = struct{}{}
		}
	}
	if len(columnSet) == 0 {
		return nil, fmt.Errorf("predicate condition must reference at least one allowed table column")
	}
	columns := make([]string, 0, len(columnSet))
	for col := range columnSet {
		columns = append(columns, col)
	}
	return &PolicyCondition{columns: columns, atoms: atoms, pg: tree}, nil
}

func renderPostgresPolicyCondition(c *PolicyCondition, alias string) (string, error) {
	tree, ok := c.pg.(*pg.ParseResult)
	if !ok || tree == nil {
		return "", fmt.Errorf("PostgreSQL predicate AST is unavailable")
	}
	cloned := proto.Clone(tree).(*pg.ParseResult)
	if err := walkPostgresProto(cloned.ProtoReflect(), func(m protoreflect.Message) error {
		cr, ok := m.Interface().(*pg.ColumnRef)
		if !ok {
			return nil
		}
		parts, err := postgresColumnParts(cr)
		if err != nil {
			return err
		}
		col := parts[len(parts)-1]
		cr.Fields = []*pg.Node{postgresStringNode(alias), postgresStringNode(col)}
		return nil
	}); err != nil {
		return "", err
	}
	out, err := pgquery.Deparse(cloned)
	if err != nil {
		return "", fmt.Errorf("deparse PostgreSQL predicate condition: %w", err)
	}
	upper := strings.ToUpper(out)
	idx := strings.Index(upper, " WHERE ")
	if idx < 0 {
		return "", fmt.Errorf("deparsed PostgreSQL predicate has no WHERE clause")
	}
	return strings.TrimSpace(out[idx+len(" WHERE "):]), nil
}

func matchPostgresRequiredPredicates(sqlText string, policies []*CompiledPredicatePolicy) (PredicateReport, error) {
	tree, err := pgquery.Parse(sqlText)
	if err != nil {
		return PredicateReport{}, fmt.Errorf("parse PostgreSQL policy query: %v", sanitizeParseError(err))
	}
	if _, err := postgresSingleSelect(tree); err != nil {
		return PredicateReport{}, fmt.Errorf("parse PostgreSQL policy query: %w", err)
	}

	var report PredicateReport
	block := 0
	err = walkPostgresQueryBlocks(tree.ProtoReflect(), nil, func(sel *pg.SelectStmt, scope map[string]struct{}) error {
		block++
		occurrences, sourceCount := postgresDirectOccurrences(sel, scope, block)
		for i := range occurrences {
			occurrences[i].sourceCount = sourceCount
			occurrences[i].where = sel.WhereClause
			for _, p := range policies {
				if !policyApplies(p, occurrences[i]) {
					continue
				}
				have, err := postgresQueryAtoms(sel.WhereClause, occurrences[i], p.Condition.allowedColumns)
				if err != nil {
					return fmt.Errorf("query block %d alias %q: %w", block, occurrences[i].alias, err)
				}
				matched := sel.WhereClause != nil && containsAllAtoms(have, p.Condition.atoms)
				report.Matches = append(report.Matches, PredicateMatch{
					PolicyID: p.PolicyID, QueryBlock: block,
					Schema: occurrences[i].schema, Table: occurrences[i].table, Alias: occurrences[i].alias,
					Condition: p.Condition.raw, Matched: matched,
					Reason: matchReason(sel.WhereClause != nil, matched),
				})
			}
		}
		return nil
	})
	if err != nil {
		return PredicateReport{}, err
	}
	return report, nil
}

func postgresSingleSelect(tree *pg.ParseResult) (*pg.SelectStmt, error) {
	if tree == nil || len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("exactly one statement is required")
	}
	sel := tree.Stmts[0].GetStmt().GetSelectStmt()
	if sel == nil {
		return nil, fmt.Errorf("a SELECT/WITH query is required")
	}
	return sel, nil
}

func flattenPostgresAnd(node *pg.Node) []*pg.Node {
	if node == nil {
		return nil
	}
	if b := node.GetBoolExpr(); b != nil && b.Boolop == pg.BoolExprType_AND_EXPR {
		var out []*pg.Node
		for _, arg := range b.Args {
			out = append(out, flattenPostgresAnd(arg)...)
		}
		return out
	}
	return []*pg.Node{node}
}

func canonicalPostgresConditionAtom(node *pg.Node, allowed map[string]struct{}) (string, []string, error) {
	cloned := proto.Clone(node).(*pg.Node)
	columnSet := map[string]struct{}{}
	err := normalizePostgresNode(cloned, true, func(parts []string) (string, bool) {
		col := strings.ToLower(parts[len(parts)-1])
		if _, ok := allowed[col]; !ok {
			return "", false
		}
		columnSet[col] = struct{}{}
		return col, true
	})
	if err != nil {
		return "", nil, fmt.Errorf("invalid PostgreSQL predicate condition: %w", err)
	}
	columns := make([]string, 0, len(columnSet))
	for col := range columnSet {
		columns = append(columns, col)
	}
	sort.Strings(columns)
	key, err := marshalPostgresCanonical(cloned)
	return key, columns, err
}

func postgresQueryAtoms(where *pg.Node, occ policyTableOccurrence, allowed map[string]struct{}) ([]string, error) {
	if where == nil {
		return nil, nil
	}
	var out []string
	for _, atom := range flattenPostgresAnd(where) {
		cloned := proto.Clone(atom).(*pg.Node)
		eligible := true
		err := normalizePostgresNode(cloned, false, func(parts []string) (string, bool) {
			col := strings.ToLower(parts[len(parts)-1])
			if len(parts) == 1 {
				if occ.sourceCount != 1 {
					eligible = false
					return "", false
				}
				return col, true
			}
			if !postgresQualifierMatches(parts[:len(parts)-1], occ) {
				eligible = false
				return "", false
			}
			return col, true
		})
		if err != nil || !eligible {
			continue
		}
		key, err := marshalPostgresCanonical(cloned)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, nil
}

func normalizePostgresNode(node proto.Message, rejectSubquery bool, bind func([]string) (string, bool)) error {
	return walkPostgresProto(node.ProtoReflect(), func(m protoreflect.Message) error {
		if rejectSubquery {
			if _, ok := m.Interface().(*pg.SubLink); ok {
				return fmt.Errorf("subqueries are not allowed in predicate conditions")
			}
		}
		cr, ok := m.Interface().(*pg.ColumnRef)
		if !ok {
			return nil
		}
		parts, err := postgresColumnParts(cr)
		if err != nil {
			return err
		}
		col, ok := bind(parts)
		if !ok {
			return fmt.Errorf("column %q is not allowed or is bound to another table", strings.Join(parts, "."))
		}
		cr.Fields = []*pg.Node{postgresStringNode(col)}
		return nil
	})
}

func marshalPostgresCanonical(node proto.Message) (string, error) {
	cloned := proto.Clone(node)
	if err := clearPostgresLocations(cloned.ProtoReflect()); err != nil {
		return "", err
	}
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(cloned)
	if err != nil {
		return "", fmt.Errorf("marshal canonical PostgreSQL predicate: %w", err)
	}
	return string(b), nil
}

func clearPostgresLocations(m protoreflect.Message) error {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		name := string(fd.Name())
		if name == "location" || name == "stmt_location" || name == "stmt_len" {
			m.Clear(fd)
			continue
		}
		if !m.Has(fd) || fd.Kind() != protoreflect.MessageKind {
			continue
		}
		if fd.IsList() {
			list := m.Get(fd).List()
			for j := 0; j < list.Len(); j++ {
				if err := clearPostgresLocations(list.Get(j).Message()); err != nil {
					return err
				}
			}
		} else {
			if err := clearPostgresLocations(m.Get(fd).Message()); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkPostgresProto(m protoreflect.Message, visit func(protoreflect.Message) error) error {
	if err := visit(m); err != nil {
		return err
	}
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !m.Has(fd) || fd.Kind() != protoreflect.MessageKind {
			continue
		}
		if fd.IsList() {
			list := m.Get(fd).List()
			for j := 0; j < list.Len(); j++ {
				if err := walkPostgresProto(list.Get(j).Message(), visit); err != nil {
					return err
				}
			}
		} else if err := walkPostgresProto(m.Get(fd).Message(), visit); err != nil {
			return err
		}
	}
	return nil
}

func postgresColumnParts(cr *pg.ColumnRef) ([]string, error) {
	if cr == nil || len(cr.Fields) == 0 {
		return nil, fmt.Errorf("empty PostgreSQL column reference")
	}
	parts := make([]string, 0, len(cr.Fields))
	for _, field := range cr.Fields {
		s := field.GetString_()
		if s == nil || s.Sval == "" {
			return nil, fmt.Errorf("wildcard or complex column references are not allowed in predicate conditions")
		}
		parts = append(parts, s.Sval)
	}
	return parts, nil
}

func postgresStringNode(s string) *pg.Node {
	return &pg.Node{Node: &pg.Node_String_{String_: &pg.String{Sval: s}}}
}

func postgresQualifierMatches(q []string, occ policyTableOccurrence) bool {
	if len(q) == 1 {
		return strings.EqualFold(q[0], occ.alias) || strings.EqualFold(q[0], occ.table)
	}
	if len(q) >= 2 {
		return strings.EqualFold(q[len(q)-2], occ.schema) && strings.EqualFold(q[len(q)-1], occ.table)
	}
	return false
}

func postgresDirectOccurrences(sel *pg.SelectStmt, scope map[string]struct{}, block int) ([]policyTableOccurrence, int) {
	var out []policyTableOccurrence
	sources := 0
	var collect func(*pg.Node)
	collect = func(node *pg.Node) {
		if node == nil {
			return
		}
		if rv := node.GetRangeVar(); rv != nil {
			sources++
			if rv.Schemaname == "" {
				if _, isCTE := scope[strings.ToLower(rv.Relname)]; isCTE {
					return
				}
			}
			alias := rv.Relname
			if rv.Alias != nil && rv.Alias.Aliasname != "" {
				alias = rv.Alias.Aliasname
			}
			out = append(out, policyTableOccurrence{queryBlock: block, schema: rv.Schemaname, table: rv.Relname, alias: alias})
			return
		}
		if join := node.GetJoinExpr(); join != nil {
			collect(join.Larg)
			collect(join.Rarg)
			return
		}
		if sample := node.GetRangeTableSample(); sample != nil {
			collect(sample.Relation)
			return
		}
		if node.GetRangeSubselect() != nil || node.GetRangeFunction() != nil || node.GetRangeTableFunc() != nil {
			sources++
		}
	}
	for _, node := range sel.FromClause {
		collect(node)
	}
	return out, sources
}

func walkPostgresQueryBlocks(root protoreflect.Message, scope map[string]struct{}, visit func(*pg.SelectStmt, map[string]struct{}) error) error {
	nextScope := scope
	if sel, ok := root.Interface().(*pg.SelectStmt); ok {
		nextScope = copyNameSet(scope)
		if sel.WithClause != nil {
			for _, n := range sel.WithClause.Ctes {
				if cte := n.GetCommonTableExpr(); cte != nil {
					nextScope[strings.ToLower(cte.Ctename)] = struct{}{}
				}
			}
		}
		if err := visit(sel, nextScope); err != nil {
			return err
		}
	}
	fields := root.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !root.Has(fd) || fd.Kind() != protoreflect.MessageKind {
			continue
		}
		if fd.IsList() {
			list := root.Get(fd).List()
			for j := 0; j < list.Len(); j++ {
				if err := walkPostgresQueryBlocks(list.Get(j).Message(), nextScope, visit); err != nil {
					return err
				}
			}
		} else if err := walkPostgresQueryBlocks(root.Get(fd).Message(), nextScope, visit); err != nil {
			return err
		}
	}
	return nil
}

func copyNameSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in)+2)
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}
