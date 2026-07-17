package dbconn

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	// registers the expression value driver the parser needs to build ASTs;
	// we only inspect statement structure (not evaluate values), so the
	// lightweight test_driver is the standard standalone choice.
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
	pg_query "github.com/wasilibs/go-pgquery"
)

// AST-based read-only validation (GO-SQL AST guard). The keyword guard in
// sqlguard.go strips comments/literals and scans for denied words — that
// blocks the obvious cases but can in principle be evaded by sufficiently
// exotic dialect syntax the stripper doesn't model. This layer parses the SQL
// with the *actual grammar of the target dialect* and inspects the statement
// tree, so a statement either parses to exactly one SELECT-class node or it
// is rejected:
//
//   - postgres: github.com/wasilibs/go-pgquery — the real PostgreSQL parser
//     (libpg_query) compiled to WASM and run in-process via wazero. Pure Go,
//     no cgo, byte-for-byte PostgreSQL grammar.
//   - mysql/mariadb: github.com/pingcap/tidb/pkg/parser — TiDB's
//     MySQL-compatible parser (MariaDB mode via SetMariaDB).
//
// The policy is fail-closed: SQL that the dialect parser cannot parse is
// rejected (it would fail on the real DB anyway), and any statement node
// other than a plain SELECT/UNION query — including data-modifying CTEs,
// SELECT INTO, INTO OUTFILE/DUMPFILE, FOR UPDATE/SHARE locking, and session
// variable assignment — is rejected even when nested arbitrarily deep.

// ValidateReadOnlyAST parses sqlText with the dialect's parser and verifies
// it is exactly one read-only SELECT-class statement.
func ValidateReadOnlyAST(dialect, sqlText string) error {
	switch dialect {
	case "postgres":
		return validatePostgresAST(sqlText)
	case "mysql":
		return validateMySQLAST(sqlText, false)
	case "mariadb":
		return validateMySQLAST(sqlText, true)
	default:
		return fmt.Errorf("unsupported dialect for AST validation: %s", dialect)
	}
}

// ValidateReadOnly runs the keyword guard (denied functions/keywords,
// operator extensions) and then the dialect AST guard. This is the guard the
// executor uses; ValidateReadOnlySQL alone remains for callers without a
// resolved dialect.
func ValidateReadOnly(d Dialect, sqlText string, extraDenied []string) error {
	if err := ValidateReadOnlySQL(sqlText, extraDenied); err != nil {
		return err
	}
	return ValidateReadOnlyAST(d.Name(), sqlText)
}

// ---- PostgreSQL ----

// validatePostgresAST walks the libpg_query parse tree (as JSON) with an
// allowlist: the only *Stmt node permitted anywhere in the tree is
// SelectStmt. That single rule covers every evasion class at once — DML/DDL
// as the top statement, data-modifying CTEs (WITH x AS (DELETE ...) SELECT),
// DO blocks, COPY, EXPLAIN, SET, etc. — because each parses to its own
// *Stmt node wherever it appears. SELECT INTO and FOR UPDATE/SHARE are
// additionally rejected via their dedicated tree fields.
func validatePostgresAST(sqlText string) error {
	jsonTree, err := pg_query.ParseToJSON(sqlText)
	if err != nil {
		return fmt.Errorf("SQL이 PostgreSQL 문법으로 파싱되지 않습니다 (fail-closed): %v", sanitizeParseError(err))
	}
	var tree struct {
		Stmts []json.RawMessage `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(jsonTree), &tree); err != nil {
		return fmt.Errorf("parse tree decode failed: %w", err)
	}
	if len(tree.Stmts) != 1 {
		return fmt.Errorf("exactly one statement is required, got %d", len(tree.Stmts))
	}
	var node any
	if err := json.Unmarshal(tree.Stmts[0], &node); err != nil {
		return fmt.Errorf("parse tree decode failed: %w", err)
	}
	return walkPGNode(node)
}

func walkPGNode(node any) error {
	switch n := node.(type) {
	case map[string]any:
		for key, child := range n {
			switch {
			case key == "SelectStmt":
				// allowed statement class; keep walking its body
			case strings.HasSuffix(key, "Stmt"):
				return fmt.Errorf("statement type %s is not allowed (read-only SELECT/WITH only)", key)
			case key == "intoClause":
				if child != nil {
					return errors.New("SELECT INTO is not allowed (creates a table)")
				}
			case key == "lockingClause":
				if child != nil {
					return errors.New("locking clauses (FOR UPDATE/SHARE) are not allowed in read-only queries")
				}
			}
			if err := walkPGNode(child); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range n {
			if err := walkPGNode(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- MySQL / MariaDB ----

// validateMySQLAST parses with TiDB's MySQL-compatible parser (MariaDB mode
// for mariadb) and requires exactly one SELECT-class statement. A visitor
// then rejects read-only-violating constructs that live inside an otherwise
// valid SELECT: INTO OUTFILE/DUMPFILE/var, FOR UPDATE / LOCK IN SHARE MODE,
// and session-variable assignment (SELECT @a := ...).
func validateMySQLAST(sqlText string, mariadb bool) error {
	p := parser.New()
	p.EnableWindowFunc(true)
	if mariadb {
		p.SetMariaDB(true)
	}
	dialectName := "MySQL"
	if mariadb {
		dialectName = "MariaDB"
	}
	stmts, _, err := p.ParseSQL(sqlText)
	if err != nil {
		return fmt.Errorf("SQL이 %s 문법으로 파싱되지 않습니다 (fail-closed): %v", dialectName, sanitizeParseError(err))
	}
	if len(stmts) != 1 {
		return fmt.Errorf("exactly one statement is required, got %d", len(stmts))
	}
	switch stmts[0].(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		// allowed statement class
	default:
		return fmt.Errorf("statement type %T is not allowed (read-only SELECT/WITH only)", stmts[0])
	}
	v := &readOnlyVisitor{}
	stmts[0].Accept(v)
	return v.err
}

// readOnlyVisitor walks every node of the statement (including subqueries and
// CTE bodies, which the parser exposes as nested SelectStmt nodes).
type readOnlyVisitor struct {
	err error
}

func (v *readOnlyVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if v.err != nil {
		return n, true
	}
	switch node := n.(type) {
	case *ast.SelectStmt:
		if node.SelectIntoOpt != nil {
			v.err = errors.New("SELECT ... INTO (OUTFILE/DUMPFILE/variable) is not allowed")
			return n, true
		}
		if node.LockInfo != nil && node.LockInfo.LockType != ast.SelectLockNone {
			v.err = errors.New("locking reads (FOR UPDATE / LOCK IN SHARE MODE) are not allowed in read-only queries")
			return n, true
		}
	case *ast.VariableExpr:
		if node.Value != nil {
			v.err = errors.New("session variable assignment (@var := ...) is not allowed")
			return n, true
		}
	}
	return n, false
}

func (v *readOnlyVisitor) Leave(n ast.Node) (ast.Node, bool) {
	return n, v.err == nil
}

// sanitizeParseError keeps parser errors single-line and bounded so they are
// safe to surface through the API.
func sanitizeParseError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}
