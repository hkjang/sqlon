package dbconn

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	pg_query "github.com/wasilibs/go-pgquery"
)

// TableRef is one physical table referenced by a query. Schema may be empty
// for bare (unqualified) references. Both parts are lower-cased for
// case-insensitive matching against live inventories.
type TableRef struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
}

func (r TableRef) String() string {
	if r.Schema == "" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

// ExtractTables parses sqlText with the dialect's parser and returns the
// physical tables it references, excluding CTE names (a CTE reference in FROM
// looks like a bare table but is not a physical table). Reuses the same
// parsers as the AST guard, so the extraction cannot be fooled by comments,
// string literals, or odd quoting.
func ExtractTables(dialect, sqlText string) ([]TableRef, error) {
	switch dialect {
	case "postgres":
		return extractTablesPostgres(sqlText)
	case "mysql", "mariadb":
		return extractTablesMySQL(sqlText, dialect == "mariadb")
	default:
		return nil, fmt.Errorf("unsupported dialect for table extraction: %s", dialect)
	}
}

func extractTablesPostgres(sqlText string) ([]TableRef, error) {
	jsonTree, err := pg_query.ParseToJSON(sqlText)
	if err != nil {
		return nil, fmt.Errorf("parse failed: %v", sanitizeParseError(err))
	}
	var tree any
	if err := json.Unmarshal([]byte(jsonTree), &tree); err != nil {
		return nil, err
	}
	var refs []TableRef
	ctes := map[string]bool{}
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if rv, ok := n["RangeVar"].(map[string]any); ok {
				schema, _ := rv["schemaname"].(string)
				name, _ := rv["relname"].(string)
				if name != "" {
					refs = append(refs, TableRef{Schema: strings.ToLower(schema), Name: strings.ToLower(name)})
				}
			}
			if cte, ok := n["CommonTableExpr"].(map[string]any); ok {
				if name, ok := cte["ctename"].(string); ok {
					ctes[strings.ToLower(name)] = true
				}
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, item := range n {
				walk(item)
			}
		}
	}
	walk(tree)
	return dedupeRefs(refs, ctes), nil
}

func extractTablesMySQL(sqlText string, mariadb bool) ([]TableRef, error) {
	p := parser.New()
	p.EnableWindowFunc(true)
	if mariadb {
		p.SetMariaDB(true)
	}
	stmts, _, err := p.ParseSQL(sqlText)
	if err != nil {
		return nil, fmt.Errorf("parse failed: %v", sanitizeParseError(err))
	}
	v := &tableCollector{ctes: map[string]bool{}}
	for _, stmt := range stmts {
		stmt.Accept(v)
	}
	return dedupeRefs(v.refs, v.ctes), nil
}

type tableCollector struct {
	refs []TableRef
	ctes map[string]bool
}

func (v *tableCollector) Enter(n ast.Node) (ast.Node, bool) {
	switch node := n.(type) {
	case *ast.TableName:
		v.refs = append(v.refs, TableRef{Schema: node.Schema.L, Name: node.Name.L})
	case *ast.CommonTableExpression:
		v.ctes[node.Name.L] = true
	}
	return n, false
}

func (v *tableCollector) Leave(n ast.Node) (ast.Node, bool) { return n, true }

// dedupeRefs drops duplicate refs and bare references that shadow a CTE name.
func dedupeRefs(refs []TableRef, ctes map[string]bool) []TableRef {
	seen := map[string]bool{}
	out := []TableRef{}
	for _, r := range refs {
		if r.Schema == "" && ctes[r.Name] {
			continue
		}
		key := r.Schema + "." + r.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}
