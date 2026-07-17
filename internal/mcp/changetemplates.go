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
