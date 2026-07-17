package mysql

import (
	"context"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Config implements observability.ConfigProvider. MySQL/MariaDB expose global
// variables through performance_schema (read-only).
type Config struct{}

const globalVarsSQL = `SELECT VARIABLE_NAME AS name, VARIABLE_VALUE AS value FROM performance_schema.global_variables`

func (Config) Config(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (map[string]string, map[string]bool, error) {
	return CollectConfig(ctx, q, p)
}

// CollectConfig is shared by MySQL and MariaDB.
func CollectConfig(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (map[string]string, map[string]bool, error) {
	rows, err := q.SystemQuery(ctx, p.ID, globalVarsSQL)
	if err != nil {
		return nil, nil, err
	}
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		name := observability.Text(row, "name")
		if name != "" {
			values[name] = observability.Text(row, "value")
		}
	}
	// MySQL/MariaDB do not report a per-variable pending-restart flag.
	return values, map[string]bool{}, nil
}
