package postgres

import (
	"context"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Config implements observability.ConfigProvider.
type Config struct{}

const settingsSQL = `SELECT name, setting, COALESCE(pending_restart, false) AS pending_restart FROM pg_catalog.pg_settings`

func (Config) Config(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (map[string]string, map[string]bool, error) {
	rows, err := q.SystemQuery(ctx, p.ID, settingsSQL)
	if err != nil {
		return nil, nil, err
	}
	values := make(map[string]string, len(rows))
	pending := map[string]bool{}
	for _, row := range rows {
		name := observability.Text(row, "name")
		if name == "" {
			continue
		}
		values[name] = observability.Text(row, "setting")
		if pr := observability.Text(row, "pending_restart"); pr == "true" || pr == "t" || pr == "1" {
			pending[name] = true
		}
	}
	return values, pending, nil
}
