package dbconn

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // registers driver "pgx"
)

// Both drivers are pure Go (no CGO, no client libraries), so unlike the old
// Oracle connector there is no conditional build: every binary can reach
// PostgreSQL, MySQL, and MariaDB. The go-sql-driver/mysql import lives in
// dialect.go (non-blank, used for DSN construction) and registers "mysql".
const driverAvailable = true

func driverAvailableFor(d Dialect) bool {
	for _, name := range sql.Drivers() {
		if name == d.DriverName() {
			return true
		}
	}
	return false
}

func driverUnavailableError(d Dialect) error {
	if d.Name() == "oracle" {
		return fmt.Errorf("Oracle driver is not included in sqlon-standard; use the CGO-enabled sqlon-oracle build (-tags oracle) with Oracle Instant Client")
	}
	return fmt.Errorf("database driver %q for %s is not compiled in", d.DriverName(), d.Name())
}

func driverCapabilities() map[string]bool {
	out := make(map[string]bool, len(SupportedTypes))
	for _, typ := range SupportedTypes {
		d, _ := DialectFor(typ)
		out[typ] = driverAvailableFor(d)
	}
	return out
}

func driverSummary() string {
	caps := driverCapabilities()
	enabled := make([]string, 0, len(caps))
	for name, ok := range caps {
		if ok {
			enabled = append(enabled, name)
		}
	}
	sort.Strings(enabled)
	return "compiled database engines: " + strings.Join(enabled, ", ")
}

// openDB resolves the dialect DSN and opens the pool.
func openDB(d Dialect, p Profile, password string) (*sql.DB, error) {
	if !driverAvailableFor(d) {
		return nil, driverUnavailableError(d)
	}
	dsn, err := d.BuildDSN(p, password)
	if err != nil {
		return nil, err
	}
	return sql.Open(d.DriverName(), dsn)
}
