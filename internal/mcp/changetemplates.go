package mcp

import (
	"fmt"
	"strings"

	"sqlon/internal/change"
)

// Change-step templates turn a structured privileged operation into a fully
// formed, immutable change.Step — command + read-only verification +
// compensation — with dialect-aware identifier quoting (SEC-011). This is how
// the legacy safe SQL builders live on: not as direct executors, but as
// generators of steps that flow through the approval-gated change workflow.
//
// Only cleanly reversible operations with a meaningful verification are
// templated here. Irreversible operations (DROP, destructive parameter
// changes) are authored by hand so the operator must supply an explicit
// compensation and cannot get a false sense of automatic reversibility.

// buildChangeStep returns step number `order` for the given action. args are
// the structured, unquoted operation parameters. Passwords are refused: a
// persisted plan must never carry a plaintext secret (SEC-002/SEC-007).
func buildChangeStep(dialect, action string, order int, args map[string]any) (change.Step, error) {
	if order <= 0 {
		order = 1
	}
	arg := func(name string) string { return strings.TrimSpace(fmt.Sprint(argValue(args, name))) }
	if _, hasPassword := args["password"]; hasPassword && arg("password") != "" {
		return change.Step{}, fmt.Errorf("비밀번호는 변경계획에 저장할 수 없습니다. 비밀번호 없이 계정을 만들고 secret 참조로 별도 설정하세요")
	}

	switch strings.ToLower(strings.TrimSpace(action)) {
	case "create_user":
		return buildCreateUser(dialect, order, arg)
	case "create_database":
		return buildCreateDatabase(dialect, order, arg)
	case "grant", "revoke":
		return buildGrant(dialect, action, order, args, arg)
	case "create_index":
		return buildCreateIndex(dialect, order, args, arg)
	default:
		return change.Step{}, fmt.Errorf("지원하지 않는 변경 액션 %q — 되돌릴 수 없는 작업은 실행/검증/보상 문장을 직접 작성하세요", action)
	}
}

func buildCreateUser(dialect string, order int, arg func(string) string) (change.Step, error) {
	name := arg("username")
	if name == "" {
		return change.Step{}, fmt.Errorf("create_user: username이 필요합니다")
	}
	switch dialect {
	case "postgres":
		qid, err := quoteIdent(dialect, name)
		if err != nil {
			return change.Step{}, err
		}
		return change.Step{
			Order:        order,
			Command:      "CREATE ROLE " + qid + " NOLOGIN",
			Verification: "SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = " + quoteLiteral(name),
			Compensation: "DROP ROLE IF EXISTS " + qid,
		}, nil
	case "mysql", "mariadb":
		userClause := quoteLiteral(name) + "@" + quoteLiteral("%")
		return change.Step{
			Order:        order,
			Command:      "CREATE USER " + userClause,
			Verification: "SELECT 1 FROM mysql.user WHERE user = " + quoteLiteral(name),
			Compensation: "DROP USER IF EXISTS " + userClause,
		}, nil
	default:
		return change.Step{}, fmt.Errorf("create_user 템플릿은 %s 엔진을 지원하지 않습니다", dialect)
	}
}

func buildCreateDatabase(dialect string, order int, arg func(string) string) (change.Step, error) {
	name := arg("name")
	if name == "" {
		return change.Step{}, fmt.Errorf("create_database: name이 필요합니다")
	}
	qid, err := quoteIdent(dialect, name)
	if err != nil {
		return change.Step{}, err
	}
	switch dialect {
	case "postgres":
		command := "CREATE DATABASE " + qid
		if owner := arg("owner"); owner != "" {
			oq, err := quoteIdent(dialect, owner)
			if err != nil {
				return change.Step{}, fmt.Errorf("invalid owner: %w", err)
			}
			command += " OWNER " + oq
		}
		if encoding := arg("encoding"); encoding != "" {
			command += " ENCODING " + quoteLiteral(encoding)
		}
		return change.Step{
			Order:        order,
			Command:      command,
			Verification: "SELECT 1 FROM pg_catalog.pg_database WHERE datname = " + quoteLiteral(name),
			Compensation: "DROP DATABASE IF EXISTS " + qid,
		}, nil
	case "mysql", "mariadb":
		command := "CREATE DATABASE " + qid
		if encoding := arg("encoding"); encoding != "" {
			// charset is an identifier-like keyword, not a string literal
			if strings.ContainsAny(encoding, " \t\n\r;'\"`") {
				return change.Step{}, fmt.Errorf("invalid encoding/charset")
			}
			command += " CHARACTER SET " + encoding
		}
		return change.Step{
			Order:        order,
			Command:      command,
			Verification: "SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = " + quoteLiteral(name),
			Compensation: "DROP DATABASE IF EXISTS " + qid,
		}, nil
	default:
		return change.Step{}, fmt.Errorf("create_database 템플릿은 %s 엔진을 지원하지 않습니다", dialect)
	}
}

// buildGrant templates a GRANT (compensation REVOKE) or REVOKE (compensation
// GRANT). The object string is passed through as written — the caller supplies
// the exact grant target — but the grantee is quoted. Verification confirms
// the grantee still resolves as a principal, the only portable post-check.
func buildGrant(dialect, action string, order int, args map[string]any, arg func(string) string) (change.Step, error) {
	privileges := strings.TrimSpace(arg("privileges"))
	object := strings.TrimSpace(arg("object"))
	grantee := strings.TrimSpace(arg("grantee"))
	if privileges == "" || object == "" || grantee == "" {
		return change.Step{}, fmt.Errorf("grant/revoke: privileges, object, grantee가 모두 필요합니다")
	}
	if strings.ContainsAny(privileges, ";'\"") || strings.ContainsAny(object, ";'\"") {
		return change.Step{}, fmt.Errorf("privileges/object에 허용되지 않는 문자가 있습니다")
	}
	gid, err := quoteIdent(dialect, grantee)
	if err != nil {
		return change.Step{}, fmt.Errorf("invalid grantee: %w", err)
	}
	grant := "GRANT " + privileges + " ON " + object + " TO " + gid
	if boolArg(args, "with_grant") {
		grant += " WITH GRANT OPTION"
	}
	revoke := "REVOKE " + privileges + " ON " + object + " FROM " + gid
	verification := granteeExistsSQL(dialect, grantee)
	if strings.EqualFold(strings.TrimSpace(action), "revoke") {
		return change.Step{Order: order, Command: revoke, Verification: verification, Compensation: grant}, nil
	}
	return change.Step{Order: order, Command: grant, Verification: verification, Compensation: revoke}, nil
}

// buildCreateIndex templates a reversible index creation (compensation DROP
// INDEX) — the canonical safe realization of TUN-009 (index DDL never runs
// directly; it flows through the approval workflow). The index name is
// synthesized deterministically when omitted; all identifiers are quoted, and
// qualified table names ("schema.table") are quoted per part.
func buildCreateIndex(dialect string, order int, args map[string]any, arg func(string) string) (change.Step, error) {
	table := strings.TrimSpace(arg("table"))
	if table == "" {
		return change.Step{}, fmt.Errorf("create_index: table이 필요합니다")
	}
	columns := columnsArg(args, arg)
	if len(columns) == 0 {
		return change.Step{}, fmt.Errorf("create_index: columns(또는 column)이 필요합니다")
	}
	name := strings.TrimSpace(arg("index"))
	if name == "" {
		name = synthIndexName(table, columns)
	}
	qName, err := quoteIdent(dialect, name)
	if err != nil {
		return change.Step{}, fmt.Errorf("invalid index name: %w", err)
	}
	qTable, err := quoteQualified(dialect, table)
	if err != nil {
		return change.Step{}, fmt.Errorf("invalid table: %w", err)
	}
	quotedCols := make([]string, 0, len(columns))
	for _, c := range columns {
		qc, err := quoteIdent(dialect, c)
		if err != nil {
			return change.Step{}, fmt.Errorf("invalid column %q: %w", c, err)
		}
		quotedCols = append(quotedCols, qc)
	}
	keyword := "CREATE INDEX"
	if boolArg(args, "unique") {
		keyword = "CREATE UNIQUE INDEX"
	}
	command := keyword + " " + qName + " ON " + qTable + " (" + strings.Join(quotedCols, ", ") + ")"

	step := change.Step{Order: order, Command: command}
	switch dialect {
	case "mysql", "mariadb":
		step.Compensation = "DROP INDEX " + qName + " ON " + qTable
		step.Verification = "SELECT 1 FROM information_schema.STATISTICS WHERE INDEX_NAME = " + quoteLiteral(name) + " AND TABLE_NAME = " + quoteLiteral(bareName(table))
	case "oracle":
		step.Compensation = "DROP INDEX " + qName
		step.Verification = "SELECT 1 FROM all_indexes WHERE index_name = " + quoteLiteral(strings.ToUpper(name))
	default: // postgres: the index is created in the table's schema
		dropTarget := qName
		if schema := schemaPart(table); schema != "" {
			qs, err := quoteIdent(dialect, schema)
			if err != nil {
				return change.Step{}, fmt.Errorf("invalid schema: %w", err)
			}
			dropTarget = qs + "." + qName
		}
		step.Compensation = "DROP INDEX IF EXISTS " + dropTarget
		step.Verification = "SELECT 1 FROM pg_indexes WHERE indexname = " + quoteLiteral(name)
	}
	return step, nil
}

// columnsArg reads columns from a "columns" arg (JSON array or comma string)
// or a single "column" arg.
func columnsArg(args map[string]any, arg func(string) string) []string {
	var out []string
	if raw, ok := args["columns"]; ok {
		if list, ok := raw.([]any); ok {
			for _, v := range list {
				if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
					out = append(out, s)
				}
			}
		} else if s := strings.TrimSpace(fmt.Sprint(raw)); s != "" {
			for _, part := range strings.Split(s, ",") {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
		}
	}
	if len(out) == 0 {
		if c := strings.TrimSpace(arg("column")); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// synthIndexName builds a deterministic index name from the bare table and
// columns, keeping to characters safe for every engine's identifier rules.
func synthIndexName(table string, columns []string) string {
	sanitize := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 32
			}
			return '_'
		}, s)
	}
	name := "idx_" + sanitize(bareName(table))
	for _, c := range columns {
		name += "_" + sanitize(c)
	}
	if len(name) > 30 { // Oracle 11g identifier ceiling is the tightest common bound
		name = name[:30]
	}
	return name
}

// quoteQualified quotes each dot-separated part of an object name so
// "schema.table" becomes "schema"."table" rather than one quoted string.
func quoteQualified(dialect, name string) (string, error) {
	parts := strings.Split(name, ".")
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		q, err := quoteIdent(dialect, part)
		if err != nil {
			return "", err
		}
		quoted = append(quoted, q)
	}
	return strings.Join(quoted, "."), nil
}

func schemaPart(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return strings.TrimSpace(name[:i])
	}
	return ""
}

func bareName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return strings.TrimSpace(name[i+1:])
	}
	return strings.TrimSpace(name)
}

func granteeExistsSQL(dialect, grantee string) string {
	switch dialect {
	case "mysql", "mariadb":
		return "SELECT 1 FROM mysql.user WHERE user = " + quoteLiteral(grantee)
	default:
		return "SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = " + quoteLiteral(grantee)
	}
}

func argValue(args map[string]any, name string) any {
	if args == nil {
		return ""
	}
	if v, ok := args[name]; ok && v != nil {
		return v
	}
	return ""
}

func boolArg(args map[string]any, name string) bool {
	switch v := argValue(args, name).(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}
