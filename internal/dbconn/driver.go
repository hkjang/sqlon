package dbconn

import (
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib" // registers driver "pgx"
)

// Both drivers are pure Go (no CGO, no client libraries), so unlike the old
// Oracle connector there is no conditional build: every binary can reach
// PostgreSQL, MySQL, and MariaDB. The go-sql-driver/mysql import lives in
// dialect.go (non-blank, used for DSN construction) and registers "mysql".
const driverAvailable = true

const driverNote = "pure-Go drivers compiled in: pgx (postgres), go-sql-driver/mysql (mysql, mariadb)"

// openDB resolves the dialect DSN and opens the pool.
func openDB(d Dialect, p Profile, password string) (*sql.DB, error) {
	dsn, err := d.BuildDSN(p, password)
	if err != nil {
		return nil, err
	}
	return sql.Open(d.DriverName(), dsn)
}
