package dbconn

import (
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	"github.com/pingcap/tidb/pkg/parser/opcode"
)

func parseMySQLPolicyCondition(condition string, allowed map[string]struct{}, mariadb bool) (*PolicyCondition, error) {
	sel, err := parseMySQLConditionSelect(condition, mariadb)
	if err != nil {
		return nil, err
	}
	columnSet := map[string]struct{}{}
	var atoms []string
	for _, atom := range flattenMySQLAnd(sel.Where) {
		v := &mysqlColumnVisitor{allowed: allowed, columns: columnSet, rejectSubquery: true}
		if _, ok := atom.Accept(v); !ok || v.err != nil {
			if v.err == nil {
				v.err = fmt.Errorf("could not inspect predicate condition")
			}
			return nil, fmt.Errorf("invalid %s predicate condition: %w", mysqlDialectName(mariadb), v.err)
		}
		key, err := restoreMySQLExpression(atom, true)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s predicate condition: %w", mysqlDialectName(mariadb), err)
		}
		atoms = append(atoms, key)
	}
	if len(columnSet) == 0 {
		return nil, fmt.Errorf("predicate condition must reference at least one allowed table column")
	}
	columns := make([]string, 0, len(columnSet))
	for col := range columnSet {
		columns = append(columns, col)
	}
	return &PolicyCondition{columns: columns, atoms: atoms, mysql: sel.Where}, nil
}

func renderMySQLPolicyCondition(c *PolicyCondition, alias string, mariadb bool) (string, error) {
	sel, err := parseMySQLConditionSelect(c.raw, mariadb)
	if err != nil {
		return "", err
	}
	v := &mysqlAliasRenderVisitor{alias: alias}
	node, ok := sel.Where.Accept(v)
	if !ok || v.err != nil {
		if v.err == nil {
			v.err = fmt.Errorf("could not bind predicate columns")
		}
		return "", v.err
	}
	expr, ok := node.(ast.ExprNode)
	if !ok {
		return "", fmt.Errorf("bound predicate is not an expression")
	}
	return restoreMySQLExpression(expr, false)
}

func matchMySQLRequiredPredicates(sqlText string, policies []*CompiledPredicatePolicy, mariadb bool) (PredicateReport, error) {
	// MySQL/MariaDB execute version comments as SQL. The TiDB grammar may treat
	// an unknown future version comment as trivia, so policy validation rejects
	// executable comments rather than risking parser/server disagreement.
	upper := strings.ToUpper(sqlText)
	if strings.Contains(upper, "/*!") || strings.Contains(upper, "/*M!") {
		return PredicateReport{}, fmt.Errorf("executable MySQL/MariaDB comments are not allowed in policy-checked SQL")
	}
	p := newMySQLParser(mariadb)
	stmts, _, err := p.ParseSQL(sqlText)
	if err != nil {
		return PredicateReport{}, fmt.Errorf("parse %s policy query: %v", mysqlDialectName(mariadb), sanitizeParseError(err))
	}
	if len(stmts) != 1 {
		return PredicateReport{}, fmt.Errorf("parse %s policy query: exactly one statement is required", mysqlDialectName(mariadb))
	}
	switch stmts[0].(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
	default:
		return PredicateReport{}, fmt.Errorf("parse %s policy query: a SELECT/WITH query is required", mysqlDialectName(mariadb))
	}

	v := &mysqlPolicyBlockVisitor{policies: policies}
	if _, ok := stmts[0].Accept(v); !ok || v.err != nil {
		if v.err == nil {
			v.err = fmt.Errorf("could not walk %s query blocks", mysqlDialectName(mariadb))
		}
		return PredicateReport{}, v.err
	}
	return PredicateReport{Matches: v.matches}, nil
}

func parseMySQLConditionSelect(condition string, mariadb bool) (*ast.SelectStmt, error) {
	p := newMySQLParser(mariadb)
	stmts, _, err := p.ParseSQL("SELECT 1 WHERE (" + condition + ")")
	if err != nil {
		return nil, fmt.Errorf("invalid %s predicate condition: %v", mysqlDialectName(mariadb), sanitizeParseError(err))
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("invalid %s predicate condition: exactly one expression is required", mysqlDialectName(mariadb))
	}
	sel, ok := stmts[0].(*ast.SelectStmt)
	if !ok || sel.Where == nil {
		return nil, fmt.Errorf("invalid %s predicate condition: WHERE expression is missing", mysqlDialectName(mariadb))
	}
	return sel, nil
}

func newMySQLParser(mariadb bool) *parser.Parser {
	p := parser.New()
	p.EnableWindowFunc(true)
	if mariadb {
		p.SetMariaDB(true)
	}
	return p
}

func mysqlDialectName(mariadb bool) string {
	if mariadb {
		return "MariaDB"
	}
	return "MySQL"
}

func flattenMySQLAnd(expr ast.ExprNode) []ast.ExprNode {
	for {
		p, ok := expr.(*ast.ParenthesesExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	if b, ok := expr.(*ast.BinaryOperationExpr); ok && b.Op == opcode.LogicAnd {
		out := flattenMySQLAnd(b.L)
		return append(out, flattenMySQLAnd(b.R)...)
	}
	return []ast.ExprNode{expr}
}

func restoreMySQLExpression(expr ast.ExprNode, stripQualifier bool) (string, error) {
	flags := format.DefaultRestoreFlags | format.RestoreSkipRedundantParentheses
	if stripQualifier {
		flags |= format.RestoreWithoutSchemaName | format.RestoreWithoutTableName
	}
	var b strings.Builder
	ctx := format.NewRestoreCtx(flags, &b)
	if err := expr.Restore(ctx); err != nil {
		return "", err
	}
	return b.String(), nil
}

type mysqlColumnVisitor struct {
	allowed        map[string]struct{}
	columns        map[string]struct{}
	rejectSubquery bool
	occ            *policyTableOccurrence
	err            error
}

func (v *mysqlColumnVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if v.err != nil {
		return n, true
	}
	switch x := n.(type) {
	case *ast.SubqueryExpr:
		if v.rejectSubquery || v.occ != nil {
			v.err = fmt.Errorf("subqueries are not allowed inside a required predicate atom")
			return n, true
		}
	case *ast.ColumnNameExpr:
		col := strings.ToLower(x.Name.Name.O)
		if v.allowed != nil {
			if _, ok := v.allowed[col]; !ok {
				v.err = fmt.Errorf("column %q is not in allowedColumns", x.Name.Name.O)
				return n, true
			}
		}
		if v.occ != nil {
			if x.Name.Table.O == "" {
				if v.occ.sourceCount != 1 {
					v.err = fmt.Errorf("unqualified column %q is ambiguous in a multi-source query block", x.Name.Name.O)
					return n, true
				}
			} else if !mysqlColumnMatchesOccurrence(x.Name, *v.occ) {
				v.err = fmt.Errorf("column %q is bound to another table occurrence", x.Name.String())
				return n, true
			}
		}
		if v.columns != nil {
			v.columns[col] = struct{}{}
		}
	}
	return n, false
}

func (v *mysqlColumnVisitor) Leave(n ast.Node) (ast.Node, bool) {
	return n, v.err == nil
}

type mysqlAliasRenderVisitor struct {
	alias string
	err   error
}

func (v *mysqlAliasRenderVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if col, ok := n.(*ast.ColumnNameExpr); ok {
		col.Name.Schema = ast.NewCIStr("")
		col.Name.Table = ast.NewCIStr(v.alias)
	}
	return n, false
}

func (v *mysqlAliasRenderVisitor) Leave(n ast.Node) (ast.Node, bool) { return n, v.err == nil }

type mysqlPolicyBlockVisitor struct {
	policies []*CompiledPredicatePolicy
	scopes   []map[string]struct{}
	pushes   []ast.Node
	block    int
	matches  []PredicateMatch
	err      error
}

func (v *mysqlPolicyBlockVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if v.err != nil {
		return n, true
	}
	switch x := n.(type) {
	case *ast.SetOprStmt:
		v.pushScope(n, mysqlWithNames(x.With))
	case *ast.SetOprSelectList:
		v.pushScope(n, mysqlWithNames(x.With))
	case *ast.SelectStmt:
		v.pushScope(n, mysqlWithNames(x.With))
		v.block++
		occurrences, sourceCount := mysqlDirectOccurrences(x, v.currentScope(), v.block)
		for i := range occurrences {
			occurrences[i].sourceCount = sourceCount
			occurrences[i].where = x.Where
			for _, p := range v.policies {
				if !policyApplies(p, occurrences[i]) {
					continue
				}
				have := mysqlQueryAtoms(x.Where, occurrences[i])
				matched := x.Where != nil && containsAllAtoms(have, p.Condition.atoms)
				v.matches = append(v.matches, PredicateMatch{
					PolicyID: p.PolicyID, QueryBlock: v.block,
					Schema: occurrences[i].schema, Table: occurrences[i].table, Alias: occurrences[i].alias,
					Condition: p.Condition.raw, Matched: matched,
					Reason: matchReason(x.Where != nil, matched),
				})
			}
		}
	}
	return n, false
}

func (v *mysqlPolicyBlockVisitor) Leave(n ast.Node) (ast.Node, bool) {
	if len(v.pushes) > 0 && v.pushes[len(v.pushes)-1] == n {
		v.pushes = v.pushes[:len(v.pushes)-1]
		v.scopes = v.scopes[:len(v.scopes)-1]
	}
	return n, v.err == nil
}

func (v *mysqlPolicyBlockVisitor) pushScope(n ast.Node, names map[string]struct{}) {
	scope := copyNameSet(v.currentScope())
	for name := range names {
		scope[name] = struct{}{}
	}
	v.scopes = append(v.scopes, scope)
	v.pushes = append(v.pushes, n)
}

func (v *mysqlPolicyBlockVisitor) currentScope() map[string]struct{} {
	if len(v.scopes) == 0 {
		return nil
	}
	return v.scopes[len(v.scopes)-1]
}

func mysqlWithNames(with *ast.WithClause) map[string]struct{} {
	out := map[string]struct{}{}
	if with == nil {
		return out
	}
	for _, cte := range with.CTEs {
		out[strings.ToLower(cte.Name.O)] = struct{}{}
	}
	return out
}

func mysqlDirectOccurrences(sel *ast.SelectStmt, scope map[string]struct{}, block int) ([]policyTableOccurrence, int) {
	if sel.From == nil || sel.From.TableRefs == nil {
		return nil, 0
	}
	var out []policyTableOccurrence
	sources := 0
	var collect func(ast.ResultSetNode)
	collect = func(node ast.ResultSetNode) {
		switch x := node.(type) {
		case *ast.Join:
			collect(x.Left)
			if x.Right != nil {
				collect(x.Right)
			}
		case *ast.TableSource:
			sources++
			t, ok := x.Source.(*ast.TableName)
			if !ok {
				return
			}
			if t.Schema.O == "" {
				if _, isCTE := scope[strings.ToLower(t.Name.O)]; isCTE {
					return
				}
			}
			alias := t.Name.O
			if x.AsName.O != "" {
				alias = x.AsName.O
			}
			out = append(out, policyTableOccurrence{queryBlock: block, schema: t.Schema.O, table: t.Name.O, alias: alias})
		case *ast.TableName:
			sources++
			if x.Schema.O == "" {
				if _, isCTE := scope[strings.ToLower(x.Name.O)]; isCTE {
					return
				}
			}
			out = append(out, policyTableOccurrence{queryBlock: block, schema: x.Schema.O, table: x.Name.O, alias: x.Name.O})
		}
	}
	collect(sel.From.TableRefs)
	return out, sources
}

func mysqlQueryAtoms(where ast.ExprNode, occ policyTableOccurrence) []string {
	if where == nil {
		return nil
	}
	var out []string
	for _, atom := range flattenMySQLAnd(where) {
		v := &mysqlColumnVisitor{occ: &occ}
		if _, ok := atom.Accept(v); !ok || v.err != nil {
			continue
		}
		key, err := restoreMySQLExpression(atom, true)
		if err == nil {
			out = append(out, key)
		}
	}
	return out
}

func mysqlColumnMatchesOccurrence(col *ast.ColumnName, occ policyTableOccurrence) bool {
	if col == nil {
		return false
	}
	if strings.EqualFold(col.Table.O, occ.alias) {
		return col.Schema.O == ""
	}
	if !strings.EqualFold(col.Table.O, occ.table) {
		return false
	}
	return col.Schema.O == "" || strings.EqualFold(col.Schema.O, occ.schema)
}
