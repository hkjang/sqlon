package mcp

import (
	"context"
	"fmt"
	"strings"
)

// DBA tool suite. These are the privileged, WRITE-capable operations a database
// administrator performs — user/role management, grants, database lifecycle,
// server settings, session control, and maintenance. They run through the
// dbconn privileged pool (AdminExec/AdminQuery), which requires the profile to
// opt in with DBAConfig. Every action is audited. Authorization (dba/admin
// role) is enforced by the dispatcher before these run.
//
// SQL is generated with dialect-aware identifier quoting; free-form values that
// must be literals (passwords) are single-quote escaped. Destructive drops
// require an explicit confirm flag.

// dbaResult standardizes tool output.
func dbaOK(action string, extra map[string]any) map[string]any {
	out := map[string]any{"status": "ok", "action": action}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func dbaErr(msg string) map[string]any {
	return map[string]any{"status": "error", "error": msg}
}

// quoteIdent quotes a SQL identifier for the dialect. Rejects embedded control
// characters; escapes the quote char by doubling.
func quoteIdent(dialect, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("identifier is empty")
	}
	if strings.ContainsAny(name, "\x00\n\r") {
		return "", fmt.Errorf("identifier contains illegal characters")
	}
	if dialect == "mysql" || dialect == "mariadb" {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`", nil
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// quoteLiteral single-quote-escapes a string literal (for passwords etc.).
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// dbaExec runs one privileged statement and audits it. The audit records the
// executed SQL with any password literal redacted.
func (s *Server) dbaExec(ctx context.Context, profile, action, sqlText, auditSQL string) map[string]any {
	// Legacy direct mutation endpoints are closed. Their SQL builders remain
	// internal until each is represented by a structured ChangePlan template.
	return dbaErr("direct DBA execution is disabled; create, approve, and execute a ChangePlan instead")
	/*
		res, err := s.DB.AdminExec(ctx, profile, sqlText, 60)
		actor := "dba"
		if u := userFrom(ctx); u != nil {
			actor = u.Username
		}
		audit := map[string]any{
			"ts": time.Now().Format(time.RFC3339Nano), "tool": "dba:" + action,
			"db_profile_id": profile, "actor": actor, "sql": auditSQL,
		}
		if err != nil {
			audit["is_error"] = true
			audit["error"] = err.Error()
			s.appendAudit(audit)
			return dbaErr(err.Error())
		}
		s.appendAudit(audit)
		return dbaOK(action, map[string]any{
			"profile": profile, "rows_affected": res.RowsAffected, "elapsed_ms": res.ElapsedMs,
			"executed_sql": auditSQL,
		})
	*/
}

// dbaDialect resolves the profile dialect or returns an error result.
func (s *Server) dbaDialect(ctx context.Context, profile string) (string, error) {
	return s.DB.ProfileDialect(ctx, profile)
}

// ---- inspection ----

func (s *Server) dbaOverview(ctx context.Context, profile string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	avail, _ := s.DB.AdminAvailable(ctx, profile)
	out := map[string]any{"status": "ok", "profile": profile, "dialect": dialect, "dba_enabled": avail}
	if !avail {
		out["note"] = "이 프로파일에 DBA 자격증명이 없습니다. db_profiles의 dba.enabled + dba.username/password_ref를 설정하세요(관리자)."
		return out
	}
	var verQ, cntUsers, cntDBs string
	if dialect == "postgres" {
		verQ = "SELECT version() AS v"
		cntUsers = "SELECT count(*) AS n FROM pg_roles"
		cntDBs = "SELECT count(*) AS n FROM pg_database WHERE datistemplate = false"
	} else {
		verQ = "SELECT version() AS v"
		cntUsers = "SELECT count(*) AS n FROM mysql.user"
		cntDBs = "SELECT count(*) AS n FROM information_schema.schemata"
	}
	if rows, err := s.DB.AdminQuery(ctx, profile, verQ); err == nil && len(rows) > 0 {
		out["version"] = rows[0]["v"]
	}
	if rows, err := s.DB.AdminQuery(ctx, profile, cntUsers); err == nil && len(rows) > 0 {
		out["role_count"] = rows[0]["n"]
	}
	if rows, err := s.DB.AdminQuery(ctx, profile, cntDBs); err == nil && len(rows) > 0 {
		out["database_count"] = rows[0]["n"]
	}
	out["note"] = "DBA 콘솔 개요입니다(권한 있는 계정으로 조회). 변경 작업은 감사 로그에 기록됩니다."
	return out
}

func (s *Server) dbaListUsers(ctx context.Context, profile string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	var q string
	if dialect == "postgres" {
		q = `SELECT rolname AS name, rolsuper AS superuser, rolcreaterole AS createrole,
		rolcreatedb AS createdb, rolcanlogin AS can_login, rolreplication AS replication,
		rolconnlimit AS conn_limit, rolvaliduntil AS valid_until
		FROM pg_roles ORDER BY rolname`
	} else {
		q = `SELECT User AS name, Host AS host,
		(Super_priv='Y') AS superuser, (Create_user_priv='Y') AS create_user,
		(Grant_priv='Y') AS can_grant, account_locked AS locked
		FROM mysql.user ORDER BY User, Host`
	}
	rows, err := s.DB.AdminQuery(ctx, profile, q)
	if err != nil {
		return dbaErr(err.Error())
	}
	return map[string]any{"status": "ok", "profile": profile, "dialect": dialect, "users": rows, "count": len(rows)}
}

func (s *Server) dbaListDatabases(ctx context.Context, profile string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	var q string
	if dialect == "postgres" {
		q = `SELECT d.datname AS name, pg_get_userbyid(d.datdba) AS owner,
		pg_encoding_to_char(d.encoding) AS encoding, d.datcollate AS collate,
		pg_size_pretty(pg_database_size(d.datname)) AS size
		FROM pg_database d WHERE d.datistemplate = false ORDER BY d.datname`
	} else {
		q = `SELECT schema_name AS name, default_character_set_name AS encoding,
		default_collation_name AS collate FROM information_schema.schemata ORDER BY schema_name`
	}
	rows, err := s.DB.AdminQuery(ctx, profile, q)
	if err != nil {
		return dbaErr(err.Error())
	}
	return map[string]any{"status": "ok", "profile": profile, "dialect": dialect, "databases": rows, "count": len(rows)}
}

func (s *Server) dbaListSettings(ctx context.Context, profile, filter string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	f := "%" + strings.ToLower(strings.TrimSpace(filter)) + "%"
	var q string
	var args []any
	if dialect == "postgres" {
		q = `SELECT name, setting AS value, unit, category, short_desc AS description,
		context, boot_val AS default_value, pending_restart
		FROM pg_settings WHERE ($1='%%' OR lower(name) LIKE $1) ORDER BY name LIMIT 500`
		args = []any{f}
	} else {
		q = `SELECT variable_name AS name, variable_value AS value
		FROM performance_schema.global_variables WHERE (? = '%%' OR lower(variable_name) LIKE ?)
		ORDER BY variable_name LIMIT 500`
		args = []any{f, f}
	}
	rows, err := s.DB.AdminQuery(ctx, profile, q, args...)
	if err != nil {
		return dbaErr(err.Error())
	}
	return map[string]any{"status": "ok", "profile": profile, "dialect": dialect, "settings": rows, "count": len(rows)}
}

func (s *Server) dbaListSessions(ctx context.Context, profile string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	var q string
	if dialect == "postgres" {
		q = `SELECT pid, usename AS username, datname AS database, client_addr::text AS client,
		state, wait_event_type AS wait_type, now()-query_start AS duration,
		left(query, 200) AS query FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() ORDER BY query_start NULLS LAST LIMIT 200`
	} else {
		q = `SELECT id AS pid, user AS username, db AS database, host AS client,
		command AS state, time AS duration_s, left(info, 200) AS query
		FROM information_schema.processlist ORDER BY time DESC LIMIT 200`
	}
	rows, err := s.DB.AdminQuery(ctx, profile, q)
	if err != nil {
		return dbaErr(err.Error())
	}
	return map[string]any{"status": "ok", "profile": profile, "dialect": dialect, "sessions": rows, "count": len(rows)}
}

// ---- mutation ----

type dbaUserOpts struct {
	Username   string
	Password   string
	CanLogin   *bool
	Superuser  *bool
	CreateDB   *bool
	CreateRole *bool
}

func (s *Server) dbaCreateUser(ctx context.Context, profile string, o dbaUserOpts) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	qid, err := quoteIdent(dialect, o.Username)
	if err != nil {
		return dbaErr(err.Error())
	}
	var sqlText, auditSQL string
	if dialect == "postgres" {
		attrs := []string{}
		login := o.CanLogin == nil || *o.CanLogin
		if login {
			attrs = append(attrs, "LOGIN")
		} else {
			attrs = append(attrs, "NOLOGIN")
		}
		if o.Superuser != nil && *o.Superuser {
			attrs = append(attrs, "SUPERUSER")
		}
		if o.CreateDB != nil && *o.CreateDB {
			attrs = append(attrs, "CREATEDB")
		}
		if o.CreateRole != nil && *o.CreateRole {
			attrs = append(attrs, "CREATEROLE")
		}
		base := "CREATE ROLE " + qid + " " + strings.Join(attrs, " ")
		if o.Password != "" {
			sqlText = base + " PASSWORD " + quoteLiteral(o.Password)
			auditSQL = base + " PASSWORD '***'"
		} else {
			sqlText, auditSQL = base, base
		}
	} else {
		// mysql: CREATE USER 'name'@'%' IDENTIFIED BY '...'
		host := "%"
		userClause := quoteLiteral(o.Username) + "@" + quoteLiteral(host)
		base := "CREATE USER " + userClause
		if o.Password != "" {
			sqlText = base + " IDENTIFIED BY " + quoteLiteral(o.Password)
			auditSQL = base + " IDENTIFIED BY '***'"
		} else {
			sqlText, auditSQL = base, base
		}
	}
	return s.dbaExec(ctx, profile, "create_user", sqlText, auditSQL)
}

func (s *Server) dbaAlterUser(ctx context.Context, profile string, o dbaUserOpts) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	qid, err := quoteIdent(dialect, o.Username)
	if err != nil {
		return dbaErr(err.Error())
	}
	if dialect == "postgres" {
		clauses := []string{}
		if o.CanLogin != nil {
			clauses = append(clauses, boolWord(*o.CanLogin, "LOGIN", "NOLOGIN"))
		}
		if o.Superuser != nil {
			clauses = append(clauses, boolWord(*o.Superuser, "SUPERUSER", "NOSUPERUSER"))
		}
		if o.CreateDB != nil {
			clauses = append(clauses, boolWord(*o.CreateDB, "CREATEDB", "NOCREATEDB"))
		}
		if o.CreateRole != nil {
			clauses = append(clauses, boolWord(*o.CreateRole, "CREATEROLE", "NOCREATEROLE"))
		}
		pwClause, pwAudit := "", ""
		if o.Password != "" {
			pwClause = " PASSWORD " + quoteLiteral(o.Password)
			pwAudit = " PASSWORD '***'"
		}
		if len(clauses) == 0 && pwClause == "" {
			return dbaErr("no changes requested")
		}
		body := " WITH " + strings.Join(append(clauses, strings.TrimPrefix(pwClause, " ")), " ")
		bodyAudit := " WITH " + strings.Join(append(clauses, strings.TrimPrefix(pwAudit, " ")), " ")
		if len(clauses) == 0 {
			body = pwClause
			bodyAudit = pwAudit
		}
		return s.dbaExec(ctx, profile, "alter_user", "ALTER ROLE "+qid+body, "ALTER ROLE "+qid+bodyAudit)
	}
	// mysql: only password change supported here
	if o.Password == "" {
		return dbaErr("mysql/mariadb alter_user supports password change only in this tool")
	}
	userClause := quoteLiteral(o.Username) + "@" + quoteLiteral("%")
	_ = qid
	return s.dbaExec(ctx, profile, "alter_user",
		"ALTER USER "+userClause+" IDENTIFIED BY "+quoteLiteral(o.Password),
		"ALTER USER "+userClause+" IDENTIFIED BY '***'")
}

func boolWord(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func (s *Server) dbaDropUser(ctx context.Context, profile, username string, confirm bool) map[string]any {
	if !confirm {
		return dbaErr("drop_user requires confirm=true")
	}
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	if dialect == "postgres" {
		qid, err := quoteIdent(dialect, username)
		if err != nil {
			return dbaErr(err.Error())
		}
		return s.dbaExec(ctx, profile, "drop_user", "DROP ROLE IF EXISTS "+qid, "DROP ROLE IF EXISTS "+qid)
	}
	userClause := quoteLiteral(username) + "@" + quoteLiteral("%")
	return s.dbaExec(ctx, profile, "drop_user", "DROP USER IF EXISTS "+userClause, "DROP USER IF EXISTS "+userClause)
}

func (s *Server) dbaGrant(ctx context.Context, profile, privileges, object, grantee string, revoke, withGrant bool) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	priv := strings.TrimSpace(privileges)
	if priv == "" {
		return dbaErr("privileges is required (e.g. SELECT, INSERT or ALL PRIVILEGES)")
	}
	obj := strings.TrimSpace(object)
	if obj == "" {
		return dbaErr("object is required (e.g. mydb.* or schema.table or DATABASE dbname)")
	}
	gid, err := quoteIdent(dialect, grantee)
	if err != nil {
		return dbaErr("invalid grantee: " + err.Error())
	}
	action := "grant"
	var stmt string
	if revoke {
		action = "revoke"
		stmt = "REVOKE " + priv + " ON " + obj + " FROM " + gid
	} else {
		stmt = "GRANT " + priv + " ON " + obj + " TO " + gid
		if withGrant {
			stmt += " WITH GRANT OPTION"
		}
	}
	return s.dbaExec(ctx, profile, action, stmt, stmt)
}

func (s *Server) dbaCreateDatabase(ctx context.Context, profile, name, owner, encoding string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	qid, err := quoteIdent(dialect, name)
	if err != nil {
		return dbaErr(err.Error())
	}
	if dialect == "postgres" {
		stmt := "CREATE DATABASE " + qid
		if o := strings.TrimSpace(owner); o != "" {
			oq, err := quoteIdent(dialect, o)
			if err != nil {
				return dbaErr("invalid owner: " + err.Error())
			}
			stmt += " OWNER " + oq
		}
		if e := strings.TrimSpace(encoding); e != "" {
			stmt += " ENCODING " + quoteLiteral(e)
		}
		return s.dbaExec(ctx, profile, "create_database", stmt, stmt)
	}
	stmt := "CREATE DATABASE " + qid
	if e := strings.TrimSpace(encoding); e != "" {
		stmt += " CHARACTER SET " + e
	}
	return s.dbaExec(ctx, profile, "create_database", stmt, stmt)
}

func (s *Server) dbaDropDatabase(ctx context.Context, profile, name string, confirm bool) map[string]any {
	if !confirm {
		return dbaErr("drop_database requires confirm=true")
	}
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	qid, err := quoteIdent(dialect, name)
	if err != nil {
		return dbaErr(err.Error())
	}
	return s.dbaExec(ctx, profile, "drop_database", "DROP DATABASE IF EXISTS "+qid, "DROP DATABASE IF EXISTS "+qid)
}

func (s *Server) dbaSetParameter(ctx context.Context, profile, parameter, value, scope string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	param := strings.TrimSpace(parameter)
	if param == "" || strings.ContainsAny(param, " \t\n\r;'\"") {
		return dbaErr("parameter must be a bare setting name")
	}
	if dialect == "postgres" {
		// ALTER SYSTEM persists to postgresql.auto.conf; reload applies non-restart params
		stmt := "ALTER SYSTEM SET " + param + " = " + quoteLiteral(value)
		res := s.dbaExec(ctx, profile, "set_parameter", stmt, stmt)
		if res["status"] == "ok" {
			// best-effort reload so non-restart params take effect immediately
			if _, rerr := s.DB.AdminExec(ctx, profile, "SELECT pg_reload_conf()", 30); rerr == nil {
				res["reloaded"] = true
			}
			res["note"] = "ALTER SYSTEM으로 영구 반영했습니다. 재시작이 필요한 파라미터는 pending_restart로 표시됩니다."
		}
		return res
	}
	sc := strings.ToUpper(strings.TrimSpace(scope))
	if sc != "GLOBAL" && sc != "SESSION" {
		sc = "GLOBAL"
	}
	stmt := "SET " + sc + " " + param + " = " + quoteLiteral(value)
	return s.dbaExec(ctx, profile, "set_parameter", stmt, stmt)
}

func (s *Server) dbaTerminateSession(ctx context.Context, profile string, pid int64, cancelOnly bool) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	if pid <= 0 {
		return dbaErr("pid is required")
	}
	var stmt, action string
	if dialect == "postgres" {
		if cancelOnly {
			action = "cancel_query"
			stmt = fmt.Sprintf("SELECT pg_cancel_backend(%d)", pid)
		} else {
			action = "terminate_session"
			stmt = fmt.Sprintf("SELECT pg_terminate_backend(%d)", pid)
		}
	} else {
		action = "terminate_session"
		stmt = fmt.Sprintf("KILL %d", pid)
		if cancelOnly {
			action = "cancel_query"
			stmt = fmt.Sprintf("KILL QUERY %d", pid)
		}
	}
	return s.dbaExec(ctx, profile, action, stmt, stmt)
}

func (s *Server) dbaRunMaintenance(ctx context.Context, profile, operation, target string) map[string]any {
	dialect, err := s.dbaDialect(ctx, profile)
	if err != nil {
		return dbaErr(err.Error())
	}
	op := strings.ToUpper(strings.TrimSpace(operation))
	allowed := map[string]bool{"VACUUM": true, "ANALYZE": true, "REINDEX": true, "OPTIMIZE": true}
	if !allowed[op] {
		return dbaErr("operation must be one of VACUUM, ANALYZE, REINDEX, OPTIMIZE")
	}
	tgt := strings.TrimSpace(target)
	var stmt string
	if dialect == "postgres" {
		switch op {
		case "VACUUM":
			stmt = "VACUUM"
			if tgt != "" {
				stmt += " " + tgt // qualified table name, caller-supplied
			}
		case "ANALYZE":
			stmt = "ANALYZE"
			if tgt != "" {
				stmt += " " + tgt
			}
		case "REINDEX":
			if tgt == "" {
				return dbaErr("REINDEX requires a target (TABLE name / INDEX name / DATABASE name)")
			}
			stmt = "REINDEX " + tgt
		default:
			return dbaErr("OPTIMIZE is a mysql operation")
		}
	} else {
		switch op {
		case "ANALYZE":
			stmt = "ANALYZE TABLE " + tgt
		case "OPTIMIZE":
			stmt = "OPTIMIZE TABLE " + tgt
		default:
			return dbaErr(op + " is not supported for mysql/mariadb (use ANALYZE or OPTIMIZE)")
		}
		if tgt == "" {
			return dbaErr("target table is required")
		}
	}
	return s.dbaExec(ctx, profile, "maintenance", stmt, stmt)
}

// dbaExecute is the general escape hatch: run an arbitrary privileged statement.
// Requires confirm=true. The statement is audited verbatim.
func (s *Server) dbaExecute(ctx context.Context, profile, sqlText string, confirm bool) map[string]any {
	if !confirm {
		return dbaErr("dba_execute requires confirm=true (this runs arbitrary privileged SQL)")
	}
	if strings.TrimSpace(sqlText) == "" {
		return dbaErr("sql is required")
	}
	return s.dbaExec(ctx, profile, "execute", sqlText, sqlText)
}
