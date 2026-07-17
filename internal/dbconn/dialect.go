package dbconn

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Dialect abstracts the differences between the supported target databases:
// PostgreSQL, MySQL, MariaDB, and Oracle. MariaDB shares the MySQL wire protocol and
// driver but differs in session variables and EXPLAIN JSON shape, so it gets
// its own dialect value.
type Dialect interface {
	// Name is the canonical lower-case dialect id: postgres | mysql | mariadb.
	Name() string
	// DriverName is the database/sql driver to open: pgx | mysql.
	DriverName() string
	// BuildDSN turns a profile + resolved password into a driver DSN. The DSN
	// always requests a read-only session so the guard has DB-level backup.
	BuildDSN(p Profile, password string) (string, error)
	// WrapLimit bounds the row count of an arbitrary SELECT/WITH statement.
	WrapLimit(sqlText string, limit int) string
	// CountWrap builds SELECT COUNT(*) over the query.
	CountWrap(sqlText string) string
	// DeniedExtras returns dialect-specific denied keywords/prefixes that are
	// merged into the built-in read-only guard list.
	DeniedExtras() []string
}

// SupportedTypes lists the accepted profile type values.
var SupportedTypes = []string{"postgres", "mysql", "mariadb", "oracle"}

// DialectFor resolves a profile type (case-insensitive; a few aliases are
// accepted) to its Dialect. Empty defaults to postgres.
func DialectFor(typ string) (Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "", "postgres", "postgresql", "pg":
		return postgresDialect{}, nil
	case "mysql":
		return mysqlDialect{name: "mysql"}, nil
	case "mariadb", "maria":
		return mysqlDialect{name: "mariadb"}, nil
	case "oracle", "ora":
		return oracleDialect{}, nil
	default:
		return nil, fmt.Errorf("unsupported db type %q (use postgres, mysql, mariadb, or oracle)", typ)
	}
}

// ---- Oracle ----

// oracleDialect deliberately contains no driver import. The standard build
// recognizes and validates Oracle profiles, while the sqlon-oracle build links
// godror and registers its database/sql driver. This keeps the standard
// PostgreSQL/MySQL/MariaDB artifact CGO-free.
type oracleDialect struct{}

func (oracleDialect) Name() string       { return "oracle" }
func (oracleDialect) DriverName() string { return "godror" }
func (oracleDialect) BuildDSN(p Profile, password string) (string, error) {
	cs := strings.TrimSpace(p.ConnectString)
	if cs == "" {
		return "", fmt.Errorf("oracle connect_string is required")
	}
	// godror accepts Easy Connect, TNS descriptors, and connection parameters.
	// Credentials are passed in the driver DSN rather than appended to an
	// arbitrary descriptor, avoiding accidental mutation of wallet/TCPS text.
	if strings.HasPrefix(cs, "oracle://") {
		return cs, nil
	}
	return fmt.Sprintf(`user="%s" password="%s" connectString="%s"`, p.Username, password, cs), nil
}
func (oracleDialect) WrapLimit(sqlText string, limit int) string {
	return fmt.Sprintf("SELECT * FROM (%s) sqlon_q FETCH FIRST %d ROWS ONLY", trimSQL(sqlText), limit)
}
func (oracleDialect) CountWrap(sqlText string) string {
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s) sqlon_q", trimSQL(sqlText))
}
func (oracleDialect) DeniedExtras() []string {
	return []string{"FOR UPDATE", "DBMS_", "UTL_", "EXECUTE IMMEDIATE", "ALTER SESSION", "FLASHBACK", "@"}
}

// connectStringRE matches the short "host:port/dbname" form.
var connectStringRE = regexp.MustCompile(`^([^:/@\s]+):(\d+)/([^?\s]+)(?:\?(.*))?$`)

// ---- PostgreSQL ----

type postgresDialect struct{}

func (postgresDialect) Name() string       { return "postgres" }
func (postgresDialect) DriverName() string { return "pgx" }

// BuildDSN accepts either a full postgres:// / postgresql:// URL or the short
// host:port/dbname form. Credentials from the profile always win, and
// default_transaction_read_only=on is enforced (GO-SQL read-only backstop).
func (d postgresDialect) BuildDSN(p Profile, password string) (string, error) {
	cs := strings.TrimSpace(p.ConnectString)
	var u *url.URL
	switch {
	case strings.HasPrefix(cs, "postgres://") || strings.HasPrefix(cs, "postgresql://"):
		parsed, err := url.Parse(cs)
		if err != nil {
			return "", fmt.Errorf("invalid postgres URL: %w", err)
		}
		u = parsed
	default:
		m := connectStringRE.FindStringSubmatch(cs)
		if m == nil {
			return "", fmt.Errorf("connect_string must be host:port/dbname or a postgres:// URL")
		}
		u = &url.URL{Scheme: "postgres", Host: m[1] + ":" + m[2], Path: "/" + m[3]}
		if m[4] != "" {
			u.RawQuery = m[4]
		}
	}
	u.User = url.UserPassword(p.Username, password)
	q := u.Query()
	q.Set("default_transaction_read_only", "on")
	if q.Get("connect_timeout") == "" {
		q.Set("connect_timeout", "5")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (postgresDialect) WrapLimit(sqlText string, limit int) string {
	return fmt.Sprintf("SELECT * FROM (%s) AS jamypg_q LIMIT %d", trimSQL(sqlText), limit)
}

func (postgresDialect) CountWrap(sqlText string) string {
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s) AS jamypg_q", trimSQL(sqlText))
}

// PostgreSQL-specific dangerous statements and functions. Entries ending in
// "_" are prefix (contains) checks; the rest are word-boundary checks.
func (postgresDialect) DeniedExtras() []string {
	return []string{
		"DO", "COPY", "VACUUM", "REINDEX", "CLUSTER", "CHECKPOINT",
		"LISTEN", "NOTIFY", "REFRESH", "SECURITY",
		"PG_SLEEP", "PG_READ_", "PG_LS_", "PG_STAT_FILE",
		"PG_TERMINATE_BACKEND", "PG_CANCEL_BACKEND", "PG_RELOAD_CONF",
		"LO_IMPORT", "LO_EXPORT", "DBLINK",
	}
}

// ---- MySQL / MariaDB ----

type mysqlDialect struct{ name string }

func (d mysqlDialect) Name() string     { return d.name }
func (mysqlDialect) DriverName() string { return "mysql" }

// BuildDSN accepts mysql://host:port/dbname URLs, go-sql-driver DSNs
// (user:pass@tcp(host:port)/dbname), or the short host:port/dbname form.
// A read-only session variable is always injected: MySQL 8 uses
// transaction_read_only, MariaDB still uses tx_read_only.
func (d mysqlDialect) BuildDSN(p Profile, password string) (string, error) {
	cs := strings.TrimSpace(p.ConnectString)
	cfg := mysql.NewConfig()
	switch {
	case strings.Contains(cs, "@tcp("):
		parsed, err := mysql.ParseDSN(cs)
		if err != nil {
			return "", fmt.Errorf("invalid mysql DSN: %w", err)
		}
		cfg = parsed
	case strings.HasPrefix(cs, "mysql://") || strings.HasPrefix(cs, "mariadb://"):
		u, err := url.Parse(cs)
		if err != nil {
			return "", fmt.Errorf("invalid %s URL: %w", d.name, err)
		}
		cfg.Net = "tcp"
		cfg.Addr = u.Host
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		for k, v := range u.Query() {
			if len(v) > 0 {
				if cfg.Params == nil {
					cfg.Params = map[string]string{}
				}
				cfg.Params[k] = v[0]
			}
		}
	default:
		m := connectStringRE.FindStringSubmatch(cs)
		if m == nil {
			return "", fmt.Errorf("connect_string must be host:port/dbname, a mysql:// URL, or a user:pass@tcp(host:port)/db DSN")
		}
		cfg.Net = "tcp"
		cfg.Addr = m[1] + ":" + m[2]
		cfg.DBName = m[3]
	}
	cfg.User = p.Username
	cfg.Passwd = password
	cfg.ParseTime = true
	cfg.Timeout = 5e9 // connect timeout 5s
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if d.name == "mariadb" {
		cfg.Params["tx_read_only"] = "1"
	} else {
		cfg.Params["transaction_read_only"] = "1"
	}
	return cfg.FormatDSN(), nil
}

// mysqlTrailingLimitRE detects an existing top-level trailing LIMIT clause.
var mysqlTrailingLimitRE = regexp.MustCompile(`(?is)\bLIMIT\s+\d+(\s*,\s*\d+)?(\s+OFFSET\s+\d+)?\s*$`)

// WrapLimit wraps plain SELECTs in a derived table. WITH queries are not
// wrapped (older MariaDB rejects CTEs inside derived tables); instead LIMIT is
// appended unless one is already present — the executor's row-scan cap is the
// final backstop either way.
func (mysqlDialect) WrapLimit(sqlText string, limit int) string {
	trimmed := trimSQL(sqlText)
	if regexp.MustCompile(`(?is)^\s*WITH\b`).MatchString(trimmed) {
		if mysqlTrailingLimitRE.MatchString(stripSQL(trimmed)) {
			return trimmed
		}
		return fmt.Sprintf("%s LIMIT %d", trimmed, limit)
	}
	return fmt.Sprintf("SELECT * FROM (%s) AS jamypg_q LIMIT %d", trimmed, limit)
}

func (mysqlDialect) CountWrap(sqlText string) string {
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s) AS jamypg_q", trimSQL(sqlText))
}

// MySQL/MariaDB-specific dangerous statements and functions.
func (mysqlDialect) DeniedExtras() []string {
	return []string{
		"REPLACE", "HANDLER", "SHUTDOWN", "INSTALL", "UNINSTALL",
		"RESET", "PURGE", "FLUSH", "KILL", "CHANGE",
		"OUTFILE", "DUMPFILE", "LOAD_FILE", "LOAD",
		"SLEEP", "BENCHMARK", "GET_LOCK", "RELEASE_LOCK", "MASTER_",
	}
}

// trimSQL removes surrounding whitespace and one trailing semicolon.
func trimSQL(sqlText string) string {
	return strings.TrimSuffix(strings.TrimSpace(sqlText), ";")
}
